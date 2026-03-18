package ublk

import (
	"bytes"
	"syscall"
	"testing"
)

const (
	testEasySectors      = uint32(2) // Two sectors give a 1024-byte request, enough to exercise the file adapter paths.
	testEasyStartSector  = uint64(4) // Synthetic sector offset used for backing-store offset assertions.
	testEasyPayloadBytes = int(testEasySectors) * 512
	testUnsupportedIOOp  = IOOp(255) // Synthetic invalid opcode used to verify unsupported-op handling.
)

type fakeBlockFile struct {
	data       []byte
	readOff    int64
	writeOff   int64
	flushCalls int
	closeCalls int
	discardOff int64
	discardLen int64
	zeroOff    int64
	zeroLen    int64
}

func newFakeBlockFile(size int) *fakeBlockFile {
	return &fakeBlockFile{data: make([]byte, size)}
}

func (f *fakeBlockFile) ReadAt(p []byte, off int64) (int, error) {
	f.readOff = off
	copy(p, f.data[off:int(off)+len(p)])
	return len(p), nil
}

func (f *fakeBlockFile) WriteAt(p []byte, off int64) (int, error) {
	f.writeOff = off
	copy(f.data[off:int(off)+len(p)], p)
	return len(p), nil
}

func (f *fakeBlockFile) Close() error {
	f.closeCalls++
	return nil
}

func (f *fakeBlockFile) Flush() error {
	f.flushCalls++
	return nil
}

func (f *fakeBlockFile) Discard(off, length int64) error {
	f.discardOff = off
	f.discardLen = length
	return nil
}

func (f *fakeBlockFile) WriteZeroes(off, length int64) error {
	f.zeroOff = off
	f.zeroLen = length
	clear(f.data[off : off+length])
	return nil
}

type fakeBlockFileNoZero struct {
	*fakeBlockFile
}

type fakeReadOnlyFile struct {
	data    []byte
	readOff int64
}

func (f *fakeReadOnlyFile) ReadAt(p []byte, off int64) (int, error) {
	f.readOff = off
	copy(p, f.data[off:int(off)+len(p)])
	return len(p), nil
}

func TestReaderAtHandlerRead(t *testing.T) {
	backing := newFakeBlockFile(8192)
	copy(backing.data[testEasyStartSector*512:], bytes.Repeat([]byte{0x5a}, testEasyPayloadBytes))
	userCopy := &fakeUserCopyFile{}
	dev := &Device{userCopyData: userCopy}
	req := &Request{
		Op:          OpRead,
		StartSector: testEasyStartSector,
		NrSectors:   testEasySectors,
		QueueID:     testQueueID,
		Tag:         testTagZero,
		dev:         dev,
	}

	handler := NewReaderAtHandler(backing, ReaderAtHandlerOptions{})
	if err := handler.HandleIO(req); err != nil {
		t.Fatalf("HandleIO: %v", err)
	}
	if backing.readOff != int64(testEasyStartSector*512) {
		t.Fatalf("read offset = %d, want %d", backing.readOff, testEasyStartSector*512)
	}
	if len(userCopy.writeData) != testEasyPayloadBytes {
		t.Fatalf("write payload len = %d, want %d", len(userCopy.writeData), testEasyPayloadBytes)
	}
	if !bytes.Equal(userCopy.writeData, bytes.Repeat([]byte{0x5a}, testEasyPayloadBytes)) {
		t.Fatalf("write payload mismatch")
	}
}

func TestReaderAtHandlerWrite(t *testing.T) {
	payload := bytes.Repeat([]byte{0x6b}, testEasyPayloadBytes)
	userCopy := &fakeUserCopyFile{readData: payload}
	dev := &Device{userCopyData: userCopy}
	req := &Request{
		Op:          OpWrite,
		StartSector: testEasyStartSector,
		NrSectors:   testEasySectors,
		QueueID:     testQueueID,
		Tag:         testTagOne,
		dev:         dev,
	}

	backing := newFakeBlockFile(8192)
	handler := NewReaderAtHandler(backing, ReaderAtHandlerOptions{})
	if err := handler.HandleIO(req); err != nil {
		t.Fatalf("HandleIO: %v", err)
	}
	if backing.writeOff != int64(testEasyStartSector*512) {
		t.Fatalf("write offset = %d, want %d", backing.writeOff, testEasyStartSector*512)
	}
	start := int(testEasyStartSector * 512)
	if got := backing.data[start : start+testEasyPayloadBytes]; !bytes.Equal(got, payload) {
		t.Fatalf("backing payload mismatch")
	}
}

func TestReaderAtHandlerFlushDiscardAndZero(t *testing.T) {
	backing := newFakeBlockFile(8192)
	handler := NewReaderAtHandler(backing, ReaderAtHandlerOptions{})

	flushReq := &Request{Op: OpFlush, dev: &Device{}}
	if err := handler.HandleIO(flushReq); err != nil {
		t.Fatalf("flush HandleIO: %v", err)
	}
	if backing.flushCalls != 1 {
		t.Fatalf("flush calls = %d, want 1", backing.flushCalls)
	}

	discardReq := &Request{
		Op:          OpDiscard,
		StartSector: testEasyStartSector,
		NrSectors:   testEasySectors,
		dev:         &Device{},
	}
	if err := handler.HandleIO(discardReq); err != nil {
		t.Fatalf("discard HandleIO: %v", err)
	}
	if backing.discardOff != int64(testEasyStartSector*512) || backing.discardLen != int64(testEasyPayloadBytes) {
		t.Fatalf("discard = off:%d len:%d, want off:%d len:%d", backing.discardOff, backing.discardLen, testEasyStartSector*512, testEasyPayloadBytes)
	}

	zeroReq := &Request{
		Op:          OpWriteZeroes,
		StartSector: testEasyStartSector,
		NrSectors:   testEasySectors,
		dev:         &Device{},
	}
	if err := handler.HandleIO(zeroReq); err != nil {
		t.Fatalf("zero HandleIO: %v", err)
	}
	if backing.zeroOff != int64(testEasyStartSector*512) || backing.zeroLen != int64(testEasyPayloadBytes) {
		t.Fatalf("zero = off:%d len:%d, want off:%d len:%d", backing.zeroOff, backing.zeroLen, testEasyStartSector*512, testEasyPayloadBytes)
	}
}

func TestReaderAtHandlerWriteZeroesFallbackAndClose(t *testing.T) {
	backing := &fakeBlockFileNoZero{fakeBlockFile: newFakeBlockFile(8192)}
	fill := bytes.Repeat([]byte{0x7f}, testEasyPayloadBytes)
	copy(backing.data[testEasyStartSector*512:], fill)

	handler := NewReaderAtHandler(backing, ReaderAtHandlerOptions{WriteZeroesChunkBytes: 128})
	zeroReq := &Request{
		Op:          OpWriteZeroes,
		StartSector: testEasyStartSector,
		NrSectors:   testEasySectors,
		dev:         &Device{},
	}
	if err := handler.HandleIO(zeroReq); err != nil {
		t.Fatalf("zero fallback HandleIO: %v", err)
	}
	start := int(testEasyStartSector * 512)
	got := backing.data[start : start+testEasyPayloadBytes]
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Fatalf("zero fallback did not clear backing range")
	}
	if err := handler.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if backing.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", backing.closeCalls)
	}
}

func TestReaderAtHandlerUnsupportedOp(t *testing.T) {
	handler := NewReaderAtHandler(newFakeBlockFile(4096), ReaderAtHandlerOptions{})
	err := handler.HandleIO(&Request{Op: testUnsupportedIOOp, dev: &Device{}})
	if err == nil {
		t.Fatalf("HandleIO error = nil, want unsupported op")
	}
}

func TestReaderAtHandlerReadOnlyWriteAndZeroes(t *testing.T) {
	readOnly := &fakeReadOnlyFile{data: make([]byte, 8192)}
	handler := NewReaderAtHandler(readOnly, ReaderAtHandlerOptions{})

	writeErr := handler.HandleIO(&Request{
		Op:          OpWrite,
		StartSector: testEasyStartSector,
		NrSectors:   testEasySectors,
		dev:         &Device{userCopyData: &fakeUserCopyFile{readData: bytes.Repeat([]byte{0x42}, testEasyPayloadBytes)}},
	})
	if writeErr != syscall.EROFS {
		t.Fatalf("write error = %v, want %v", writeErr, syscall.EROFS)
	}

	zeroErr := handler.HandleIO(&Request{
		Op:          OpWriteZeroes,
		StartSector: testEasyStartSector,
		NrSectors:   testEasySectors,
		dev:         &Device{},
	})
	if zeroErr != syscall.EROFS {
		t.Fatalf("zero error = %v, want %v", zeroErr, syscall.EROFS)
	}

	if err := handler.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
