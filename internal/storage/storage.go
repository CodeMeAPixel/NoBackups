package storage

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/codemeapixel/nobackups/internal/config"
)

type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type Backend interface {
	Name() string
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
}

func New(d *config.Destination) (Backend, error) {
	switch d.Type {
	case "s3":
		return NewS3(d)
	case "local":
		return NewLocal(d)
	default:
		return nil, fmt.Errorf("unknown destination type %q", d.Type)
	}
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "/" + key
}
