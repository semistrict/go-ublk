package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type serverSpec struct {
	name  string
	start func(context.Context, *config, string) (*server, error)
}

type server struct {
	name       string
	cmd        *exec.Cmd
	blockPath  string
	stdoutBuf  *tailBuffer
	stderrBuf  *tailBuffer
	stdoutLog  string
	stderrLog  string
	stdoutFile *os.File
	stderrFile *os.File
}

type tailBuffer struct {
	data []byte
	max  int
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	if len(p) >= b.max {
		b.data = append(b.data[:0], p[len(p)-b.max:]...)
		return len(p), nil
	}
	if over := len(b.data) + len(p) - b.max; over > 0 {
		b.data = append([]byte(nil), b.data[over:]...)
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *tailBuffer) String() string {
	return string(b.data)
}

type sample struct {
	name       string
	mode       string
	bytes      int64
	duration   time.Duration
	throughput float64
}

type config struct {
	rustRepo        string
	rustBinary      string
	goBinary        string
	goCPUProfile    string
	goHeapProfile   string
	goAllocsProfile string
	queues          int
	depth           int
	bufSize         int
	sectors         uint64
	bs              int
	count           int
	warmupCount     int
	writeOnly       bool
	readOnly        bool
	workdir         string
	rustAsync       bool
	debug           bool
	keepTemp        bool
	ddDirect        bool
	ddTimeout       time.Duration
	readyTimeout    time.Duration
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseFlags() *config {
	cfg := &config{}
	flag.StringVar(&cfg.rustRepo, "rust-repo", "/Users/ramon/src/libublk-rs", "path to libublk-rs repo")
	flag.StringVar(&cfg.rustBinary, "rust-bin", "", "prebuilt libublk-rs null example binary")
	flag.StringVar(&cfg.goBinary, "go-bin", "", "prebuilt go-ublk null target binary")
	flag.StringVar(&cfg.goCPUProfile, "go-cpu-profile", "", "write a CPU profile for the go-ublk server")
	flag.StringVar(&cfg.goHeapProfile, "go-heap-profile", "", "write a live heap profile for the go-ublk server")
	flag.StringVar(&cfg.goAllocsProfile, "go-allocs-profile", "", "write a cumulative allocation profile for the go-ublk server")
	flag.IntVar(&cfg.queues, "queues", 1, "number of hardware queues")
	flag.IntVar(&cfg.depth, "depth", 128, "queue depth")
	flag.IntVar(&cfg.bufSize, "buf-size", 512*1024, "max IO buffer size")
	flag.Uint64Var(&cfg.sectors, "sectors", 250<<30, "device size in 512-byte sectors")
	flag.IntVar(&cfg.bs, "bs", 4096, "dd block size in bytes")
	flag.IntVar(&cfg.count, "count", 10240, "dd block count per measured run")
	flag.IntVar(&cfg.warmupCount, "warmup-count", 256, "dd block count for warmup")
	flag.BoolVar(&cfg.writeOnly, "write-only", false, "run only write throughput")
	flag.BoolVar(&cfg.readOnly, "read-only", false, "run only read throughput")
	flag.BoolVar(&cfg.rustAsync, "rust-async", false, "run libublk-rs null example with --async")
	flag.BoolVar(&cfg.debug, "debug", false, "emit timestamped progress logs to stderr")
	flag.BoolVar(&cfg.keepTemp, "keep-temp", false, "keep temp dir with captured logs")
	flag.BoolVar(&cfg.ddDirect, "dd-direct", false, "run dd with direct IO flags to reduce page-cache effects")
	flag.DurationVar(&cfg.ddTimeout, "dd-timeout", 30*time.Second, "timeout for each dd invocation")
	flag.DurationVar(&cfg.readyTimeout, "ready-timeout", 10*time.Second, "timeout waiting for server readiness")
	flag.Parse()
	cfg.workdir, _ = os.Getwd()
	return cfg
}

func run(cfg *config) error {
	if cfg.writeOnly && cfg.readOnly {
		return fmt.Errorf("cannot set both -write-only and -read-only")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root inside the Linux guest")
	}
	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		return fmt.Errorf("/dev/ublk-control missing: %w", err)
	}
	if _, err := exec.LookPath("dd"); err != nil {
		return fmt.Errorf("dd not found: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tmpDir, err := os.MkdirTemp("", "go-ublk-perf-*")
	if err != nil {
		return err
	}
	if cfg.debug || cfg.keepTemp {
		logf(cfg, "temp dir: %s", tmpDir)
	}
	defer func() {
		if cfg.keepTemp {
			return
		}
		_ = os.RemoveAll(tmpDir)
	}()

	goBin, err := ensureGoBinary(cfg, tmpDir)
	if err != nil {
		return err
	}
	rustBin, err := ensureRustBinary(cfg, tmpDir)
	if err != nil {
		return err
	}

	specs := []serverSpec{
		{name: "go-ublk", start: func(ctx context.Context, cfg *config, dir string) (*server, error) {
			ready := filepath.Join(dir, "go.ready")
			args := []string{
				"--queues", strconv.Itoa(cfg.queues),
				"--depth", strconv.Itoa(cfg.depth),
				"--buf-size", strconv.Itoa(cfg.bufSize),
				"--sectors", strconv.FormatUint(cfg.sectors, 10),
				"--ready-file", ready,
				"--skip-read-copy",
			}
			if cfg.goCPUProfile != "" {
				args = append(args, "--cpu-profile", cfg.goCPUProfile)
			}
			if cfg.goHeapProfile != "" {
				args = append(args, "--heap-profile", cfg.goHeapProfile)
			}
			if cfg.goAllocsProfile != "" {
				args = append(args, "--allocs-profile", cfg.goAllocsProfile)
			}
			if cfg.debug {
				args = append(args, "--log-interval", "2s")
			}
			return startServer(ctx, cfg, "go-ublk", goBin, args, ready)
		}},
		{name: "libublk-rs", start: func(ctx context.Context, cfg *config, dir string) (*server, error) {
			args := []string{
				"add",
				"--foreground",
				"--user_copy",
				"--number", "-1",
				"--queues", strconv.Itoa(cfg.queues),
				"--depth", strconv.Itoa(cfg.depth),
				"--buf_size", strconv.Itoa(cfg.bufSize),
			}
			if cfg.rustAsync {
				args = append(args, "--async")
			}
			return startServer(ctx, cfg, "libublk-rs", rustBin, args, "")
		}},
	}

	var results []sample
	for _, spec := range specs {
		dir := filepath.Join(tmpDir, spec.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		logf(cfg, "starting server %s", spec.name)
		srv, err := spec.start(ctx, cfg, dir)
		if err != nil {
			return err
		}
		err = func() error {
			defer stopServer(srv)
			logf(cfg, "%s reported block path %s", srv.name, srv.blockPath)
			if err := waitForBlockDevice(srv.blockPath); err != nil {
				return err
			}
			logf(cfg, "%s block device ready", srv.name)
			if cfg.warmupCount > 0 {
				if !cfg.writeOnly {
					if _, err := runDD(cfg, srv, srv.blockPath, "read", cfg.bs, cfg.warmupCount); err != nil {
						return fmt.Errorf("%s warmup read: %w", srv.name, err)
					}
				}
				if !cfg.readOnly {
					if _, err := runDD(cfg, srv, srv.blockPath, "write", cfg.bs, cfg.warmupCount); err != nil {
						return fmt.Errorf("%s warmup write: %w", srv.name, err)
					}
				}
			}
			if !cfg.writeOnly {
				dd, err := runDD(cfg, srv, srv.blockPath, "read", cfg.bs, cfg.count)
				if err != nil {
					return fmt.Errorf("%s read: %w", srv.name, err)
				}
				results = append(results, sample{
					name:       srv.name,
					mode:       "read",
					bytes:      dd.bytes,
					duration:   dd.duration,
					throughput: float64(dd.bytes) / dd.duration.Seconds(),
				})
			}
			if !cfg.readOnly {
				dd, err := runDD(cfg, srv, srv.blockPath, "write", cfg.bs, cfg.count)
				if err != nil {
					return fmt.Errorf("%s write: %w", srv.name, err)
				}
				results = append(results, sample{
					name:       srv.name,
					mode:       "write",
					bytes:      dd.bytes,
					duration:   dd.duration,
					throughput: float64(dd.bytes) / dd.duration.Seconds(),
				})
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}

	printResults(results)
	return nil
}

func ensureGoBinary(cfg *config, dir string) (string, error) {
	if cfg.goBinary != "" {
		return cfg.goBinary, nil
	}
	out := filepath.Join(dir, "go-ublk-null")
	cmd := exec.Command("/usr/local/go/bin/go", "build", "-o", out, "./cmd/go-ublk-null")
	cmd.Dir = cfg.workdir
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build go-ublk null target: %w\n%s", err, outBytes)
	}
	return out, nil
}

func ensureRustBinary(cfg *config, dir string) (string, error) {
	if cfg.rustBinary != "" {
		return cfg.rustBinary, nil
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		return "", fmt.Errorf("cargo not found; pass -rust-bin or install cargo in the guest")
	}
	cmd := exec.Command("cargo", "build", "--release", "--example", "null")
	cmd.Dir = cfg.rustRepo
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build libublk-rs null example: %w\n%s", err, outBytes)
	}
	return filepath.Join(cfg.rustRepo, "target", "release", "examples", "null"), nil
}

func startServer(ctx context.Context, cfg *config, name, bin string, args []string, readyFile string) (*server, error) {
	before, _ := listBlockDevices()
	cmd := exec.CommandContext(ctx, bin, args...)
	stdout := newTailBuffer(64 << 10)
	stderr := newTailBuffer(64 << 10)
	dir := filepath.Dir(readyFile)
	if dir == "." || dir == "" {
		dir = os.TempDir()
	}
	stdoutLog := filepath.Join(dir, name+".stdout.log")
	stderrLog := filepath.Join(dir, name+".stderr.log")
	stdoutFile, err := os.Create(stdoutLog)
	if err != nil {
		return nil, fmt.Errorf("create %s stdout log: %w", name, err)
	}
	stderrFile, err := os.Create(stderrLog)
	if err != nil {
		_ = stdoutFile.Close()
		return nil, fmt.Errorf("create %s stderr log: %w", name, err)
	}
	cmd.Stdout = io.MultiWriter(stdout, stdoutFile)
	cmd.Stderr = io.MultiWriter(stderr, stderrFile)
	logf(cfg, "%s command: %s %s", name, bin, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}

	blockPath, err := waitForReadyFile(cfg, readyFile, stdout, name, before)
	if err != nil {
		_ = terminateProcess(cmd.Process)
		_, _ = cmd.Process.Wait()
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			err = fmt.Errorf("%w\nstderr:\n%s", err, msg)
		}
		return nil, err
	}
	return &server{
		name:       name,
		cmd:        cmd,
		blockPath:  blockPath,
		stdoutBuf:  stdout,
		stderrBuf:  stderr,
		stdoutLog:  stdoutLog,
		stderrLog:  stderrLog,
		stdoutFile: stdoutFile,
		stderrFile: stderrFile,
	}, nil
}

func waitForReadyFile(cfg *config, path string, stdout fmt.Stringer, name string, before map[string]struct{}) (string, error) {
	re := regexp.MustCompile(`/dev/ublkb\d+`)
	deadline := time.Now().Add(cfg.readyTimeout)
	for time.Now().Before(deadline) {
		if matches := re.FindAllString(stdout.String(), -1); len(matches) > 0 {
			logf(cfg, "%s ready from stdout: %s", name, matches[len(matches)-1])
			return matches[len(matches)-1], nil
		}
		if path != "" {
			data, err := os.ReadFile(path)
			if err == nil {
				logf(cfg, "%s ready from file %s", name, path)
				return strings.TrimSpace(string(data)), nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("%s ready file: %w", name, err)
			}
		}
		if path, ok := findNewBlockDevice(before); ok {
			logf(cfg, "%s ready from new block device scan: %s", name, path)
			return path, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", fmt.Errorf("%s did not become ready within %s", name, cfg.readyTimeout)
}

func waitForBlockDevice(path string) error {
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("block device %s did not appear", path)
}

func listBlockDevices() (map[string]struct{}, error) {
	matches, err := filepath.Glob("/dev/ublkb*")
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		out[m] = struct{}{}
	}
	return out, nil
}

func findNewBlockDevice(before map[string]struct{}) (string, bool) {
	after, err := listBlockDevices()
	if err != nil {
		return "", false
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			return path, true
		}
	}
	return "", false
}

func stopServer(s *server) {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = terminateProcess(s.cmd.Process)
	_, _ = s.cmd.Process.Wait()
	if s.stdoutFile != nil {
		_ = s.stdoutFile.Close()
	}
	if s.stderrFile != nil {
		_ = s.stderrFile.Close()
	}
}

func terminateProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	time.Sleep(200 * time.Millisecond)
	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

type ddResult struct {
	bytes    int64
	duration time.Duration
}

func runDD(cfg *config, srv *server, path, mode string, bs, count int) (*ddResult, error) {
	if err := dropCaches(); err != nil {
		return nil, fmt.Errorf("drop caches: %w", err)
	}

	args := []string{"bs=" + strconv.Itoa(bs), "count=" + strconv.Itoa(count), "status=none"}
	switch mode {
	case "read":
		args = append(args, "if="+path, "of=/dev/null")
		if cfg.ddDirect {
			args = append(args, "iflag=direct")
		}
	case "write":
		args = append(args, "if=/dev/zero", "of="+path, "conv=fdatasync")
		if cfg.ddDirect {
			args = append(args, "oflag=direct")
		}
	default:
		return nil, fmt.Errorf("unsupported mode %q", mode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ddTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dd", args...)
	logf(cfg, "%s %s dd start: %s", srv.name, mode, strings.Join(cmd.Args, " "))
	start := time.Now()
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)
	logf(cfg, "%s %s dd end after %s", srv.name, mode, dur.Round(time.Millisecond))
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("dd timed out after %s\nserver stdout log: %s\nserver stderr log: %s\nserver stdout tail:\n%s\nserver stderr tail:\n%s",
			cfg.ddTimeout, srv.stdoutLog, srv.stderrLog, tailString(srv.stdoutBuf.String(), 4000), tailString(srv.stderrBuf.String(), 4000))
	}
	if err != nil {
		return nil, fmt.Errorf("dd failed: %w\n%s", err, out)
	}
	return &ddResult{
		bytes:    int64(bs) * int64(count),
		duration: dur,
	}, nil
}

func logf(cfg *config, format string, args ...any) {
	if !cfg.debug {
		return
	}
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[%s] %s\n", time.Now().Format("15:04:05.000"), msg)
}

func tailString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func dropCaches() error {
	if err := exec.Command("sync").Run(); err != nil {
		return err
	}
	return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o644)
}

func printResults(results []sample) {
	fmt.Printf("%-12s %-6s %12s %12s %12s\n", "impl", "mode", "bytes", "seconds", "MiB/s")
	for _, r := range results {
		fmt.Printf("%-12s %-6s %12d %12.3f %12.1f\n",
			r.name,
			r.mode,
			r.bytes,
			r.duration.Seconds(),
			r.throughput/(1024*1024),
		)
	}

	byMode := map[string]map[string]sample{}
	for _, r := range results {
		if byMode[r.mode] == nil {
			byMode[r.mode] = map[string]sample{}
		}
		byMode[r.mode][r.name] = r
	}
	for _, mode := range []string{"read", "write"} {
		goRes, okGo := byMode[mode]["go-ublk"]
		rustRes, okRust := byMode[mode]["libublk-rs"]
		if !okGo || !okRust {
			continue
		}
		ratio := goRes.throughput / rustRes.throughput
		fmt.Printf("%s ratio go/libublk-rs: %.3fx\n", mode, ratio)
	}
}
