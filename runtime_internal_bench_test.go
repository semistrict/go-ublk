package ublk

import (
	"io"
	"syscall"
	"testing"
)

const (
	benchmarkQueueDepth = uint16(64) // Synthetic queue depth used to model the steady-state hot path.
	benchmarkQueueID    = uint16(1)  // Synthetic queue identifier used for fake request descriptors.
	benchmarkBatchOne   = uint32(1)  // Single-ready CQE batch to model the least favorable batching case.
	benchmarkBatchEight = uint32(8)  // Eight-ready CQE batch to model the typical batched fast path.

	benchmarkRequestSectors = uint32(128) // 128 sectors = 64 KiB per synthetic request.
	benchmarkRequestBytes   = int64(benchmarkRequestSectors) * 512
)

type benchmarkQueueRing struct {
	queueDepth    uint16
	availableSQEs uint32
	readyBatch    uint32
	totalRequests int

	pendingSlots []ioURingSQE
	pendingCount int

	completedRequests int
	nextTag           uint16
	readyRemaining    uint32
	sentTerminalCQE   bool
	currentCQE        ioURingCQE
}

func newBenchmarkQueueRing(queueDepth uint16, availableSQEs uint32, totalRequests int) *benchmarkQueueRing {
	pendingCapacity := int(queueDepth) + int(availableSQEs) + 1
	if pendingCapacity < 1 {
		pendingCapacity = 1
	}
	return &benchmarkQueueRing{
		queueDepth:    queueDepth,
		availableSQEs: availableSQEs,
		readyBatch:    availableSQEs,
		totalRequests: totalRequests,
		pendingSlots:  make([]ioURingSQE, pendingCapacity),
	}
}

func (r *benchmarkQueueRing) GetSQE() *ioURingSQE {
	if r.pendingCount >= len(r.pendingSlots) {
		return nil
	}
	sqe := &r.pendingSlots[r.pendingCount]
	*sqe = ioURingSQE{}
	r.pendingCount++
	return sqe
}

func (r *benchmarkQueueRing) Submit() error {
	r.pendingCount = 0
	return nil
}

func (r *benchmarkQueueRing) SubmitAndWaitCQE() (*ioURingCQE, error) {
	r.pendingCount = 0
	return r.nextReadyCQE()
}

func (r *benchmarkQueueRing) WaitCQE() (*ioURingCQE, error) {
	return r.nextReadyCQE()
}

func (r *benchmarkQueueRing) TryCQE() *ioURingCQE {
	if r.readyRemaining == 0 || r.completedRequests >= r.totalRequests {
		return nil
	}
	r.readyRemaining--
	return r.successCQE()
}

func (r *benchmarkQueueRing) SeenCQE(*ioURingCQE) {}

func (r *benchmarkQueueRing) AvailableSQEs() uint32 {
	return r.availableSQEs
}

func (r *benchmarkQueueRing) nextReadyCQE() (*ioURingCQE, error) {
	if r.completedRequests >= r.totalRequests {
		if r.sentTerminalCQE {
			return nil, io.EOF
		}
		r.sentTerminalCQE = true
		r.currentCQE = ioURingCQE{Res: -int32(syscall.ENODEV)}
		return &r.currentCQE, nil
	}

	cqe := r.successCQE()
	remainingRequests := r.totalRequests - r.completedRequests
	if remainingRequests < 0 {
		remainingRequests = 0
	}
	if remainingRequests == 0 || r.readyBatch <= benchmarkBatchOne {
		r.readyRemaining = 0
		return cqe, nil
	}

	burst := r.readyBatch - benchmarkBatchOne
	if remainingRequests < int(burst) {
		r.readyRemaining = uint32(remainingRequests)
		return cqe, nil
	}
	r.readyRemaining = burst
	return cqe, nil
}

func (r *benchmarkQueueRing) successCQE() *ioURingCQE {
	tag := r.nextTag
	r.nextTag++
	if r.nextTag == r.queueDepth {
		r.nextTag = 0
	}
	r.completedRequests++
	r.currentCQE = ioURingCQE{
		UserData: uint64(tag),
		Res:      0,
	}
	return &r.currentCQE
}

type benchmarkUserCopyFile struct {
	backing []byte
}

func newBenchmarkUserCopyFile() *benchmarkUserCopyFile {
	return &benchmarkUserCopyFile{
		backing: make([]byte, benchmarkRequestBytes),
	}
}

func (f *benchmarkUserCopyFile) ReadAt(p []byte, off int64) (int, error) {
	copyWrappedInto(p, f.backing, off)
	return len(p), nil
}

func (f *benchmarkUserCopyFile) WriteAt(p []byte, off int64) (int, error) {
	copyWrappedOut(f.backing, p, off)
	return len(p), nil
}

func copyWrappedInto(dst []byte, src []byte, off int64) {
	if len(dst) == 0 || len(src) == 0 {
		return
	}
	start := int(off % int64(len(src)))
	if start < 0 {
		start += len(src)
	}
	copied := copy(dst, src[start:])
	if copied == len(dst) {
		return
	}
	copy(dst[copied:], src[:len(dst)-copied])
}

func copyWrappedOut(dst []byte, src []byte, off int64) {
	if len(src) == 0 || len(dst) == 0 {
		return
	}
	start := int(off % int64(len(dst)))
	if start < 0 {
		start += len(dst)
	}
	copied := copy(dst[start:], src)
	if copied == len(src) {
		return
	}
	copy(dst[:len(src)-copied], src[copied:])
}

type benchmarkQueueConfig struct {
	name          string
	availableSQEs uint32
	loadDesc      func(tag uint16) ioDesc
	handler       func(*Device) Handler
}

type benchmarkZeroCopyConfig struct {
	name          string
	availableSQEs uint32
	loadDesc      func(tag uint16) ioDesc
	handler       ZeroCopyHandler
}

func BenchmarkSyntheticUserQueue(b *testing.B) {
	configs := []benchmarkQueueConfig{
		{
			name:          "noop_read_batch1",
			availableSQEs: benchmarkBatchOne,
			loadDesc: func(uint16) ioDesc {
				return ioDesc{OpFlags: uint32(OpRead), NrSectors: benchmarkRequestSectors}
			},
			handler: func(*Device) Handler {
				return HandlerFunc(func(*Request) error { return nil })
			},
		},
		{
			name:          "noop_read_batch8",
			availableSQEs: benchmarkBatchEight,
			loadDesc: func(uint16) ioDesc {
				return ioDesc{OpFlags: uint32(OpRead), NrSectors: benchmarkRequestSectors}
			},
			handler: func(*Device) Handler {
				return HandlerFunc(func(*Request) error { return nil })
			},
		},
		{
			name:          "copy_mixed_batch8",
			availableSQEs: benchmarkBatchEight,
			loadDesc: func(tag uint16) ioDesc {
				op := OpRead
				if tag%2 == 1 {
					op = OpWrite
				}
				return ioDesc{OpFlags: uint32(op), NrSectors: benchmarkRequestSectors}
			},
			handler: func(d *Device) Handler {
				perTag := make([][]byte, benchmarkQueueDepth)
				writePayload := make([]byte, benchmarkRequestBytes)
				for tag := range perTag {
					perTag[tag] = make([]byte, benchmarkRequestBytes)
				}
				return HandlerFunc(func(req *Request) error {
					switch req.Op {
					case OpRead:
						_, err := req.WriteData(writePayload)
						return err
					case OpWrite:
						_, err := req.ReadData(perTag[req.Tag])
						return err
					default:
						return nil
					}
				})
			},
		},
	}

	for _, cfg := range configs {
		b.Run(cfg.name, func(b *testing.B) {
			device := &Device{
				info: DevInfo{
					QueueDepth: benchmarkQueueDepth,
				},
				stopped:      make(chan struct{}),
				userCopyData: newBenchmarkUserCopyFile(),
			}
			ring := newBenchmarkQueueRing(benchmarkQueueDepth, cfg.availableSQEs, b.N)
			handler := cfg.handler(device)
			b.ReportAllocs()
			b.SetBytes(benchmarkRequestBytes)
			b.ResetTimer()
			err := device.runUserQueueLoop(ring, benchmarkQueueID, cfg.loadDesc, nil, handler)
			b.StopTimer()
			if err != nil {
				b.Fatalf("runUserQueueLoop: %v", err)
			}
		})
	}
}

func BenchmarkSyntheticZeroCopyQueue(b *testing.B) {
	configs := []benchmarkZeroCopyConfig{
		{
			name:          "noop_read_batch8",
			availableSQEs: benchmarkBatchEight,
			loadDesc: func(uint16) ioDesc {
				return ioDesc{OpFlags: uint32(OpRead), NrSectors: benchmarkRequestSectors}
			},
			handler: ZeroCopyHandlerFunc(func(*ZeroCopyRequest) error { return nil }),
		},
		{
			name:          "noop_mixed_batch8",
			availableSQEs: benchmarkBatchEight,
			loadDesc: func(tag uint16) ioDesc {
				op := OpRead
				if tag%2 == 1 {
					op = OpWrite
				}
				return ioDesc{OpFlags: uint32(op), NrSectors: benchmarkRequestSectors}
			},
			handler: ZeroCopyHandlerFunc(func(*ZeroCopyRequest) error { return nil }),
		},
	}

	for _, cfg := range configs {
		b.Run(cfg.name, func(b *testing.B) {
			device := &Device{
				info: DevInfo{
					QueueDepth: benchmarkQueueDepth,
				},
				stopped: make(chan struct{}),
			}
			ring := newBenchmarkQueueRing(benchmarkQueueDepth, cfg.availableSQEs, b.N)
			b.ReportAllocs()
			b.SetBytes(benchmarkRequestBytes)
			b.ResetTimer()
			err := device.runZeroCopyQueueLoop(ring, benchmarkQueueID, testControlFD, cfg.loadDesc, cfg.handler)
			b.StopTimer()
			if err != nil {
				b.Fatalf("runZeroCopyQueueLoop: %v", err)
			}
		})
	}
}
