// Package tarx adapts gRPC chunk streams to io.Reader/io.Writer so tar
// archives can be piped straight through pods/exec.
package tarx

import "io"

// DefaultChunkSize bounds each CopyOut/CopyIn message.
const DefaultChunkSize = 64 * 1024

// ChunkWriter buffers writes and emits fixed-size chunks via send; Flush
// emits the remainder.
type ChunkWriter struct {
	size int
	buf  []byte
	send func([]byte) error
}

// NewChunkWriter returns a writer emitting chunks of size bytes (DefaultChunkSize
// when size <= 0). send must not retain the slice.
func NewChunkWriter(size int, send func([]byte) error) *ChunkWriter {
	if size <= 0 {
		size = DefaultChunkSize
	}
	return &ChunkWriter{size: size, buf: make([]byte, 0, size), send: send}
}

func (w *ChunkWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(w.size-len(w.buf), len(p))
		w.buf = append(w.buf, p[:k]...)
		p, n = p[k:], n+k
		if len(w.buf) == w.size {
			if err := w.Flush(); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

// Flush sends any buffered bytes.
func (w *ChunkWriter) Flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	err := w.send(w.buf)
	w.buf = w.buf[:0]
	return err
}

// ChunkReader concatenates the payloads returned by recv until it returns an
// error (io.EOF ends the stream cleanly).
type ChunkReader struct {
	recv func() ([]byte, error)
	cur  []byte
}

func NewChunkReader(recv func() ([]byte, error)) *ChunkReader {
	return &ChunkReader{recv: recv}
}

func (r *ChunkReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		b, err := r.recv()
		if err != nil {
			return 0, err
		}
		r.cur = b
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

var _ io.Writer = (*ChunkWriter)(nil)
