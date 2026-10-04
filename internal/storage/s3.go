package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/codemeapixel/nobackups/internal/config"
)

type S3 struct {
	name     string
	client   *minio.Client
	bucket   string
	prefix   string
	partSize uint64
	class    string
}

func NewS3(d *config.Destination) (*S3, error) {
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(d.AccessKeyID, d.SecretAccessKey, d.SessionToken),
		Secure: d.UseSSL == nil || *d.UseSSL,
		Region: d.Region,
	}
	if d.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	} else {
		opts.BucketLookup = minio.BucketLookupAuto
	}
	if d.CAFile != "" || d.InsecureSkipVerify {
		tr, err := minio.DefaultTransport(opts.Secure)
		if err != nil {
			return nil, err
		}
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: d.InsecureSkipVerify} //nolint:gosec // opt-in
		if d.CAFile != "" {
			pem, err := os.ReadFile(d.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read ca_file: %w", err)
			}
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("ca_file %s contains no certificates", d.CAFile)
			}
			tlsCfg.RootCAs = pool
		}
		tr.TLSClientConfig = tlsCfg
		opts.Transport = tr
	}
	client, err := minio.New(d.Endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("destination %s: %w", d.Name, err)
	}
	return &S3{
		name:     d.Name,
		client:   client,
		bucket:   d.Bucket,
		prefix:   d.Prefix,
		partSize: uint64(d.PartSizeMB) << 20,
		class:    d.StorageClass,
	}, nil
}

func (s *S3) Name() string { return s.name }

func (s *S3) Put(ctx context.Context, key string, r io.Reader) error {
	_, err := s.client.PutObject(ctx, s.bucket, joinKey(s.prefix, key), r, -1, minio.PutObjectOptions{
		ContentType:  "application/octet-stream",
		PartSize:     s.partSize,
		StorageClass: s.class,
	})
	return err
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, joinKey(s.prefix, key), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	full := joinKey(s.prefix, prefix)
	var out []Object
	for o := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: full, Recursive: true}) {
		if o.Err != nil {
			return nil, o.Err
		}
		key := o.Key
		if s.prefix != "" {
			key = strings.TrimPrefix(key, s.prefix+"/")
		}
		out = append(out, Object{Key: key, Size: o.Size, LastModified: o.LastModified})
	}
	return out, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, joinKey(s.prefix, key), minio.RemoveObjectOptions{})
}

func (s *S3) CheckBucket(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("bucket %q does not exist", s.bucket)
	}
	return nil
}
