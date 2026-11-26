package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/humanitec-architecture/s3-container-runner/internal/contract"
)

// s3API is the subset of the S3 client the runner calls; tests use a fake.
type s3API interface {
	CreateBucket(ctx context.Context, in *s3.CreateBucketInput, optFns ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	DeleteBucket(ctx context.Context, in *s3.DeleteBucketInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketOutput, error)
}

// bucketRunner creates and destroys one bucket, then polls S3 until the change is visible.
type bucketRunner struct {
	client s3API
	log    *slog.Logger
	// The wait before the second poll; it doubles after each attempt, up to maxDelay.
	firstDelay time.Duration
	maxDelay   time.Duration
}

func newBucketRunner(client s3API, log *slog.Logger) *bucketRunner {
	return &bucketRunner{client: client, log: log, firstDelay: time.Second, maxDelay: 10 * time.Second}
}

// newS3Client uses the static credentials in AWS_CREDENTIALS_FILE if set, otherwise the default credential chain
// (IRSA or EKS Pod Identity in the cluster).
func newS3Client(ctx context.Context, getenv func(string) string, log *slog.Logger) (s3API, error) {
	var opts []func(*config.LoadOptions) error
	if path := getenv(contract.EnvAWSCredentialsFile); path != "" {
		creds, err := readStaticCredentials(path)
		if err != nil {
			return nil, err
		}
		opts = append(opts, config.WithCredentialsProvider(creds))
		log.Info("using static credentials", "file", path)
	} else {
		log.Info("using the default credential chain")
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		// A custom endpoint (the S3 emulator in the integration tests) serves buckets by path, not by subdomain.
		o.UsePathStyle = getenv("AWS_ENDPOINT_URL_S3") != ""
	}), nil
}

// readStaticCredentials reads an access key pair from a JSON file; its errors never include the file's content.
func readStaticCredentials(path string) (credentials.StaticCredentialsProvider, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return credentials.StaticCredentialsProvider{}, fmt.Errorf("reading %s: %w", contract.EnvAWSCredentialsFile, err)
	}
	var c struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
	}
	if err := json.Unmarshal(b, &c); err != nil || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return credentials.StaticCredentialsProvider{}, fmt.Errorf(
			"%s must be a JSON object with non-empty access_key_id and secret_access_key", contract.EnvAWSCredentialsFile)
	}
	return credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, ""), nil
}

// create creates the bucket; a bucket this account already owns counts as created.
func (r *bucketRunner) create(ctx context.Context, bucket, region string) error {
	in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	// us-east-1 is the default location and rejects an explicit constraint.
	if region != "us-east-1" {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(region),
		}
	}

	_, err := r.client.CreateBucket(ctx, in)
	switch {
	case err == nil:
		r.log.Info("bucket creation requested", "bucket", bucket)
	case hasCode(err, "BucketAlreadyOwnedByYou"):
		// The Job runs again on every deployment.
		r.log.Info("bucket already exists in this account", "bucket", bucket)
	case hasCode(err, "BucketAlreadyExists"):
		return fmt.Errorf("bucket %s already exists and belongs to another account; bucket names are global", bucket)
	default:
		return fmt.Errorf("creating bucket %s: %w", bucket, err)
	}
	return nil
}

// destroy empties and deletes the bucket; a missing bucket counts as deleted.
func (r *bucketRunner) destroy(ctx context.Context, bucket string) error {
	if err := r.empty(ctx, bucket); err != nil {
		return err
	}

	_, err := r.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	switch {
	case err == nil:
		r.log.Info("bucket deletion requested", "bucket", bucket)
	case hasCode(err, "NoSuchBucket"):
		r.log.Info("bucket already gone", "bucket", bucket)
	default:
		return fmt.Errorf("deleting bucket %s: %w", bucket, err)
	}
	return nil
}

// empty deletes every object in the bucket, one page (at most 1000 keys) per DeleteObjects call.
func (r *bucketRunner) empty(ctx context.Context, bucket string) error {
	pages := s3.NewListObjectsV2Paginator(r.client, &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		MaxKeys: aws.Int32(1000),
	})
	deleted := 0
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if hasCode(err, "NoSuchBucket") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("listing objects in %s: %w", bucket, err)
		}
		if len(page.Contents) == 0 {
			continue
		}

		ids := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: obj.Key})
		}
		out, err := r.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			// Quiet: the response lists only the keys that failed.
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("deleting objects in %s: %w", bucket, err)
		}
		// DeleteObjects succeeds as a call even when single keys fail.
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("deleting objects in %s: %d of %d keys failed, first %q: %s: %s",
				bucket, len(out.Errors), len(ids), aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
		}
		deleted += len(ids)
		r.log.Info("deleted objects", "bucket", bucket, "count", len(ids), "total", deleted)
	}
	return nil
}

// waitUntil polls HeadBucket until the bucket exists (wantExists) or is gone, backing off between attempts.
func (r *bucketRunner) waitUntil(ctx context.Context, bucket string, wantExists bool) error {
	start := time.Now()
	delay := r.firstDelay
	for attempt := 1; ; attempt++ {
		_, err := r.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
		exists := err == nil
		if err != nil && !hasCode(err, "NotFound") {
			return fmt.Errorf("checking bucket %s: %w", bucket, err)
		}

		r.log.Info("polled bucket", "bucket", bucket, "attempt", attempt,
			"elapsed", time.Since(start).Round(time.Millisecond), "exists", exists)
		if exists == wantExists {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for bucket %s (exists=%t) after %d attempts: %w", bucket, wantExists, attempt, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(2*delay, r.maxDelay)
	}
}

// hasCode reports whether err is an S3 API error with the given code, modeled (types.NoSuchBucket) or not.
func hasCode(err error, code string) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}
