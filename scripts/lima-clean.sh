#!/usr/bin/env bash
set -euo pipefail

instance="ublk"
dry_run="0"
vacuum_journal="1"

usage() {
  cat <<'EOF'
Usage: scripts/lima-clean.sh [options]

Frees disk space in the Lima guest by removing common Go build caches,
temporary files, and package-manager residue.

Options:
  --instance NAME   Lima instance name (default: ublk)
  --dry-run         Show what would run without deleting anything
  --no-journal      Skip journal vacuuming
  -h, --help        Show this help
EOF
}

while (($# > 0)); do
  case "$1" in
    --instance)
      instance="$2"
      shift 2
      ;;
    --dry-run)
      dry_run="1"
      shift
      ;;
    --no-journal)
      vacuum_journal="0"
      shift
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

printf 'Cleaning Lima instance %s\n' "$instance"

limactl shell "$instance" -- sudo env \
  GO_UBLK_DRY_RUN="$dry_run" \
  GO_UBLK_VACUUM_JOURNAL="$vacuum_journal" \
  bash -s <<'EOF'
set -euo pipefail

run() {
  if [[ "$GO_UBLK_DRY_RUN" == "1" ]]; then
    printf 'DRY-RUN: %s\n' "$*"
    return 0
  fi
  "$@"
}

run_sh() {
  if [[ "$GO_UBLK_DRY_RUN" == "1" ]]; then
    printf 'DRY-RUN: %s\n' "$*"
    return 0
  fi
  bash -lc "$*"
}

print_usage() {
  echo "Disk usage:"
  df -h /
  echo
  echo "Major directories:"
  du -sh /root/.cache /root/go/pkg/mod /root/go/bin /tmp /var/tmp /var/cache/apt 2>/dev/null || true
  echo
}

print_usage

if command -v go >/dev/null 2>&1; then
  run go clean -cache -testcache -modcache
fi

run_sh 'rm -rf /root/.cache/go-build /root/.cache/golangci-lint'
run_sh 'find /tmp -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true'
run_sh 'find /var/tmp -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true'

if command -v apt-get >/dev/null 2>&1; then
  run apt-get clean
  run_sh 'rm -rf /var/lib/apt/lists/*'
fi

if [[ "$GO_UBLK_VACUUM_JOURNAL" == "1" ]] && command -v journalctl >/dev/null 2>&1; then
  run journalctl --vacuum-size=100M || true
fi

print_usage
EOF
