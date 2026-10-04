package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/codemeapixel/nobackups/internal/config"
)

type Local struct {
	name string
	root string
}

func NewLocal(d *config.Destination) (*Local, error) {
	return &Local{name: d.Name, root: filepath.Join(d.Path, filepath.FromSlash(d.Prefix))}, nil
}

func (l *Local) Name() string { return l.name }

func (l *Local) path(key string) string { return filepath.Join(l.root, filepath.FromSlash(key)) }

func (l *Local) Put(ctx context.Context, key string, r io.Reader) error {
	dst := l.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, ctxReader{ctx, r})
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func (l *Local) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return os.Open(l.path(key))
}

func (l *Local) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	base := l.path(prefix)
	err := filepath.WalkDir(base, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if de.IsDir() || strings.HasSuffix(p, ".partial") {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		out = append(out, Object{Key: filepath.ToSlash(rel), Size: info.Size(), LastModified: info.ModTime()})
		return nil
	})
	return out, err
}

func (l *Local) Delete(_ context.Context, key string) error {
	err := os.Remove(l.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
