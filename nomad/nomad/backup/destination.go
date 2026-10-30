// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"
)

// ErrNotFound is returned for a missing object.
var ErrNotFound = errors.New("object not found")

// Destination is a store of backup objects. Keys are relative to the
// destination's prefix.
type Destination interface {
	// Name is the name of the destination in the configuration.
	Name() string

	// Location returns the URL of a key, for display.
	Location(key string) string

	// Put uploads an object. A negative size streams it with a multipart
	// upload of fixed part size (S3), or into a temporary file renamed when
	// complete (directory).
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error

	// Get downloads an object.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Size returns the size of a stored object.
	Size(ctx context.Context, key string) (int64, error)

	// DeletePrefix deletes every object, and aborts every incomplete
	// upload, under a key prefix.
	DeletePrefix(ctx context.Context, prefix string) error

	// AbortIncompleteUploads aborts the incomplete multipart uploads (S3) or
	// removes the temporary files (directory) under a key prefix, for example
	// after a leader change or a crash during a backup.
	AbortIncompleteUploads(ctx context.Context, prefix string) (int, error)

	// PresignGet returns a pre-signed download URL of an object; a
	// directory destination returns ErrNoDownloadURL.
	PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error)
}

// s3Destination is an AWS S3 or S3-compatible destination.
type s3Destination struct {
	name   string
	bucket string
	prefix string
	client *minio.Client

	sse               encrypt.ServerSide
	partSize          uint64
	uploadConcurrency int
}

// newS3Destination creates the client of an S3 destination. It does not
// connect to the store.
func newS3Destination(cfg *DestinationConfig, region string, partSize uint64, concurrency int) (*s3Destination, error) {
	host, secure, err := parseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	var creds *credentials.Credentials
	switch {
	case cfg.AccessKey != "":
		creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken)
	case cfg.CredentialsFile != "":
		creds = credentials.NewFileAWSCredentials(cfg.CredentialsFile, cfg.Profile)
	default:
		// The server's environment: environment variables, the default
		// shared credentials file and the instance profile.
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.EnvMinio{},
			&credentials.FileAWSCredentials{Profile: cfg.Profile},
			&credentials.IAM{},
		})
	}

	transport, err := minio.DefaultTransport(secure)
	if err != nil {
		return nil, err
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in ca_file %s", cfg.CAFile)
		}
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		transport.TLSClientConfig.RootCAs = pool
	}

	lookup := minio.BucketLookupAuto
	if cfg.ForcePathStyle {
		lookup = minio.BucketLookupPath
	}

	client, err := minio.New(host, &minio.Options{
		Creds:        creds,
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    http.RoundTripper(transport),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 client: %w", err)
	}

	var sse encrypt.ServerSide
	switch cfg.SSE {
	case "AES256":
		sse = encrypt.NewSSE()
	case "aws:kms":
		sse, err = encrypt.NewSSEKMS(cfg.KMSKey, nil)
		if err != nil {
			return nil, fmt.Errorf("invalid kms_key: %w", err)
		}
	}

	prefix := cfg.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	prefix = strings.Trim(strings.ReplaceAll(prefix, "${region}", region), "/")

	return &s3Destination{
		name:              cfg.Name,
		bucket:            cfg.Bucket,
		prefix:            prefix,
		client:            client,
		sse:               sse,
		partSize:          partSize,
		uploadConcurrency: concurrency,
	}, nil
}

// parseEndpoint returns the host and whether TLS is used. An empty endpoint
// is AWS S3.
func parseEndpoint(endpoint string) (string, bool, error) {
	if endpoint == "" {
		return "s3.amazonaws.com", true, nil
	}
	if !strings.Contains(endpoint, "://") {
		return strings.TrimSuffix(endpoint, "/"), true, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "https":
		return u.Host, true, nil
	case "http":
		return u.Host, false, nil
	default:
		return "", false, fmt.Errorf("invalid endpoint scheme %q", u.Scheme)
	}
}

func (d *s3Destination) Name() string { return d.name }

func (d *s3Destination) key(key string) string {
	if d.prefix == "" {
		return key
	}
	return path.Join(d.prefix, key)
}

func (d *s3Destination) Location(key string) string {
	loc := "s3://" + d.bucket + "/" + d.key(key)
	if strings.HasSuffix(key, "/") && !strings.HasSuffix(loc, "/") {
		loc += "/"
	}
	return loc
}

func (d *s3Destination) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	opts := minio.PutObjectOptions{
		ContentType:          contentType,
		ServerSideEncryption: d.sse,
		PartSize:             d.partSize,
	}
	if size < 0 && d.uploadConcurrency > 1 {
		opts.NumThreads = uint(d.uploadConcurrency)
		opts.ConcurrentStreamParts = true
	}
	_, err := d.client.PutObject(ctx, d.bucket, d.key(key), r, size, opts)
	if err != nil {
		return fmt.Errorf("failed to upload %s: %w", d.Location(key), err)
	}
	return nil
}

func (d *s3Destination) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	opts := minio.GetObjectOptions{}
	obj, err := d.client.GetObject(ctx, d.bucket, d.key(key), opts)
	if err != nil {
		return nil, d.wrapErr(key, err)
	}
	// GetObject is lazy: stat to report a missing object here.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, d.wrapErr(key, err)
	}
	return obj, nil
}

func (d *s3Destination) Size(ctx context.Context, key string) (int64, error) {
	info, err := d.client.StatObject(ctx, d.bucket, d.key(key), minio.StatObjectOptions{})
	if err != nil {
		return 0, d.wrapErr(key, err)
	}
	return info.Size, nil
}

func (d *s3Destination) wrapErr(key string, err error) error {
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NotFound":
		return fmt.Errorf("%s: %w", d.Location(key), ErrNotFound)
	}
	return fmt.Errorf("%s: %w", d.Location(key), err)
}

func (d *s3Destination) DeletePrefix(ctx context.Context, prefix string) error {
	if _, err := d.AbortIncompleteUploads(ctx, prefix); err != nil {
		return err
	}

	full := d.key(prefix)
	if strings.HasSuffix(prefix, "/") {
		full += "/"
	}
	objects := d.client.ListObjects(ctx, d.bucket, minio.ListObjectsOptions{Prefix: full, Recursive: true})
	for rerr := range d.client.RemoveObjects(ctx, d.bucket, objects, minio.RemoveObjectsOptions{}) {
		if rerr.Err != nil {
			return fmt.Errorf("failed to delete %s: %w", rerr.ObjectName, rerr.Err)
		}
	}
	return nil
}

func (d *s3Destination) AbortIncompleteUploads(ctx context.Context, prefix string) (int, error) {
	full := d.key(prefix)
	if strings.HasSuffix(prefix, "/") || prefix == "" {
		full = strings.TrimSuffix(full, "/") + "/"
	}
	var aborted int
	for upload := range d.client.ListIncompleteUploads(ctx, d.bucket, full, true) {
		if upload.Err != nil {
			return aborted, fmt.Errorf("failed to list incomplete uploads: %w", upload.Err)
		}
		if err := d.client.RemoveIncompleteUpload(ctx, d.bucket, upload.Key); err != nil {
			return aborted, fmt.Errorf("failed to abort the upload of %s: %w", upload.Key, err)
		}
		aborted++
	}
	return aborted, nil
}

func (d *s3Destination) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	u, err := d.client.PresignedGetObject(ctx, d.bucket, d.key(key), expiry, nil)
	if err != nil {
		return "", fmt.Errorf("failed to pre-sign %s: %w", d.Location(key), err)
	}
	return u.String(), nil
}

// NewDestination creates a destination from its configuration. The region
// replaces ${region} in the prefix.
func NewDestination(cfg *DestinationConfig, c *Config, region string) (Destination, error) {
	switch cfg.Type {
	case DestinationTypeS3:
		return newS3Destination(cfg, region, c.PartSize, c.UploadConcurrency)
	case DestinationTypeDirectory:
		return newDirDestination(cfg, region)
	default:
		return nil, fmt.Errorf("unsupported destination type %q", cfg.Type)
	}
}
