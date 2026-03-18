#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

BENCH_COUNT=3
BENCH_TIME="1s"
OUTPUT_DIR="$ROOT_DIR/.tmp/sim-bench"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --count)
      BENCH_COUNT="$2"
      shift 2
      ;;
    --benchtime)
      BENCH_TIME="$2"
      shift 2
      ;;
    --out-dir)
      OUTPUT_DIR="$2"
      shift 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

mkdir -p "$OUTPUT_DIR"
OUTPUT_FILE="$OUTPUT_DIR/latest.txt"

cd "$ROOT_DIR"
go test -run '^$' -bench '^BenchmarkSynthetic' -benchmem -count "$BENCH_COUNT" -benchtime "$BENCH_TIME" . | tee "$OUTPUT_FILE"

echo
echo "saved benchmark output to $OUTPUT_FILE"
