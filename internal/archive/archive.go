package archive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Stats struct {
	Files    int64
	Dirs     int64
	Bytes    int64
	Skipped  int64
	Warnings int64
}

type Options struct {
	Sources       []string
	Exclude       []string
	OneFileSystem bool
	Log           *slog.Logger
}

func Create(w io.Writer, opts Options) (Stats, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tw := tar.NewWriter(w)
	a := &archiver{tw: tw, opts: opts, log: log, seen: map[string]bool{}}
	for _, src := range opts.Sources {
		if err := a.addSource(filepath.Clean(src)); err != nil {
			return a.stats, err
		}
	}
	return a.stats, tw.Close()
}

type archiver struct {
	tw    *tar.Writer
	opts  Options
	log   *slog.Logger
	stats Stats
	seen  map[string]bool
	buf   []byte
}

func (a *archiver) warn(msg string, args ...any) {
	a.stats.Warnings++
	a.log.Warn(msg, args...)
}

func (a *archiver) addSource(src string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("source %s: %w", src, err)
	}
	rootDev := deviceOf(info)

	if err := a.addParents(src); err != nil {
		return err
	}

	return filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if p == src {
				return err
			}
			a.warn("cannot read path, skipping", "path", p, "err", err)
			a.stats.Skipped++
			if de != nil && de.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p != src && Excluded(p, a.opts.Exclude) {
			a.stats.Skipped++
			if de.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := de.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			a.warn("cannot stat, skipping", "path", p, "err", err)
			a.stats.Skipped++
			return nil
		}
		if de.IsDir() && p != src && a.opts.OneFileSystem && deviceOf(info) != rootDev {
			a.log.Debug("skipping mount point (one_file_system)", "path", p)
			_ = a.addEntry(p, info)
			return fs.SkipDir
		}
		return a.addEntry(p, info)
	})
}

func (a *archiver) addParents(p string) error {
	var parents []string
	for d := filepath.Dir(p); d != "/" && d != "."; d = filepath.Dir(d) {
		parents = append(parents, d)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		info, err := os.Lstat(parents[i])
		if err != nil {
			return err
		}
		if err := a.addEntry(parents[i], info); err != nil {
			return err
		}
	}
	return nil
}

func (a *archiver) addEntry(p string, info fs.FileInfo) error {
	name := strings.TrimPrefix(filepath.ToSlash(p), "/")
	if name == "" || a.seen[name] {
		return nil
	}
	a.seen[name] = true

	mode := info.Mode()
	if mode&(fs.ModeSocket|fs.ModeNamedPipe) != 0 {
		a.stats.Skipped++
		return nil
	}
	var link string
	if mode&fs.ModeSymlink != 0 {
		l, err := os.Readlink(p)
		if err != nil {
			a.warn("cannot read symlink, skipping", "path", p, "err", err)
			return nil
		}
		link = l
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		a.warn("unsupported file, skipping", "path", p, "err", err)
		a.stats.Skipped++
		return nil
	}
	hdr.Name = name
	if info.IsDir() {
		hdr.Name += "/"
	}
	hdr.Format = tar.FormatPAX
	hdr.ModTime = info.ModTime().Truncate(time.Second)
	hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}

	if !mode.IsRegular() {
		if err := a.tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			a.stats.Dirs++
		}
		return nil
	}

	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		a.warn("cannot open file, skipping", "path", p, "err", err)
		a.stats.Skipped++
		return nil
	}
	defer f.Close()

	if err := a.tw.WriteHeader(hdr); err != nil {
		return err
	}
	if a.buf == nil {
		a.buf = make([]byte, 1<<20)
	}
	n, err := io.CopyBuffer(a.tw, io.LimitReader(f, hdr.Size), a.buf)
	if err != nil {
		var pe *fs.PathError
		if !errors.As(err, &pe) {
			return err
		}
		a.warn("read error, file truncated in archive", "path", p, "err", err)
	}
	if n < hdr.Size {
		a.warn("file shrank while reading, padding", "path", p, "expected", hdr.Size, "read", n)
		if _, err := io.CopyN(a.tw, zeroReader{}, hdr.Size-n); err != nil {
			return err
		}
	}
	a.stats.Files++
	a.stats.Bytes += hdr.Size
	return nil
}

func Excluded(p string, patterns []string) bool {
	base := filepath.Base(p)
	for _, pat := range patterns {
		if strings.Contains(pat, "/") {
			pat = strings.TrimSuffix(pat, "/")
			for q := p; q != "/" && q != "."; q = filepath.Dir(q) {
				if ok, _ := filepath.Match(pat, q); ok {
					return true
				}
			}
		} else if ok, _ := filepath.Match(pat, base); ok {
			return true
		}
	}
	return false
}

func deviceOf(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev) //nolint:unconvert
	}
	return 0
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type ExtractOptions struct {
	Target string
	Paths  []string
	Log    *slog.Logger
}

func Extract(r io.Reader, opts ExtractOptions) (Stats, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var st Stats
	target, err := filepath.Abs(opts.Target)
	if err != nil {
		return st, err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return st, err
	}
	filters := make([]string, 0, len(opts.Paths))
	for _, p := range opts.Paths {
		filters = append(filters, strings.Trim(path.Clean("/"+p), "/"))
	}
	root := os.Geteuid() == 0

	type dirMeta struct {
		path string
		hdr  *tar.Header
	}
	var dirs []dirMeta

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, err
		}
		name := strings.Trim(path.Clean("/"+hdr.Name), "/")
		if name == "" || !matchFilter(name, filters) {
			continue
		}
		dst := filepath.Join(target, filepath.FromSlash(name))
		if dst != target && !strings.HasPrefix(dst, target+string(os.PathSeparator)) {
			return st, fmt.Errorf("refusing to extract %q outside target", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return st, err
		}
		mode := fs.FileMode(hdr.Mode).Perm() | fs.FileMode(hdr.Mode)&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return st, err
			}
			dirs = append(dirs, dirMeta{dst, hdr})
			st.Dirs++
			continue
		case tar.TypeReg:
			_ = os.Remove(dst)
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return st, err
			}
			n, err := io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return st, fmt.Errorf("write %s: %w", dst, err)
			}
			st.Files++
			st.Bytes += n
		case tar.TypeSymlink:
			_ = os.Remove(dst)
			if err := os.Symlink(hdr.Linkname, dst); err != nil {
				return st, err
			}
			if root {
				_ = os.Lchown(dst, hdr.Uid, hdr.Gid)
			}
			st.Files++
			continue
		case tar.TypeLink:
			src := filepath.Join(target, filepath.FromSlash(strings.Trim(path.Clean("/"+hdr.Linkname), "/")))
			_ = os.Remove(dst)
			if err := os.Link(src, dst); err != nil {
				log.Warn("cannot create hard link", "path", dst, "err", err)
				st.Warnings++
			}
			st.Files++
			continue
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			if !root {
				st.Skipped++
				continue
			}
			devMode := uint32(mode)
			switch hdr.Typeflag {
			case tar.TypeChar:
				devMode |= syscall.S_IFCHR
			case tar.TypeBlock:
				devMode |= syscall.S_IFBLK
			default:
				devMode |= syscall.S_IFIFO
			}
			_ = os.Remove(dst)
			dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
			if err := unix.Mknod(dst, devMode, int(dev)); err != nil {
				log.Warn("cannot create device node", "path", dst, "err", err)
				st.Warnings++
				continue
			}
		default:
			st.Skipped++
			continue
		}
		applyMeta(dst, hdr, mode, root, log, &st)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		mode := fs.FileMode(d.hdr.Mode).Perm() | fs.FileMode(d.hdr.Mode)&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)
		applyMeta(d.path, d.hdr, mode, root, log, &st)
	}
	return st, nil
}

func applyMeta(p string, hdr *tar.Header, mode fs.FileMode, root bool, log *slog.Logger, st *Stats) {
	if root {
		if err := os.Lchown(p, hdr.Uid, hdr.Gid); err != nil {
			log.Warn("chown failed", "path", p, "err", err)
			st.Warnings++
		}
	}
	if err := os.Chmod(p, mode); err != nil {
		log.Warn("chmod failed", "path", p, "err", err)
		st.Warnings++
	}
	if err := os.Chtimes(p, hdr.ModTime, hdr.ModTime); err != nil {
		log.Warn("chtimes failed", "path", p, "err", err)
		st.Warnings++
	}
}

func matchFilter(name string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if f == "" || name == f || strings.HasPrefix(name, f+"/") || strings.HasPrefix(f, name+"/") {
			return true
		}
	}
	return false
}
