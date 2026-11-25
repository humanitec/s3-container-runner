package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envFrom turns a map into a getenv function.
func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// fakeFactory returns f as the S3 client.
func fakeFactory(f *fakeS3) clientFactory {
	return func(context.Context, func(string) string, *slog.Logger) (s3API, error) { return f, nil }
}

func baseEnv(dir, action string) map[string]string {
	return map[string]string{
		"ACTION":            action,
		"SCRIPTS_DIRECTORY": dir,
		"OUTPUTS_FILE":      "outputs.json",
		"BUCKET_NAME":       testBucket,
		"AWS_REGION":        "eu-west-1",
	}
}

func TestRunCreateWritesOutputs(t *testing.T) {
	t.Chdir(t.TempDir()) // run enters SCRIPTS_DIRECTORY; t.Chdir restores the working directory.
	dir := t.TempDir()
	var stderr bytes.Buffer

	err := run(t.Context(), nil, envFrom(baseEnv(dir, "create")), &stderr, fakeFactory(&fakeS3{}))
	require.NoError(t, err, stderr.String())

	// OUTPUTS_FILE is relative, so it lands in SCRIPTS_DIRECTORY.
	path := filepath.Join(dir, "outputs.json")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"bucket":"demo-bucket","region":"eu-west-1","arn":"arn:aws:s3:::demo-bucket"}`, string(b))
	assert.Contains(t, stderr.String(), "action=create bucket=demo-bucket region=eu-west-1")
	assert.Contains(t, stderr.String(), `msg="polled bucket" bucket=demo-bucket attempt=1`, "run polls after create")
	assert.NotContains(t, stderr.String(), "time=", "the time attribute is dropped")
}

func TestRunDestroyWritesNoOutputs(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	var stderr bytes.Buffer
	f := &fakeS3{pages: [][]string{{"a", "b", "c"}}, headErrs: []error{errNotFound}}

	err := run(t.Context(), nil, envFrom(baseEnv(dir, "destroy")), &stderr, fakeFactory(f))
	require.NoError(t, err, stderr.String())

	assert.Equal(t, [][]string{{"a", "b", "c"}}, f.deleteBatches)
	assert.Contains(t, stderr.String(), "bucket deletion requested")
	assert.NoFileExists(t, filepath.Join(dir, "outputs.json"))
	assert.Contains(t, stderr.String(), `msg="polled bucket" bucket=demo-bucket attempt=1`, "run polls after destroy")
	assert.Contains(t, stderr.String(), "bucket destroyed")
}

func TestRunInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     func(m map[string]string)
		factory clientFactory
		wantErr string
	}{
		{name: "missing action", env: func(m map[string]string) { delete(m, "ACTION") }, wantErr: "no action: set ACTION"},
		{name: "unknown action", env: func(m map[string]string) { m["ACTION"] = "update" }, wantErr: `unsupported action "update"`},
		{name: "missing bucket", env: func(m map[string]string) { delete(m, "BUCKET_NAME") }, wantErr: "BUCKET_NAME is required"},
		{name: "missing region", env: func(m map[string]string) { delete(m, "AWS_REGION") }, wantErr: "AWS_REGION is required"},
		{name: "missing scripts directory", env: func(m map[string]string) { m["SCRIPTS_DIRECTORY"] = "/does/not/exist" }, wantErr: "entering SCRIPTS_DIRECTORY"},
		{name: "action flag is not supported", args: []string{"-action", "create"}, wantErr: "flag provided but not defined: -action"},
		{
			name: "client error",
			factory: func(context.Context, func(string) string, *slog.Logger) (s3API, error) {
				return nil, errors.New("loading AWS config: boom")
			},
			wantErr: "loading AWS config: boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			env := baseEnv(t.TempDir(), "create")
			if tt.env != nil {
				tt.env(env)
			}
			f := &fakeS3{}
			factory := tt.factory
			if factory == nil {
				factory = fakeFactory(f)
			}

			err := run(t.Context(), tt.args, envFrom(env), &bytes.Buffer{}, factory)

			require.ErrorContains(t, err, tt.wantErr)
			assert.Empty(t, f.createInputs, "no S3 call on invalid input")
		})
	}
}

func TestRunTimeoutBoundsPolling(t *testing.T) {
	t.Chdir(t.TempDir())
	f := &fakeS3{headErrs: []error{errNotFound}}

	err := run(t.Context(), []string{"-timeout", "50ms"}, envFrom(baseEnv(t.TempDir(), "create")), &bytes.Buffer{}, fakeFactory(f))

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestReport(t *testing.T) {
	errorFile := filepath.Join(t.TempDir(), "error")
	var stderr bytes.Buffer

	report(&stderr, errorFile, errors.New("bucket demo-bucket already exists"))

	assert.Equal(t, "s3-container-runner: bucket demo-bucket already exists\n", stderr.String())
	b, err := os.ReadFile(errorFile)
	require.NoError(t, err)
	assert.Equal(t, "bucket demo-bucket already exists\n", string(b))
}
