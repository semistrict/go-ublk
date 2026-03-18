package ublk

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const (
	testLifecycleQueues = 2 // Two queues are enough to exercise serve coordination and error fan-in.
)

func newHookedDevice(queueCount uint16, hooks *deviceHooks) *Device {
	return &Device{
		info: DevInfo{
			NrHwQueues: queueCount,
		},
		stopped: make(chan struct{}),
		hooks:   hooks,
	}
}

func newServeTestDevice(t *testing.T, flags uint64, hooks *deviceHooks) *Device {
	t.Helper()
	return &Device{
		info: DevInfo{
			NrHwQueues: 1,
			QueueDepth: 1,
			Flags:      flags,
		},
		charFile: tempCharFile(t),
		stopped:  make(chan struct{}),
		hooks:    hooks,
	}
}

func newReadyThenStopRing() *fakeQueueRing {
	ring := &fakeQueueRing{
		cqes: []*ioURingCQE{{UserData: uint64(testTagZero), Res: 0}},
	}
	ring.beforeSubmitAndWait = func() {
		ring.cqes = append(ring.cqes, &ioURingCQE{UserData: uint64(testTagZero), Res: -int32(syscall.ENODEV)})
	}
	return ring
}

func TestServeLifecycleStartsAfterQueuesReady(t *testing.T) {
	startCalls := 0
	dev := newHookedDevice(testLifecycleQueues, &deviceHooks{
		startControl: func(*Device) error {
			startCalls++
			return nil
		},
	})

	queueErr := errors.New("queue boom")
	err := dev.serve(testLifecycleQueues, func(qid uint16, ready chan<- error) error {
		ready <- nil
		if qid == 1 {
			return queueErr
		}
		return nil
	})
	if startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", startCalls)
	}
	if err == nil || !strings.Contains(err.Error(), "queue 1") || !strings.Contains(err.Error(), queueErr.Error()) {
		t.Fatalf("error = %v, want joined queue error", err)
	}
}

func TestServeLifecycleSetupFailureStopsBeforeStart(t *testing.T) {
	startCalls := 0
	stopCalls := 0
	dev := newHookedDevice(testLifecycleQueues, &deviceHooks{
		startControl: func(*Device) error {
			startCalls++
			return nil
		},
		stopControl: func(*Device) error {
			stopCalls++
			return nil
		},
	})

	setupErr := errors.New("setup boom")
	err := dev.serve(testLifecycleQueues, func(qid uint16, ready chan<- error) error {
		if qid == 0 {
			ready <- setupErr
			return nil
		}
		ready <- nil
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "queue setup") || !strings.Contains(err.Error(), setupErr.Error()) {
		t.Fatalf("error = %v, want setup failure", err)
	}
	if startCalls != 0 {
		t.Fatalf("start calls = %d, want 0", startCalls)
	}
	if stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", stopCalls)
	}
}

func TestServeLifecycleStartFailureStopsWorkers(t *testing.T) {
	startCalls := 0
	stopCalls := 0
	dev := newHookedDevice(testLifecycleQueues, &deviceHooks{
		startControl: func(*Device) error {
			startCalls++
			return errors.New("start boom")
		},
		stopControl: func(*Device) error {
			stopCalls++
			return nil
		},
	})

	err := dev.serve(testLifecycleQueues, func(_ uint16, ready chan<- error) error {
		ready <- nil
		<-dev.stopped
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "start device") || !strings.Contains(err.Error(), "start boom") {
		t.Fatalf("error = %v, want start failure", err)
	}
	if startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", startCalls)
	}
	if stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", stopCalls)
	}
}

func TestStopPortableIsIdempotent(t *testing.T) {
	stopCalls := 0
	dev := newHookedDevice(1, &deviceHooks{
		stopControl: func(*Device) error {
			stopCalls++
			return nil
		},
	})

	if err := dev.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := dev.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", stopCalls)
	}
}

func TestDeletePortableLifecycleFallbackAndIdempotence(t *testing.T) {
	stopCalls := 0
	stopWaitCalls := 0
	deleteCalls := 0
	closeCharCalls := 0
	closeRingCalls := 0
	closeFileCalls := 0
	interruptCalls := 0
	waitTimeouts := make([]time.Duration, 0, 2)

	dev := newHookedDevice(1, &deviceHooks{
		stopControl: func(*Device) error {
			stopCalls++
			return nil
		},
		stopControlWait: func(*Device) error {
			stopWaitCalls++
			return nil
		},
		deleteControl: func(*Device) error {
			deleteCalls++
			return nil
		},
		closeChar: func(*Device) error {
			closeCharCalls++
			return nil
		},
		closeCtrlRing: func(*Device) error {
			closeRingCalls++
			return nil
		},
		closeCtrlFile: func(*Device) error {
			closeFileCalls++
			return nil
		},
		waitServe: func(_ *Device, timeout time.Duration) bool {
			waitTimeouts = append(waitTimeouts, timeout)
			return timeout == 0
		},
		interruptIORings: func(*Device) {
			interruptCalls++
		},
	})

	if err := dev.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := dev.Delete(); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if stopCalls != 1 || stopWaitCalls != 1 || deleteCalls != 1 {
		t.Fatalf("control calls = stop:%d wait:%d delete:%d, want 1 each", stopCalls, stopWaitCalls, deleteCalls)
	}
	if closeCharCalls != 1 || closeRingCalls != 1 || closeFileCalls != 1 {
		t.Fatalf("close calls = char:%d ring:%d file:%d, want 1 each", closeCharCalls, closeRingCalls, closeFileCalls)
	}
	if interruptCalls != 1 {
		t.Fatalf("interrupt calls = %d, want 1", interruptCalls)
	}
	if len(waitTimeouts) != 2 || waitTimeouts[0] != queueShutdownGracePeriod || waitTimeouts[1] != 0 {
		t.Fatalf("wait timeouts = %v, want [%v 0s]", waitTimeouts, queueShutdownGracePeriod)
	}
}

func TestAffinityCPUsFromMask(t *testing.T) {
	aff := &QueueAffinity{
		mask: []byte{
			0b00001011, // CPUs 0, 1, and 3
			0b00000010, // CPU 9
		},
	}

	got := aff.CPUs()
	want := []int{0, 1, 3, 9}
	if len(got) != len(want) {
		t.Fatalf("cpus len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cpus[%d] = %d, want %d (%v)", i, got[i], want[i], got)
		}
	}
}

func TestAffinityGetQueueCmd(t *testing.T) {
	const (
		testDeviceID = int32(7) // Synthetic device id used to verify queue-affinity command encoding.
	)

	buf := make([]byte, affinityBufSize)
	cmd := newGetQueueAffinityCmd(testDeviceID, testQueueID, buf)

	if cmd.DevID != uint32(testDeviceID) {
		t.Fatalf("DevID = %d, want %d", cmd.DevID, testDeviceID)
	}
	if cmd.QueueID != testQueueID {
		t.Fatalf("QueueID = %d, want %d", cmd.QueueID, testQueueID)
	}
	if cmd.Len != uint16(len(buf)) {
		t.Fatalf("Len = %d, want %d", cmd.Len, len(buf))
	}
	if cmd.Addr == 0 {
		t.Fatalf("Addr = 0, want non-zero")
	}
}

func TestNewDeviceWithHooksDefaults(t *testing.T) {
	var (
		capturedInfo DevInfo
		openPath     string
	)
	dev, err := newDeviceWithHooks(DeviceOptions{}, &deviceHooks{
		createControlResources: func() (*os.File, *ioURing, error) {
			return tempCharFile(t), nil, nil
		},
		addControlDevice: func(_ *Device, info *DevInfo) error {
			capturedInfo = *info
			info.DevID = 7
			return nil
		},
		openCharDevice: func(_ *Device, path string) (*os.File, error) {
			openPath = path
			return tempCharFile(t), nil
		},
	})
	if err != nil {
		t.Fatalf("newDeviceWithHooks: %v", err)
	}

	if capturedInfo.NrHwQueues != defaultHardwareQueues {
		t.Fatalf("queues = %d, want %d", capturedInfo.NrHwQueues, defaultHardwareQueues)
	}
	if capturedInfo.QueueDepth != defaultQueueDepth {
		t.Fatalf("queue depth = %d, want %d", capturedInfo.QueueDepth, defaultQueueDepth)
	}
	if capturedInfo.MaxIOBufBytes != defaultMaxIOBufBytes {
		t.Fatalf("max io buf = %d, want %d", capturedInfo.MaxIOBufBytes, defaultMaxIOBufBytes)
	}
	if capturedInfo.Flags&FlagUserCopy == 0 || capturedInfo.Flags&FlagCmdIoctlEncode == 0 {
		t.Fatalf("flags = %#x, want user-copy + ioctl-encode", capturedInfo.Flags)
	}
	if openPath != charDevicePathForID(7) {
		t.Fatalf("open path = %q, want %q", openPath, charDevicePathForID(7))
	}
	if dev.ID() != 7 {
		t.Fatalf("ID = %d, want 7", dev.ID())
	}
	if dev.BlockDevPath() != blockDevicePathForID(7) {
		t.Fatalf("BlockDevPath = %q, want %q", dev.BlockDevPath(), blockDevicePathForID(7))
	}
	if dev.CharDevPath() != charDevicePathForID(7) {
		t.Fatalf("CharDevPath = %q, want %q", dev.CharDevPath(), charDevicePathForID(7))
	}
	if dev.Info().QueueDepth != defaultQueueDepth {
		t.Fatalf("Info queue depth = %d, want %d", dev.Info().QueueDepth, defaultQueueDepth)
	}
}

func TestNewDeviceWithHooksZeroCopyFlags(t *testing.T) {
	var capturedFlags uint64
	_, err := newDeviceWithHooks(DeviceOptions{Flags: FlagSupportZeroCopy | FlagUserCopy}, &deviceHooks{
		createControlResources: func() (*os.File, *ioURing, error) {
			return tempCharFile(t), nil, nil
		},
		addControlDevice: func(_ *Device, info *DevInfo) error {
			capturedFlags = info.Flags
			info.DevID = 8
			return nil
		},
		openCharDevice: func(*Device, string) (*os.File, error) {
			return tempCharFile(t), nil
		},
	})
	if err != nil {
		t.Fatalf("newDeviceWithHooks: %v", err)
	}
	if capturedFlags&FlagSupportZeroCopy == 0 || capturedFlags&FlagAutoBufReg == 0 || capturedFlags&FlagCmdIoctlEncode == 0 {
		t.Fatalf("flags = %#x, want zero-copy + auto-buf-reg + ioctl-encode", capturedFlags)
	}
	if capturedFlags&FlagUserCopy != 0 {
		t.Fatalf("flags = %#x, want user-copy cleared", capturedFlags)
	}
}

func TestNewDeviceWithHooksCharRetry(t *testing.T) {
	const successfulAttempt = 3 // Third attempt proves we retry and sleep between failures.

	attempts := 0
	sleeps := 0
	_, err := newDeviceWithHooks(DeviceOptions{}, &deviceHooks{
		createControlResources: func() (*os.File, *ioURing, error) {
			return tempCharFile(t), nil, nil
		},
		addControlDevice: func(_ *Device, info *DevInfo) error {
			info.DevID = 9
			return nil
		},
		openCharDevice: func(*Device, string) (*os.File, error) {
			attempts++
			if attempts < successfulAttempt {
				return nil, errors.New("not yet")
			}
			return tempCharFile(t), nil
		},
		sleep: func(time.Duration) {
			sleeps++
		},
	})
	if err != nil {
		t.Fatalf("newDeviceWithHooks: %v", err)
	}
	if attempts != successfulAttempt {
		t.Fatalf("attempts = %d, want %d", attempts, successfulAttempt)
	}
	if sleeps != successfulAttempt-1 {
		t.Fatalf("sleeps = %d, want %d", sleeps, successfulAttempt-1)
	}
}

func TestNewDeviceWithHooksOpenCharFailureCleanup(t *testing.T) {
	deleteCalls := 0
	closeRingCalls := 0
	closeFileCalls := 0
	attempts := 0
	wantErr := errors.New("char open boom")

	_, err := newDeviceWithHooks(DeviceOptions{}, &deviceHooks{
		createControlResources: func() (*os.File, *ioURing, error) {
			return tempCharFile(t), nil, nil
		},
		addControlDevice: func(_ *Device, info *DevInfo) error {
			info.DevID = 10
			return nil
		},
		openCharDevice: func(*Device, string) (*os.File, error) {
			attempts++
			return nil, wantErr
		},
		sleep: func(time.Duration) {},
		deleteControl: func(*Device) error {
			deleteCalls++
			return nil
		},
		closeCtrlRing: func(*Device) error {
			closeRingCalls++
			return nil
		},
		closeCtrlFile: func(*Device) error {
			closeFileCalls++
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("error = %v, want char open failure", err)
	}
	if attempts != charDevicePollAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, charDevicePollAttempts)
	}
	if deleteCalls != 1 || closeRingCalls != 1 || closeFileCalls != 1 {
		t.Fatalf("cleanup calls = delete:%d ring:%d file:%d, want 1 each", deleteCalls, closeRingCalls, closeFileCalls)
	}
}

func TestSetParamsUsesControlHook(t *testing.T) {
	var got Params
	dev := newHookedDevice(1, &deviceHooks{
		setParams: func(_ *Device, params *Params) error {
			got = *params
			return nil
		},
	})

	params := &Params{}
	if err := dev.SetParams(params); err != nil {
		t.Fatalf("SetParams: %v", err)
	}
	wantLen := uint32(unsafe.Sizeof(*params))
	if got.Len != wantLen {
		t.Fatalf("Len = %d, want %d", got.Len, wantLen)
	}
}

func TestGetParamsUsesControlHook(t *testing.T) {
	var captured Params
	dev := newHookedDevice(1, &deviceHooks{
		getParams: func(_ *Device, params *Params) error {
			captured = *params
			params.Basic.DevSectors = 123
			return nil
		},
	})

	params, err := dev.GetParams()
	if err != nil {
		t.Fatalf("GetParams: %v", err)
	}
	wantLen := uint32(unsafe.Sizeof(Params{}))
	if captured.Len != wantLen {
		t.Fatalf("Len = %d, want %d", captured.Len, wantLen)
	}
	if captured.Types != ParamTypeAll {
		t.Fatalf("Types = %#x, want %#x", captured.Types, ParamTypeAll)
	}
	if params.Basic.DevSectors != 123 {
		t.Fatalf("DevSectors = %d, want 123", params.Basic.DevSectors)
	}
}

func TestServeUsesPreparedUserQueue(t *testing.T) {
	cmdBuf := make([]byte, ioDescSize)
	encodeIODesc(cmdBuf, testTagZero, ioDesc{OpFlags: uint32(OpRead), NrSectors: testSectorCount})

	startCalls := 0
	affinityCalls := 0
	releaseCalls := 0
	handled := 0
	dev := newServeTestDevice(t, 0, &deviceHooks{
		startControl: func(*Device) error {
			startCalls++
			return nil
		},
		applyQueueAffinity: func(*Device, uint16) {
			affinityCalls++
		},
		prepareUserQueue: func(*Device, uint16) (*preparedUserQueue, error) {
			return &preparedUserQueue{
				ring:   newReadyThenStopRing(),
				cmdBuf: cmdBuf,
				release: func() {
					releaseCalls++
				},
			}, nil
		},
	})

	err := dev.Serve(HandlerFunc(func(*Request) error {
		handled++
		return nil
	}))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if startCalls != 1 || affinityCalls != 1 || releaseCalls != 1 || handled != 1 {
		t.Fatalf("counts = start:%d affinity:%d release:%d handled:%d, want 1 each", startCalls, affinityCalls, releaseCalls, handled)
	}
}

func TestServeZeroCopyUsesPreparedQueue(t *testing.T) {
	cmdBuf := make([]byte, ioDescSize)
	encodeIODesc(cmdBuf, testTagZero, ioDesc{OpFlags: uint32(OpRead), NrSectors: testSectorCount})

	startCalls := 0
	releaseCalls := 0
	handled := 0
	dev := newServeTestDevice(t, FlagSupportZeroCopy, &deviceHooks{
		startControl: func(*Device) error {
			startCalls++
			return nil
		},
		applyQueueAffinity: func(*Device, uint16) {},
		prepareZeroCopyQueue: func(*Device, uint16) (*preparedZeroCopyQueue, error) {
			return &preparedZeroCopyQueue{
				ring:   newReadyThenStopRing(),
				cmdBuf: cmdBuf,
				charFD: testControlFD,
				release: func() {
					releaseCalls++
				},
			}, nil
		},
	})

	err := dev.ServeZeroCopy(ZeroCopyHandlerFunc(func(*ZeroCopyRequest) error {
		handled++
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeZeroCopy: %v", err)
	}
	if startCalls != 1 || releaseCalls != 1 || handled != 1 {
		t.Fatalf("counts = start:%d release:%d handled:%d, want 1 each", startCalls, releaseCalls, handled)
	}
}

func TestServeZeroCopyRejectsMissingFlag(t *testing.T) {
	dev := newServeTestDevice(t, 0, nil)
	err := dev.ServeZeroCopy(ZeroCopyHandlerFunc(func(*ZeroCopyRequest) error { return nil }))
	if err == nil || !strings.Contains(err.Error(), "FlagSupportZeroCopy") {
		t.Fatalf("error = %v, want zero-copy flag rejection", err)
	}
}
