package test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/gruntwork-io/terragrunt/aws_helper"
	"github.com/gruntwork-io/terragrunt/cli"
	"github.com/gruntwork-io/terragrunt/config"
	"github.com/gruntwork-io/terragrunt/remote"
	"github.com/gruntwork-io/terragrunt/util"
	terragruntDynamoDb "github.com/gruntwork-io/terragrunt/locks/dynamodb"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"bytes"
	"time"
)

// hard-code this to match the test fixture for now
const (
	TERRAFORM_REMOTE_STATE_S3_REGION    = "us-west-2"
	TEST_FIXTURE_PATH                   = "fixture/"
	TEST_FIXTURE_LOCK_PATH              = "fixture-lock/"
	TEST_FIXTURE_INCLUDE_PATH           = "fixture-include/"
	TEST_FIXTURE_INCLUDE_CHILD_REL_PATH = "qa/my-app"
	TEST_FIXTURE_STACK                  = "fixture-stack/"
	TERRAFORM_FOLDER                    = ".terraform"
	DEFAULT_TEST_REGION                 = "us-east-1"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func TestTerragruntWorksWithLocalTerraformVersion(t *testing.T) {
	t.Parallel()

	cleanupTerraformFolder(t, TEST_FIXTURE_PATH)

	s3BucketName := fmt.Sprintf("terragrunt-test-bucket-%s", strings.ToLower(uniqueId()))
	tmpTerragruntConfigPath := createTmpTerragruntConfig(t, TEST_FIXTURE_PATH, s3BucketName)

	defer deleteS3Bucket(t, TERRAFORM_REMOTE_STATE_S3_REGION, s3BucketName)
	defer cleanupTableForTest(t, "terragrunt_locks_test_fixture")

	runTerragrunt(t, fmt.Sprintf("terragrunt apply --terragrunt-non-interactive --terragrunt-config %s --terragrunt-working-dir %s", tmpTerragruntConfigPath, TEST_FIXTURE_PATH))
	validateS3BucketExists(t, TERRAFORM_REMOTE_STATE_S3_REGION, s3BucketName)
}

func TestAcquireAndReleaseLock(t *testing.T) {
	t.Parallel()

	cleanupTerraformFolder(t, TEST_FIXTURE_LOCK_PATH)

	terragruntConfigPath := path.Join(TEST_FIXTURE_LOCK_PATH, config.DefaultTerragruntConfigPath)

	defer cleanupTableForTest(t, "terragrunt_locks_test_fixture_lock")

	// Acquire a long-term lock
	runTerragrunt(t, fmt.Sprintf("terragrunt acquire-lock --terragrunt-non-interactive --terragrunt-config %s --terragrunt-working-dir %s", terragruntConfigPath, TEST_FIXTURE_LOCK_PATH))

	// Try to apply the templates. Since a lock has been acquired, and max_lock_retries is set to 1, this should
	// fail quickly.
	err := runTerragruntCommand(t, fmt.Sprintf("terragrunt apply --terragrunt-non-interactive --terragrunt-config %s --terragrunt-working-dir %s", terragruntConfigPath, TEST_FIXTURE_LOCK_PATH))

	if assert.NotNil(t, err, "Expected to get an error when trying to apply templates after a long-term lock has already been acquired, but got nil") {
		assert.Contains(t, err.Error(), "Unable to acquire lock")
	}

	// Release the lock
	runTerragrunt(t, fmt.Sprintf("terragrunt release-lock --terragrunt-non-interactive --terragrunt-config %s --terragrunt-working-dir %s", terragruntConfigPath, TEST_FIXTURE_LOCK_PATH))

	// Try to apply the templates. Since the lock has been released, this should work without errors.
	runTerragrunt(t, fmt.Sprintf("terragrunt apply --terragrunt-non-interactive --terragrunt-config %s --terragrunt-working-dir %s", terragruntConfigPath, TEST_FIXTURE_LOCK_PATH))
}

func TestTerragruntWorksWithIncludes(t *testing.T) {
	t.Parallel()

	childPath := util.JoinPath(TEST_FIXTURE_INCLUDE_PATH, TEST_FIXTURE_INCLUDE_CHILD_REL_PATH)
	cleanupTerraformFolder(t, childPath)

	s3BucketName := fmt.Sprintf("terragrunt-test-bucket-%s", strings.ToLower(uniqueId()))

	tmpTerragruntConfigPath := createTmpTerragruntConfigWithParentAndChild(t, TEST_FIXTURE_INCLUDE_PATH, TEST_FIXTURE_INCLUDE_CHILD_REL_PATH, s3BucketName)

	defer deleteS3Bucket(t, TERRAFORM_REMOTE_STATE_S3_REGION, s3BucketName)
	defer cleanupTableForTest(t, "terragrunt_locks_test_fixture_include")

	runTerragrunt(t, fmt.Sprintf("terragrunt apply --terragrunt-non-interactive --terragrunt-config %s --terragrunt-working-dir %s", tmpTerragruntConfigPath, childPath))
}

func TestTerragruntSpinUpAndTearDown(t *testing.T) {
	t.Parallel()

	s3BucketName := fmt.Sprintf("terragrunt-test-bucket-%s", strings.ToLower(uniqueId()))

	tmpEnvPath := copyEnvironment(t, TEST_FIXTURE_STACK)

	rootTerragruntConfigPath := util.JoinPath(tmpEnvPath, "fixture-stack", config.DefaultTerragruntConfigPath)
	copyTerragruntConfigAndFillPlaceholders(t, rootTerragruntConfigPath, rootTerragruntConfigPath, s3BucketName)

	mgmtEnvironmentPath := fmt.Sprintf("%s/fixture-stack/mgmt", tmpEnvPath)
	stageEnvironmentPath := fmt.Sprintf("%s/fixture-stack/stage", tmpEnvPath)

	defer deleteS3Bucket(t, TERRAFORM_REMOTE_STATE_S3_REGION, s3BucketName)
	defer cleanupTableForTest(t, "terragrunt_locks_test_fixture_stack")

	runTerragrunt(t, fmt.Sprintf("terragrunt spin-up --terragrunt-non-interactive --terragrunt-working-dir %s -var terraform_remote_state_s3_bucket=\"%s\"", mgmtEnvironmentPath, s3BucketName))
	runTerragrunt(t, fmt.Sprintf("terragrunt spin-up --terragrunt-non-interactive --terragrunt-working-dir %s -var terraform_remote_state_s3_bucket=\"%s\"", stageEnvironmentPath, s3BucketName))

	runTerragrunt(t, fmt.Sprintf("terragrunt tear-down --terragrunt-non-interactive --terragrunt-working-dir %s -var terraform_remote_state_s3_bucket=\"%s\"", stageEnvironmentPath, s3BucketName))
	runTerragrunt(t, fmt.Sprintf("terragrunt tear-down --terragrunt-non-interactive --terragrunt-working-dir %s -var terraform_remote_state_s3_bucket=\"%s\"", mgmtEnvironmentPath, s3BucketName))
}

func cleanupTerraformFolder(t *testing.T, templatesPath string) {
	terraformFolder := util.JoinPath(templatesPath, TERRAFORM_FOLDER)
	if !util.FileExists(terraformFolder) {
		return
	}

	if err := os.RemoveAll(terraformFolder); err != nil {
		t.Fatalf("Error while removing %s folder: %v", terraformFolder, err)
	}
}

func runTerragruntCommand(t *testing.T, command string) error {
	args := strings.Split(command, " ")

	app := cli.CreateTerragruntCli("TEST")
	return app.Run(args)
}

func runTerragrunt(t *testing.T, command string) {
	if err := runTerragruntCommand(t, command); err != nil {
		t.Fatalf("Failed to run Terragrunt command '%s' due to error: %s", command, err)
	}
}

func copyEnvironment(t *testing.T, environmentPath string) string {
	tmpDir, err := ioutil.TempDir("", "terragrunt-stack-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir due to error: %v", err)
	}

	err = filepath.Walk(environmentPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		destPath := util.JoinPath(tmpDir, path)

		destPathDir := filepath.Dir(destPath)
		if err := os.MkdirAll(destPathDir, 0777); err != nil {
			return err
		}

		return copyFile(path, destPath)
	})

	if err != nil {
		t.Fatalf("Error walking file path %s due to error: %v", environmentPath, err)
	}

	return tmpDir
}

func copyFile(srcPath string, destPath string) error {
	contents, err := ioutil.ReadFile(srcPath)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(destPath, contents, 0644)
}

func createTmpTerragruntConfigWithParentAndChild(t *testing.T, parentPath string, childRelPath string, s3BucketName string) string {
	tmpDir, err := ioutil.TempDir("", "terragrunt-parent-child-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir due to error: %v", err)
	}

	childDestPath := util.JoinPath(tmpDir, childRelPath)

	if err := os.MkdirAll(childDestPath, 0777); err != nil {
		t.Fatalf("Failed to create temp dir %s due to error %v", childDestPath, err)
	}

	parentTerragruntSrcPath := util.JoinPath(parentPath, config.DefaultTerragruntConfigPath)
	parentTerragruntDestPath := util.JoinPath(tmpDir, config.DefaultTerragruntConfigPath)
	copyTerragruntConfigAndFillPlaceholders(t, parentTerragruntSrcPath, parentTerragruntDestPath, s3BucketName)

	childTerragruntSrcPath := util.JoinPath(util.JoinPath(parentPath, childRelPath), config.DefaultTerragruntConfigPath)
	childTerragruntDestPath := util.JoinPath(childDestPath, config.DefaultTerragruntConfigPath)
	copyTerragruntConfigAndFillPlaceholders(t, childTerragruntSrcPath, childTerragruntDestPath, s3BucketName)

	return childTerragruntDestPath
}

func createTmpTerragruntConfig(t *testing.T, templatesPath string, s3BucketName string) string {
	tmpTerragruntConfigFile, err := ioutil.TempFile("", config.DefaultTerragruntConfigPath)
	if err != nil {
		t.Fatalf("Failed to create temp file due to error: %v", err)
	}

	originalTerragruntConfigPath := path.Join(templatesPath, config.DefaultTerragruntConfigPath)
	copyTerragruntConfigAndFillPlaceholders(t, originalTerragruntConfigPath, tmpTerragruntConfigFile.Name(), s3BucketName)

	return tmpTerragruntConfigFile.Name()
}

func copyTerragruntConfigAndFillPlaceholders(t *testing.T, configSrcPath string, configDestPath string, s3BucketName string) {
	originalContents, err := util.ReadFileAsString(configSrcPath)
	if err != nil {
		t.Fatalf("Error reading Terragrunt config at %s: %v", configSrcPath, err)
	}

	newContents := strings.Replace(originalContents, "__FILL_IN_BUCKET_NAME__", s3BucketName, -1)

	if err := ioutil.WriteFile(configDestPath, []byte(newContents), 0444); err != nil {
		t.Fatalf("Error writing temp Terragrunt config to %s: %v", configDestPath, err)
	}
}

// Returns a unique (ish) id we can attach to resources and tfstate files so they don't conflict with each other
// Uses base 62 to generate a 6 character string that's unlikely to collide with the handful of tests we run in
// parallel. Based on code here: http://stackoverflow.com/a/9543797/483528
func uniqueId() string {
	const BASE_62_CHARS = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	const UNIQUE_ID_LENGTH = 6 // Should be good for 62^6 = 56+ billion combinations

	var out bytes.Buffer


	for i := 0; i < UNIQUE_ID_LENGTH; i++ {
		out.WriteByte(BASE_62_CHARS[rand.Intn(len(BASE_62_CHARS))])
	}

	return out.String()
}


// Check that the S3 Bucket of the given name and region exists. Terragrunt should create this bucket during the test.
func validateS3BucketExists(t *testing.T, awsRegion string, bucketName string) {
	s3Client, err := remote.CreateS3Client(awsRegion)
	if err != nil {
		t.Fatalf("Error creating S3 client: %v", err)
	}

	remoteStateConfig := remote.RemoteStateConfigS3{Bucket: bucketName, Region: awsRegion}
	assert.True(t, remote.DoesS3BucketExist(s3Client, &remoteStateConfig), "Terragrunt failed to create remote state S3 bucket %s", bucketName)
}

// Delete the specified S3 bucket to clean up after a test
func deleteS3Bucket(t *testing.T, awsRegion string, bucketName string) {
	s3Client, err := remote.CreateS3Client(awsRegion)
	if err != nil {
		t.Fatalf("Error creating S3 client: %v", err)
	}

	t.Logf("Deleting test s3 bucket %s", bucketName)

	out, err := s3Client.ListObjectVersions(&s3.ListObjectVersionsInput{Bucket: aws.String(bucketName)})
	if err != nil {
		t.Fatalf("Failed to list object versions in s3 bucket %s: %v", bucketName, err)
	}

	objectIdentifiers := []*s3.ObjectIdentifier{}
	for _, version := range out.Versions {
		objectIdentifiers = append(objectIdentifiers, &s3.ObjectIdentifier{
			Key:       version.Key,
			VersionId: version.VersionId,
		})
	}

	if len(objectIdentifiers) > 0 {
		deleteInput := &s3.DeleteObjectsInput{
			Bucket: aws.String(bucketName),
			Delete: &s3.Delete{Objects: objectIdentifiers},
		}
		if _, err := s3Client.DeleteObjects(deleteInput); err != nil {
			t.Fatalf("Error deleting all versions of all objects in bucket %s: %v", bucketName, err)
		}
	}

	if _, err := s3Client.DeleteBucket(&s3.DeleteBucketInput{Bucket: aws.String(bucketName)}); err != nil {
		t.Fatalf("Failed to delete S3 bucket %s: %v", bucketName, err)
	}
}

// Create an authenticated client for DynamoDB
func createDynamoDbClient(awsRegion string) (*dynamodb.DynamoDB, error) {
	config, err := aws_helper.CreateAwsConfig(awsRegion)
	if err != nil {
		return nil, err
	}

	return dynamodb.New(session.New(), config), nil
}

func createDynamoDbClientForTest(t *testing.T) *dynamodb.DynamoDB {
	client, err := createDynamoDbClient(DEFAULT_TEST_REGION)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func cleanupTableForTest(t *testing.T, tableName string) {
	client := createDynamoDbClientForTest(t)
	err := terragruntDynamoDb.DeleteTable(tableName, client)
	assert.Nil(t, err, "Unexpected error: %v", err)
}
