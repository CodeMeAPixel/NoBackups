package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/codemeapixel/nobackups/internal/config"
)

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func compressWriter(w io.Writer, algo string, level int) (io.WriteCloser, error) {
	switch algo {
	case "zstd":
		opts := []zstd.EOption{zstd.WithEncoderConcurrency(2)}
		if level != 0 {
			opts = append(opts, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)))
		}
		return zstd.NewWriter(w, opts...)
	case "gzip":
		if level == 0 {
			level = gzip.DefaultCompression
		}
		return gzip.NewWriterLevel(w, level)
	case "none":
		return nopWriteCloser{w}, nil
	}
	return nil, fmt.Errorf("unknown compression %q", algo)
}

func decompressReader(r io.Reader, algo string) (io.ReadCloser, error) {
	switch algo {
	case "zstd":
		d, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	case "gzip":
		return gzip.NewReader(r)
	case "none":
		return io.NopCloser(r), nil
	}
	return nil, fmt.Errorf("unknown compression %q", algo)
}

func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

func recipients(e *config.Encryption) ([]age.Recipient, error) {
	pass := e.Passphrase
	if e.PassphraseFile != "" {
		var err error
		if pass, err = readSecretFile(e.PassphraseFile); err != nil {
			return nil, fmt.Errorf("passphrase_file: %w", err)
		}
	}
	if pass != "" {
		r, err := age.NewScryptRecipient(pass)
		if err != nil {
			return nil, err
		}
		return []age.Recipient{r}, nil
	}
	var out []age.Recipient
	for _, s := range e.Recipients {
		r, err := age.ParseX25519Recipient(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("recipient %q: %w", s, err)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, errors.New("no encryption recipients configured")
	}
	return out, nil
}

func identities(e *config.Encryption) ([]age.Identity, error) {
	if e == nil {
		return nil, errors.New("snapshot is encrypted but the job has no encryption settings")
	}
	pass := e.Passphrase
	if e.PassphraseFile != "" {
		var err error
		if pass, err = readSecretFile(e.PassphraseFile); err != nil {
			return nil, fmt.Errorf("passphrase_file: %w", err)
		}
	}
	if pass != "" {
		id, err := age.NewScryptIdentity(pass)
		if err != nil {
			return nil, err
		}
		return []age.Identity{id}, nil
	}
	if e.IdentityFile == "" {
		return nil, errors.New("snapshot is encrypted to recipients; set encryption.identity_file (or pass --identity) to restore")
	}
	f, err := os.Open(e.IdentityFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return age.ParseIdentities(f)
}
