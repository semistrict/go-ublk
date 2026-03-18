#!/usr/bin/env python3
import argparse
import csv
import glob
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
DEFAULT_RUST_REPO = Path("/Users/ramon/src/libublk-rs")
DEFAULT_OUT_DIR = ROOT / ".tmp" / "bench-suite"
SHARED_RUST_HOME = ROOT / ".tmp" / "lima-rust"
SHARED_CARGO_HOME = SHARED_RUST_HOME / "cargo"
SHARED_RUSTUP_HOME = SHARED_RUST_HOME / "rustup"
RUST_TOOLCHAIN_BIN = SHARED_RUSTUP_HOME / "toolchains" / "stable-aarch64-unknown-linux-gnu" / "bin"
RUST_CARGO_BIN = RUST_TOOLCHAIN_BIN / "cargo"


def rust_env():
    env = os.environ.copy()
    env["CARGO_HOME"] = str(SHARED_CARGO_HOME)
    env["RUSTUP_HOME"] = str(SHARED_RUSTUP_HOME)
    env["PATH"] = f"{RUST_TOOLCHAIN_BIN}:{env.get('PATH', '')}"
    env["RUSTC"] = str(RUST_TOOLCHAIN_BIN / "rustc")
    env["RUSTDOC"] = str(RUST_TOOLCHAIN_BIN / "rustdoc")
    return env


def run(cmd, *, cwd=None, timeout=None, env=None, check=True, capture_output=True):
    result = subprocess.run(
        cmd,
        cwd=cwd,
        timeout=timeout,
        env=env,
        text=True,
        capture_output=capture_output,
    )
    if check and result.returncode != 0:
        raise RuntimeError(
            f"command failed ({result.returncode}): {' '.join(cmd)}\n"
            f"stdout:\n{result.stdout}\n\nstderr:\n{result.stderr}"
        )
    return result


def sync_and_drop_caches():
    run(["sync"])
    Path("/proc/sys/vm/drop_caches").write_text("3\n")


def wait_for_block_device(before, ready_file=None, stdout_path=None, timeout=20.0):
    deadline = time.time() + timeout
    pattern = re.compile(r"/dev/ublkb\d+")
    while time.time() < deadline:
        current = set(glob.glob("/dev/ublkb*"))
        new = sorted(current - before)
        if new:
            return new[0]
        if ready_file and ready_file.exists():
            data = ready_file.read_text().strip()
            if data:
                return data
        if stdout_path and stdout_path.exists():
            text = stdout_path.read_text(errors="replace")
            matches = pattern.findall(text)
            if matches:
                return matches[-1]
        time.sleep(0.05)
    raise RuntimeError("block device did not appear in time")


class Server:
    def __init__(self, name, proc, block_path, stdout_path, stderr_path, cleanup=None):
        self.name = name
        self.proc = proc
        self.block_path = block_path
        self.stdout_path = stdout_path
        self.stderr_path = stderr_path
        self.cleanup = cleanup

    def stop(self):
        if self.proc is not None and self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=3)
        if self.cleanup is not None:
            self.cleanup()

def build_binaries(work_dir: Path, rust_repo: Path):
    binaries = {
        "go_null": work_dir / "go-ublk-null",
        "go_ramdisk": work_dir / "go-ublk-ramdisk",
        "rust_null": rust_repo / "target" / "release" / "examples" / "null",
        "rust_ramdisk": rust_repo / "target" / "release" / "examples" / "ramdisk",
    }
    run(["/usr/local/go/bin/go", "build", "-o", str(binaries["go_null"]), "./cmd/go-ublk-null"], cwd=ROOT)
    run(["/usr/local/go/bin/go", "build", "-o", str(binaries["go_ramdisk"]), "./cmd/go-ublk-ramdisk"], cwd=ROOT)
    run([str(RUST_CARGO_BIN), "build", "--release", "--example", "null", "--example", "ramdisk"], cwd=rust_repo, env=rust_env())
    return binaries


def start_go_server(work_dir: Path, go_bin: Path, queues: int, depth: int, buf_size: int, sectors: int):
    work_dir.mkdir(parents=True, exist_ok=True)
    before = set(glob.glob("/dev/ublkb*"))
    ready_file = work_dir / "go.ready"
    stdout_path = work_dir / "go.stdout.log"
    stderr_path = work_dir / "go.stderr.log"
    out = stdout_path.open("w")
    err = stderr_path.open("w")
    proc = subprocess.Popen(
        [
            str(go_bin),
            "--queues", str(queues),
            "--depth", str(depth),
            "--buf-size", str(buf_size),
            "--sectors", str(sectors),
            "--ready-file", str(ready_file),
            "--skip-read-copy",
        ],
        cwd=ROOT,
        stdout=out,
        stderr=err,
        text=True,
    )
    try:
        block_path = wait_for_block_device(before, ready_file=ready_file, stdout_path=stdout_path)
        return Server("go-ublk", proc, block_path, stdout_path, stderr_path)
    except Exception:
        proc.terminate()
        raise


def start_go_ramdisk_server(work_dir: Path, go_bin: Path, queues: int, depth: int, buf_size: int, sectors: int):
    work_dir.mkdir(parents=True, exist_ok=True)
    before = set(glob.glob("/dev/ublkb*"))
    ready_file = work_dir / "go.ready"
    stdout_path = work_dir / "go.stdout.log"
    stderr_path = work_dir / "go.stderr.log"
    out = stdout_path.open("w")
    err = stderr_path.open("w")
    proc = subprocess.Popen(
        [
            str(go_bin),
            "--queues", str(queues),
            "--depth", str(depth),
            "--buf-size", str(buf_size),
            "--sectors", str(sectors),
            "--ready-file", str(ready_file),
        ],
        cwd=ROOT,
        stdout=out,
        stderr=err,
        text=True,
    )
    try:
        block_path = wait_for_block_device(before, ready_file=ready_file, stdout_path=stdout_path)
        return Server("go-ublk", proc, block_path, stdout_path, stderr_path)
    except Exception:
        proc.terminate()
        raise


def start_rust_server(work_dir: Path, rust_repo: Path, rust_bin: Path, queues: int, depth: int, buf_size: int):
    work_dir.mkdir(parents=True, exist_ok=True)
    before = set(glob.glob("/dev/ublkb*"))
    stdout_path = work_dir / "rust.stdout.log"
    stderr_path = work_dir / "rust.stderr.log"
    out = stdout_path.open("w")
    err = stderr_path.open("w")
    proc = subprocess.Popen(
        [
            str(rust_bin),
            "add",
            "--foreground",
            "--user_copy",
            "--number", "-1",
            "--queues", str(queues),
            "--depth", str(depth),
            "--buf_size", str(buf_size),
        ],
        cwd=rust_repo,
        stdout=out,
        stderr=err,
        text=True,
        env=rust_env(),
    )
    try:
        block_path = wait_for_block_device(before, stdout_path=stdout_path)
        return Server("libublk-rs", proc, block_path, stdout_path, stderr_path)
    except Exception:
        proc.terminate()
        raise


def start_rust_ramdisk_server(work_dir: Path, rust_repo: Path, rust_bin: Path, sectors: int):
    work_dir.mkdir(parents=True, exist_ok=True)
    before = set(glob.glob("/dev/ublkb*"))
    stdout_path = work_dir / "rust.stdout.log"
    stderr_path = work_dir / "rust.stderr.log"
    out = stdout_path.open("w")
    err = stderr_path.open("w")
    size_mb = max(1, (sectors * 512 + (1 << 20) - 1) // (1 << 20))
    proc = subprocess.Popen(
        [
            str(rust_bin),
            "add",
            "-1",
            str(size_mb),
        ],
        cwd=rust_repo,
        stdout=out,
        stderr=err,
        text=True,
        env=rust_env(),
    )
    try:
        block_path = wait_for_block_device(before, stdout_path=stdout_path)
        proc.wait(timeout=5)
        dev_id = int(re.search(r"/dev/ublkb(\d+)$", block_path).group(1))

        def cleanup():
            run([str(rust_bin), "del", str(dev_id)], cwd=rust_repo, check=False, env=rust_env())

        return Server("libublk-rs", proc, block_path, stdout_path, stderr_path, cleanup=cleanup)
    except Exception:
        proc.terminate()
        raise


def mount_fresh_ext4(dev_path: str, mount_dir: Path):
    run(["umount", mount_dir.as_posix()], check=False)
    mount_dir.mkdir(parents=True, exist_ok=True)
    run(["mkfs.ext4", "-F", "-q", dev_path], timeout=60)
    run(["mount", "-o", "noatime", dev_path, mount_dir.as_posix()])


def unmount(mount_dir: Path):
    run(["sync"], check=False)
    run(["umount", mount_dir.as_posix()], check=False)


def dd_bench(server: Server, mode: str, bs: int, count: int):
    sync_and_drop_caches()
    if mode == "read":
        cmd = ["dd", f"if={server.block_path}", "of=/dev/null", f"bs={bs}", f"count={count}", "status=none"]
    else:
        cmd = ["dd", "if=/dev/zero", f"of={server.block_path}", f"bs={bs}", f"count={count}", "conv=fdatasync", "status=none"]
    start = time.monotonic()
    run(cmd, timeout=180)
    duration = time.monotonic() - start
    return {
        "metric": "MiB/s",
        "value": (bs * count) / duration / (1024 * 1024),
        "bytes": bs * count,
        "seconds": duration,
    }


def fio_bench(name: str, extra_args, *, timeout=180):
    sync_and_drop_caches()
    cmd = [
        "fio",
        "--output-format=json",
        "--name=" + name,
        "--ioengine=io_uring",
        "--direct=1",
        "--group_reporting=1",
    ] + extra_args
    result = run(cmd, timeout=timeout)
    data = json.loads(result.stdout)
    job = data["jobs"][0]
    if job["read"]["bw_bytes"] > 0 and job["write"]["bw_bytes"] == 0:
        payload = job["read"]
    elif job["write"]["bw_bytes"] > 0 and job["read"]["bw_bytes"] == 0:
        payload = job["write"]
    else:
        payload = {
            "bw_bytes": job["read"]["bw_bytes"] + job["write"]["bw_bytes"],
            "iops": job["read"]["iops"] + job["write"]["iops"],
        }
    return {
        "metric": "MiB/s",
        "value": payload["bw_bytes"] / (1024 * 1024),
        "iops": payload.get("iops", 0),
        "raw": data,
    }


def fs_mark_bench(base_dir: Path):
    sync_and_drop_caches()
    target = base_dir / "f"
    shutil.rmtree(target, ignore_errors=True)
    target.mkdir(parents=True, exist_ok=True)
    result = run(
        [
            "fs_mark",
            "-d", target.as_posix(),
            "-D", "4",
            "-N", "250",
            "-n", "1000",
            "-s", "0",
            "-t", "4",
            "-L", "1",
            "-S", "0",
            "-w", "4096",
        ],
        timeout=180,
    )
    rows = [
        line.split()
        for line in result.stdout.splitlines()
        if line.strip() and not line.lstrip().startswith("#")
    ]
    if not rows or len(rows[-1]) < 4:
        raise RuntimeError(f"unable to parse fs_mark output:\n{result.stdout}")
    return {
        "metric": "files/sec",
        "value": float(rows[-1][3]),
        "raw": result.stdout,
    }


def dbench_bench(base_dir: Path):
    sync_and_drop_caches()
    target = base_dir / "d"
    shutil.rmtree(target, ignore_errors=True)
    target.mkdir(parents=True, exist_ok=True)
    result = run(
        ["dbench", "4", "-D", target.as_posix(), "-t", "20"],
        timeout=240,
    )
    combined = result.stdout + "\n" + result.stderr
    match = re.search(r"Throughput\s+([0-9.]+)\s+MB/sec", combined)
    if not match:
        raise RuntimeError(f"unable to parse dbench output:\n{combined}")
    return {
        "metric": "MB/s",
        "value": float(match.group(1)),
        "raw": combined,
    }


RAW_WORKLOADS = [
    ("raw_dd_read", lambda server, mount_dir: dd_bench(server, "read", 4096, 262144)),
    ("raw_dd_write", lambda server, mount_dir: dd_bench(server, "write", 4096, 262144)),
    ("fio_raw_seqread", lambda server, mount_dir: fio_bench("raw_seqread", [
        "--filename=" + server.block_path, "--rw=read", "--bs=128k", "--iodepth=32", "--runtime=20", "--time_based=1",
    ])),
    ("fio_raw_seqwrite", lambda server, mount_dir: fio_bench("raw_seqwrite", [
        "--filename=" + server.block_path, "--rw=write", "--bs=128k", "--iodepth=32", "--runtime=20", "--time_based=1",
    ])),
    ("fio_raw_randread", lambda server, mount_dir: fio_bench("raw_randread", [
        "--filename=" + server.block_path, "--rw=randread", "--bs=4k", "--iodepth=32", "--runtime=20", "--time_based=1",
    ])),
    ("fio_raw_randwrite", lambda server, mount_dir: fio_bench("raw_randwrite", [
        "--filename=" + server.block_path, "--rw=randwrite", "--bs=4k", "--iodepth=32", "--runtime=20", "--time_based=1",
    ])),
    ("fio_raw_randrw", lambda server, mount_dir: fio_bench("raw_randrw", [
        "--filename=" + server.block_path, "--rw=randrw", "--rwmixread=70", "--bs=4k", "--iodepth=32", "--runtime=20", "--time_based=1",
    ])),
]

FS_WORKLOADS = [
    ("fio_fs_seqwrite", lambda server, mount_dir: fio_bench("fs_seqwrite", [
        "--directory=" + mount_dir.as_posix(), "--filename=fio-seq.dat", "--size=512m",
        "--rw=write", "--bs=128k", "--iodepth=32", "--runtime=20", "--time_based=1", "--fsync_on_close=1",
    ])),
    ("fio_fs_randrw", lambda server, mount_dir: fio_bench("fs_randrw", [
        "--directory=" + mount_dir.as_posix(), "--filename=fio-rand.dat", "--size=512m",
        "--rw=randrw", "--rwmixread=70", "--bs=4k", "--iodepth=32", "--runtime=20", "--time_based=1", "--fsync_on_close=1",
    ])),
    ("fs_mark", lambda server, mount_dir: fs_mark_bench(mount_dir)),
    ("dbench", lambda server, mount_dir: dbench_bench(mount_dir)),
]


def print_table(rows):
    print(f"{'workload':<18} {'impl':<12} {'metric':<12} {'value':>12}")
    for row in rows:
        print(f"{row['workload']:<18} {row['impl']:<12} {row['metric']:<12} {row['value']:>12.1f}")


def print_ratios(rows):
    grouped = {}
    for row in rows:
        grouped.setdefault(row["workload"], {})[row["impl"]] = row
    print("\nRatios (go/libublk-rs):")
    for workload in sorted(grouped):
        group = grouped[workload]
        if "go-ublk" in group and "libublk-rs" in group:
            go_val = group["go-ublk"]["value"]
            rust_val = group["libublk-rs"]["value"]
            if rust_val:
                print(f"{workload:<18} {go_val / rust_val:>8.3f}x")


def write_results_snapshot(run_dir: Path, results, errors):
    json_path = run_dir / "results.json"
    csv_path = run_dir / "results.csv"
    errors_path = run_dir / "errors.json"
    json_path.write_text(json.dumps(results, indent=2))
    errors_path.write_text(json.dumps(errors, indent=2))
    with csv_path.open("w", newline="") as fh:
        writer = csv.DictWriter(fh, fieldnames=["workload", "impl", "metric", "value"])
        writer.writeheader()
        for row in results:
            writer.writerow({
                "workload": row["workload"],
                "impl": row["impl"],
                "metric": row["metric"],
                "value": row["value"],
            })
    return json_path, csv_path, errors_path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--out-dir", default=str(DEFAULT_OUT_DIR))
    parser.add_argument("--rust-repo", default=str(DEFAULT_RUST_REPO))
    parser.add_argument("--queues", type=int, default=1)
    parser.add_argument("--depth", type=int, default=128)
    parser.add_argument("--buf-size", type=int, default=512 * 1024)
    parser.add_argument("--sectors", type=int, default=(16 << 30) // 512)
    parser.add_argument("--fs-sectors", type=int, default=(1 << 30) // 512)
    args = parser.parse_args()

    if os.geteuid() != 0:
        raise SystemExit("must run as root in the Lima guest")

    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    run_dir = out_dir / time.strftime("%Y%m%d-%H%M%S")
    run_dir.mkdir(parents=True, exist_ok=True)
    latest = out_dir / "latest"
    if latest.exists() or latest.is_symlink():
        latest.unlink()
    latest.symlink_to(run_dir.name)

    rust_repo = Path(args.rust_repo)
    work_dir = run_dir / "work"
    work_dir.mkdir(parents=True, exist_ok=True)
    mount_dir = Path("/mnt/ublkbench")
    mount_dir.mkdir(parents=True, exist_ok=True)

    binaries = build_binaries(work_dir, rust_repo)

    raw_implementations = [
        ("go-ublk", lambda: start_go_server(work_dir / "go-null", binaries["go_null"], args.queues, args.depth, args.buf_size, args.sectors)),
        ("libublk-rs", lambda: start_rust_server(work_dir / "rust-null", rust_repo, binaries["rust_null"], args.queues, args.depth, args.buf_size)),
    ]
    fs_implementations = [
        ("go-ublk", lambda: start_go_ramdisk_server(work_dir / "go-ramdisk", binaries["go_ramdisk"], args.queues, args.depth, args.buf_size, args.fs_sectors)),
        ("libublk-rs", lambda: start_rust_ramdisk_server(work_dir / "rust-ramdisk", rust_repo, binaries["rust_ramdisk"], args.fs_sectors)),
    ]

    results = []
    errors = []
    for impl_name, starter in raw_implementations:
        server = starter()
        try:
            for workload_name, workload_fn in RAW_WORKLOADS:
                print(f"START {impl_name} {workload_name}", flush=True)
                try:
                    result = workload_fn(server, mount_dir)
                except Exception as err:
                    errors.append({"impl": impl_name, "workload": workload_name, "error": str(err)})
                    write_results_snapshot(run_dir, results, errors)
                    print(f"FAIL  {impl_name} {workload_name}", flush=True)
                    break
                result["workload"] = workload_name
                result["impl"] = impl_name
                results.append(result)
                write_results_snapshot(run_dir, results, errors)
                print(f"DONE  {impl_name} {workload_name}", flush=True)
        finally:
            server.stop()

    for impl_name, starter in fs_implementations:
        server = starter()
        try:
            for workload_name, workload_fn in FS_WORKLOADS:
                print(f"START {impl_name} {workload_name}", flush=True)
                try:
                    mount_fresh_ext4(server.block_path, mount_dir)
                    result = workload_fn(server, mount_dir)
                except Exception as err:
                    errors.append({"impl": impl_name, "workload": workload_name, "error": str(err)})
                    write_results_snapshot(run_dir, results, errors)
                    print(f"FAIL  {impl_name} {workload_name}", flush=True)
                    break
                else:
                    result["workload"] = workload_name
                    result["impl"] = impl_name
                    results.append(result)
                    write_results_snapshot(run_dir, results, errors)
                    print(f"DONE  {impl_name} {workload_name}", flush=True)
                finally:
                    unmount(mount_dir)
        finally:
            server.stop()
            unmount(mount_dir)

    json_path, csv_path, errors_path = write_results_snapshot(run_dir, results, errors)

    print_table(results)
    print_ratios(results)
    if errors:
        print("\nErrors:")
        for row in errors:
            print(f"  {row['impl']} {row['workload']}: {row['error']}")
    print(f"\nArtifacts:\n  {json_path}\n  {csv_path}\n  {errors_path}")


if __name__ == "__main__":
    main()
