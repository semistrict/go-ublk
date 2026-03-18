#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
instance="ublk"
mode="all"
toolchain_root="$repo_root/.tmp/lima-rust"
cargo_home="$toolchain_root/cargo"
rustup_home="$toolchain_root/rustup"

build_packages=(
  make
  gcc
  g++
  pkg-config
  git
  curl
  ca-certificates
  zstd
  e2fsprogs
  util-linux
)

bench_packages=(
  fio
  fsmark
  dbench
  bonnie++
  stress-ng
)

usage() {
  cat <<'EOF'
Usage: scripts/lima-setup.sh [options]

Installs common build and benchmark dependencies into the Lima guest.

Options:
  --instance NAME   Lima instance name (default: ublk)
  --build-only      Install only build dependencies
  --bench-only      Install only benchmark dependencies
  -h, --help        Show this help
EOF
}

install_rust_toolchain() {
  printf '\nInstalling current Rust toolchain with rustup\n'
  limactl shell "$instance" -- bash -lc '
set -euo pipefail
export RUSTUP_HOME='"$(printf "%q" "$rustup_home")"'
export PATH="$HOME/.cargo/bin:$PATH"
mkdir -p "$RUSTUP_HOME" '"$(printf "%q" "$cargo_home")"'
if ! command -v rustup >/dev/null 2>&1; then
  curl --proto "=https" --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal
fi
rustup toolchain install stable --profile minimal
rustup default stable
"$RUSTUP_HOME/toolchains/stable-aarch64-unknown-linux-gnu/bin/cargo" --version
"$RUSTUP_HOME/toolchains/stable-aarch64-unknown-linux-gnu/bin/rustc" --version
'
}

while (($# > 0)); do
  case "$1" in
    --instance)
      instance="$2"
      shift 2
      ;;
    --build-only)
      mode="build"
      shift
      ;;
    --bench-only)
      mode="bench"
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

packages=()
if [[ "$mode" == "all" || "$mode" == "build" ]]; then
  packages+=("${build_packages[@]}")
fi
if [[ "$mode" == "all" || "$mode" == "bench" ]]; then
  packages+=("${bench_packages[@]}")
fi

if ((${#packages[@]} == 0)); then
  echo "no packages selected" >&2
  exit 1
fi

printf 'Installing packages in Lima instance %s (%s mode)\n' "$instance" "$mode"
limactl shell "$instance" -- sudo bash -lc \
  "DEBIAN_FRONTEND=noninteractive apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y ${packages[*]@Q} && apt-get clean"

if [[ "$mode" == "all" || "$mode" == "build" ]]; then
  install_rust_toolchain
fi

printf '\nTool summary:\n'
limactl shell "$instance" -- bash -lc '
set -euo pipefail
export RUSTUP_HOME='"$(printf "%q" "$rustup_home")"'
export PATH="$HOME/.cargo/bin:$PATH"
for tool in go cargo gcc make fio fs_mark dbench bonnie++ stress-ng mkfs.ext4 mount; do
  if [[ "$tool" == cargo && -x "$RUSTUP_HOME/toolchains/stable-aarch64-unknown-linux-gnu/bin/cargo" ]]; then
    printf "  %-10s %s\n" "$tool" "$RUSTUP_HOME/toolchains/stable-aarch64-unknown-linux-gnu/bin/cargo"
  elif command -v "$tool" >/dev/null 2>&1; then
    printf "  %-10s %s\n" "$tool" "$(command -v "$tool")"
  else
    printf "  %-10s missing\n" "$tool"
  fi
done
'
