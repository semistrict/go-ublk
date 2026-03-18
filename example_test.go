//go:build linux

package ublk_test

import (
	"fmt"
	"os"
	"syscall"

	"github.com/semistrict/go-ublk"
)

// ExampleNewDevice_null demonstrates creating a null block device
// that discards all writes and returns zeroes for reads.
func ExampleNewDevice_null() {
	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: 64,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = dev.Delete() }()

	err = dev.SetParams(&ublk.Params{
		Types: ublk.ParamTypeBasic,
		Basic: ublk.ParamBasic{
			LogicalBSShift:  9,
			PhysicalBSShift: 12,
			IOOptShift:      12,
			IOMinShift:      9,
			MaxSectors:      1024,
			DevSectors:      1 << 20, // 512 MiB
		},
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	go func() {
		_ = dev.Serve(ublk.HandlerFunc(func(req *ublk.Request) error {
			switch req.Op {
			case ublk.OpRead:
				buf := make([]byte, int(req.NrSectors)*512)
				_, err := req.WriteData(buf)
				return err
			case ublk.OpWrite, ublk.OpFlush, ublk.OpDiscard:
				return nil
			default:
				return fmt.Errorf("unsupported op: %s", req.Op)
			}
		}))
	}()

	fmt.Printf("null device %d at %s\n", dev.ID(), dev.BlockDevPath())
	if err := dev.Stop(); err != nil {
		fmt.Println("error:", err)
		return
	}
	// Output is non-deterministic (device ID varies), so no Output: comment.
}

// ExampleNewDevice_ramdisk demonstrates creating a RAM-backed block device.
func ExampleNewDevice_ramdisk() {
	const sectors = 2048 // 1 MiB
	data := make([]byte, sectors*512)

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: 64,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = dev.Delete() }()

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
		fmt.Println("error:", err)
		return
	}

	go func() {
		_ = dev.Serve(ublk.HandlerFunc(func(req *ublk.Request) error {
			off := int(req.StartSector) * 512
			size := int(req.NrSectors) * 512
			switch req.Op {
			case ublk.OpRead:
				_, err := req.WriteData(data[off : off+size])
				return err
			case ublk.OpWrite:
				buf := make([]byte, size)
				_, err := req.ReadData(buf)
				if err != nil {
					return err
				}
				copy(data[off:off+size], buf)
				return nil
			case ublk.OpFlush:
				return nil
			default:
				return fmt.Errorf("unsupported op: %s", req.Op)
			}
		}))
	}()

	fmt.Printf("ramdisk %d at %s\n", dev.ID(), dev.BlockDevPath())
	if err := dev.Stop(); err != nil {
		fmt.Println("error:", err)
		return
	}
}

// ExampleNewReaderAtHandler_fileBacked demonstrates exposing an *os.File as a
// block device through the easy ReaderAt/WriterAt adapter.
func ExampleNewReaderAtHandler_fileBacked() {
	// Create a temporary backing file.
	f, err := os.CreateTemp("", "ublk-readerat-*")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = os.Remove(f.Name()) }()

	const sectors = 2048 // 1 MiB backing file
	if err := f.Truncate(sectors * 512); err != nil {
		fmt.Println("error:", err)
		_ = f.Close()
		return
	}

	handler := ublk.NewReaderAtHandler(f, ublk.ReaderAtHandlerOptions{})
	defer func() { _ = handler.Close() }()

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: 64,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = dev.Delete() }()

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
		fmt.Println("error:", err)
		return
	}

	go func() {
		_ = dev.Serve(handler)
	}()

	fmt.Printf("file-backed device %d at %s using %s\n", dev.ID(), dev.BlockDevPath(), f.Name())
	if err := dev.Stop(); err != nil {
		fmt.Println("error:", err)
		return
	}
}

// ExampleNewDevice_zeroCopyLoop demonstrates creating a file-backed loop
// device using zero-copy IO.
func ExampleNewDevice_zeroCopyLoop() {
	// Create a temporary backing file.
	f, err := os.CreateTemp("", "ublk-example-*")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()

	const sectors = 2048
	if err := f.Truncate(sectors * 512); err != nil {
		fmt.Println("error:", err)
		return
	}
	backingFd := int32(f.Fd())

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: 64,
		Flags:      ublk.FlagSupportZeroCopy,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer func() { _ = dev.Delete() }()

	err = dev.SetParams(&ublk.Params{
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
			DiscardAlignment:      512,
			DiscardGranularity:    4096,
			MaxDiscardSectors:     1024,
			MaxWriteZeroesSectors: 1024,
			MaxDiscardSegments:    1,
		},
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	go func() {
		_ = dev.ServeZeroCopy(ublk.ZeroCopyHandlerFunc(func(req *ublk.ZeroCopyRequest) error {
			off := int64(req.StartSector) * 512
			size := uint32(req.NrSectors) * 512
			switch req.Op {
			case ublk.OpRead:
				return req.ReadFixed(backingFd, off, size)
			case ublk.OpWrite:
				return req.WriteFixed(backingFd, off, size)
			case ublk.OpFlush:
				return f.Sync()
			case ublk.OpDiscard:
				return punchHole(backingFd, off, int64(size))
			case ublk.OpWriteZeroes:
				return zeroRange(backingFd, off, int64(size))
			default:
				return nil
			}
		}))
	}()

	fmt.Printf("loop device %d at %s backed by %s\n", dev.ID(), dev.BlockDevPath(), f.Name())
	if err := dev.Stop(); err != nil {
		fmt.Println("error:", err)
		return
	}
}

func punchHole(fd int32, off, length int64) error {
	const mode = 0x01 | 0x02 // FALLOC_FL_KEEP_SIZE | FALLOC_FL_PUNCH_HOLE
	_, _, errno := syscall.Syscall6(syscall.SYS_FALLOCATE,
		uintptr(fd), uintptr(mode), uintptr(off), uintptr(length), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func zeroRange(fd int32, off, length int64) error {
	const mode = 0x01 | 0x10 // FALLOC_FL_KEEP_SIZE | FALLOC_FL_ZERO_RANGE
	_, _, errno := syscall.Syscall6(syscall.SYS_FALLOCATE,
		uintptr(fd), uintptr(mode), uintptr(off), uintptr(length), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
