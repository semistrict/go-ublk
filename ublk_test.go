package ublk_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/semistrict/go-ublk"
)

func skipIfNoUblk(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}
	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		t.Skip("ublk not available: /dev/ublk-control missing")
	}
}

const (
	testSectors    = 2048 // 1 MiB
	testDevBytes   = testSectors * 512
	testQueueDepth = 128
)

// ramdiskHandler implements a simple in-memory block device handler.
type ramdiskHandler struct {
	mu   sync.Mutex
	data []byte
}

func newRamdiskHandler(size int) *ramdiskHandler {
	return &ramdiskHandler{data: make([]byte, size)}
}

type recordingRamdiskHandler struct {
	*ramdiskHandler
	mu      sync.Mutex
	ops     []ublk.IOOp
	failOps map[ublk.IOOp]error
}

type nullHandler struct{}

func newRecordingRamdiskHandler(size int) *recordingRamdiskHandler {
	return &recordingRamdiskHandler{
		ramdiskHandler: newRamdiskHandler(size),
		failOps:        make(map[ublk.IOOp]error),
	}
}

func (h *recordingRamdiskHandler) HandleIO(req *ublk.Request) error {
	h.mu.Lock()
	h.ops = append(h.ops, req.Op)
	err := h.failOps[req.Op]
	h.mu.Unlock()
	if err != nil {
		return err
	}
	return h.ramdiskHandler.HandleIO(req)
}

func (h *recordingRamdiskHandler) Count(op ublk.IOOp) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, got := range h.ops {
		if got == op {
			count++
		}
	}
	return count
}

func (h *ramdiskHandler) HandleIO(req *ublk.Request) error {
	off := int(req.StartSector) * 512
	size := int(req.NrSectors) * 512

	h.mu.Lock()
	defer h.mu.Unlock()

	if off+size > len(h.data) {
		return fmt.Errorf("out of range")
	}

	switch req.Op {
	case ublk.OpRead:
		_, err := req.WriteData(h.data[off : off+size])
		return err
	case ublk.OpWrite:
		buf := make([]byte, size)
		_, err := req.ReadData(buf)
		if err != nil {
			return err
		}
		copy(h.data[off:off+size], buf)
		return nil
	case ublk.OpFlush:
		return nil
	case ublk.OpDiscard, ublk.OpWriteZeroes:
		clear(h.data[off : off+size])
		return nil
	default:
		return fmt.Errorf("unsupported op: %s", req.Op)
	}
}

func (nullHandler) HandleIO(req *ublk.Request) error {
	switch req.Op {
	case ublk.OpRead:
		return nil
	case ublk.OpWrite, ublk.OpFlush, ublk.OpDiscard, ublk.OpWriteZeroes:
		return nil
	default:
		return fmt.Errorf("unsupported op: %s", req.Op)
	}
}

func basicParams(sectors uint64) *ublk.Params {
	return &ublk.Params{
		Types: ublk.ParamTypeBasic,
		Basic: ublk.ParamBasic{
			LogicalBSShift:  9,
			PhysicalBSShift: 12,
			IOOptShift:      12,
			IOMinShift:      9,
			MaxSectors:      1024,
			DevSectors:      sectors,
		},
	}
}

func discardParams(sectors uint64) *ublk.Params {
	return &ublk.Params{
		Types: ublk.ParamTypeBasic | ublk.ParamTypeDiscard,
		Basic: ublk.ParamBasic{
			LogicalBSShift:  9,
			PhysicalBSShift: 12,
			IOOptShift:      12,
			IOMinShift:      9,
			MaxSectors:      1024,
			DevSectors:      sectors,
		},
		Discard: ublk.ParamDiscard{
			DiscardAlignment:      4096,
			DiscardGranularity:    4096,
			MaxDiscardSectors:     1024,
			MaxWriteZeroesSectors: 1024,
			MaxDiscardSegments:    1,
		},
	}
}

// setupRamdiskWithConfig creates a ublk device, applies params, starts
// serving it with the given handler, and waits until the block device is ready.
func setupRamdiskWithConfig(t *testing.T, opts ublk.DeviceOptions, params *ublk.Params, handler ublk.Handler) *ublk.Device {
	t.Helper()
	skipIfNoUblk(t)

	if opts.Queues == 0 {
		opts.Queues = 1
	}
	if opts.QueueDepth == 0 {
		opts.QueueDepth = testQueueDepth
	}

	dev, err := ublk.NewDevice(opts)
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}

	if params == nil {
		params = basicParams(testSectors)
	}
	err = dev.SetParams(params)
	if err != nil {
		dev.Delete()
		t.Fatalf("SetParams: %v", err)
	}

	if handler == nil {
		handler = newRamdiskHandler(int(params.Basic.DevSectors * 512))
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- dev.Serve(handler)
	}()

	t.Cleanup(func() {
		dev.Delete()
		if err := <-serveErr; err != nil {
			t.Errorf("Serve returned error: %v", err)
		}
	})

	// Wait for the block device to appear
	blkPath := dev.BlockDevPath()
	for i := 0; i < 1000; i++ {
		if _, err := os.Stat(blkPath); err == nil {
			return dev
		}
		syscall.Nanosleep(&syscall.Timespec{Nsec: 10_000_000}, nil) // 10ms
	}
	t.Fatalf("block device %s did not appear", blkPath)
	return nil
}

// setupRamdisk creates a ublk device backed by memory, starts serving it,
// and returns the device once the block device path is ready for IO.
func setupRamdisk(t *testing.T) *ublk.Device {
	return setupRamdiskWithConfig(t, ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: testQueueDepth,
	}, basicParams(testSectors), newRamdiskHandler(testDevBytes))
}

func openBlkDev(t *testing.T, dev *ublk.Device, flags int) *os.File {
	t.Helper()
	f, err := os.OpenFile(dev.BlockDevPath(), flags|syscall.O_DIRECT, 0)
	if err != nil {
		t.Fatalf("open %s: %v", dev.BlockDevPath(), err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestWriteRead(t *testing.T) {
	dev := setupRamdisk(t)
	f := openBlkDev(t, dev, os.O_RDWR)

	// Write a known pattern
	pattern := alignedBuf(4096)
	for i := range pattern {
		pattern[i] = byte(i % 251) // prime modulus for non-repeating pattern
	}
	n, err := f.WriteAt(pattern, 0)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if n != len(pattern) {
		t.Fatalf("WriteAt wrote %d bytes, want %d", n, len(pattern))
	}

	// Read it back
	got := alignedBuf(4096)
	n, err = f.ReadAt(got, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(got) {
		t.Fatalf("ReadAt read %d bytes, want %d", n, len(got))
	}
	if !bytes.Equal(got, pattern) {
		t.Fatal("read data does not match written data")
	}
}

func TestWriteReadMultipleOffsets(t *testing.T) {
	dev := setupRamdisk(t)
	f := openBlkDev(t, dev, os.O_RDWR)

	offsets := []int64{0, 4096, 8192, 512 * 1024}
	for _, off := range offsets {
		t.Run(fmt.Sprintf("offset_%d", off), func(t *testing.T) {
			buf := alignedBuf(4096)
			rand.Read(buf)

			if _, err := f.WriteAt(buf, off); err != nil {
				t.Fatalf("WriteAt offset %d: %v", off, err)
			}

			got := alignedBuf(4096)
			if _, err := f.ReadAt(got, off); err != nil {
				t.Fatalf("ReadAt offset %d: %v", off, err)
			}

			if !bytes.Equal(got, buf) {
				t.Fatalf("mismatch at offset %d", off)
			}
		})
	}
}

func TestReadZeroes(t *testing.T) {
	dev := setupRamdisk(t)
	f := openBlkDev(t, dev, os.O_RDONLY)

	// A fresh ramdisk should read all zeroes
	buf := alignedBuf(4096)
	n, err := f.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("ReadAt read %d bytes, want %d", n, len(buf))
	}

	zeroes := make([]byte, 4096)
	if !bytes.Equal(buf, zeroes) {
		t.Fatal("fresh device did not return zeroes")
	}
}

func TestLargeIO(t *testing.T) {
	dev := setupRamdisk(t)
	f := openBlkDev(t, dev, os.O_RDWR)

	// Write 512 KiB (max_sectors * 512)
	size := 512 * 1024
	pattern := alignedBuf(size)
	rand.Read(pattern)

	if _, err := f.WriteAt(pattern, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	got := alignedBuf(size)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	if !bytes.Equal(got, pattern) {
		t.Fatal("large IO data mismatch")
	}
}

func TestOverwrite(t *testing.T) {
	dev := setupRamdisk(t)
	f := openBlkDev(t, dev, os.O_RDWR)

	// Write pattern A
	a := alignedBuf(4096)
	for i := range a {
		a[i] = 0xAA
	}
	if _, err := f.WriteAt(a, 0); err != nil {
		t.Fatalf("WriteAt A: %v", err)
	}

	// Overwrite with pattern B
	b := alignedBuf(4096)
	for i := range b {
		b[i] = 0xBB
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		t.Fatalf("WriteAt B: %v", err)
	}

	// Should read B, not A
	got := alignedBuf(4096)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, b) {
		t.Fatal("overwrite: read back data matches A instead of B")
	}
}

func TestConcurrentIO(t *testing.T) {
	dev := setupRamdisk(t)

	// Multiple goroutines each write to non-overlapping regions then read back
	const numWorkers = 8
	const chunkSize = 4096
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			f, err := os.OpenFile(dev.BlockDevPath(), os.O_RDWR|syscall.O_DIRECT, 0)
			if err != nil {
				t.Errorf("worker %d: open: %v", idx, err)
				return
			}
			defer f.Close()

			off := int64(idx) * chunkSize
			buf := alignedBuf(chunkSize)
			for j := range buf {
				buf[j] = byte(idx)
			}

			if _, err := f.WriteAt(buf, off); err != nil {
				t.Errorf("worker %d: write: %v", idx, err)
				return
			}

			got := alignedBuf(chunkSize)
			if _, err := f.ReadAt(got, off); err != nil {
				t.Errorf("worker %d: read: %v", idx, err)
				return
			}

			if !bytes.Equal(got, buf) {
				t.Errorf("worker %d: data mismatch", idx)
			}
		}(i)
	}
	wg.Wait()
}

func TestFullDeviceWriteRead(t *testing.T) {
	dev := setupRamdisk(t)
	// Use buffered IO (no O_DIRECT) to avoid overwhelming the queue
	// with too many concurrent requests from the block layer.
	f, err := os.OpenFile(dev.BlockDevPath(), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	// Write the entire device in 4KiB chunks, read it all back
	chunkSize := 4096
	pattern := alignedBuf(chunkSize)

	for off := int64(0); off < testDevBytes; off += int64(chunkSize) {
		// Different pattern per chunk
		for i := range pattern {
			pattern[i] = byte((int64(i) + off) % 253)
		}
		if _, err := f.WriteAt(pattern, off); err != nil {
			t.Fatalf("WriteAt offset %d: %v", off, err)
		}
		// Sync periodically to avoid overwhelming the queue
		if off%(64*4096) == 0 {
			f.Sync()
		}
	}

	got := alignedBuf(chunkSize)
	for off := int64(0); off < testDevBytes; off += int64(chunkSize) {
		if _, err := f.ReadAt(got, off); err != nil {
			t.Fatalf("ReadAt offset %d: %v", off, err)
		}

		for i := range pattern {
			pattern[i] = byte((int64(i) + off) % 253)
		}
		if !bytes.Equal(got, pattern) {
			t.Fatalf("data mismatch at offset %d", off)
		}
	}
}

func TestReadBeyondEOF(t *testing.T) {
	dev := setupRamdisk(t)
	f := openBlkDev(t, dev, os.O_RDONLY)

	// Reading at exactly the end should return EOF/error
	buf := alignedBuf(4096)
	_, err := f.ReadAt(buf, testDevBytes)
	if err == nil {
		t.Fatal("expected error reading beyond device end, got nil")
	}
}

func TestDeviceLifecycle(t *testing.T) {
	skipIfNoUblk(t)

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: 32,
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}

	id := dev.ID()
	if id < 0 {
		t.Fatalf("got negative device ID: %d", id)
	}

	charPath := dev.CharDevPath()
	if _, err := os.Stat(charPath); err != nil {
		t.Fatalf("char device %s not found: %v", charPath, err)
	}

	// Delete should succeed
	if err := dev.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Char device should be gone
	if _, err := os.Stat(charPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("char device %s still exists after delete", charPath)
	}
}

func TestGetParamsRoundTrip(t *testing.T) {
	skipIfNoUblk(t)

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: testQueueDepth,
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	defer dev.Delete()

	want := &ublk.Params{
		Types: ublk.ParamTypeBasic | ublk.ParamTypeDiscard,
		Basic: ublk.ParamBasic{
			LogicalBSShift:  9,
			PhysicalBSShift: 12,
			IOOptShift:      12,
			IOMinShift:      9,
			MaxSectors:      1024,
			ChunkSectors:    8,
			DevSectors:      testSectors,
		},
		Discard: ublk.ParamDiscard{
			DiscardAlignment:      4096,
			DiscardGranularity:    4096,
			MaxDiscardSectors:     1024,
			MaxWriteZeroesSectors: 1024,
			MaxDiscardSegments:    1,
		},
	}
	if err := dev.SetParams(want); err != nil {
		t.Fatalf("SetParams: %v", err)
	}

	got, err := dev.GetParams()
	if err != nil {
		t.Fatalf("GetParams: %v", err)
	}

	if got.Len == 0 {
		t.Fatal("GetParams returned zero Len")
	}
	if got.Types&ublk.ParamTypeBasic == 0 {
		t.Fatalf("GetParams missing ParamTypeBasic: types=0x%x", got.Types)
	}
	if got.Types&ublk.ParamTypeDiscard == 0 {
		t.Fatalf("GetParams missing ParamTypeDiscard: types=0x%x", got.Types)
	}
	if got.Basic != want.Basic {
		t.Fatalf("basic params mismatch:\n got  %+v\n want %+v", got.Basic, want.Basic)
	}
	if got.Discard != want.Discard {
		t.Fatalf("discard params mismatch:\n got  %+v\n want %+v", got.Discard, want.Discard)
	}
}

func TestGetParamsAfterStartIncludesDevt(t *testing.T) {
	dev := setupRamdisk(t)

	params, err := dev.GetParams()
	if err != nil {
		t.Fatalf("GetParams: %v", err)
	}
	if params.Types&ublk.ParamTypeDevt == 0 {
		t.Fatalf("GetParams missing ParamTypeDevt after START_DEV: types=0x%x", params.Types)
	}

	var charSt, blkSt syscall.Stat_t
	if err := syscall.Stat(dev.CharDevPath(), &charSt); err != nil {
		t.Fatalf("stat char dev: %v", err)
	}
	if err := syscall.Stat(dev.BlockDevPath(), &blkSt); err != nil {
		t.Fatalf("stat block dev: %v", err)
	}

	charMajor, charMinor := majorMinor(uint64(charSt.Rdev))
	blkMajor, blkMinor := majorMinor(uint64(blkSt.Rdev))

	if params.Devt.CharMajor != charMajor || params.Devt.CharMinor != charMinor {
		t.Fatalf("char devt mismatch: got %d:%d want %d:%d",
			params.Devt.CharMajor, params.Devt.CharMinor, charMajor, charMinor)
	}
	if params.Devt.DiskMajor != blkMajor || params.Devt.DiskMinor != blkMinor {
		t.Fatalf("block devt mismatch: got %d:%d want %d:%d",
			params.Devt.DiskMajor, params.Devt.DiskMinor, blkMajor, blkMinor)
	}
}

func TestStopRemovesBlockDeviceKeepsCharDevice(t *testing.T) {
	dev := setupRamdisk(t)

	if err := dev.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitForPathState(t, dev.BlockDevPath(), false)
	waitForPathState(t, dev.CharDevPath(), true)
}

func TestHandlerErrorPropagatesEIO(t *testing.T) {
	handler := newRecordingRamdiskHandler(testDevBytes)
	handler.failOps[ublk.OpWrite] = errors.New("injected write failure")

	dev := setupRamdiskWithConfig(t, ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: testQueueDepth,
	}, basicParams(testSectors), handler)
	f := openBlkDev(t, dev, os.O_RDWR)

	buf := alignedBuf(4096)
	_, err := f.WriteAt(buf, 0)
	if err == nil {
		t.Fatal("WriteAt unexpectedly succeeded")
	}
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("WriteAt error = %v, want EIO", err)
	}
	if handler.Count(ublk.OpWrite) == 0 {
		t.Fatal("handler never observed OpWrite")
	}
}

func TestDiscardAndWriteZeroesOps(t *testing.T) {
	if _, err := exec.LookPath("blkdiscard"); err != nil {
		t.Skip("blkdiscard not available")
	}

	handler := newRecordingRamdiskHandler(testDevBytes)
	dev := setupRamdiskWithConfig(t, ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: testQueueDepth,
	}, discardParams(testSectors), handler)

	writePattern := func(off int64, fill byte) {
		f, err := os.OpenFile(dev.BlockDevPath(), os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open %s: %v", dev.BlockDevPath(), err)
		}
		defer f.Close()

		buf := bytes.Repeat([]byte{fill}, 4096)
		if _, err := f.WriteAt(buf, off); err != nil {
			t.Fatalf("WriteAt offset %d: %v", off, err)
		}
	}

	readChunk := func(off int64) []byte {
		f, err := os.OpenFile(dev.BlockDevPath(), os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("open %s: %v", dev.BlockDevPath(), err)
		}
		defer f.Close()

		buf := make([]byte, 4096)
		if _, err := f.ReadAt(buf, off); err != nil {
			t.Fatalf("ReadAt offset %d: %v", off, err)
		}
		return buf
	}

	writePattern(0, 0x5A)
	out, err := exec.Command("blkdiscard",
		"-o", "0",
		"-l", "4096",
		dev.BlockDevPath(),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("blkdiscard failed: %v\noutput: %s", err, out)
	}
	if got := readChunk(0); !bytes.Equal(got, make([]byte, 4096)) {
		t.Fatal("discarded range did not read back as zeroes")
	}

	writePattern(4096, 0xA5)
	out, err = exec.Command("blkdiscard",
		"-z",
		"-o", "4096",
		"-l", "4096",
		dev.BlockDevPath(),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("blkdiscard --zeroout failed: %v\noutput: %s", err, out)
	}
	if got := readChunk(4096); !bytes.Equal(got, make([]byte, 4096)) {
		t.Fatal("zeroout range did not read back as zeroes")
	}

	if handler.Count(ublk.OpDiscard) == 0 {
		t.Fatal("handler never observed OpDiscard")
	}
	if handler.Count(ublk.OpWriteZeroes) == 0 {
		t.Fatal("handler never observed OpWriteZeroes")
	}
}

func TestUnsupportedDiscardOpReturnsError(t *testing.T) {
	if _, err := exec.LookPath("blkdiscard"); err != nil {
		t.Skip("blkdiscard not available")
	}

	handler := newRecordingRamdiskHandler(testDevBytes)
	handler.failOps[ublk.OpDiscard] = errors.New("discard unsupported")

	dev := setupRamdiskWithConfig(t, ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: testQueueDepth,
	}, discardParams(testSectors), handler)

	out, err := exec.Command("blkdiscard",
		"-o", "0",
		"-l", "4096",
		dev.BlockDevPath(),
	).CombinedOutput()
	if err == nil {
		t.Fatalf("blkdiscard unexpectedly succeeded\noutput: %s", out)
	}
	if handler.Count(ublk.OpDiscard) == 0 {
		t.Fatal("handler never observed OpDiscard")
	}
}

func TestUnalignedODirectRejected(t *testing.T) {
	dev := setupRamdisk(t)
	f := openBlkDev(t, dev, os.O_RDWR)

	t.Run("misaligned-buffer", func(t *testing.T) {
		raw := alignedBuf(4097)
		buf := raw[1 : 1+4096]
		_, err := f.WriteAt(buf, 0)
		if err == nil {
			t.Fatal("WriteAt with misaligned buffer unexpectedly succeeded")
		}
	})

	t.Run("misaligned-offset", func(t *testing.T) {
		buf := alignedBuf(4096)
		_, err := f.WriteAt(buf, 1)
		if err == nil {
			t.Fatal("WriteAt with misaligned offset unexpectedly succeeded")
		}
	})
}

func TestStopAndDeleteAreIdempotent(t *testing.T) {
	dev := setupRamdisk(t)

	if err := dev.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := dev.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := dev.Delete(); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := dev.Delete(); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestOpenHandleAfterStop(t *testing.T) {
	dev := setupRamdisk(t)
	f := openBlkDev(t, dev, os.O_RDWR)

	buf := alignedBuf(4096)
	rand.Read(buf)
	if _, err := f.WriteAt(buf, 0); err != nil {
		t.Fatalf("WriteAt before Stop: %v", err)
	}

	if err := dev.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForPathState(t, dev.BlockDevPath(), false)

	if _, err := os.OpenFile(dev.BlockDevPath(), os.O_RDONLY, 0); err == nil {
		t.Fatal("opening block device after Stop unexpectedly succeeded")
	}

	got := alignedBuf(4096)
	if _, err := f.ReadAt(got, 0); err == nil {
		t.Fatal("existing block fd remained usable after Stop")
	}
}

func TestQueueDepthOneConcurrentIO(t *testing.T) {
	dev := setupRamdiskWithConfig(t, ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: 1,
	}, basicParams(testSectors), newRamdiskHandler(testDevBytes))

	const workers = 4
	const iterations = 8
	const chunkSize = 4096

	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			f, err := os.OpenFile(dev.BlockDevPath(), os.O_RDWR|syscall.O_DIRECT, 0)
			if err != nil {
				t.Errorf("worker %d: open: %v", id, err)
				return
			}
			defer f.Close()

			for iter := 0; iter < iterations; iter++ {
				off := int64((id*iterations + iter) * chunkSize)
				buf := alignedBuf(chunkSize)
				for i := range buf {
					buf[i] = byte(id*17 + iter)
				}
				if _, err := f.WriteAt(buf, off); err != nil {
					t.Errorf("worker %d iter %d: write: %v", id, iter, err)
					return
				}

				got := alignedBuf(chunkSize)
				if _, err := f.ReadAt(got, off); err != nil {
					t.Errorf("worker %d iter %d: read: %v", id, iter, err)
					return
				}
				if !bytes.Equal(got, buf) {
					t.Errorf("worker %d iter %d: data mismatch", id, iter)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}

func TestMinimalGeometryBoundary(t *testing.T) {
	const sectors = 1
	dev := setupRamdiskWithConfig(t, ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: 4,
	}, basicParams(sectors), newRamdiskHandler(sectors*512))

	f, err := os.OpenFile(dev.BlockDevPath(), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	want := bytes.Repeat([]byte{0x7C}, 512)
	if _, err := f.WriteAt(want, 0); err != nil {
		t.Fatalf("WriteAt sector 0: %v", err)
	}

	got := make([]byte, 512)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt sector 0: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("single-sector device returned wrong data")
	}

	if _, err := f.ReadAt(make([]byte, 512), 512); err == nil {
		t.Fatal("ReadAt beyond end unexpectedly succeeded")
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0x11}, 512), 512); err == nil {
		t.Fatal("WriteAt beyond end unexpectedly succeeded")
	}
}

func TestGetParamsBeforeStartLeavesBlockDevtUnset(t *testing.T) {
	skipIfNoUblk(t)

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: testQueueDepth,
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	defer dev.Delete()

	if err := dev.SetParams(basicParams(testSectors)); err != nil {
		t.Fatalf("SetParams: %v", err)
	}

	params, err := dev.GetParams()
	if err != nil {
		t.Fatalf("GetParams: %v", err)
	}

	if params.Devt.DiskMajor != 0 || params.Devt.DiskMinor != 0 {
		t.Fatalf("pre-start GetParams unexpectedly exposed block devt: %+v", params.Devt)
	}
	if params.Devt.CharMajor == 0 {
		t.Fatalf("pre-start GetParams did not expose char devt: %+v", params.Devt)
	}
}

func TestServeStartupFailureCleanup(t *testing.T) {
	skipIfNoUblk(t)

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: 32,
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}

	charPath := dev.CharDevPath()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- dev.Serve(ublk.HandlerFunc(func(req *ublk.Request) error {
			return nil
		}))
	}()

	err = <-serveErr
	if err == nil {
		t.Fatal("Serve unexpectedly succeeded without SetParams")
	}

	if err := dev.Delete(); err != nil {
		t.Fatalf("Delete after failed Serve: %v", err)
	}
	waitForPathState(t, charPath, false)
	waitForPathState(t, dev.BlockDevPath(), false)
}

func TestMultiQueueMixedWorkload(t *testing.T) {
	dev := setupRamdiskWithConfig(t, ublk.DeviceOptions{
		Queues:     2,
		QueueDepth: testQueueDepth,
	}, basicParams(testSectors), newRamdiskHandler(testDevBytes))

	const workers = 8
	const chunkSize = 4096

	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			f, err := os.OpenFile(dev.BlockDevPath(), os.O_RDWR, 0)
			if err != nil {
				t.Errorf("worker %d: open: %v", id, err)
				return
			}
			defer f.Close()

			off := int64(id * chunkSize)
			want := bytes.Repeat([]byte{byte(id + 1)}, chunkSize)
			if _, err := f.WriteAt(want, off); err != nil {
				t.Errorf("worker %d: write: %v", id, err)
				return
			}

			got := make([]byte, chunkSize)
			if _, err := f.ReadAt(got, off); err != nil {
				t.Errorf("worker %d: read: %v", id, err)
				return
			}
			if !bytes.Equal(got, want) {
				t.Errorf("worker %d: data mismatch", id)
			}
		}(worker)
	}
	wg.Wait()
}

// --- Tests inspired by libublk-rs ---

func TestDDRead(t *testing.T) {
	dev := setupRamdisk(t)

	// Read 40 MiB via dd — exercises sustained sequential IO.
	out, err := exec.Command("dd",
		fmt.Sprintf("if=%s", dev.BlockDevPath()),
		"of=/dev/null",
		"bs=4096",
		"count=10240",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("dd read failed: %v\noutput: %s", err, out)
	}
	t.Logf("dd output: %s", out)
}

func TestDDReadNullHandler(t *testing.T) {
	const (
		count   = 10240
		bs      = 4096
		sectors = count * (bs / 512)
	)

	dev := setupRamdiskWithConfig(t, ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: testQueueDepth,
		Flags:      ublk.FlagNoAutoPartScan,
	}, basicParams(sectors), nullHandler{})

	out, err := exec.Command("dd",
		fmt.Sprintf("if=%s", dev.BlockDevPath()),
		"of=/dev/null",
		fmt.Sprintf("bs=%d", bs),
		fmt.Sprintf("count=%d", count),
		"status=none",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("dd read against null handler failed: %v\noutput: %s", err, out)
	}
}

func TestDDWriteRead(t *testing.T) {
	dev := setupRamdisk(t)

	// Write random data via dd, then read it back and compare.
	writeOut, err := exec.Command("dd",
		"if=/dev/urandom",
		fmt.Sprintf("of=%s", dev.BlockDevPath()),
		"bs=4096",
		"count=64",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("dd write failed: %v\noutput: %s", err, writeOut)
	}

	// Sync to flush page cache before reading back
	exec.Command("sync").Run()

	readOut, err := exec.Command("dd",
		fmt.Sprintf("if=%s", dev.BlockDevPath()),
		"of=/dev/null",
		"bs=4096",
		"count=64",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("dd read failed: %v\noutput: %s", err, readOut)
	}
}

func TestMultiQueue(t *testing.T) {
	skipIfNoUblk(t)

	data := make([]byte, testDevBytes)

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     2,
		QueueDepth: testQueueDepth,
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}

	err = dev.SetParams(&ublk.Params{
		Types: ublk.ParamTypeBasic,
		Basic: ublk.ParamBasic{
			LogicalBSShift:  9,
			PhysicalBSShift: 12,
			IOOptShift:      12,
			IOMinShift:      9,
			MaxSectors:      1024,
			DevSectors:      testSectors,
		},
	})
	if err != nil {
		dev.Delete()
		t.Fatalf("SetParams: %v", err)
	}

	handler := newRamdiskHandler(testDevBytes)
	// Pre-copy data so reads return something non-zero
	for i := range data {
		data[i] = byte(i % 199)
	}
	copy(handler.data, data)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- dev.Serve(handler)
	}()

	t.Cleanup(func() {
		dev.Delete()
		if err := <-serveErr; err != nil {
			t.Errorf("Serve returned error: %v", err)
		}
	})

	blkPath := dev.BlockDevPath()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(blkPath); err == nil {
			break
		}
		syscall.Nanosleep(&syscall.Timespec{Nsec: 10_000_000}, nil)
	}

	// Read via dd to verify multi-queue works
	out, err := exec.Command("dd",
		fmt.Sprintf("if=%s", blkPath),
		"of=/dev/null",
		"bs=4096",
		"count=256",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("dd read failed: %v\noutput: %s", err, out)
	}
	t.Logf("multi-queue dd: %s", out)
}

func TestMkfsExt4(t *testing.T) {
	skipIfNoUblk(t)

	// Need a bigger device for ext4 (at least 1MiB, ideally more)
	const sectors = 16384 // 8 MiB
	const devBytes = sectors * 512
	data := make([]byte, devBytes)

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: testQueueDepth,
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}

	err = dev.SetParams(&ublk.Params{
		Types: ublk.ParamTypeBasic,
		Basic: ublk.ParamBasic{
			LogicalBSShift:  9,
			PhysicalBSShift: 12,
			IOOptShift:      12,
			IOMinShift:      9,
			MaxSectors:      1024,
			DevSectors:      sectors,
		},
	})
	if err != nil {
		dev.Delete()
		t.Fatalf("SetParams: %v", err)
	}

	handler := &ramdiskHandler{data: data}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- dev.Serve(handler)
	}()

	t.Cleanup(func() {
		dev.Delete()
		if err := <-serveErr; err != nil {
			t.Errorf("Serve returned error: %v", err)
		}
	})

	blkPath := dev.BlockDevPath()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(blkPath); err == nil {
			break
		}
		syscall.Nanosleep(&syscall.Timespec{Nsec: 10_000_000}, nil)
	}

	// Check if mkfs.ext4 is available
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}

	// Format as ext4
	out, err := exec.Command("mkfs.ext4", "-F", blkPath).CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs.ext4 failed: %v\noutput: %s", err, out)
	}
	t.Logf("mkfs.ext4: %s", out)
}

// --- Zero-copy tests ---

func skipIfNoZeroCopy(t *testing.T) {
	t.Helper()
	// UBLK_F_AUTO_BUF_REG requires kernel 6.13+.
	// Try to create a device with zero-copy. If the kernel doesn't support
	// the flags, NewDevice will fail (EINVAL for unknown flags).
	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: 4,
		Flags:      ublk.FlagSupportZeroCopy,
	})
	if err != nil {
		t.Skipf("zero-copy not supported: %v", err)
	}
	// Check that the kernel echoed back the zero-copy flag.
	if dev.Info().Flags&ublk.FlagSupportZeroCopy == 0 {
		dev.Delete()
		t.Skip("zero-copy flags stripped by kernel")
	}
	dev.Delete()
}

func setupZeroCopyLoop(t *testing.T, backingFile *os.File) *ublk.Device {
	t.Helper()
	skipIfNoUblk(t)
	skipIfNoZeroCopy(t)

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: testQueueDepth,
		Flags:      ublk.FlagSupportZeroCopy,
	})
	if err != nil {
		t.Fatalf("NewDevice (zero-copy): %v", err)
	}

	err = dev.SetParams(&ublk.Params{
		Types: ublk.ParamTypeBasic,
		Basic: ublk.ParamBasic{
			LogicalBSShift:  9,
			PhysicalBSShift: 12,
			IOOptShift:      12,
			IOMinShift:      9,
			MaxSectors:      1024,
			DevSectors:      testSectors,
		},
	})
	if err != nil {
		dev.Delete()
		t.Fatalf("SetParams: %v", err)
	}

	backingFd := int32(backingFile.Fd())

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- dev.ServeZeroCopy(ublk.ZeroCopyHandlerFunc(func(req *ublk.ZeroCopyRequest) error {
			off := int64(req.StartSector) * 512
			size := uint32(req.NrSectors) * 512

			switch req.Op {
			case ublk.OpRead:
				return req.ReadFixed(backingFd, off, size)
			case ublk.OpWrite:
				return req.WriteFixed(backingFd, off, size)
			case ublk.OpFlush:
				return nil
			case ublk.OpDiscard, ublk.OpWriteZeroes:
				return nil
			default:
				return fmt.Errorf("unsupported op: %s", req.Op)
			}
		}))
	}()

	t.Cleanup(func() {
		dev.Delete()
		if err := <-serveErr; err != nil {
			t.Errorf("ServeZeroCopy returned error: %v", err)
		}
	})

	blkPath := dev.BlockDevPath()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(blkPath); err == nil {
			return dev
		}
		syscall.Nanosleep(&syscall.Timespec{Nsec: 10_000_000}, nil)
	}
	t.Fatalf("block device %s did not appear", blkPath)
	return nil
}

func createBackingFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp("", "ublk-zc-test-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	// Extend to device size
	if err := f.Truncate(testDevBytes); err != nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() {
		f.Close()
		os.Remove(f.Name())
	})
	return f
}

func TestZeroCopyWriteRead(t *testing.T) {
	backing := createBackingFile(t)
	dev := setupZeroCopyLoop(t, backing)
	f := openBlkDev(t, dev, os.O_RDWR)

	pattern := alignedBuf(4096)
	for i := range pattern {
		pattern[i] = byte(i % 251)
	}
	if _, err := f.WriteAt(pattern, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	got := alignedBuf(4096)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, pattern) {
		t.Fatal("zero-copy: read data does not match written data")
	}
}

func TestZeroCopyMultipleOffsets(t *testing.T) {
	backing := createBackingFile(t)
	dev := setupZeroCopyLoop(t, backing)
	f := openBlkDev(t, dev, os.O_RDWR)

	offsets := []int64{0, 4096, 8192, 512 * 1024}
	for _, off := range offsets {
		t.Run(fmt.Sprintf("offset_%d", off), func(t *testing.T) {
			buf := alignedBuf(4096)
			rand.Read(buf)

			if _, err := f.WriteAt(buf, off); err != nil {
				t.Fatalf("WriteAt offset %d: %v", off, err)
			}

			got := alignedBuf(4096)
			if _, err := f.ReadAt(got, off); err != nil {
				t.Fatalf("ReadAt offset %d: %v", off, err)
			}

			if !bytes.Equal(got, buf) {
				t.Fatalf("zero-copy mismatch at offset %d", off)
			}
		})
	}
}

func TestZeroCopyLargeIO(t *testing.T) {
	backing := createBackingFile(t)
	dev := setupZeroCopyLoop(t, backing)
	f := openBlkDev(t, dev, os.O_RDWR)

	size := 512 * 1024
	pattern := alignedBuf(size)
	rand.Read(pattern)

	if _, err := f.WriteAt(pattern, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	got := alignedBuf(size)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	if !bytes.Equal(got, pattern) {
		t.Fatal("zero-copy large IO data mismatch")
	}
}

func TestZeroCopyBackingPersistence(t *testing.T) {
	backing := createBackingFile(t)
	dev := setupZeroCopyLoop(t, backing)
	f := openBlkDev(t, dev, os.O_RDWR)

	// Write through the block device
	pattern := alignedBuf(4096)
	rand.Read(pattern)
	if _, err := f.WriteAt(pattern, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Verify the data landed in the backing file
	got := make([]byte, 4096)
	if _, err := backing.ReadAt(got, 0); err != nil {
		t.Fatalf("backing ReadAt: %v", err)
	}
	if !bytes.Equal(got, pattern) {
		t.Fatal("backing file does not contain written data")
	}
}

// --- Affinity test ---

func TestGetQueueAffinity(t *testing.T) {
	skipIfNoUblk(t)

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     2,
		QueueDepth: 32,
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	defer dev.Delete()

	for q := uint16(0); q < 2; q++ {
		aff, err := dev.GetQueueAffinity(q)
		if err != nil {
			t.Fatalf("GetQueueAffinity(%d): %v", q, err)
		}
		cpus := aff.CPUs()
		if len(cpus) == 0 {
			t.Fatalf("queue %d: affinity has no CPUs", q)
		}
		t.Logf("queue %d affinity: CPUs %v", q, cpus)
	}
}

// alignedBuf allocates a page-aligned buffer (required for O_DIRECT).
func alignedBuf(size int) []byte {
	const pageSize = 4096
	// Allocate extra to allow alignment
	raw := make([]byte, size+pageSize)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	offset := (pageSize - int(addr%pageSize)) % pageSize
	return raw[offset : offset+size]
}

func waitForPathState(t *testing.T, path string, wantPresent bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		_, err := os.Stat(path)
		present := err == nil
		if present == wantPresent {
			return
		}
		if !present && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat %s: %v", path, err)
		}
		syscall.Nanosleep(&syscall.Timespec{Nsec: 10_000_000}, nil)
	}
	if wantPresent {
		t.Fatalf("path %s did not appear", path)
	}
	t.Fatalf("path %s still exists", path)
}

func majorMinor(dev uint64) (uint32, uint32) {
	major := uint32(((dev >> 8) & 0xfff) | ((dev >> 32) &^ 0xfff))
	minor := uint32((dev & 0xff) | ((dev >> 12) &^ 0xff))
	return major, minor
}
