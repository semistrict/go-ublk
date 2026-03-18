# go-ublk

`go-ublk` is an experimental Go library and toolset for building Linux
`ublk` userspace block devices.

The repo currently includes:
- the core `ublk` package
- a null target in `cmd/go-ublk-null`
- a ramdisk target in `cmd/go-ublk-ramdisk`
- a comparison harness in `cmd/perf-compare`
- repeatable Lima benchmark and profiling scripts in `scripts/`

## Requirements

Runtime integration tests and examples require:
- Linux with `ublk` support
- `/dev/ublk-control`
- root privileges

GitHub Actions only runs the portable checks:
- `make build`
- `make test`
- `make check`

The `Makefile` is host-native and does not shell out to Lima. The `test`
target is intentionally limited to unit tests that do not require `ublk`
kernel support. Full Linux integration coverage and benchmarks are exercised
separately in Lima.

## Local checks

```sh
make build
make test
make check
```

## Benchmarks and profiling

Set up the Lima guest once:

```sh
scripts/lima-setup.sh
```

Run the repeatable benchmark suite:

```sh
scripts/lima-bench-suite.sh
```

Run focused profiling flows:

```sh
scripts/lima-read-profile.sh
scripts/lima-write-profile.sh
```

More script details are in [scripts/README.md](scripts/README.md).
