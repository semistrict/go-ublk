// Package ublk provides a Go interface to the Linux ublk (userspace block device) subsystem.
//
// ublk allows implementing block devices in userspace. The kernel driver communicates
// with userspace via io_uring passthrough commands. This package uses UBLK_F_USER_COPY
// mode where data is transferred via pread/pwrite on /dev/ublkcN, which is the simplest
// and most Go-friendly approach.
//
// Basic usage:
//
//	dev, _ := ublk.NewDevice(ublk.DeviceOptions{
//	    Queues:     1,
//	    QueueDepth: 64,
//	    DevSectors: 1 << 20, // 512 MiB
//	})
//	defer dev.Delete()
//
//	dev.SetBasicParams(ublk.BasicParams{
//	    LogicalBlockShift:  9,
//	    PhysicalBlockShift: 12,
//	    DevSectors:         1 << 20,
//	})
//
//	dev.Serve(handler)
package ublk

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	controlDevPath     = "/dev/ublk-control"
	devicePollInterval = 10 * time.Millisecond // wait between device-path retries
)

// Device represents a ublk device.
type Device struct {
	id   int32
	info DevInfo

	ctrlFile     *os.File // /dev/ublk-control
	charFile     *os.File // /dev/ublkcN
	userCopyData userCopyReadWriter

	ctrlRing  *ioURing // io_uring for control commands
	ioRings   []*ioURing
	ioRingsMu sync.Mutex

	// legacyCmds is true when the kernel doesn't support ioctl-encoded
	// commands (UBLK_F_CMD_IOCTL_ENCODE). Detected at device creation.
	legacyCmds bool

	stopped    chan struct{}
	serveWg    sync.WaitGroup // tracks serve goroutines for clean shutdown
	deleteOnce sync.Once
	deleteErr  error
}

// DeviceOptions configures a new ublk device.
type DeviceOptions struct {
	// Number of hardware queues (default: 1).
	Queues uint16
	// Queue depth - max outstanding IOs per queue (default: 128).
	QueueDepth uint16
	// Max IO buffer size in bytes (default: 512KiB).
	MaxIOBufBytes uint32
	// Feature flags. FlagUserCopy is added automatically unless
	// FlagSupportZeroCopy is set (the two modes are mutually exclusive).
	Flags uint64
}

// Handler processes IO requests for a ublk device.
type Handler interface {
	// HandleIO is called for each IO request. The request describes the operation
	// and provides methods to read/write the IO data buffer.
	HandleIO(req *Request) error
}

// HandlerFunc is an adapter to use a function as a Handler.
type HandlerFunc func(req *Request) error

func (f HandlerFunc) HandleIO(req *Request) error {
	return f(req)
}

// Request represents a single IO request from the kernel.
type Request struct {
	// Op is the IO operation (OpRead, OpWrite, OpFlush, etc).
	Op IOOp
	// Flags contains IO flags (FUA, etc).
	Flags uint32
	// StartSector is the starting sector (512-byte units).
	StartSector uint64
	// NrSectors is the number of sectors.
	NrSectors uint32
	// Tag is the request tag (index into the queue).
	Tag uint16
	// QueueID is the queue this request belongs to.
	QueueID uint16

	dev *Device
}

// ReadData reads the IO data buffer into buf. For write requests, this reads
// the data the host wants to write. Uses pread on /dev/ublkcN.
func (r *Request) ReadData(buf []byte) (int, error) {
	off := ublkIOBufOffset(r.QueueID, r.Tag)
	ioDebugf("ReadData q=%d tag=%d size=%d off=%d", r.QueueID, r.Tag, len(buf), off)
	return readFullAt(r.dev.activeUserCopyTarget(), buf, int64(off))
}

// WriteData writes buf into the IO data buffer. For read requests, this provides
// the data to return to the host. Uses pwrite on /dev/ublkcN.
func (r *Request) WriteData(buf []byte) (int, error) {
	off := ublkIOBufOffset(r.QueueID, r.Tag)
	ioDebugf("WriteData q=%d tag=%d size=%d off=%d", r.QueueID, r.Tag, len(buf), off)
	n, err := writeFullAt(r.dev.activeUserCopyTarget(), buf, int64(off))
	if err == nil || len(buf) == 0 || !errors.Is(err, syscall.EINVAL) || n == len(buf) {
		if err != nil {
			ioDebugf("WriteData error q=%d tag=%d size=%d off=%d wrote=%d err=%v", r.QueueID, r.Tag, len(buf), off, n, err)
		}
		return n, err
	}

	// Some kernels appear to reject repeated user-copy writes from certain
	// caller-owned slices under sustained load. Retry the unwritten tail once
	// from a fresh scratch buffer before surfacing the error.
	retryBuf := make([]byte, len(buf)-n)
	copy(retryBuf, buf[n:])
	m, retryErr := writeFullAt(r.dev.charFile, retryBuf, int64(off)+int64(n))
	if retryErr != nil {
		ioDebugf("WriteData retry error q=%d tag=%d size=%d off=%d wrote=%d retryWrote=%d err=%v", r.QueueID, r.Tag, len(buf), off, n, m, retryErr)
	}
	return n + m, retryErr
}

type readerAt interface {
	ReadAt([]byte, int64) (int, error)
}

type writerAt interface {
	WriteAt([]byte, int64) (int, error)
}

const (
	userCopyRetryLimit = 8
	userCopyRetryBase  = 10 * time.Microsecond
)

func retryUserCopyDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	delay := userCopyRetryBase << (attempt - 1)
	maxDelay := 128 * userCopyRetryBase
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func isRetryableUserCopyError(err error) bool {
	return errors.Is(err, syscall.EINVAL)
}

func readFullAt(f readerAt, buf []byte, off int64) (int, error) {
	n := 0
	retries := 0
	for n < len(buf) {
		m, err := f.ReadAt(buf[n:], off+int64(n))
		n += m
		if err != nil {
			if m == 0 && isRetryableUserCopyError(err) && retries < userCopyRetryLimit {
				retries++
				time.Sleep(retryUserCopyDelay(retries))
				continue
			}
			if errors.Is(err, io.EOF) && n == len(buf) {
				return n, nil
			}
			if errors.Is(err, io.EOF) {
				return n, io.ErrUnexpectedEOF
			}
			return n, err
		}
		retries = 0
		if m == 0 {
			return n, io.ErrUnexpectedEOF
		}
	}
	return n, nil
}

func writeFullAt(f writerAt, buf []byte, off int64) (int, error) {
	n := 0
	retries := 0
	for n < len(buf) {
		m, err := f.WriteAt(buf[n:], off+int64(n))
		n += m
		if err != nil {
			if m == 0 && isRetryableUserCopyError(err) && retries < userCopyRetryLimit {
				retries++
				time.Sleep(retryUserCopyDelay(retries))
				continue
			}
			return n, err
		}
		retries = 0
		if m == 0 {
			return n, io.ErrShortWrite
		}
	}
	return n, nil
}

func ublkIOBufOffset(queueID, tag uint16) uint64 {
	return ublkSrvIOBufOffset +
		(uint64(tag) << ublkIOBufBits) +
		(uint64(queueID) << (ublkIOBufBits + ublkTagBits))
}

// NewDevice creates a new ublk device via /dev/ublk-control.
func NewDevice(opts DeviceOptions) (*Device, error) {
	if opts.Queues == 0 {
		opts.Queues = 1
	}
	if opts.QueueDepth == 0 {
		opts.QueueDepth = 128
	}
	if opts.MaxIOBufBytes == 0 {
		opts.MaxIOBufBytes = 512 * 1024
	}
	if opts.Flags&FlagSupportZeroCopy != 0 {
		// Zero-copy mode: do NOT add user-copy (mutually exclusive).
		// AUTO_BUF_REG is added for the ServeZeroCopy path.
		opts.Flags |= FlagAutoBufReg | FlagCmdIoctlEncode
	} else {
		opts.Flags |= FlagUserCopy | FlagCmdIoctlEncode
	}
	// Also don't retry with legacy encoding if zero-copy flags cause EINVAL —
	// that means the kernel doesn't support them, not an encoding issue.

	ctrlFile, err := os.OpenFile(controlDevPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", controlDevPath, err)
	}

	ring, err := newIOURing(4, true) // SQE128 for control commands
	if err != nil {
		_ = ctrlFile.Close()
		return nil, fmt.Errorf("create control io_uring: %w", err)
	}

	dev := &Device{
		id:       -1,
		ctrlFile: ctrlFile,
		ctrlRing: ring,
		stopped:  make(chan struct{}),
	}

	info := DevInfo{
		NrHwQueues:    opts.Queues,
		QueueDepth:    opts.QueueDepth,
		MaxIOBufBytes: opts.MaxIOBufBytes,
		DevID:         ^uint32(0), // -1: let kernel assign
		UblksrvPID:    int32(syscall.Getpid()),
		Flags:         opts.Flags,
	}

	if err := dev.ctrlAddDev(&info); err != nil {
		_ = ring.Close()
		_ = ctrlFile.Close()
		return nil, fmt.Errorf("add device: %w", err)
	}

	dev.info = info
	dev.id = int32(info.DevID)

	// Wait for udev to create the char device node.
	charPath := fmt.Sprintf("/dev/ublkc%d", dev.id)
	for attempts := 0; attempts < 50; attempts++ {
		dev.charFile, err = os.OpenFile(charPath, os.O_RDWR, 0)
		if err == nil {
			break
		}
		time.Sleep(devicePollInterval)
	}
	if dev.charFile == nil {
		_ = dev.ctrlDelDev()
		_ = ring.Close()
		_ = ctrlFile.Close()
		return nil, fmt.Errorf("open %s: %w", charPath, err)
	}

	return dev, nil
}

// ID returns the device ID.
func (d *Device) ID() int32 { return d.id }

// Info returns the device info as returned by the kernel.
func (d *Device) Info() DevInfo { return d.info }

// BlockDevPath returns the path to the block device (e.g., /dev/ublkb0).
func (d *Device) BlockDevPath() string {
	return fmt.Sprintf("/dev/ublkb%d", d.id)
}

// CharDevPath returns the path to the character device (e.g., /dev/ublkc0).
func (d *Device) CharDevPath() string {
	return fmt.Sprintf("/dev/ublkc%d", d.id)
}

// SetParams sets device parameters.
func (d *Device) SetParams(params *Params) error {
	params.Len = uint32(unsafe.Sizeof(*params))
	return d.ctrlSetParams(params)
}

// GetParams retrieves device parameters.
func (d *Device) GetParams() (*Params, error) {
	params := &Params{
		Len:   uint32(unsafe.Sizeof(Params{})),
		Types: ParamTypeAll,
	}
	if err := d.ctrlGetParams(params); err != nil {
		return nil, err
	}
	return params, nil
}

// Serve starts the device and serves IO requests using the given handler.
// It blocks until the device is stopped or an error occurs.
// Each queue is served by a dedicated goroutine pinned to its affinity.
func (d *Device) Serve(h Handler) error {
	return d.serve(int(d.info.NrHwQueues), func(qid uint16, ready chan<- error) error {
		return d.serveQueue(qid, h, ready)
	})
}

// serve is the shared serve loop for both user-copy and zero-copy modes.
// It spawns a goroutine per queue, waits for all queues to submit their
// initial FETCH commands, then calls START_DEV.
func (d *Device) serve(nQueues int, queueFn func(uint16, chan<- error) error) error {
	readyChs := make([]chan error, nQueues)
	for i := range readyChs {
		readyChs[i] = make(chan error, 1)
	}

	errCh := make(chan error, nQueues)

	for q := 0; q < nQueues; q++ {
		d.serveWg.Add(1)
		go func(qid uint16) {
			defer d.serveWg.Done()
			if err := queueFn(qid, readyChs[qid]); err != nil {
				errCh <- fmt.Errorf("queue %d: %w", qid, err)
			}
		}(uint16(q))
	}

	// Wait for all queues to be ready (FETCHes submitted).
	for _, ch := range readyChs {
		if err := <-ch; err != nil {
			_ = d.Stop()
			d.serveWg.Wait()
			return fmt.Errorf("queue setup: %w", err)
		}
	}

	// All queues have submitted FETCHes. Now start the device.
	if err := d.ctrlStartDev(); err != nil {
		_ = d.Stop()
		d.serveWg.Wait()
		return fmt.Errorf("start device: %w", err)
	}

	d.serveWg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Stop submits a STOP_DEV command (non-blocking). The serve goroutines
// will see ENODEV and exit. Call Delete to fully clean up.
func (d *Device) Stop() error {
	select {
	case <-d.stopped:
		return nil
	default:
	}
	close(d.stopped)
	return d.ctrlStopDev()
}

func (d *Device) isStopped() bool {
	select {
	case <-d.stopped:
		return true
	default:
		return false
	}
}

func (d *Device) waitServe(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		d.serveWg.Wait()
		close(done)
	}()

	if timeout <= 0 {
		<-done
		return true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// Delete stops and deletes the device, releasing all resources.
func (d *Device) Delete() error {
	d.deleteOnce.Do(func() {
		d.deleteErr = d.delete()
	})
	return d.deleteErr
}

func (d *Device) delete() error {
	var err error

	// Send STOP_DEV to trigger ENODEV on pending IO commands.
	err = errors.Join(err, d.Stop())

	// Let STOP_DEV abort pending IO so queue goroutines can exit cleanly.
	// Only interrupt the rings if shutdown gets stuck.
	if !d.waitServe(2 * time.Second) {
		d.ioRingsMu.Lock()
		for _, ring := range d.ioRings {
			if ring != nil {
				ring.Interrupt()
			}
		}
		d.ioRingsMu.Unlock()
		d.waitServe(0)
	}

	// Close the char device before deleting (kernel requires no open refs).
	if d.charFile != nil {
		err = errors.Join(err, d.charFile.Close())
		d.charFile = nil
	}

	// Drain STOP_DEV CQE, then delete.
	err = errors.Join(err, d.ctrlStopDevWait())
	err = errors.Join(err, d.ctrlDelDev())
	if d.ctrlRing != nil {
		err = errors.Join(err, d.ctrlRing.Close())
	}
	if d.ctrlFile != nil {
		err = errors.Join(err, d.ctrlFile.Close())
	}
	return err
}

func (d *Device) registerIORing(ring *ioURing) {
	d.ioRingsMu.Lock()
	d.ioRings = append(d.ioRings, ring)
	d.ioRingsMu.Unlock()
}

func (d *Device) serveQueue(qid uint16, h Handler, ready chan<- error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ioDebugf("serveQueue start q=%d depth=%d", qid, d.info.QueueDepth)

	d.setQueueAffinity(qid)

	depth := int(d.info.QueueDepth)

	ring, err := newIOURing(uint32(depth), false)
	if err != nil {
		ready <- fmt.Errorf("create io_uring: %w", err)
		return err
	}
	d.registerIORing(ring)
	defer func() { _ = ring.Close() }()

	// mmap the command buffer for this queue
	cmdBufSize := ublkMaxCmdBufSize(d.info.QueueDepth)
	cmdBufOffset := int64(ublkSrvCmdBufOffset) + int64(qid)*int64(ublkMaxCmdBufSize(ublkMaxQueueDepth))

	cmdBuf, err := syscall.Mmap(
		int(d.charFile.Fd()),
		cmdBufOffset,
		int(cmdBufSize),
		syscall.PROT_READ,
		syscall.MAP_SHARED|mmapPopulateFlag,
	)
	if err != nil {
		ready <- fmt.Errorf("mmap cmd buf: %w", err)
		return err
	}
	defer func() { _ = syscall.Munmap(cmdBuf) }()

	if err := d.submitInitialFetches(ring, qid); err != nil {
		ready <- err
		return err
	}

	if err := ring.Submit(); err != nil {
		ready <- fmt.Errorf("submit initial fetches: %w", err)
		return err
	}

	// Signal that this queue is ready (FETCHes submitted).
	ready <- nil

	return d.runUserQueueLoop(ring, qid, func(tag uint16) ioDesc {
		return loadIODesc(cmdBuf, tag)
	}, func(tag uint16) [ioDescSize]byte {
		return snapshotIODescBytes(cmdBuf, tag)
	}, h)
}

func (d *Device) submitFetch(ring queueRing, qid, tag uint16) error {
	sqe := ring.GetSQE()
	if sqe == nil {
		return fmt.Errorf("no SQE available")
	}
	cmdOp := d.ioOp(ublkUIoFetchReq, IoFetchReq)
	prepUringCmd(sqe, cmdOp, int32(d.charFile.Fd()), qid, tag, 0, 0)
	sqeSetU64(sqe, sqeOffUserData, uint64(tag))
	return nil
}

func (d *Device) submitCommitAndFetch(ring queueRing, qid, tag uint16, result int32) error {
	sqe := ring.GetSQE()
	if sqe == nil {
		return fmt.Errorf("no SQE available")
	}
	cmdOp := d.ioOp(ublkUIoCommitAndFetchReq, IoCommitAndFetchReq)
	prepUringCmd(sqe, cmdOp, int32(d.charFile.Fd()), qid, tag, result, 0)
	sqeSetU64(sqe, sqeOffUserData, uint64(tag))
	return nil
}

func loadIODesc(cmdBuf []byte, tag uint16) ioDesc {
	offset := int(tag) * ioDescSize
	return ioDesc{
		OpFlags:     atomic.LoadUint32((*uint32)(unsafe.Pointer(&cmdBuf[offset]))),
		NrSectors:   atomic.LoadUint32((*uint32)(unsafe.Pointer(&cmdBuf[offset+4]))),
		StartSector: atomic.LoadUint64((*uint64)(unsafe.Pointer(&cmdBuf[offset+8]))),
		Addr:        atomic.LoadUint64((*uint64)(unsafe.Pointer(&cmdBuf[offset+16]))),
	}
}

func snapshotIODescBytes(cmdBuf []byte, tag uint16) [ioDescSize]byte {
	offset := int(tag) * ioDescSize
	var raw [ioDescSize]byte
	copy(raw[:], cmdBuf[offset:offset+ioDescSize])
	return raw
}

func ublkMaxCmdBufSize(depth uint16) uint32 {
	return uint32(depth) * uint32(ioDescSize)
}
