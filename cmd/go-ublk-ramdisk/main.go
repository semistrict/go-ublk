package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/semistrict/go-ublk"
)

func main() {
	var (
		queues       = flag.Uint("queues", 1, "number of hardware queues")
		depth        = flag.Uint("depth", 128, "queue depth")
		bufSize      = flag.Uint("buf-size", 512*1024, "max io buffer size in bytes")
		sectors      = flag.Uint64("sectors", (1<<30)/512, "device size in 512-byte sectors")
		readyFile    = flag.String("ready-file", "", "path to write the block device path when ready")
		autoPartScan = flag.Bool("auto-part-scan", false, "allow automatic partition scanning after START_DEV")
		logInterval  = flag.Duration("log-interval", 0, "periodically log request counters to stderr")
	)
	flag.Parse()

	var flags uint64
	if !*autoPartScan {
		flags |= ublk.FlagNoAutoPartScan
	}

	dataBytes := *sectors * 512
	if dataBytes > uint64(^uint(0)) {
		fatalf("device size %d bytes exceeds process addressable memory", dataBytes)
	}
	data := make([]byte, int(dataBytes))

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:        uint16(*queues),
		QueueDepth:    uint16(*depth),
		MaxIOBufBytes: uint32(*bufSize),
		Flags:         flags,
	})
	if err != nil {
		fatalf("NewDevice: %v", err)
	}
	defer dev.Delete()

	if err := dev.SetParams(&ublk.Params{
		Types: ublk.ParamTypeBasic,
		Basic: ublk.ParamBasic{
			LogicalBSShift:  9,
			PhysicalBSShift: 12,
			IOOptShift:      12,
			IOMinShift:      9,
			MaxSectors:      1024,
			DevSectors:      *sectors,
		},
	}); err != nil {
		fatalf("SetParams: %v", err)
	}

	stats := ramdiskStats{
		byTag: make([]atomic.Uint64, int(*queues)*int(*depth)),
	}
	writeBufs := make([][]byte, int(*queues)*int(*depth))
	for i := range writeBufs {
		writeBufs[i] = make([]byte, int(*bufSize))
	}

	if *logInterval > 0 {
		stopLog := make(chan struct{})
		defer close(stopLog)
		go logStatsLoop(stopLog, *logInterval, &stats)
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- dev.Serve(ublk.HandlerFunc(func(req *ublk.Request) error {
			atomic.AddUint64(&stats.total, 1)
			stats.byTag[int(req.QueueID)*int(*depth)+int(req.Tag)].Add(1)

			off := int(req.StartSector) * 512
			size := int(req.NrSectors) * 512
			if size > int(*bufSize) {
				return fmt.Errorf("io size %d exceeds max io buf %d", size, *bufSize)
			}
			if off < 0 || off+size > len(data) {
				return fmt.Errorf("io range [%d,%d) out of bounds for %d-byte ramdisk", off, off+size, len(data))
			}

			switch req.Op {
			case ublk.OpRead:
				atomic.AddUint64(&stats.reads, 1)
				_, err := req.WriteData(data[off : off+size])
				return err
			case ublk.OpWrite:
				atomic.AddUint64(&stats.writes, 1)
				buf := writeBufs[int(req.QueueID)*int(*depth)+int(req.Tag)][:size]
				if _, err := req.ReadData(buf); err != nil {
					return err
				}
				copy(data[off:off+size], buf)
				return nil
			case ublk.OpFlush:
				atomic.AddUint64(&stats.flushes, 1)
				return nil
			case ublk.OpDiscard, ublk.OpWriteZeroes:
				atomic.AddUint64(&stats.discards, 1)
				clear(data[off : off+size])
				return nil
			default:
				atomic.AddUint64(&stats.other, 1)
				return fmt.Errorf("unsupported op: %s", req.Op)
			}
		}))
	}()

	if err := waitForDevice(dev); err != nil {
		fatalf("wait for %s: %v", dev.BlockDevPath(), err)
	}

	if *readyFile != "" {
		if err := os.MkdirAll(filepath.Dir(*readyFile), 0o755); err != nil {
			fatalf("create ready-file dir: %v", err)
		}
		if err := os.WriteFile(*readyFile, []byte(dev.BlockDevPath()+"\n"), 0o644); err != nil {
			fatalf("write ready-file: %v", err)
		}
	}

	fmt.Println(dev.BlockDevPath())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		_ = dev.Stop()
		if err := <-serveErr; err != nil {
			fatalf("Serve: %v", err)
		}
	case err := <-serveErr:
		if err != nil {
			fatalf("Serve: %v", err)
		}
	}
}

func waitForDevice(dev *ublk.Device) error {
	path := dev.BlockDevPath()
	for i := 0; i < 1000; i++ {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if err := ensureBlockNode(dev); err != nil {
			return err
		}
		ts := syscall.Timespec{Nsec: 10_000_000}
		_ = syscall.Nanosleep(&ts, nil)
	}
	return fmt.Errorf("timed out")
}

func ensureBlockNode(dev *ublk.Device) error {
	params, err := dev.GetParams()
	if err != nil {
		return nil
	}
	if params.Devt.DiskMajor == 0 {
		return nil
	}
	if _, err := os.Stat(dev.BlockDevPath()); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	mode := uint32(syscall.S_IFBLK | 0o600)
	devNum := int(mkdev(params.Devt.DiskMajor, params.Devt.DiskMinor))
	if err := syscall.Mknod(dev.BlockDevPath(), mode, devNum); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func mkdev(major, minor uint32) uint64 {
	return uint64(minor&0xff) |
		(uint64(major&0xfff) << 8) |
		(uint64(minor&^uint32(0xff)) << 12) |
		(uint64(major&^uint32(0xfff)) << 32)
}

type ramdiskStats struct {
	total    uint64
	reads    uint64
	writes   uint64
	flushes  uint64
	discards uint64
	other    uint64
	byTag    []atomic.Uint64
}

func logStatsLoop(stop <-chan struct{}, interval time.Duration, stats *ramdiskStats) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			topTag, topCount, activeTags := statsSnapshot(stats)
			fmt.Fprintf(os.Stderr, "stats total=%d reads=%d writes=%d flushes=%d discards=%d other=%d active_tags=%d top_tag=%d top_tag_count=%d\n",
				atomic.LoadUint64(&stats.total),
				atomic.LoadUint64(&stats.reads),
				atomic.LoadUint64(&stats.writes),
				atomic.LoadUint64(&stats.flushes),
				atomic.LoadUint64(&stats.discards),
				atomic.LoadUint64(&stats.other),
				activeTags,
				topTag,
				topCount,
			)
		}
	}
}

func statsSnapshot(stats *ramdiskStats) (topTag int, topCount uint64, activeTags int) {
	for i := range stats.byTag {
		n := stats.byTag[i].Load()
		if n == 0 {
			continue
		}
		activeTags++
		if n > topCount {
			topTag = i
			topCount = n
		}
	}
	return topTag, topCount, activeTags
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
