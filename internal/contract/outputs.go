package contract

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
)

// Outputs are the non-secret outputs of the bucket resource.
type Outputs struct {
	Bucket string `json:"bucket"`
	Region string `json:"region"`
	ARN    string `json:"arn"`
}

// WriteOutputs writes o to path as JSON; an empty path is a no-op.
func WriteOutputs(path string, o Outputs) (err error) {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	f, err := openPrivate(path, os.O_TRUNC)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	_, err = f.Write(append(b, '\n'))
	return err
}

// openPrivate opens path for writing without following a symlink and forces mode 0600, even on an existing file.
func openPrivate(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|flag, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
