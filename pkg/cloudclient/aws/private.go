package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"time"

	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go/aws/awserr"
	ocmlog "github.com/openshift-online/ocm-sdk-go/logging"
	"github.com/openshift/osd-network-verifier/pkg/helpers"
)

var (
	instanceCount int = 1
	defaultAmi        = map[string]string{
		// using Amazon Linux 2 AMI (HVM) - Kernel 5.10
		"us-east-1":      "ami-0ed9277fb7eb570c9",
		"us-east-2":      "ami-002068ed284fb165b",
		"us-west-1":      "ami-03af6a70ccd8cb578",
		"us-west-2":      "ami-00f7e5c52c0f43726",
		"ca-central-1":   "ami-0bae7412735610274",
		"eu-north-1":     "ami-06bfd6343550d4a29",
		"eu-central-1":   "ami-05d34d340fb1d89e5",
		"eu-west-1":      "ami-04dd4500af104442f",
		"eu-west-2":      "ami-0d37e07bd4ff37148",
		"eu-west-3":      "ami-0d3c032f5934e1b41",
		"eu-south-1":     "",
		"ap-northeast-1": "",
		"ap-northeast-2": "",
		"ap-northeast-3": "",
		"ap-east-1":      "",
		"ap-south-1":     "",
		"ap-southeast-1": "",
		"ap-southeast-2": "",
		"sa-east-1":      "",
		"af-south-1":     "",
		"me-south-1":     "",
	}
	// TODO find a location for future docker images
	networkValidatorImage string = "quay.io/app-sre/osd-network-verifier:v81-bf8360e"
	userdataEndVerifier   string = "USERDATA END"
)

func newClient(ctx context.Context, logger ocmlog.Logger, accessID, accessSecret, sessiontoken, region, instanceType string, tags map[string]string) (*Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.StaticCredentialsProvider{
			Value: aws.Credentials{
				AccessKeyID: accessID, SecretAccessKey: accessSecret, SessionToken: sessiontoken,
			},
		}),
	)
	if err != nil {
		return nil, err
	}

	c := &Client{
		ec2Client:    ec2.NewFromConfig(cfg),
		region:       region,
		instanceType: instanceType,
		tags:         tags,
		logger:       logger,
	}

	// Validates the provided instance type will work with the verifier
	// NOTE a "nitro" EC2 instance type is required to be used
	if err := c.validateInstanceType(ctx); err != nil {
		return nil, fmt.Errorf("Instance type %s is invalid: %s", c.instanceType, err)
	}

	return c, nil
}

func buildTags(tags map[string]string) []ec2Types.TagSpecification {
	tagList := []ec2Types.Tag{}
	for k, v := range tags {
		t := ec2Types.Tag{
			Key:   aws.String(k),
			Value: aws.String(v),
		}
		tagList = append(tagList, t)
	}

	tagSpec := ec2Types.TagSpecification{
		ResourceType: ec2Types.ResourceTypeInstance,
		Tags:         tagList,
	}

	return []ec2Types.TagSpecification{tagSpec}
}

func (c Client) validateInstanceType(ctx context.Context) error {
	// Describe the provided instance type only
	//      https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/ec2#DescribeInstanceTypesInput
	descInput := ec2.DescribeInstanceTypesInput{
		InstanceTypes: []ec2Types.InstanceType{ec2Types.InstanceType(c.instanceType)},
	}

	c.logger.Debug(ctx, "Gathering description of instance type %s from EC2", c.instanceType)
	descOut, err := c.ec2Client.DescribeInstanceTypes(ctx, &descInput)
	if err != nil {
		// Check for invalid instance type error and return a cleaner error
		re := regexp.MustCompile("400.*api error InvalidInstanceType")
		if re.Match([]byte(err.Error())) {
			err = fmt.Errorf("Instance type %s does not exist", c.instanceType)
		}
		return fmt.Errorf("Unable to gather list of supported instance types from EC2: %s", err)
	}
	c.logger.Debug(ctx, "Full describe instance types output contains %d instance types", len(descOut.InstanceTypes))

	found := false
	for _, t := range descOut.InstanceTypes {
		if string(t.InstanceType) == c.instanceType {
			found = true
			if t.Hypervisor != ec2Types.InstanceTypeHypervisorNitro {
				return fmt.Errorf("Instance type must use hypervisor type 'nitro' to support reliable result collection")
			}
			c.logger.Debug(ctx, "Instance type %s has hypervisor %s", c.instanceType, t.Hypervisor)
			break
		}
	}

	if !found {
		return fmt.Errorf("Instance type %s not found in EC2 API", c.instanceType)
	}

	return nil
}

func (c Client) createEC2Instance(ctx context.Context, amiID string, instanceCount int, vpcSubnetID, userdata string, tags map[string]string) (ec2.RunInstancesOutput, error) {
	// Build our request, converting the go base types into the pointers required by the SDK
	instanceReq := ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		MaxCount:     aws.Int32(int32(instanceCount)),
		MinCount:     aws.Int32(int32(instanceCount)),
		InstanceType: ec2Types.InstanceType(c.instanceType),
		// Because we're making this VPC aware, we also have to include a network interface specification
		NetworkInterfaces: []ec2Types.InstanceNetworkInterfaceSpecification{
			{
				AssociatePublicIpAddress: aws.Bool(true),
				DeviceIndex:              aws.Int32(0),
				SubnetId:                 aws.String(vpcSubnetID),
			},
		},
		UserData:          aws.String(userdata),
		TagSpecifications: buildTags(tags),
	}
	// Finally, we make our request
	instanceResp, err := c.ec2Client.RunInstances(ctx, &instanceReq)
	if err != nil {
		return ec2.RunInstancesOutput{}, err
	}

	for _, i := range instanceResp.Instances {
		c.logger.Info(ctx, "Created instance with ID: %s", *i.InstanceId)
	}

	return *instanceResp, nil
}

// Returns state code as int
func (c Client) describeEC2Instances(ctx context.Context, instanceID string) (int, error) {
	// States and codes
	// 0 : pending
	// 16 : running
	// 32 : shutting-down
	// 48 : terminated
	// 64 : stopping
	// 80 : stopped
	// 401 : failed
	result, err := c.ec2Client.DescribeInstanceStatus(ctx, &ec2.DescribeInstanceStatusInput{
		InstanceIds: []string{instanceID},
	})

	if err != nil {
		c.logger.Error(ctx, "Errors while describing the instance status: %s", err.Error())
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == "UnauthorizedOperation" {
				return 401, err
			}
		}
		return 0, err
	}

	if len(result.InstanceStatuses) > 1 {
		return 0, errors.New("more than one EC2 instance found")
	}

	if len(result.InstanceStatuses) == 0 {
		// Don't return an error here as if the instance is still too new, it may not be
		// returned at all.
		//return 0, errors.New("no EC2 instances found")
		c.logger.Debug(ctx, "Instance %s has no status yet", instanceID)
		return 0, nil
	}

	return int(*result.InstanceStatuses[0].InstanceState.Code), nil
}

func (c Client) waitForEC2InstanceCompletion(ctx context.Context, instanceID string) error {
	//wait for the instance to run
	totalWait := 25 * 60
	currentWait := 1
	// Double the wait time until we reach totalWait seconds
	for totalWait > 0 {
		currentWait = currentWait * 2
		if currentWait > totalWait {
			currentWait = totalWait
		}
		totalWait -= currentWait
		time.Sleep(time.Duration(currentWait) * time.Second)
		code, descError := c.describeEC2Instances(ctx, instanceID)
		if code == 16 { // 16 represents a successful region initialization
			// Instance is running, break
			break
		} else if code == 401 { // 401 represents an UnauthorizedOperation error
			// Missing permission to perform operations, account needs to fail
			return fmt.Errorf("missing required permissions for account: %s", descError)
		}

		if descError != nil {
			// Log an error and make sure that instance is terminated
			descErrorMsg := fmt.Sprintf("Could not get EC2 instance state, terminating instance %s", instanceID)

			if descError, ok := descError.(awserr.Error); ok {
				descErrorMsg = fmt.Sprintf("Could not get EC2 instance state: %s, terminating instance %s", descError.Code(), instanceID)
			}

			return fmt.Errorf("%s: %s", descError, descErrorMsg)
		}
	}

	c.logger.Info(ctx, "EC2 Instance: %s Running", instanceID)
	return nil
}

func generateUserData(variables map[string]string) (string, error) {
	template, err := os.ReadFile("config/userdata.yaml")
	if err != nil {
		return "", err
	}
	variableMapper := func(varName string) string {
		return variables[varName]
	}
	data := os.Expand(string(template), variableMapper)

	return base64.StdEncoding.EncodeToString([]byte(data)), nil
}

func (c Client) findUnreachableEndpoints(ctx context.Context, instanceID string) ([]string, error) {
	var match []string

	// Compile the regular expressions once
	reVerify := regexp.MustCompile(userdataEndVerifier)
	reUnreachableErrors := regexp.MustCompile(`Unable to reach (\S+)`)

	latest := true
	input := ec2.GetConsoleOutputInput{
		InstanceId: &instanceID,
		Latest:     &latest,
	}

	err := helpers.PollImmediate(30*time.Second, 10*time.Minute, func() (bool, error) {
		output, err := c.ec2Client.GetConsoleOutput(ctx, &input)
		if err == nil && output.Output != nil {
			// First, gather the ec2 console output
			scriptOutput, err := base64.StdEncoding.DecodeString(*output.Output)
			if err != nil {
				// unable to decode output. we will try again
				c.logger.Debug(ctx, "Error while collecting console output, will retry on next check interval: %s", err)
				return false, nil
			}

			// In the early stages, an ec2 instance may be running but the console is not populated with any data, retry if that is the case
			if len(scriptOutput) < 1 {
				c.logger.Debug(ctx, "EC2 console output not yet populated with data, continuing to wait...")
				return false, nil
			}

			// Check for the specific string we output in the gerated userdata file at the end to verify the userdata script has run
			// It is possible we get EC2 console output, but the userdata script has not yet completed.
			verifyMatch := reVerify.FindString(string(scriptOutput))
			if len(verifyMatch) < 1 {
				c.logger.Debug(ctx, "EC2 console output contains data, but end of userdata script not seen, continuing to wait...")
				return false, nil
			}

			// If debug logging is enabled, output the full console log that appears to include the full userdata run
			c.logger.Debug(ctx, "Full EC2 console output:\n---\n%s\n---", scriptOutput)

			match = reUnreachableErrors.FindAllString(string(scriptOutput), -1)

			return true, nil
		}
		c.logger.Debug(ctx, "Waiting for UserData script to complete...")
		return false, nil
	})
	return match, err
}

func (c Client) terminateEC2Instance(ctx context.Context, instanceID string) error {
	input := ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	}
	_, err := c.ec2Client.TerminateInstances(ctx, &input)
	if err != nil {
		c.logger.Error(ctx, "Unable to terminate EC2 instance: %s", err.Error())
		return err
	}

	return nil
}

func (c *Client) validateEgress(ctx context.Context, vpcSubnetID, cloudImageID string, timeout time.Duration) error {
	c.logger.Debug(ctx, "Using configured timeout of %s for each egress request", timeout.String())
	// Generate the userData file
	userDataVariables := map[string]string{
		"AWS_REGION":               c.region,
		"USERDATA_BEGIN":           "USERDATA BEGIN",
		"USERDATA_END":             userdataEndVerifier,
		"VALIDATOR_START_VERIFIER": "VALIDATOR START",
		"VALIDATOR_END_VERIFIER":   "VALIDATOR END",
		"VALIDATOR_IMAGE":          networkValidatorImage,
		"TIMEOUT":                  timeout.String(),
	}
	userData, err := generateUserData(userDataVariables)
	if err != nil {
		err = fmt.Errorf("Unable to generate UserData file: %s", err.Error())
		return err
	}
	c.logger.Debug(ctx, "Base64-encoded generated userdata script:\n---\n%s\n---", userData)

	// If a cloud image wasn't provided by the caller,
	if cloudImageID == "" {
		// use defaultAmi for the region instead
		cloudImageID = defaultAmi[c.region]

		if cloudImageID == "" {
			return fmt.Errorf("No default AMI found for region %s ", c.region)
		}
	}

	// Create an ec2 instance
	instance, err := c.createEC2Instance(ctx, cloudImageID, instanceCount, vpcSubnetID, userData, c.tags)
	if err != nil {
		err = fmt.Errorf("Unable to create EC2 Instance: %s", err.Error())
		return err
	}
	instanceID := *instance.Instances[0].InstanceId

	// Wait for the ec2 instance to be running
	c.logger.Debug(ctx, "Waiting for EC2 instance %s to be running", instanceID)
	err = c.waitForEC2InstanceCompletion(ctx, instanceID)
	if err != nil {
		err = fmt.Errorf("Error while waiting for EC2 instance to start: %s", err.Error())
		return err
	}
	c.logger.Info(ctx, "Gathering and parsing console log output...")
	unreachableEndpoints, err := c.findUnreachableEndpoints(ctx, instanceID)
	if err != nil {
		c.logger.Error(ctx, "Error parsing output from console log: %s", err.Error())
		return err
	}

	c.logger.Info(ctx, "Terminating ec2 instance with id %s", instanceID)
	if err := c.terminateEC2Instance(ctx, instanceID); err != nil {
		err = fmt.Errorf("Error terminating instances: %s", err.Error())
		return err
	}
	if len(unreachableEndpoints) > 0 {
		return fmt.Errorf("multiple targets unreachable %q", unreachableEndpoints)
	}
	return nil
}
