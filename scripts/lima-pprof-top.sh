#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

instance="ublk"
binary="$repo_root/.tmp/write-profile/go-ublk-null"
profile=""
pprof_args=()

usage() {
  cat <<'EOF'
Usage: scripts/lima-pprof-top.sh PROFILE [options] [-- extra pprof args]

Runs `go tool pprof -top` inside the Lima guest against a saved profile.

Options:
  --instance NAME         Lima instance name (default: ublk)
  --binary PATH           Go binary used to record the profile
                          (default: .tmp/write-profile/go-ublk-null)
  -h, --help              Show this help

Examples:
  scripts/lima-pprof-top.sh .tmp/write-profile/go-write.cpu
  scripts/lima-pprof-top.sh .tmp/write-profile/go-write.allocs -- --sample_index=alloc_space
EOF
}

while (($# > 0)); do
  case "$1" in
    --instance)
      instance="$2"
      shift 2
      ;;
    --binary)
      binary="$2"
      shift 2
      ;;
    --)
      shift
      pprof_args=("$@")
      break
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      if [[ -z "$profile" ]]; then
        profile="$1"
        shift
      else
        echo "unexpected argument: $1" >&2
        usage >&2
        exit 1
      fi
      ;;
  esac
done

if [[ -z "$profile" ]]; then
  usage >&2
  exit 1
fi

if [[ ! -f "$profile" ]]; then
  echo "profile not found: $profile" >&2
  exit 1
fi

if [[ ! -f "$binary" ]]; then
  echo "binary not found: $binary" >&2
  exit 1
fi

pprof_cmd=(/usr/local/go/bin/go tool pprof -top)
if ((${#pprof_args[@]})); then
  pprof_cmd+=("${pprof_args[@]}")
fi
pprof_cmd+=("$binary" "$profile")

printf -v remote_cmd 'cd %q &&' "$repo_root"
for arg in "${pprof_cmd[@]}"; do
  printf -v remote_cmd '%s %q' "$remote_cmd" "$arg"
done

limactl shell "$instance" -- bash -lc "$remote_cmd"
