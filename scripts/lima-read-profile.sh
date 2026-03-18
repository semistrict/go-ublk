#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

instance="ublk"
count="1048576"
warmup_count="4096"
queues="1"
depth="128"
buf_size="$((512 * 1024))"
out_dir="$repo_root/.tmp/read-profile"
keep_temp=0
rust_async=0
dd_direct=0
dd_timeout="30s"
extra_args=()

usage() {
  cat <<'EOF'
Usage: scripts/lima-read-profile.sh [options] [-- extra perf-compare args]

Runs the read-only perf comparison inside the Lima guest and writes Go CPU
and allocs profiles to a stable path in the mounted workspace.

Options:
  --instance NAME         Lima instance name (default: ublk)
  --count N               Measured dd block count (default: 1048576)
  --warmup-count N        Warmup dd block count (default: 4096)
  --queues N              Number of queues (default: 1)
  --depth N               Queue depth (default: 128)
  --buf-size N            Max IO buffer size in bytes (default: 524288)
  --out-dir PATH          Output directory (default: .tmp/read-profile)
  --keep-temp             Keep perf-compare temp dirs
  --rust-async            Run libublk-rs with --async
  --dd-direct             Run dd with direct IO flags
  --dd-timeout DURATION   Timeout for each dd run (default: 30s, or 120s with --dd-direct)
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
    --out-dir)
      out_dir="$2"
      shift 2
      ;;
    --keep-temp)
      keep_temp=1
      shift
      ;;
    --rust-async)
      rust_async=1
      shift
      ;;
    --dd-direct)
      dd_direct=1
      shift
      ;;
    --dd-timeout)
      dd_timeout="$2"
      shift 2
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

mkdir -p "$out_dir"

guest_repo="$repo_root"
guest_out="$out_dir"
guest_bin="$guest_out/go-ublk-null"
cpu_profile="$guest_out/go-read.cpu"
allocs_profile="$guest_out/go-read.allocs"

perf_args=(
  --go-bin "$guest_bin"
  --read-only
  --count "$count"
  --warmup-count "$warmup_count"
  --queues "$queues"
  --depth "$depth"
  --buf-size "$buf_size"
  --go-cpu-profile "$cpu_profile"
  --go-allocs-profile "$allocs_profile"
)

if ((keep_temp)); then
  perf_args+=(--keep-temp)
fi

if ((rust_async)); then
  perf_args+=(--rust-async)
fi

if ((dd_direct)); then
  perf_args+=(--dd-direct)
  if [[ "$dd_timeout" == "30s" ]]; then
    dd_timeout="120s"
  fi
fi

perf_args+=(--dd-timeout "$dd_timeout")

if ((${#extra_args[@]})); then
  perf_args+=("${extra_args[@]}")
fi

printf 'Building go-ublk null target in %s\n' "$guest_bin"
limactl shell "$instance" -- sudo bash -lc \
  "cd '$guest_repo' && /usr/local/go/bin/go build -o '$guest_bin' ./cmd/go-ublk-null"

printf 'Running read-only perf compare in Lima instance %s\n' "$instance"
limactl shell "$instance" -- sudo bash -lc \
  "cd '$guest_repo' && /usr/local/go/bin/go run ./cmd/perf-compare ${perf_args[*]@Q}"

cat <<EOF

Artifacts:
  binary: $guest_bin
  cpu:    $cpu_profile
  allocs: $allocs_profile

Next:
  scripts/lima-pprof-top.sh '$cpu_profile' --binary '$guest_bin'
  scripts/lima-pprof-top.sh '$allocs_profile' --binary '$guest_bin' -- --sample_index=alloc_space
EOF
