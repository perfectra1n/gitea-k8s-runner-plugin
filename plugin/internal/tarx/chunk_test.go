package tarx

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestChunkWriterSplitsAtSize(t *testing.T) {
	var got [][]byte
	w := NewChunkWriter(4, func(b []byte) error {
		got = append(got, append([]byte(nil), b...))
		return nil
	})
	for _, s := range []string{"ab", "cdefghij", "k"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	want := []string{"abcd", "efgh", "ijk"}
	if len(got) != len(want) {
		t.Fatalf("chunks = %q, want %q", got, want)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("chunk %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestChunkWriterDefaultSizeIs64KiB(t *testing.T) {
	var sizes []int
	w := NewChunkWriter(0, func(b []byte) error { sizes = append(sizes, len(b)); return nil })
	if _, err := w.Write(make([]byte, 150*1024)); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if want := []int{65536, 65536, 22528}; !equalInts(sizes, want) {
		t.Fatalf("sizes = %v, want %v", sizes, want)
	}
}

func TestChunkWriterEmptyFlushSendsNothing(t *testing.T) {
	w := NewChunkWriter(4, func([]byte) error { t.Fatal("unexpected send"); return nil })
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestChunkWriterPropagatesSendError(t *testing.T) {
	boom := errors.New("boom")
	w := NewChunkWriter(2, func([]byte) error { return boom })
	if _, err := w.Write([]byte("abc")); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestChunkReader(t *testing.T) {
	chunks := [][]byte{[]byte("ab"), nil, []byte("cde")}
	r := NewChunkReader(func() ([]byte, error) {
		if len(chunks) == 0 {
			return nil, io.EOF
		}
		c := chunks[0]
		chunks = chunks[1:]
		return c, nil
	})
	b, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(b, []byte("abcde")) {
		t.Fatalf("read %q, %v", b, err)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
