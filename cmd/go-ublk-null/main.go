package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/semistrict/go-ublk"
)

func main() {
	var (
		queues        = flag.Uint("queues", 1, "number of hardware queues")
		depth         = flag.Uint("depth", 128, "queue depth")
		bufSize       = flag.Uint("buf-size", 512*1024, "max io buffer size in bytes")
		sectors       = flag.Uint64("sectors", 250<<30, "device size in 512-byte sectors")
		readyFile     = flag.String("ready-file", "", "path to write the block device path when ready")
		autoPartScan  = flag.Bool("auto-part-scan", false, "allow automatic partition scanning after START_DEV")
		cpuProfile    = flag.String("cpu-profile", "", "write CPU profile to this path after the device is ready")
		heapProfile   = flag.String("heap-profile", "", "write live heap profile to this path on exit")
		allocsProfile = flag.String("allocs-profile", "", "write cumulative allocation profile to this path on exit")
		logInterval   = flag.Duration("log-interval", 0, "periodically log request counters to stderr")
		skipReadCopy  = flag.Bool("skip-read-copy", false, "do not pwrite read data to /dev/ublkcN; benchmark-only mode matching libublk-rs null")
	)
	flag.Parse()

	var flags uint64
	if !*autoPartScan {
		flags |= ublk.FlagNoAutoPartScan
	}

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:        uint16(*queues),
		QueueDepth:    uint16(*depth),
		MaxIOBufBytes: uint32(*bufSize),
		Flags:         flags,
	})
	if err != nil {
		fatalf("NewDevice: %v", err)
	}
	defer func() { _ = dev.Delete() }()

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

	stats := nullStats{
		byTag: make([]atomic.Uint64, int(*queues)*int(*depth)),
	}
	readBufs := make([][]byte, int(*queues)*int(*depth))
	for i := range readBufs {
		readBufs[i] = make([]byte, int(*bufSize))
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
			switch req.Op {
			case ublk.OpRead:
				atomic.AddUint64(&stats.reads, 1)
				if *skipReadCopy {
					return nil
				}
				size := int(req.NrSectors) * 512
				if size > int(*bufSize) {
					return fmt.Errorf("read size %d exceeds max io buf %d", size, *bufSize)
				}
				buf := readBufs[int(req.QueueID)*int(*depth)+int(req.Tag)][:size]
				n, err := req.WriteData(buf)
				if err != nil {
					fmt.Fprintf(os.Stderr, "write-data error queue=%d tag=%d size=%d wrote=%d err=%v\n",
						req.QueueID, req.Tag, size, n, err)
				}
				return err
			case ublk.OpWrite, ublk.OpFlush, ublk.OpDiscard, ublk.OpWriteZeroes:
				atomic.AddUint64(&stats.writes, 1)
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

	prof, err := startProfiles(*cpuProfile, *heapProfile, *allocsProfile)
	if err != nil {
		fatalf("start profiles: %v", err)
	}
	defer func() {
		if err := prof.Close(); err != nil {
			fatalf("write profiles: %v", err)
		}
	}()

	fmt.Println(dev.BlockDevPath())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		_ = sig
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

type profileSession struct {
	cpuFile       *os.File
	heapProfile   string
	allocsProfile string
}

type nullStats struct {
	total  uint64
	reads  uint64
	writes uint64
	other  uint64
	byTag  []atomic.Uint64
}

func logStatsLoop(stop <-chan struct{}, interval time.Duration, stats *nullStats) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			topTag, topCount, activeTags := statsSnapshot(stats)
			fmt.Fprintf(os.Stderr, "stats total=%d reads=%d writes=%d other=%d active_tags=%d top_tag=%d top_tag_count=%d\n",
				atomic.LoadUint64(&stats.total),
				atomic.LoadUint64(&stats.reads),
				atomic.LoadUint64(&stats.writes),
				atomic.LoadUint64(&stats.other),
				activeTags,
				topTag,
				topCount,
			)
		}
	}
}

func statsSnapshot(stats *nullStats) (topTag int, topCount uint64, activeTags int) {
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

func startProfiles(cpuProfile, heapProfile, allocsProfile string) (*profileSession, error) {
	p := &profileSession{
		heapProfile:   heapProfile,
		allocsProfile: allocsProfile,
	}
	if cpuProfile == "" {
		return p, nil
	}

	if err := os.MkdirAll(filepath.Dir(cpuProfile), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(cpuProfile)
	if err != nil {
		return nil, err
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	p.cpuFile = f
	return p, nil
}

func (p *profileSession) Close() error {
	if p.cpuFile != nil {
		pprof.StopCPUProfile()
		if err := p.cpuFile.Close(); err != nil {
			return err
		}
	}
	if err := writeNamedProfile(p.heapProfile, writeHeapProfile); err != nil {
		return err
	}
	if err := writeNamedProfile(p.allocsProfile, writeAllocsProfile); err != nil {
		return err
	}
	return nil
}

func writeNamedProfile(path string, write func(*os.File) error) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return write(f)
}

func writeHeapProfile(f *os.File) error {
	runtime.GC()
	return pprof.WriteHeapProfile(f)
}

func writeAllocsProfile(f *os.File) error {
	runtime.GC()
	return pprof.Lookup("allocs").WriteTo(f, 0)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
