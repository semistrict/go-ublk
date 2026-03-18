package ublk

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

const (
	testControlFD        = int32(42) // synthetic fd used for SQE encoding assertions
	testQueueID          = uint16(1) // synthetic queue id for queue-loop tests
	testTagZero          = uint16(0) // first synthetic request tag
	testTagOne           = uint16(1) // second synthetic request tag
	testSectorCount      = uint32(8) // 8 sectors => 4096-byte request payload
	testStartSector      = uint64(64)
	testAvailableSQEs    = uint32(8)
	testUserDataSentinel = uint64(1)
)

type fakeQueueRing struct {
	noSQE               bool
	availableSQEs       uint32
	sqes                []ioURingSQE
	submitted           [][]ioURingSQE
	nextSubmitIndex     int
	cqes                []*ioURingCQE
	submitErr           error
	waitErr             error
	submitAndWaitErr    error
	beforeWait          func()
	beforeSubmitAndWait func()
	seenCount           int
	submitCalls         int
	submitAndWaitCalls  int
	waitCalls           int
	tryCalls            int
}

func (r *fakeQueueRing) GetSQE() *ioURingSQE {
	if r.noSQE {
		return nil
	}
	r.sqes = append(r.sqes, ioURingSQE{})
	return &r.sqes[len(r.sqes)-1]
}

func (r *fakeQueueRing) Submit() error {
	r.submitCalls++
	if r.submitErr != nil {
		return r.submitErr
	}
	r.recordSubmitted()
	return nil
}

func (r *fakeQueueRing) SubmitAndWaitCQE() (*ioURingCQE, error) {
	r.submitAndWaitCalls++
	if r.beforeSubmitAndWait != nil {
		r.beforeSubmitAndWait()
	}
	if r.submitAndWaitErr != nil {
		return nil, r.submitAndWaitErr
	}
	r.recordSubmitted()
	return r.popCQE()
}

func (r *fakeQueueRing) WaitCQE() (*ioURingCQE, error) {
	r.waitCalls++
	if r.beforeWait != nil {
		r.beforeWait()
	}
	if r.waitErr != nil {
		return nil, r.waitErr
	}
	return r.popCQE()
}

func (r *fakeQueueRing) TryCQE() *ioURingCQE {
	r.tryCalls++
	cqe, err := r.popCQE()
	if err != nil {
		return nil
	}
	return cqe
}

func (r *fakeQueueRing) SeenCQE(*ioURingCQE) {
	r.seenCount++
}

func (r *fakeQueueRing) AvailableSQEs() uint32 {
	if r.availableSQEs == 0 {
		return testAvailableSQEs
	}
	return r.availableSQEs
}

func (r *fakeQueueRing) recordSubmitted() {
	if r.nextSubmitIndex >= len(r.sqes) {
		return
	}
	batch := append([]ioURingSQE(nil), r.sqes[r.nextSubmitIndex:]...)
	r.submitted = append(r.submitted, batch)
	r.nextSubmitIndex = len(r.sqes)
}

func (r *fakeQueueRing) popCQE() (*ioURingCQE, error) {
	if len(r.cqes) == 0 {
		return nil, io.EOF
	}
	cqe := r.cqes[0]
	r.cqes = r.cqes[1:]
	return cqe, nil
}

type fakeUserCopyFile struct {
	readData  []byte
	readOff   int64
	writeData []byte
	writeOff  int64
}

func (f *fakeUserCopyFile) ReadAt(p []byte, off int64) (int, error) {
	f.readOff = off
	n := copy(p, f.readData)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *fakeUserCopyFile) WriteAt(p []byte, off int64) (int, error) {
	f.writeOff = off
	f.writeData = append(f.writeData[:0], p...)
	return len(p), nil
}

func tempCharFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp("", "go-ublk-runtime-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	})
	return f
}

func newTestDevice(t *testing.T, depth uint16) *Device {
	t.Helper()
	return &Device{
		info: DevInfo{
			QueueDepth: depth,
		},
		charFile: tempCharFile(t),
		stopped:  make(chan struct{}),
	}
}

func encodeIODesc(cmdBuf []byte, tag uint16, iod ioDesc) {
	offset := int(tag) * ioDescSize
	*(*uint32)(unsafe.Pointer(&cmdBuf[offset])) = iod.OpFlags
	*(*uint32)(unsafe.Pointer(&cmdBuf[offset+4])) = iod.NrSectors
	*(*uint64)(unsafe.Pointer(&cmdBuf[offset+8])) = iod.StartSector
	*(*uint64)(unsafe.Pointer(&cmdBuf[offset+16])) = iod.Addr
}

func decodeIOCmd(sqe ioURingSQE) ioCmd {
	return *(*ioCmd)(unsafe.Pointer(&sqe[sqeOffCmd]))
}

func sqeU64(sqe ioURingSQE, off int) uint64 {
	return *(*uint64)(unsafe.Pointer(&sqe[off]))
}

func sqeI32(sqe ioURingSQE, off int) int32 {
	return *(*int32)(unsafe.Pointer(&sqe[off]))
}

func TestRequestReadDataUsesUserCopyTarget(t *testing.T) {
	const readLen = 16

	fakeIO := &fakeUserCopyFile{readData: []byte("0123456789abcdef")}
	dev := &Device{userCopyData: fakeIO}
	req := &Request{QueueID: testQueueID, Tag: testTagOne, dev: dev}

	buf := make([]byte, readLen)
	n, err := req.ReadData(buf)
	if err != nil {
		t.Fatalf("ReadData: %v", err)
	}
	if n != readLen {
		t.Fatalf("ReadData n = %d, want %d", n, readLen)
	}

	wantOff := int64(ublkIOBufOffset(testQueueID, testTagOne))
	if fakeIO.readOff != wantOff {
		t.Fatalf("ReadData off = %d, want %d", fakeIO.readOff, wantOff)
	}
}

func TestRequestWriteDataUsesUserCopyTarget(t *testing.T) {
	payload := []byte("payload")
	fakeIO := &fakeUserCopyFile{}
	dev := &Device{userCopyData: fakeIO}
	req := &Request{QueueID: testQueueID, Tag: testTagZero, dev: dev}

	n, err := req.WriteData(payload)
	if err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("WriteData n = %d, want %d", n, len(payload))
	}
	if string(fakeIO.writeData) != string(payload) {
		t.Fatalf("WriteData payload = %q, want %q", fakeIO.writeData, payload)
	}

	wantOff := int64(ublkIOBufOffset(testQueueID, testTagZero))
	if fakeIO.writeOff != wantOff {
		t.Fatalf("WriteData off = %d, want %d", fakeIO.writeOff, wantOff)
	}
}

func TestCtrlSubmitAndWaitSuccess(t *testing.T) {
	ring := &fakeQueueRing{
		cqes: []*ioURingCQE{{UserData: testUserDataSentinel, Res: 7}},
	}
	cmd := &ctrlCmd{DevID: 123, QueueID: testQueueID}

	res, err := submitCtrlCmdAndWait(ring, testControlFD, ublkUCmdStartDev, cmd)
	if err != nil {
		t.Fatalf("submitCtrlCmdAndWait: %v", err)
	}
	if res != 7 {
		t.Fatalf("submitCtrlCmdAndWait res = %d, want 7", res)
	}
	if len(ring.submitted) != 1 || len(ring.submitted[0]) != 1 {
		t.Fatalf("submitted batches = %d/%d, want 1 batch with 1 SQE", len(ring.submitted), len(ring.submitted[0]))
	}

	sqe := ring.submitted[0][0]
	if sqe[sqeOffOpcode] != ioringOpURingCmd {
		t.Fatalf("SQE opcode = %d, want %d", sqe[sqeOffOpcode], ioringOpURingCmd)
	}
	if got := sqeI32(sqe, sqeOffFd); got != testControlFD {
		t.Fatalf("SQE fd = %d, want %d", got, testControlFD)
	}
	if got := sqeU64(sqe, sqeOffOff); got != uint64(ublkUCmdStartDev) {
		t.Fatalf("SQE off/cmd = %#x, want %#x", got, uint64(ublkUCmdStartDev))
	}
	if got := sqeU64(sqe, sqeOffUserData); got != testUserDataSentinel {
		t.Fatalf("SQE user_data = %#x, want %#x", got, testUserDataSentinel)
	}

	gotCmd := *(*ctrlCmd)(unsafe.Pointer(&sqe[sqeOffCmd]))
	if gotCmd.DevID != cmd.DevID || gotCmd.QueueID != cmd.QueueID {
		t.Fatalf("ctrl cmd = %+v, want %+v", gotCmd, *cmd)
	}
}

func TestCtrlSubmitAndWaitErrors(t *testing.T) {
	t.Run("no sqe", func(t *testing.T) {
		_, err := submitCtrlCmdAndWait(&fakeQueueRing{noSQE: true}, testControlFD, ublkUCmdStartDev, &ctrlCmd{})
		if err == nil || !strings.Contains(err.Error(), "no SQE") {
			t.Fatalf("error = %v, want no SQE", err)
		}
	})

	t.Run("submit", func(t *testing.T) {
		want := errors.New("submit boom")
		_, err := submitCtrlCmdAndWait(&fakeQueueRing{submitErr: want}, testControlFD, ublkUCmdStartDev, &ctrlCmd{})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	})

	t.Run("wait", func(t *testing.T) {
		want := errors.New("wait boom")
		_, err := submitCtrlCmdAndWait(&fakeQueueRing{waitErr: want}, testControlFD, ublkUCmdStartDev, &ctrlCmd{})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	})
}

func TestQueueInitialFetches(t *testing.T) {
	dev := newTestDevice(t, 2)
	ring := &fakeQueueRing{}

	if err := dev.submitInitialFetches(ring, testQueueID); err != nil {
		t.Fatalf("submitInitialFetches: %v", err)
	}
	if len(ring.sqes) != 2 {
		t.Fatalf("queued SQEs = %d, want 2", len(ring.sqes))
	}

	firstCmd := decodeIOCmd(ring.sqes[0])
	secondCmd := decodeIOCmd(ring.sqes[1])
	if firstCmd.QID != testQueueID || firstCmd.Tag != testTagZero {
		t.Fatalf("first fetch = %+v, want qid=%d tag=%d", firstCmd, testQueueID, testTagZero)
	}
	if secondCmd.QID != testQueueID || secondCmd.Tag != testTagOne {
		t.Fatalf("second fetch = %+v, want qid=%d tag=%d", secondCmd, testQueueID, testTagOne)
	}
}

func TestQueueUserLoopSuccess(t *testing.T) {
	dev := newTestDevice(t, 1)
	cmdBuf := make([]byte, ioDescSize)
	encodeIODesc(cmdBuf, testTagZero, ioDesc{
		OpFlags:     uint32(OpRead),
		NrSectors:   testSectorCount,
		StartSector: testStartSector,
	})
	ring := &fakeQueueRing{
		cqes: []*ioURingCQE{{UserData: uint64(testTagZero), Res: 0}},
	}
	ring.beforeSubmitAndWait = func() {
		ring.cqes = append(ring.cqes, &ioURingCQE{UserData: uint64(testTagZero), Res: -int32(syscall.ENODEV)})
	}

	var got Request
	err := dev.runUserQueueLoop(ring, testQueueID, func(tag uint16) ioDesc {
		return loadIODesc(cmdBuf, tag)
	}, func(tag uint16) [ioDescSize]byte {
		return snapshotIODescBytes(cmdBuf, tag)
	}, HandlerFunc(func(req *Request) error {
		got = *req
		return nil
	}))
	if err != nil {
		t.Fatalf("runUserQueueLoop: %v", err)
	}
	if got.Op != OpRead || got.Tag != testTagZero || got.QueueID != testQueueID || got.NrSectors != testSectorCount || got.StartSector != testStartSector {
		t.Fatalf("request = %+v", got)
	}
	if ring.submitAndWaitCalls != 1 {
		t.Fatalf("SubmitAndWaitCQE calls = %d, want 1", ring.submitAndWaitCalls)
	}
	if len(ring.submitted) != 1 || len(ring.submitted[0]) != 1 {
		t.Fatalf("submitted commit batches = %d/%d, want 1 batch with 1 SQE", len(ring.submitted), len(ring.submitted[0]))
	}

	cmd := decodeIOCmd(ring.submitted[0][0])
	wantBytes := int32(testSectorCount) * 512
	if cmd.Result != wantBytes {
		t.Fatalf("commit result = %d, want %d", cmd.Result, wantBytes)
	}
}

func TestQueueUserLoopHandlerErrorMapsToEIO(t *testing.T) {
	dev := newTestDevice(t, 1)
	cmdBuf := make([]byte, ioDescSize)
	encodeIODesc(cmdBuf, testTagZero, ioDesc{OpFlags: uint32(OpWrite), NrSectors: testSectorCount})
	ring := &fakeQueueRing{
		cqes: []*ioURingCQE{
			{UserData: uint64(testTagZero), Res: 0},
			{UserData: uint64(testTagZero), Res: -int32(syscall.ENODEV)},
		},
	}

	err := dev.runUserQueueLoop(ring, testQueueID, func(tag uint16) ioDesc {
		return loadIODesc(cmdBuf, tag)
	}, nil, HandlerFunc(func(*Request) error {
		return errors.New("boom")
	}))
	if err != nil {
		t.Fatalf("runUserQueueLoop: %v", err)
	}

	cmd := decodeIOCmd(ring.submitted[0][0])
	if cmd.Result != -int32(syscall.EIO) {
		t.Fatalf("commit result = %d, want %d", cmd.Result, -int32(syscall.EIO))
	}
}

func TestQueueUserLoopBatchesReadyCQEs(t *testing.T) {
	dev := newTestDevice(t, 2)
	cmdBuf := make([]byte, 2*ioDescSize)
	encodeIODesc(cmdBuf, testTagZero, ioDesc{OpFlags: uint32(OpRead), NrSectors: testSectorCount})
	encodeIODesc(cmdBuf, testTagOne, ioDesc{OpFlags: uint32(OpWrite), NrSectors: testSectorCount})
	ring := &fakeQueueRing{
		availableSQEs: 2,
		cqes: []*ioURingCQE{
			{UserData: uint64(testTagZero), Res: 0},
			{UserData: uint64(testTagOne), Res: 0},
			{UserData: uint64(testTagZero), Res: -int32(syscall.ENODEV)},
		},
	}

	handled := 0
	err := dev.runUserQueueLoop(ring, testQueueID, func(tag uint16) ioDesc {
		return loadIODesc(cmdBuf, tag)
	}, nil, HandlerFunc(func(*Request) error {
		handled++
		return nil
	}))
	if err != nil {
		t.Fatalf("runUserQueueLoop: %v", err)
	}
	if handled != 2 {
		t.Fatalf("handled = %d, want 2", handled)
	}
	if len(ring.submitted) != 1 || len(ring.submitted[0]) != 2 {
		t.Fatalf("submitted batches = %d/%d, want 1 batch with 2 SQEs", len(ring.submitted), len(ring.submitted[0]))
	}
}

func TestQueueUserLoopBusyPendingCommit(t *testing.T) {
	dev := newTestDevice(t, 1)
	cmdBuf := make([]byte, ioDescSize)
	encodeIODesc(cmdBuf, testTagZero, ioDesc{OpFlags: uint32(OpRead), NrSectors: testSectorCount})
	ring := &fakeQueueRing{
		cqes: []*ioURingCQE{
			{UserData: uint64(testTagZero), Res: 0},
			{UserData: uint64(testTagZero), Res: -int32(syscall.EBUSY)},
			{UserData: uint64(testTagZero), Res: -int32(syscall.ENODEV)},
		},
	}

	err := dev.runUserQueueLoop(ring, testQueueID, func(tag uint16) ioDesc {
		return loadIODesc(cmdBuf, tag)
	}, nil, HandlerFunc(func(*Request) error { return nil }))
	if err != nil {
		t.Fatalf("runUserQueueLoop: %v", err)
	}
	if len(ring.submitted) != 1 || len(ring.submitted[0]) != 1 {
		t.Fatalf("submitted batches = %d/%d, want 1 batch with 1 SQE", len(ring.submitted), len(ring.submitted[0]))
	}
}

func TestQueueUserLoopStoppedBadFDExit(t *testing.T) {
	dev := newTestDevice(t, 1)
	ring := &fakeQueueRing{
		waitErr: syscall.EBADF,
		beforeWait: func() {
			select {
			case <-dev.stopped:
			default:
				close(dev.stopped)
			}
		},
	}

	err := dev.runUserQueueLoop(ring, testQueueID, func(uint16) ioDesc { return ioDesc{} }, nil, HandlerFunc(func(*Request) error { return nil }))
	if err != nil {
		t.Fatalf("runUserQueueLoop: %v", err)
	}
}

func TestQueueUserLoopUnexpectedCQEError(t *testing.T) {
	dev := newTestDevice(t, 1)
	ring := &fakeQueueRing{
		cqes: []*ioURingCQE{{UserData: uint64(testTagZero), Res: -int32(syscall.EINVAL)}},
	}

	err := dev.runUserQueueLoop(ring, testQueueID, func(uint16) ioDesc { return ioDesc{} }, nil, HandlerFunc(func(*Request) error { return nil }))
	if err == nil || !strings.Contains(err.Error(), "cqe error") {
		t.Fatalf("error = %v, want CQE error", err)
	}
}

func TestZeroCopyReadFixedAndWriteFixed(t *testing.T) {
	t.Run("read fixed success", func(t *testing.T) {
		ring := &fakeQueueRing{
			cqes: []*ioURingCQE{{UserData: targetIOFlag | uint64(testTagZero), Res: int32(testSectorCount) * 512}},
		}
		req := &ZeroCopyRequest{Tag: testTagZero, BufIndex: testTagZero, ring: ring}

		if err := req.ReadFixed(testControlFD, int64(testStartSector*512), uint32(testSectorCount*512)); err != nil {
			t.Fatalf("ReadFixed: %v", err)
		}
		if len(ring.submitted) != 1 || len(ring.submitted[0]) != 1 {
			t.Fatalf("submitted batches = %d/%d, want 1 batch with 1 SQE", len(ring.submitted), len(ring.submitted[0]))
		}
		sqe := ring.submitted[0][0]
		if sqe[sqeOffOpcode] != ioringOpReadFixed {
			t.Fatalf("opcode = %d, want %d", sqe[sqeOffOpcode], ioringOpReadFixed)
		}
		if got := sqeU64(sqe, sqeOffUserData); got != targetIOFlag|uint64(testTagZero) {
			t.Fatalf("user_data = %#x, want %#x", got, targetIOFlag|uint64(testTagZero))
		}
	})

	t.Run("wait target cqe skips non target", func(t *testing.T) {
		ring := &fakeQueueRing{
			cqes: []*ioURingCQE{
				{UserData: uint64(testTagZero), Res: 0},
				{UserData: targetIOFlag | uint64(testTagOne), Res: int32(testSectorCount) * 512},
			},
		}
		req := &ZeroCopyRequest{Tag: testTagOne, BufIndex: testTagOne, ring: ring}

		if err := req.WriteFixed(testControlFD, int64(testStartSector*512), uint32(testSectorCount*512)); err != nil {
			t.Fatalf("WriteFixed: %v", err)
		}
		if ring.seenCount != 2 {
			t.Fatalf("seen CQEs = %d, want 2", ring.seenCount)
		}
	})
}

func TestQueueZeroCopyLoopHandlerErrorMapsToEIO(t *testing.T) {
	dev := newTestDevice(t, 1)
	cmdBuf := make([]byte, ioDescSize)
	encodeIODesc(cmdBuf, testTagZero, ioDesc{OpFlags: uint32(OpRead), NrSectors: testSectorCount, StartSector: testStartSector})
	ring := &fakeQueueRing{
		cqes: []*ioURingCQE{
			{UserData: uint64(testTagZero), Res: 0},
			{UserData: uint64(testTagZero), Res: -int32(syscall.ENODEV)},
		},
	}

	var got ZeroCopyRequest
	err := dev.runZeroCopyQueueLoop(ring, testQueueID, testControlFD, func(tag uint16) ioDesc {
		return loadIODesc(cmdBuf, tag)
	}, ZeroCopyHandlerFunc(func(req *ZeroCopyRequest) error {
		got = *req
		return errors.New("boom")
	}))
	if err != nil {
		t.Fatalf("runZeroCopyQueueLoop: %v", err)
	}
	if got.BufIndex != testTagZero || got.QueueID != testQueueID || got.StartSector != testStartSector {
		t.Fatalf("request = %+v", got)
	}
	cmd := decodeIOCmd(ring.submitted[0][0])
	if cmd.Result != -int32(syscall.EIO) {
		t.Fatalf("commit result = %d, want %d", cmd.Result, -int32(syscall.EIO))
	}
}
