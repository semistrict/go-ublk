package ublk

import (
	"errors"
	"io"
	"syscall"
	"testing"
)

type chunkedReaderAt struct {
	data      []byte
	chunkSize int
}

func (r *chunkedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.chunkSize {
		n = r.chunkSize
	}
	if remain := len(r.data) - int(off); n > remain {
		n = remain
	}
	copy(p[:n], r.data[off:int(off)+n])
	if int(off)+n == len(r.data) {
		return n, io.EOF
	}
	return n, nil
}

type chunkedWriterAt struct {
	data      []byte
	chunkSize int
	failAt    int64
	failErr   error
}

func (w *chunkedWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if w.failErr != nil && off >= w.failAt {
		return 0, w.failErr
	}
	n := len(p)
	if n > w.chunkSize {
		n = w.chunkSize
	}
	copy(w.data[off:int(off)+n], p[:n])
	return n, nil
}

type zeroWriterAt struct{}

func (zeroWriterAt) WriteAt([]byte, int64) (int, error) {
	return 0, nil
}

type flakyReaderAt struct {
	data        []byte
	remainingNG int
}

func (r *flakyReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.remainingNG > 0 {
		r.remainingNG--
		return 0, syscall.EINVAL
	}
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

type flakyWriterAt struct {
	data        []byte
	remainingNG int
}

func (w *flakyWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if w.remainingNG > 0 {
		w.remainingNG--
		return 0, syscall.EINVAL
	}
	return copy(w.data[off:], p), nil
}

func TestReadFullAtLoopsUntilBufferFilled(t *testing.T) {
	src := &chunkedReaderAt{
		data:      []byte("abcdefgh"),
		chunkSize: 3,
	}
	buf := make([]byte, 8)
	n, err := readFullAt(src, buf, 0)
	if err != nil {
		t.Fatalf("readFullAt err = %v", err)
	}
	if n != len(buf) {
		t.Fatalf("readFullAt n = %d, want %d", n, len(buf))
	}
	if got := string(buf); got != "abcdefgh" {
		t.Fatalf("readFullAt data = %q, want %q", got, "abcdefgh")
	}
}

func TestReadFullAtReturnsUnexpectedEOFOnZeroProgress(t *testing.T) {
	src := &chunkedReaderAt{
		data:      []byte("abc"),
		chunkSize: 3,
	}
	buf := make([]byte, 4)
	n, err := readFullAt(src, buf, 0)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("readFullAt err = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if n != 3 {
		t.Fatalf("readFullAt n = %d, want 3", n)
	}
}

func TestWriteFullAtLoopsUntilBufferWritten(t *testing.T) {
	dst := &chunkedWriterAt{
		data:      make([]byte, 8),
		chunkSize: 3,
	}
	n, err := writeFullAt(dst, []byte("abcdefgh"), 0)
	if err != nil {
		t.Fatalf("writeFullAt err = %v", err)
	}
	if n != 8 {
		t.Fatalf("writeFullAt n = %d, want 8", n)
	}
	if got := string(dst.data); got != "abcdefgh" {
		t.Fatalf("writeFullAt data = %q, want %q", got, "abcdefgh")
	}
}

func TestWriteFullAtReturnsShortWriteOnZeroProgress(t *testing.T) {
	n, err := writeFullAt(zeroWriterAt{}, []byte("abc"), 0)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeFullAt err = %v, want %v", err, io.ErrShortWrite)
	}
	if n != 0 {
		t.Fatalf("writeFullAt n = %d, want 0", n)
	}
}

func TestWriteFullAtReturnsUnderlyingError(t *testing.T) {
	dst := &chunkedWriterAt{
		data:      make([]byte, 8),
		chunkSize: 3,
		failAt:    3,
		failErr:   errors.New("boom"),
	}
	n, err := writeFullAt(dst, []byte("abcdefgh"), 0)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("writeFullAt err = %v, want boom", err)
	}
	if n != 3 {
		t.Fatalf("writeFullAt n = %d, want 3", n)
	}
}

func TestReadFullAtRetriesTransientEINVAL(t *testing.T) {
	src := &flakyReaderAt{
		data:        []byte("abcdefgh"),
		remainingNG: 2,
	}
	buf := make([]byte, 8)
	n, err := readFullAt(src, buf, 0)
	if err != nil {
		t.Fatalf("readFullAt err = %v", err)
	}
	if n != len(buf) {
		t.Fatalf("readFullAt n = %d, want %d", n, len(buf))
	}
	if got := string(buf); got != "abcdefgh" {
		t.Fatalf("readFullAt data = %q, want %q", got, "abcdefgh")
	}
}

func TestWriteFullAtRetriesTransientEINVAL(t *testing.T) {
	dst := &flakyWriterAt{
		data:        make([]byte, 8),
		remainingNG: 2,
	}
	n, err := writeFullAt(dst, []byte("abcdefgh"), 0)
	if err != nil {
		t.Fatalf("writeFullAt err = %v", err)
	}
	if n != 8 {
		t.Fatalf("writeFullAt n = %d, want 8", n)
	}
	if got := string(dst.data); got != "abcdefgh" {
		t.Fatalf("writeFullAt data = %q, want %q", got, "abcdefgh")
	}
}

func TestWriteFullAtStopsRetryingAfterLimit(t *testing.T) {
	dst := &flakyWriterAt{
		data:        make([]byte, 8),
		remainingNG: userCopyRetryLimit + 1,
	}
	n, err := writeFullAt(dst, []byte("abcdefgh"), 0)
	if !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("writeFullAt err = %v, want EINVAL", err)
	}
	if n != 0 {
		t.Fatalf("writeFullAt n = %d, want 0", n)
	}
}
