//go:build integration

// Package integration runs the runner image against moto, an S3 emulator; it needs Docker.
package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	motoImage = "motoserver/moto:5.2.6"
	motoAlias = "moto"
	motoPort  = "5000/tcp"
	region    = "eu-west-1"

	// Moto ignores the credentials' values and serves every request from one default account,
	// unless the request names another one in the x-moto-account-id header.
	otherAccount = "111111111111"

	// The image runs as uid 1000, and /tmp is the only directory distroless lets it write to.
	scriptsDir = "/tmp"

	subtestTimeout = 2 * time.Minute
)

// runnerImage is the image under test; `make test-integration` builds it and sets RUNNER_IMAGE.
func runnerImage(t *testing.T) string {
	t.Helper()
	image := os.Getenv("RUNNER_IMAGE")
	if image == "" {
		t.Fatal("RUNNER_IMAGE is unset: run `make test-integration`, which builds the image")
	}
	return image
}

// TestRunner runs the image as the Container Driver would, against moto on a shared Docker network.
func TestRunner(t *testing.T) {
	image := runnerImage(t)

	nw, err := network.New(context.Background())
	testcontainers.CleanupNetwork(t, nw)
	require.NoError(t, err)

	moto, err := testcontainers.Run(context.Background(), motoImage,
		testcontainers.WithExposedPorts(motoPort),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/moto-api/").WithPort(motoPort)),
		network.WithNetwork([]string{motoAlias}, nw),
	)
	testcontainers.CleanupContainer(t, moto)
	require.NoError(t, err)

	endpoint, err := moto.PortEndpoint(context.Background(), motoPort, "http")
	require.NoError(t, err)
	// Independent of the runner's code: assertions go through this client only.
	s3c := newClient(endpoint, "")

	// env is what the Container Driver sets, with credentials as env vars for the default chain.
	env := func(action, bucket string) map[string]string {
		return map[string]string{
			"ACTION":                action,
			"SCRIPTS_DIRECTORY":     scriptsDir,
			"OUTPUTS_FILE":          "outputs.json", // relative to SCRIPTS_DIRECTORY
			"ERROR_FILE":            path.Join(scriptsDir, "error"),
			"BUCKET_NAME":           bucket,
			"AWS_REGION":            region,
			"AWS_ACCESS_KEY_ID":     "test",
			"AWS_SECRET_ACCESS_KEY": "test",
			"AWS_ENDPOINT_URL_S3":   "http://" + motoAlias + ":5000",
		}
	}
	run := func(ctx context.Context, t *testing.T, action, bucket string) runResult {
		t.Helper()
		return runImage(ctx, t, image, nw, env(action, bucket), nil)
	}

	scenario(t, "create", func(t *testing.T, ctx context.Context) {
		const bucket = "it-create"
		r := run(ctx, t, "create", bucket)
		require.Equal(t, 0, r.code, r.logs)
		r.assertNoFile(ctx, t, "error")

		assert.JSONEq(t, `{"bucket":"it-create","region":"eu-west-1","arn":"arn:aws:s3:::it-create"}`, r.file(ctx, t, "outputs.json"))
		assert.Contains(t, r.logs, "attempt=1")
		assert.True(t, bucketExists(ctx, t, s3c, bucket))
		loc, err := s3c.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(bucket)})
		require.NoError(t, err)
		assert.Equal(t, types.BucketLocationConstraint(region), loc.LocationConstraint)
	})

	scenario(t, "create with static credentials from a file", func(t *testing.T, ctx context.Context) {
		const bucket = "it-static-creds"
		e := env("create", bucket)
		// Without the env pair, the default chain finds nothing in the container: success means the file was used.
		delete(e, "AWS_ACCESS_KEY_ID")
		delete(e, "AWS_SECRET_ACCESS_KEY")
		e["AWS_CREDENTIALS_FILE"] = "aws-credentials.json" // relative to SCRIPTS_DIRECTORY
		files := map[string]string{
			"aws-credentials.json": `{"access_key_id":"AKIDFROMFILE","secret_access_key":"file-secret"}`,
		}

		r := runImage(ctx, t, image, nw, e, files)
		require.Equal(t, 0, r.code, r.logs)
		assert.True(t, bucketExists(ctx, t, s3c, bucket))
		assert.NotContains(t, r.logs, "file-secret")
	})

	scenario(t, "create twice is idempotent", func(t *testing.T, ctx context.Context) {
		const bucket = "it-create-twice"
		for i := range 2 {
			r := run(ctx, t, "create", bucket)
			require.Equal(t, 0, r.code, "run %d: %s", i+1, r.logs)
			assert.JSONEq(t, `{"bucket":"it-create-twice","region":"eu-west-1","arn":"arn:aws:s3:::it-create-twice"}`, r.file(ctx, t, "outputs.json"))
			if i == 1 {
				assert.Contains(t, r.logs, "bucket already exists in this account")
			}
		}
		assert.True(t, bucketExists(ctx, t, s3c, bucket))
	})

	scenario(t, "destroy an empty bucket", func(t *testing.T, ctx context.Context) {
		const bucket = "it-destroy-empty"
		createBucket(ctx, t, s3c, bucket)

		r := run(ctx, t, "destroy", bucket)
		require.Equal(t, 0, r.code, r.logs)
		r.assertNoFile(ctx, t, "outputs.json")
		r.assertNoFile(ctx, t, "error")
		assert.False(t, bucketExists(ctx, t, s3c, bucket))
	})

	scenario(t, "destroy a bucket with more than 1000 objects", func(t *testing.T, ctx context.Context) {
		const bucket, objects = "it-destroy-full", 1050
		createBucket(ctx, t, s3c, bucket)
		for i := range objects {
			_, err := s3c.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(fmt.Sprintf("dir/obj-%04d", i)),
				Body:   strings.NewReader("x"),
			})
			require.NoError(t, err)
		}

		r := run(ctx, t, "destroy", bucket)
		require.Equal(t, 0, r.code, r.logs)
		// Two pages: S3 lists at most 1000 keys per call.
		assert.Contains(t, r.logs, "count=1000 total=1000")
		assert.Contains(t, r.logs, "count=50 total=1050")
		assert.False(t, bucketExists(ctx, t, s3c, bucket))
	})

	scenario(t, "destroy a bucket that is already gone", func(t *testing.T, ctx context.Context) {
		r := run(ctx, t, "destroy", "it-never-created")
		require.Equal(t, 0, r.code, r.logs)
		r.assertNoFile(ctx, t, "error")
		assert.Contains(t, r.logs, "bucket already gone")
	})

	scenario(t, "create fails when another account owns the name", func(t *testing.T, ctx context.Context) {
		const bucket = "it-taken"
		createBucket(ctx, t, newClient(endpoint, otherAccount), bucket)

		r := run(ctx, t, "create", bucket)
		assert.Equal(t, 1, r.code, r.logs)
		assert.Equal(t, "bucket it-taken already exists and belongs to another account; bucket names are global\n", r.file(ctx, t, "error"))
		r.assertNoFile(ctx, t, "outputs.json")
	})
}

// scenario runs a subtest with a bounded context, so a hung run fails one scenario instead of the suite.
func scenario(t *testing.T, name string, f func(t *testing.T, ctx context.Context)) bool {
	t.Helper()
	return t.Run(name, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), subtestTimeout)
		defer cancel()
		f(t, ctx)
	})
}

// newClient talks to moto from the host, as account if set, otherwise as moto's default account.
func newClient(endpoint, account string) *s3.Client {
	opts := s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
	}
	if account != "" {
		opts.APIOptions = append(opts.APIOptions, smithyhttp.AddHeaderValue("x-moto-account-id", account))
	}
	return s3.New(opts)
}

func createBucket(ctx context.Context, t *testing.T, c *s3.Client, bucket string) {
	t.Helper()
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket:                    aws.String(bucket),
		CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)},
	})
	require.NoError(t, err)
}

func bucketExists(ctx context.Context, t *testing.T, c *s3.Client, bucket string) bool {
	t.Helper()
	_, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return false
	}
	require.NoError(t, err)
	return true
}

// runResult is one finished run of the image.
type runResult struct {
	ctr  testcontainers.Container
	code int
	logs string
}

// runImage runs image to completion on nw with exactly env, like the Container Driver's Job, after copying files into scriptsDir.
func runImage(ctx context.Context, t *testing.T, image string, nw *testcontainers.DockerNetwork, env, files map[string]string) runResult {
	t.Helper()
	ctrFiles := make([]testcontainers.ContainerFile, 0, len(files))
	for name, content := range files {
		ctrFiles = append(ctrFiles, testcontainers.ContainerFile{
			Reader:            strings.NewReader(content),
			ContainerFilePath: path.Join(scriptsDir, name),
			// Copied files are owned by root, so uid 1000 needs the read bit for others.
			FileMode: 0o644,
		})
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:      image,
			Env:        env,
			Networks:   []string{nw.Name},
			Files:      ctrFiles,
			WaitingFor: wait.ForExit(),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	require.NoError(t, err)

	state, err := ctr.State(ctx)
	require.NoError(t, err)
	logs, err := ctr.Logs(ctx)
	require.NoError(t, err)
	defer logs.Close()
	b, err := io.ReadAll(logs)
	require.NoError(t, err)
	return runResult{ctr: ctr, code: state.ExitCode, logs: string(b)}
}

// file returns the content of name in scriptsDir; it fails the test if the file is missing.
func (r runResult) file(ctx context.Context, t *testing.T, name string) string {
	t.Helper()
	rc, err := r.ctr.CopyFileFromContainer(ctx, path.Join(scriptsDir, name))
	require.NoError(t, err, name)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(t, err, name)
	return string(b)
}

func (r runResult) assertNoFile(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	rc, err := r.ctr.CopyFileFromContainer(ctx, path.Join(scriptsDir, name))
	if err == nil {
		_ = rc.Close()
	}
	assert.True(t, errdefs.IsNotFound(err), "%s must not exist, got err %v", name, err)
}
