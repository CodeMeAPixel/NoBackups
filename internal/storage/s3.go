package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
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
	if os.Getenv("NOBACKUPS_S3_TRACE") != "" {
		client.TraceOn(os.Stderr)
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
	return s.upload(ctx, joinKey(s.prefix, key), r, int64(s.partSize))
}

func (s *S3) upload(ctx context.Context, object string, r io.Reader, partSize int64) error {
	core := minio.Core{Client: s.client}
	opts := minio.PutObjectOptions{ContentType: "application/octet-stream", StorageClass: s.class}
	buf := make([]byte, partSize)

	n, err := io.ReadFull(r, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		_, err = core.PutObject(ctx, s.bucket, object, bytes.NewReader(buf[:n]), int64(n), md5Base64(buf[:n]), "", opts)
		return err
	}
	if err != nil {
		return err
	}

	uploadID, err := core.NewMultipartUpload(ctx, s.bucket, object, opts)
	if err != nil {
		return err
	}
	parts, err := s.uploadParts(ctx, core, object, uploadID, r, buf, n)
	if err == nil {
		_, err = core.CompleteMultipartUpload(ctx, s.bucket, object, uploadID, parts, opts)
	}
	if err != nil {
		_ = core.AbortMultipartUpload(context.WithoutCancel(ctx), s.bucket, object, uploadID)
		return err
	}
	return nil
}

func (s *S3) uploadParts(ctx context.Context, core minio.Core, object, uploadID string, r io.Reader, buf []byte, n int) ([]minio.CompletePart, error) {
	var parts []minio.CompletePart
	for num := 1; n > 0; num++ {
		if num > maxParts {
			return nil, fmt.Errorf("backup is larger than %d parts of %d MB; raise part_size_mb", maxParts, len(buf)>>20)
		}
		data := buf[:n]
		part, err := core.PutObjectPart(ctx, s.bucket, object, uploadID, num, bytes.NewReader(data), int64(n),
			minio.PutObjectPartOptions{Md5Base64: md5Base64(data)})
		if err != nil {
			return nil, fmt.Errorf("upload part %d: %w", num, err)
		}
		parts = append(parts, minio.CompletePart{PartNumber: num, ETag: quoteETag(part.ETag)})

		if n, err = io.ReadFull(r, buf); err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, err
		}
	}
	return parts, nil
}

const maxParts = 10000

func quoteETag(etag string) string {
	etag = strings.TrimPrefix(etag, "W/")
	return `"` + strings.Trim(etag, `"`) + `"`
}

func md5Base64(b []byte) string {
	sum := md5.Sum(b)
	return base64.StdEncoding.EncodeToString(sum[:])
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

func (s *S3) CheckMultipart(ctx context.Context, key string) error {
	const part = 5 << 20
	data := make([]byte, part+1024)
	object := joinKey(s.prefix, key)
	if err := s.upload(ctx, object, bytes.NewReader(data), part); err != nil {
		return err
	}
	return s.client.RemoveObject(ctx, s.bucket, object, minio.RemoveObjectOptions{})
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
