// Command s3-container-runner creates or destroys an S3 bucket; the Humanitec Container Driver runs it as a Kubernetes Job.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/humanitec/s3-container-runner/internal/contract"
)

const (
	binaryName     = "s3-container-runner"
	defaultTimeout = 5 * time.Minute
)

var version = "dev"

// clientFactory builds the S3 client; tests swap it for a fake.
type clientFactory func(ctx context.Context, getenv func(string) string, log *slog.Logger) (s3API, error)

func main() {
	// run enters SCRIPTS_DIRECTORY, so a relative ERROR_FILE must be resolved first.
	errorFile := os.Getenv(contract.EnvErrorFile)
	if abs, err := filepath.Abs(errorFile); err == nil && errorFile != "" {
		errorFile = abs
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stderr, newS3Client)
	stop()
	if err != nil {
		report(os.Stderr, errorFile, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, newClient clientFactory) error {
	fs := flag.NewFlagSet(binaryName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", defaultTimeout, "bound on the whole run, polling included")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	action, err := contract.ParseAction(getenv(contract.EnvAction))
	if err != nil {
		return err
	}
	bucket := getenv(contract.EnvBucketName)
	if bucket == "" {
		return fmt.Errorf("%s is required", contract.EnvBucketName)
	}
	region := getenv(contract.EnvAWSRegion)
	if region == "" {
		return fmt.Errorf("%s is required", contract.EnvAWSRegion)
	}

	// The outputs and credentials files are relative to the shared directory.
	if dir := getenv(contract.EnvScriptsDirectory); dir != "" {
		if err := os.Chdir(dir); err != nil {
			return fmt.Errorf("entering %s: %w", contract.EnvScriptsDirectory, err)
		}
	}

	log := newLogger(stderr)
	log.Info("starting", "version", version, "action", action, "bucket", bucket, "region", region)

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	client, err := newClient(ctx, getenv, log)
	if err != nil {
		return err
	}
	r := newBucketRunner(client, log)

	if action == contract.ActionDestroy {
		// 1. Call the API: empty the bucket and delete it.
		if err := r.destroy(ctx, bucket); err != nil {
			return err
		}
		// 2. Poll until S3 no longer finds the bucket.
		if err := r.waitUntil(ctx, bucket, false); err != nil {
			return err
		}
		log.Info("bucket destroyed", "bucket", bucket)
		return nil
	}

	// 1. Call the API: create the bucket.
	if err := r.create(ctx, bucket, region); err != nil {
		return err
	}
	// 2. Poll until the bucket is reachable.
	if err := r.waitUntil(ctx, bucket, true); err != nil {
		return err
	}
	// 3. Write the outputs for the Platform Orchestrator.
	outputs := contract.Outputs{Bucket: bucket, Region: region, ARN: "arn:aws:s3:::" + bucket}
	if err := contract.WriteOutputs(getenv(contract.EnvOutputsFile), outputs); err != nil {
		return fmt.Errorf("writing %s: %w", contract.EnvOutputsFile, err)
	}
	log.Info("bucket ready, outputs written", "bucket", bucket)
	return nil
}

// newLogger drops the time attribute: the Job's log collector adds its own.
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// report prints err to w and appends it to errorFile, which the Driver copies into the Resource status.
func report(w io.Writer, errorFile string, err error) {
	_, _ = fmt.Fprintf(w, "%s: %v\n", binaryName, err)
	if rerr := contract.ReportError(errorFile, err); rerr != nil {
		_, _ = fmt.Fprintf(w, "%s: writing %s: %v\n", binaryName, contract.EnvErrorFile, rerr)
	}
}
