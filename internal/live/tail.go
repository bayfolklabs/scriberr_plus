package live

import (
	"errors"
	"io"
	"os"
	"sync"
)

var errTailClosed = errors.New("tail reader closed")

// growth publishes how many bytes of a session's recording are committed, and
// whether the session input has ended. Tail readers wait on it for new data.
type growth struct {
	mu      sync.Mutex
	size    int64
	closed  bool
	changed chan struct{} // closed and replaced on every change
	done    chan struct{} // closed once the input has ended
}

func newGrowth(size int64) *growth {
	return &growth{size: size, changed: make(chan struct{}), done: make(chan struct{})}
}

// set records a new committed size and wakes waiting readers
func (g *growth) set(size int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if size == g.size {
		return
	}
	g.size = size
	close(g.changed)
	g.changed = make(chan struct{})
}

// close marks the input as ended: readers drain the committed bytes, then get io.EOF
func (g *growth) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	close(g.done)
	close(g.changed)
	g.changed = make(chan struct{})
}

func (g *growth) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

func (g *growth) snapshot() (int64, bool, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.size, g.closed, g.changed
}

// tailReader is an io.Reader over a growing file. It reads only committed bytes,
// blocks while it has caught up, and returns io.EOF once the input has ended and
// everything was read. Close unblocks a pending Read.
type tailReader struct {
	f         *os.File
	off       int64
	g         *growth
	done      chan struct{}
	closeOnce sync.Once
}

// openTail opens path for reading from byte 0. Go opens files on Windows with
// FILE_SHARE_READ|FILE_SHARE_WRITE, so this works while the chunk store appends.
func openTail(path string, g *growth) (*tailReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &tailReader{f: f, g: g, done: make(chan struct{})}, nil
}

func (t *tailReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		select {
		case <-t.done:
			return 0, errTailClosed
		default:
		}

		size, closed, changed := t.g.snapshot()
		if t.off < size {
			want := int64(len(p))
			if avail := size - t.off; avail < want {
				want = avail
			}
			n, err := t.f.ReadAt(p[:want], t.off)
			t.off += int64(n)
			if n > 0 {
				return n, nil
			}
			if err != nil && err != io.EOF {
				return 0, err
			}
			if closed {
				return 0, io.EOF // the file is shorter than committed; nothing more will come
			}
			// Committed bytes are not visible yet; wait for the next change.
		} else if closed {
			return 0, io.EOF
		}

		select {
		case <-changed:
		case <-t.done:
			return 0, errTailClosed
		}
	}
}

// Close unblocks any pending Read and closes the file
func (t *tailReader) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.done)
		err = t.f.Close()
	})
	return err
}
