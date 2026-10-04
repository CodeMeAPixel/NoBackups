package backup

import (
	"errors"
	"io"
	"sync"
)

type fanout struct {
	mu    sync.Mutex
	pipes []*io.PipeWriter
	alive []bool
}

func newFanout(pipes []*io.PipeWriter) *fanout {
	alive := make([]bool, len(pipes))
	for i := range alive {
		alive[i] = true
	}
	return &fanout{pipes: pipes, alive: alive}
}

var errAllFailed = errors.New("all destinations failed")

func (f *fanout) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	any := false
	for i, w := range f.pipes {
		if !f.alive[i] {
			continue
		}
		if _, err := w.Write(p); err != nil {
			f.alive[i] = false
			continue
		}
		any = true
	}
	if !any {
		return 0, errAllFailed
	}
	return len(p), nil
}

func (f *fanout) closeAll(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.pipes {
		w.CloseWithError(err)
	}
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
