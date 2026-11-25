package contract

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAction(t *testing.T) {
	tests := []struct {
		in      string
		want    Action
		wantErr string
	}{
		{in: "create", want: ActionCreate},
		{in: "destroy", want: ActionDestroy},
		{in: "", wantErr: "no action: set ACTION"},
		{in: "Create", wantErr: `unsupported action "Create"`},
		{in: "update", wantErr: `unsupported action "update"`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseAction(tt.in)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestWriteOutputs(t *testing.T) {
	o := Outputs{Bucket: "b", Region: "eu-west-1", ARN: "arn:aws:s3:::b"}
	const want = `{"bucket":"b","region":"eu-west-1","arn":"arn:aws:s3:::b"}` + "\n"

	t.Run("writes JSON with mode 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "outputs.json")
		require.NoError(t, WriteOutputs(path, o))
		assertFile(t, path, want)
	})

	t.Run("truncates an existing file and tightens its mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "outputs.json")
		require.NoError(t, os.WriteFile(path, []byte("a much longer previous content than the new one ......................."), 0o644))
		require.NoError(t, WriteOutputs(path, o))
		assertFile(t, path, want)
	})

	t.Run("empty path is a no-op", func(t *testing.T) {
		require.NoError(t, WriteOutputs("", o))
	})

	t.Run("refuses a symlink", func(t *testing.T) {
		path := symlinkTo(t)
		require.Error(t, WriteOutputs(path, o))
	})
}

func TestReportError(t *testing.T) {
	t.Run("appends each error on its own line with mode 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "error")
		require.NoError(t, ReportError(path, errors.New("first")))
		require.NoError(t, ReportError(path, errors.New("second")))
		assertFile(t, path, "first\nsecond\n")
	})

	t.Run("empty path is a no-op", func(t *testing.T) {
		require.NoError(t, ReportError("", errors.New("boom")))
	})

	t.Run("nil error is a no-op", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "error")
		require.NoError(t, ReportError(path, nil))
		assert.NoFileExists(t, path)
	})

	t.Run("refuses a symlink", func(t *testing.T) {
		path := symlinkTo(t)
		require.Error(t, ReportError(path, errors.New("boom")))
	})
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(b))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// symlinkTo returns a symlink to a file that must stay untouched.
func symlinkTo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("keep"), 0o644))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))
	t.Cleanup(func() {
		b, err := os.ReadFile(target)
		require.NoError(t, err)
		assert.Equal(t, "keep", string(b), "symlink target unchanged")
	})
	return link
}
