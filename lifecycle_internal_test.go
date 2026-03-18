package ublk

import (
	"errors"
	"strings"
	"testing"
	"time"
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
