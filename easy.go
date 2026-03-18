package ublk

import (
	"fmt"
	"io"
	"syscall"
)

const (
	// defaultWriteZeroesChunkBytes bounds the scratch zero buffer used when the
	// backing store does not expose a native write-zeroes operation.
	defaultWriteZeroesChunkBytes = 64 * 1024
)

// Flusher flushes buffered state to stable storage.
type Flusher interface {
	Flush() error
}

// Syncer syncs buffered state to stable storage.
type Syncer interface {
	Sync() error
}

// Discarder releases storage for the given byte range.
type Discarder interface {
	Discard(off, length int64) error
}

// Zeroer writes zeroes to the given byte range.
type Zeroer interface {
	WriteZeroes(off, length int64) error
}

// BlockFile is the minimal backing-store surface for the easy-mode file
// handler. The caller retains ownership of the backing store and may close it
// through the returned handler when convenient.
type BlockFile interface {
	io.ReaderAt
	io.WriterAt
	io.Closer
}

// ReaderAtHandlerOptions configures NewReaderAtHandler.
type ReaderAtHandlerOptions struct {
	// WriteZeroesChunkBytes bounds the scratch zero buffer used to emulate
	// write-zeroes operations when the backing store does not implement Zeroer.
	// If zero, a 64 KiB default is used.
	WriteZeroesChunkBytes int
}

// ReaderAtHandler adapts a random-access reader, and any optional capabilities
// it also implements, as a ublk handler.
//
// The adapter maps block-device reads to ReaderAt calls. If the backing value
// also implements WriterAt, block-device writes and write-zeroes are enabled;
// otherwise those requests fail with EROFS. Flush, discard, and close use
// optional side interfaces when present.
type ReaderAtHandler struct {
	reader              io.ReaderAt
	writer              io.WriterAt
	closer              io.Closer
	flusher             Flusher
	syncer              Syncer
	discarder           Discarder
	zeroer              Zeroer
	writeZeroesChunkLen int
}

// NewReaderAtHandler returns an easy-mode handler backed by a random-access
// reader and any optional interfaces it also implements.
func NewReaderAtHandler(backing io.ReaderAt, opts ReaderAtHandlerOptions) *ReaderAtHandler {
	chunkLen := opts.WriteZeroesChunkBytes
	if chunkLen <= 0 {
		chunkLen = defaultWriteZeroesChunkBytes
	}
	handler := &ReaderAtHandler{
		reader:              backing,
		writeZeroesChunkLen: chunkLen,
	}
	handler.writer, _ = backing.(io.WriterAt)
	handler.closer, _ = backing.(io.Closer)
	handler.flusher, _ = backing.(Flusher)
	handler.syncer, _ = backing.(Syncer)
	handler.discarder, _ = backing.(Discarder)
	handler.zeroer, _ = backing.(Zeroer)
	return handler
}

// Close closes the wrapped backing store.
func (h *ReaderAtHandler) Close() error {
	if h.closer == nil {
		return nil
	}
	return h.closer.Close()
}

// HandleIO implements Handler for the wrapped backing store.
func (h *ReaderAtHandler) HandleIO(req *Request) error {
	off := int64(req.StartSector) * 512
	size := int(req.NrSectors) * 512

	switch req.Op {
	case OpRead:
		buf := make([]byte, size)
		if _, err := readFullAt(h.reader, buf, off); err != nil {
			return err
		}
		_, err := req.WriteData(buf)
		return err
	case OpWrite:
		if h.writer == nil {
			return syscall.EROFS
		}
		buf := make([]byte, size)
		if _, err := req.ReadData(buf); err != nil {
			return err
		}
		_, err := writeFullAt(h.writer, buf, off)
		return err
	case OpFlush:
		return h.flush()
	case OpDiscard:
		return h.discard(off, int64(size))
	case OpWriteZeroes:
		return h.writeZeroes(off, int64(size))
	default:
		return fmt.Errorf("unsupported op: %s", req.Op)
	}
}

func (h *ReaderAtHandler) flush() error {
	if h.flusher != nil {
		return h.flusher.Flush()
	}
	if h.syncer != nil {
		return h.syncer.Sync()
	}
	return nil
}

func (h *ReaderAtHandler) discard(off, length int64) error {
	if h.discarder != nil {
		return h.discarder.Discard(off, length)
	}
	return nil
}

func (h *ReaderAtHandler) writeZeroes(off, length int64) error {
	if h.zeroer != nil {
		return h.zeroer.WriteZeroes(off, length)
	}
	if length <= 0 {
		return nil
	}
	if h.writer == nil {
		return syscall.EROFS
	}

	chunkLen := h.writeZeroesChunkLen
	if chunkLen <= 0 {
		chunkLen = defaultWriteZeroesChunkBytes
	}
	zeroes := make([]byte, chunkLen)
	remaining := length
	currentOff := off
	for remaining > 0 {
		writeLen := len(zeroes)
		if remaining < int64(writeLen) {
			writeLen = int(remaining)
		}
		if _, err := writeFullAt(h.writer, zeroes[:writeLen], currentOff); err != nil {
			return err
		}
		currentOff += int64(writeLen)
		remaining -= int64(writeLen)
	}
	return nil
}
