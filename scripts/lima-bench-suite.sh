#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

instance="ublk"
out_dir="$repo_root/.tmp/bench-suite"
queues="1"
depth="128"
buf_size="$((512 * 1024))"
sectors="$(((16 << 30) / 512))"
fs_sectors="$(((1 << 30) / 512))"
rust_repo="/Users/ramon/src/libublk-rs"

usage() {
  cat <<'EOF'
Usage: scripts/lima-bench-suite.sh [options]

Runs the repeatable benchmark suite inside the Lima guest against both
go-ublk and libublk-rs. Raw block workloads use null targets; filesystem
workloads use RAM-backed targets.

Options:
  --instance NAME   Lima instance name (default: ublk)
  --out-dir PATH    Output directory (default: .tmp/bench-suite)
  --queues N        Queue count (default: 1)
  --depth N         Queue depth (default: 128)
  --buf-size N      Max IO buffer size in bytes (default: 524288)
  --sectors N       Device size in 512-byte sectors (default: 16 GiB)
  --fs-sectors N    Filesystem device size in 512-byte sectors (default: 1 GiB)
  --rust-repo PATH  Rust repo path (default: /Users/ramon/src/libublk-rs)
  -h, --help        Show this help
EOF
}

while (($# > 0)); do
  case "$1" in
    --instance)
      instance="$2"
      shift 2
      ;;
    --out-dir)
      out_dir="$2"
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
    --sectors)
      sectors="$2"
      shift 2
      ;;
    --fs-sectors)
      fs_sectors="$2"
      shift 2
      ;;
    --rust-repo)
      rust_repo="$2"
      shift 2
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

limactl shell "$instance" -- sudo bash -lc \
  "cd '$repo_root' && python3 ./scripts/guest-bench-suite.py \
    --out-dir '$out_dir' \
    --rust-repo '$rust_repo' \
    --queues '$queues' \
    --depth '$depth' \
    --buf-size '$buf_size' \
    --sectors '$sectors' \
    --fs-sectors '$fs_sectors'"
