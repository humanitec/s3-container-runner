package contract

import (
	"errors"
	"fmt"
	"os"
)

// ReportError appends err and a newline to path; an empty path or a nil err is a no-op.
func ReportError(path string, err error) (rerr error) {
	if path == "" || err == nil {
		return nil
	}
	f, rerr := openPrivate(path, os.O_APPEND)
	if rerr != nil {
		return rerr
	}
	defer func() { rerr = errors.Join(rerr, f.Close()) }()
	_, rerr = fmt.Fprintln(f, err.Error())
	return rerr
}
