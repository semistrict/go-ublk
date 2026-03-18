#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

instance="ublk"
count="10240"
warmup_count="256"
queues="1"
depth="128"
buf_size="$((512 * 1024))"
write_only=0
read_only=0
rust_async=0
keep_temp=0
extra_args=()

usage() {
  cat <<'EOF'
Usage: scripts/lima-perf-compare.sh [options] [-- extra perf-compare args]

Runs the plain Go-vs-Rust perf comparison inside the Lima guest without
enabling Go profiling. This is the apples-to-apples wrapper to use for
regression checks.

Options:
  --instance NAME         Lima instance name (default: ublk)
  --count N               Measured dd block count (default: 10240)
  --warmup-count N        Warmup dd block count (default: 256)
  --queues N              Number of queues (default: 1)
  --depth N               Queue depth (default: 128)
  --buf-size N            Max IO buffer size in bytes (default: 524288)
  --write-only            Run only write throughput
  --read-only             Run only read throughput
  --rust-async            Run libublk-rs with --async
  --keep-temp             Keep perf-compare temp dirs
  -h, --help              Show this help
EOF
}

while (($# > 0)); do
  case "$1" in
    --instance)
      instance="$2"
      shift 2
      ;;
    --count)
      count="$2"
      shift 2
      ;;
    --warmup-count)
      warmup_count="$2"
      shift 2
      ;;
    --queues)
      queues="$2"
      shift 2
      ;;
    --depth)
      depth="$2"
      shift 2
      ;;
    --buf-size)
      buf_size="$2"
      shift 2
      ;;
    --write-only)
      write_only=1
      shift
      ;;
    --read-only)
      read_only=1
      shift
      ;;
    --rust-async)
      rust_async=1
      shift
      ;;
    --keep-temp)
      keep_temp=1
      shift
      ;;
    --)
      shift
      extra_args=("$@")
      break
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

perf_args=(
  --count "$count"
  --warmup-count "$warmup_count"
  --queues "$queues"
  --depth "$depth"
  --buf-size "$buf_size"
)

if ((write_only)); then
  perf_args+=(--write-only)
fi

if ((read_only)); then
  perf_args+=(--read-only)
fi

if ((rust_async)); then
  perf_args+=(--rust-async)
fi

if ((keep_temp)); then
  perf_args+=(--keep-temp)
fi

if ((${#extra_args[@]})); then
  perf_args+=("${extra_args[@]}")
fi

limactl shell "$instance" -- sudo bash -lc \
  "cd '$repo_root' && /usr/local/go/bin/go run ./cmd/perf-compare ${perf_args[*]@Q}"
