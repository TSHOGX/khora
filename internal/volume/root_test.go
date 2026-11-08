package volume

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCompleteAtResumesShortReadUntilEOF(t *testing.T) {
	payload := []byte("abcdefghij")
	buf := make([]byte, len(payload)+4)
	n, err := completeAt(buf, 0, true, "pread", "f", func(p []byte, off int64) (int, error) {
		if int(off) >= len(payload) {
			return 0, nil
		}
		p[0] = payload[off]
		return 1, nil
	})
	if err != io.EOF {
		t.Fatalf("err = %v; want EOF after the file ends", err)
	}
	if n != len(payload) || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("read %d bytes %q; want %q", n, buf[:n], payload)
	}
}

func TestCompleteAtRetriesEINTRAndResumesShortWrite(t *testing.T) {
	payload := []byte("abcdefghij")
	var got []byte
	interrupted := false
	n, err := completeAt(payload, 3, false, "pwrite", "f", func(p []byte, off int64) (int, error) {
		if !interrupted {
			interrupted = true
			return 0, unix.EINTR
		}
		for int64(len(got)) <= off {
			got = append(got, 0)
		}
		got[off] = p[0]
		return 1, nil
	})
	if err != nil || n != len(payload) {
		t.Fatalf("write = %d, %v; want %d bytes", n, err, len(payload))
	}
	if !bytes.Equal(got[3:], payload) {
		t.Fatalf("written %q; want %q at offset 3", got[3:], payload)
	}
}

func TestCompleteAtReturnsPartialReadError(t *testing.T) {
	want := errors.New("disk")
	n, err := completeAt(make([]byte, 4), 0, true, "pread", "f", func(p []byte, off int64) (int, error) {
		if off == 0 {
			p[0] = 'x'
			return 1, nil
		}
		return 0, want
	})
	if n != 1 || !errors.Is(err, want) {
		t.Fatalf("read = %d, %v; want 1 byte and %v", n, err, want)
	}
}

func TestLocalRootEmptyWriteAndRoundTrip(t *testing.T) {
	r, err := NewLocalRoot("vol", t.TempDir())
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer r.Close()

	n, err := r.WriteAt("empty", nil, 0)
	if err != nil || n != 0 {
		t.Fatalf("empty write = %d, %v", n, err)
	}
	fi, err := r.Stat("empty")
	if err != nil {
		t.Fatalf("stat empty: %v", err)
	}
	if fi.IsDir || fi.Size != 0 {
		t.Fatalf("empty stat = dir %v size %d", fi.IsDir, fi.Size)
	}
	buf := make([]byte, 8)
	n, err = r.ReadAt("empty", buf, 0)
	if n != 0 || err != io.EOF {
		t.Fatalf("read empty = %d, %v; want EOF", n, err)
	}

	payload := bytes.Repeat([]byte("xyz"), 5000)
	if n, err := r.WriteAt("blob", payload, 0); err != nil || n != len(payload) {
		t.Fatalf("write = %d, %v", n, err)
	}
	got := make([]byte, len(payload))
	n, err = r.ReadAt("blob", got, 0)
	if err != nil || n != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("read = %d, %v, match %v", n, err, bytes.Equal(got[:min(n, len(got))], payload))
	}
	// A read past the last byte is EOF, including when the previous read filled
	// the buffer exactly.
	n, err = r.ReadAt("blob", buf, int64(len(payload)))
	if n != 0 || err != io.EOF {
		t.Fatalf("read at end = %d, %v; want EOF", n, err)
	}
}

func TestLocalRootReadAtStopsAtEOF(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/note", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewLocalRoot("vol", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	buf := make([]byte, 8)
	n, err := r.ReadAt("note", buf, 0)
	if n != 2 || err != io.EOF || string(buf[:n]) != "hi" {
		t.Fatalf("read = %d %q %v", n, buf[:n], err)
	}
}
