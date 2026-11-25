package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBucket = "demo-bucket"

// fakeS3 is an in-memory s3API: its fields script what S3 returns and record what the runner sent.
type fakeS3 struct {
	createErr    error
	createInputs []*s3.CreateBucketInput

	// headErrs[i] is the result of the (i+1)th HeadBucket call (nil means 200); the last one repeats.
	headErrs  []error
	headCalls int

	// pages are the bucket's keys as ListObjectsV2 returns them, one page per call.
	pages       [][]string
	listErr     error
	listMaxKeys int32
	// failKeys get a per-key AccessDenied in the DeleteObjects response.
	failKeys      []string
	deleteBatches [][]string

	deleteBucketErr error
}

// apiErr wraps err the way the SDK does, so callers see an OperationError around the API error.
func apiErr(op string, err error) error {
	return &smithy.OperationError{ServiceID: "S3", OperationName: op, Err: err}
}

var errNotFound = apiErr("HeadBucket", &types.NotFound{})

func (f *fakeS3) CreateBucket(_ context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.createInputs = append(f.createInputs, in)
	return &s3.CreateBucketOutput{}, f.createErr
}

func (f *fakeS3) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.headCalls++
	if len(f.headErrs) > 0 {
		if err := f.headErrs[min(f.headCalls, len(f.headErrs))-1]; err != nil {
			return nil, err
		}
	}
	return &s3.HeadBucketOutput{}, nil
}

// ListObjectsV2 returns one page per call; the continuation token is the next page's index.
func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.listMaxKeys = aws.ToInt32(in.MaxKeys)
	i, _ := strconv.Atoi(aws.ToString(in.ContinuationToken)) // no token: first page
	out := &s3.ListObjectsV2Output{}
	if i < len(f.pages) {
		for _, k := range f.pages[i] {
			out.Contents = append(out.Contents, types.Object{Key: aws.String(k)})
		}
	}
	if i+1 < len(f.pages) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(strconv.Itoa(i + 1))
	}
	return out, nil
}

func (f *fakeS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	var batch []string
	out := &s3.DeleteObjectsOutput{}
	for _, id := range in.Delete.Objects {
		batch = append(batch, aws.ToString(id.Key))
		if slices.Contains(f.failKeys, aws.ToString(id.Key)) {
			out.Errors = append(out.Errors, types.Error{Key: id.Key, Code: aws.String("AccessDenied"), Message: aws.String("Access Denied")})
		}
	}
	f.deleteBatches = append(f.deleteBatches, batch)
	return out, nil
}

func (f *fakeS3) DeleteBucket(context.Context, *s3.DeleteBucketInput, ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	return &s3.DeleteBucketOutput{}, f.deleteBucketErr
}

// newTestRunner returns a runner with millisecond backoff and a buffer holding its logs.
func newTestRunner(f *fakeS3) (*bucketRunner, *bytes.Buffer) {
	var logs bytes.Buffer
	r := newBucketRunner(f, newLogger(&logs))
	r.firstDelay, r.maxDelay = time.Millisecond, 4*time.Millisecond
	return r, &logs
}

func TestWaitUntil(t *testing.T) {
	tests := []struct {
		name       string
		wantExists bool
		headErrs   []error
		timeout    time.Duration
		wantCalls  int
		wantErr    string
	}{
		{name: "ready on first try", wantExists: true, wantCalls: 1},
		{name: "ready after 3 tries", wantExists: true, headErrs: []error{errNotFound, errNotFound, nil}, wantCalls: 3},
		{name: "gone on first try", wantExists: false, headErrs: []error{errNotFound}, wantCalls: 1},
		{name: "gone after 2 tries", wantExists: false, headErrs: []error{nil, errNotFound}, wantCalls: 2},
		{name: "non-404 error stops polling", wantExists: true, headErrs: []error{errNotFound, apiErr("HeadBucket", &smithy.GenericAPIError{Code: "AccessDenied"})}, wantCalls: 2, wantErr: "AccessDenied"},
		{name: "context timeout", wantExists: true, headErrs: []error{errNotFound}, timeout: 30 * time.Millisecond, wantErr: "context deadline exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeS3{headErrs: tt.headErrs}
			r, logs := newTestRunner(f)
			ctx := t.Context()
			if tt.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.timeout)
				defer cancel()
			}

			err := r.waitUntil(ctx, testBucket, tt.wantExists)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			if tt.wantCalls > 0 {
				assert.Equal(t, tt.wantCalls, f.headCalls)
				// Every successful attempt is logged with its number.
				polled := tt.wantCalls
				if tt.wantErr != "" {
					polled--
				}
				assert.Equal(t, polled, strings.Count(logs.String(), "msg=\"polled bucket\""), logs.String())
				assert.Contains(t, logs.String(), "attempt=1")
			} else {
				assert.Greater(t, f.headCalls, 1, "kept polling until the deadline")
			}
		})
	}
}

func TestWaitUntilBacksOff(t *testing.T) {
	f := &fakeS3{headErrs: []error{errNotFound, errNotFound, errNotFound, errNotFound, nil}}
	r, _ := newTestRunner(f)
	r.firstDelay, r.maxDelay = 10*time.Millisecond, 20*time.Millisecond

	start := time.Now()
	require.NoError(t, r.waitUntil(t.Context(), testBucket, true))

	// 10ms, then 20ms capped three times: at least 70ms between the first and the fifth attempt.
	assert.GreaterOrEqual(t, time.Since(start), 70*time.Millisecond)
	assert.Equal(t, 5, f.headCalls)
}

func TestCreate(t *testing.T) {
	tests := []struct {
		name             string
		region           string
		createErr        error
		wantConstraint   types.BucketLocationConstraint
		wantNoConstraint bool
		wantErr          string
		wantLogs         string
	}{
		{
			name:           "new bucket",
			region:         "eu-west-1",
			wantConstraint: "eu-west-1",
			wantLogs:       "bucket creation requested",
		},
		{
			name:             "us-east-1 sends no location constraint",
			region:           "us-east-1",
			wantNoConstraint: true,
		},
		{
			name:           "already owned by you is success",
			region:         "eu-west-1",
			createErr:      apiErr("CreateBucket", &types.BucketAlreadyOwnedByYou{}),
			wantConstraint: "eu-west-1",
			wantLogs:       "bucket already exists in this account",
		},
		{
			name:           "already exists in another account fails",
			region:         "eu-west-1",
			createErr:      apiErr("CreateBucket", &types.BucketAlreadyExists{}),
			wantConstraint: "eu-west-1",
			wantErr:        "already exists and belongs to another account",
		},
		{
			name:           "other create error fails",
			region:         "eu-west-1",
			createErr:      apiErr("CreateBucket", &smithy.GenericAPIError{Code: "AccessDenied"}),
			wantConstraint: "eu-west-1",
			wantErr:        "creating bucket demo-bucket",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeS3{createErr: tt.createErr}
			r, logs := newTestRunner(f)

			err := r.create(t.Context(), testBucket, tt.region)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, f.createInputs, 1)
			in := f.createInputs[0]
			assert.Equal(t, testBucket, aws.ToString(in.Bucket))
			if tt.wantNoConstraint {
				assert.Nil(t, in.CreateBucketConfiguration)
			} else {
				require.NotNil(t, in.CreateBucketConfiguration)
				assert.Equal(t, tt.wantConstraint, in.CreateBucketConfiguration.LocationConstraint)
			}
			assert.Zero(t, f.headCalls, "create doesn't poll; run calls waitUntil")
			assert.Contains(t, logs.String(), tt.wantLogs)
		})
	}
}

func TestDestroy(t *testing.T) {
	noSuchBucketList := apiErr("ListObjectsV2", &types.NoSuchBucket{})
	// DeleteBucket has no modeled NoSuchBucket, so the SDK returns a generic API error.
	noSuchBucketDelete := apiErr("DeleteBucket", &smithy.GenericAPIError{Code: "NoSuchBucket"})

	tests := []struct {
		name        string
		fake        fakeS3
		wantBatches [][]string
		wantErr     string
	}{
		{name: "empty bucket"},
		{
			name:        "one DeleteObjects call per page",
			fake:        fakeS3{pages: [][]string{{"a", "b"}, {"c", "d"}, {"e"}}},
			wantBatches: [][]string{{"a", "b"}, {"c", "d"}, {"e"}},
		},
		{
			name: "bucket already gone",
			fake: fakeS3{listErr: noSuchBucketList, deleteBucketErr: noSuchBucketDelete},
		},
		{
			name:        "bucket deleted between listing and DeleteBucket",
			fake:        fakeS3{pages: [][]string{{"a", "b"}}, deleteBucketErr: noSuchBucketDelete},
			wantBatches: [][]string{{"a", "b"}},
		},
		{
			name:        "per-key delete errors fail the run",
			fake:        fakeS3{pages: [][]string{{"a", "b", "c", "d"}}, failKeys: []string{"b", "d"}},
			wantBatches: [][]string{{"a", "b", "c", "d"}},
			wantErr:     `2 of 4 keys failed, first "b": AccessDenied`,
		},
		{
			name:    "list error fails the run",
			fake:    fakeS3{listErr: apiErr("ListObjectsV2", &smithy.GenericAPIError{Code: "AccessDenied"})},
			wantErr: "listing objects in demo-bucket",
		},
		{
			name:    "DeleteBucket error fails the run",
			fake:    fakeS3{deleteBucketErr: apiErr("DeleteBucket", &smithy.GenericAPIError{Code: "BucketNotEmpty"})},
			wantErr: "BucketNotEmpty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &tt.fake
			r, _ := newTestRunner(f)

			err := r.destroy(t.Context(), testBucket)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantBatches, f.deleteBatches)
			if f.listErr == nil {
				assert.Equal(t, int32(1000), f.listMaxKeys, "asks for S3's per-call maximum, which DeleteObjects also accepts")
			}
		})
	}
}

func TestNewS3ClientCredentials(t *testing.T) {
	// Keep the developer's ~/.aws out of the test; the env pair is what the default chain finds first.
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "none"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "ENVKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret")
	credsFile := filepath.Join(dir, "aws-credentials.json")
	require.NoError(t, os.WriteFile(credsFile, []byte(`{"access_key_id":"FILEKEY","secret_access_key":"file-secret"}`), 0o600))

	tests := []struct {
		name       string
		env        map[string]string
		wantKey    string
		wantSource string
		wantLog    string
	}{
		{name: "static credentials from the file", env: map[string]string{"AWS_CREDENTIALS_FILE": credsFile}, wantKey: "FILEKEY", wantSource: "StaticCredentials", wantLog: "using static credentials"},
		{name: "default chain without the file", env: map[string]string{}, wantKey: "ENVKEY", wantSource: "EnvConfigCredentials", wantLog: "using the default credential chain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			client, err := newS3Client(t.Context(), envFrom(tt.env), newLogger(&logs))
			require.NoError(t, err)
			assert.Contains(t, logs.String(), tt.wantLog)

			got, err := client.(*s3.Client).Options().Credentials.Retrieve(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.wantKey, got.AccessKeyID)
			assert.Equal(t, tt.wantSource, got.Source)
		})
	}
}

func TestReadStaticCredentials(t *testing.T) {
	tests := []struct {
		name    string
		content string // empty means the file doesn't exist
		wantErr string
	}{
		{name: "valid", content: `{"access_key_id":"AKIDEXAMPLE","secret_access_key":"s3cr3t-value"}`},
		{name: "missing file", wantErr: "reading AWS_CREDENTIALS_FILE"},
		{name: "not JSON", content: `access_key_id=AKIDEXAMPLE s3cr3t-value`, wantErr: "must be a JSON object"},
		{name: "missing secret", content: `{"access_key_id":"AKIDEXAMPLE"}`, wantErr: "must be a JSON object"},
		{name: "empty key", content: `{"access_key_id":"","secret_access_key":"s3cr3t-value"}`, wantErr: "must be a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "aws-credentials.json")
			if tt.content != "" {
				require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			}

			creds, err := readStaticCredentials(path)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.NotContains(t, err.Error(), "s3cr3t", "the secret never ends up in the error")
				return
			}
			require.NoError(t, err)
			got, err := creds.Retrieve(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "AKIDEXAMPLE", got.AccessKeyID)
			assert.Equal(t, "s3cr3t-value", got.SecretAccessKey)
			assert.Empty(t, got.SessionToken)
		})
	}
}
