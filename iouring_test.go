package ublk

import (
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestIOURingGetSQEClearsOnlyOne64ByteSlot(t *testing.T) {
	r := &ioURing{
		sqRingMem: make([]byte, 8),
		sqHeadOff: 0,
		sqTailOff: 4,
		sqMask:    1,
		sqEntries: 2,
		sqeSize:   64,
		sqesMem:   make([]byte, 128),
	}

	for i := range r.sqesMem {
		r.sqesMem[i] = 0xaa
	}

	sqe := r.GetSQE()
	if sqe == nil {
		t.Fatal("GetSQE() = nil, want slot")
	}

	for i := 0; i < 64; i++ {
		if got := r.sqesMem[i]; got != 0 {
			t.Fatalf("sqesMem[%d] = %#x, want 0 in first SQE", i, got)
		}
	}
	for i := 64; i < 128; i++ {
		if got := r.sqesMem[i]; got != 0xaa {
			t.Fatalf("sqesMem[%d] = %#x, want untouched second SQE", i, got)
		}
	}
}

func TestIOURingGetSQEClearsFull128ByteSlot(t *testing.T) {
	r := &ioURing{
		sqRingMem: make([]byte, 8),
		sqHeadOff: 0,
		sqTailOff: 4,
		sqMask:    0,
		sqEntries: 1,
		sqeSize:   128,
		sqesMem:   make([]byte, 128),
	}

	for i := range r.sqesMem {
		r.sqesMem[i] = 0xaa
	}

	sqe := r.GetSQE()
	if sqe == nil {
		t.Fatal("GetSQE() = nil, want slot")
	}

	for i := range r.sqesMem {
		if got := r.sqesMem[i]; got != 0 {
			t.Fatalf("sqesMem[%d] = %#x, want full 128-byte clear", i, got)
		}
	}
}

func TestIOURingTryCQEAndAvailableSQEs(t *testing.T) {
	r := &ioURing{
		sqRingMem: make([]byte, 8),
		sqHeadOff: 0,
		sqTailOff: 4,
		sqEntries: 8,
		cqRingMem: make([]byte, 24),
		cqHeadOff: 0,
		cqTailOff: 4,
		cqMask:    0,
		cqesOff:   8,
	}

	if got := r.TryCQE(); got != nil {
		t.Fatalf("TryCQE() = %v, want nil on empty ring", got)
	}
	if got := r.AvailableSQEs(); got != 8 {
		t.Fatalf("AvailableSQEs() = %d, want 8", got)
	}

	atomic.StoreUint32(r.sqTail(), 3)
	atomic.StoreUint32(r.sqHead(), 1)
	if got := r.AvailableSQEs(); got != 6 {
		t.Fatalf("AvailableSQEs() = %d, want 6", got)
	}

	atomic.StoreUint32(r.cqTail(), 1)
	cqe := r.TryCQE()
	if cqe == nil {
		t.Fatal("TryCQE() = nil, want available CQE")
	}
}

func TestIOURingSubmitRetriesPartialSubmissions(t *testing.T) {
	r := &ioURing{
		sqRingMem: make([]byte, 8),
		sqHeadOff: 0,
		sqTailOff: 4,
	}
	atomic.StoreUint32(r.sqTail(), 4)

	origEnter := ioURingEnter
	defer func() { ioURingEnter = origEnter }()

	calls := 0
	ioURingEnter = func(fd int, toSubmit, minComplete, flags uint32) (uint32, syscall.Errno) {
		calls++
		switch calls {
		case 1:
			if toSubmit != 4 {
				t.Fatalf("first submit requested %d SQEs, want 4", toSubmit)
			}
			atomic.StoreUint32(r.sqHead(), 1)
			return 1, 0
		case 2:
			if toSubmit != 3 {
				t.Fatalf("second submit requested %d SQEs, want 3", toSubmit)
			}
			atomic.StoreUint32(r.sqHead(), 4)
			return 3, 0
		default:
			t.Fatalf("unexpected extra io_uring_enter call %d", calls)
			return 0, 0
		}
	}

	if err := r.Submit(); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("Submit() called io_uring_enter %d times, want 2", calls)
	}
}

func TestIOURingSubmitRejectsZeroProgress(t *testing.T) {
	r := &ioURing{
		sqRingMem: make([]byte, 8),
		sqHeadOff: 0,
		sqTailOff: 4,
	}
	atomic.StoreUint32(r.sqTail(), 1)

	origEnter := ioURingEnter
	defer func() { ioURingEnter = origEnter }()

	ioURingEnter = func(fd int, toSubmit, minComplete, flags uint32) (uint32, syscall.Errno) {
		return 0, 0
	}

	err := r.Submit()
	if err == nil {
		t.Fatal("Submit() error = nil, want zero-progress error")
	}
	if !strings.Contains(err.Error(), "submitted 0") {
		t.Fatalf("Submit() error = %v, want zero-progress detail", err)
	}
}

func TestIOURingSubmitUsesLocalSubmitCursor(t *testing.T) {
	r := &ioURing{
		sqRingMem: make([]byte, 8),
		sqHeadOff: 0,
		sqTailOff: 4,
		sqSubmit:  4,
	}
	atomic.StoreUint32(r.sqHead(), 0) // kernel hasn't freed old entries yet
	atomic.StoreUint32(r.sqTail(), 5) // one new SQE was queued locally

	origEnter := ioURingEnter
	defer func() { ioURingEnter = origEnter }()

	ioURingEnter = func(fd int, toSubmit, minComplete, flags uint32) (uint32, syscall.Errno) {
		if toSubmit != 1 {
			t.Fatalf("Submit() requested %d SQEs, want only the newly queued 1", toSubmit)
		}
		return 1, 0
	}

	if err := r.Submit(); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if r.sqSubmit != 5 {
		t.Fatalf("sqSubmit = %d, want 5", r.sqSubmit)
	}
}
