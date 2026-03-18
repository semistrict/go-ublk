# Profiling Scripts

Install common guest-side dependencies first:

```sh
scripts/lima-setup.sh
```

Free space in the guest:

```sh
scripts/lima-clean.sh
scripts/lima-clean.sh --dry-run
```

Run the repeatable comparison suite:

```sh
scripts/lima-bench-suite.sh
```

Artifacts land under `.tmp/bench-suite/latest/`.

Latest Lima suite snapshot:

```text
Artifact set:
  .tmp/bench-suite/20260318-111251/

Raw block ratios (go/libublk-rs):
  raw_dd_read:      1.197x
  raw_dd_write:     1.156x
  fio_raw_seqread:  0.993x
  fio_raw_seqwrite: 0.925x
  fio_raw_randread: 0.971x
  fio_raw_randwrite: 0.957x
  fio_raw_randrw:   0.957x

Filesystem:
  go-ublk produced valid fs numbers
  libublk-rs failed at mkfs.ext4 with Input/output error in this environment
```

Run the plain Go-vs-Rust perf comparison without Go profiling:

```sh
scripts/lima-perf-compare.sh
scripts/lima-perf-compare.sh --write-only --count 262144 --warmup-count 4096
```

Latest focused compare snapshots:

```text
Read-only direct dd:
  go-ublk:    376.1 MiB/s
  libublk-rs: 362.5 MiB/s
  ratio:      1.037x

Write-only unprofiled dd:
  go-ublk:    2887.8 MiB/s
  libublk-rs: 2991.7 MiB/s
  ratio:      0.965x
```

Run the host-native synthetic queue benchmarks:

```sh
scripts/run-sim-bench.sh
```

Useful knobs:

```sh
scripts/run-sim-bench.sh --count 5 --benchtime 2s
scripts/run-sim-bench.sh --out-dir /Users/ramon/src/go-ublk/.tmp/sim-bench-long
```

Write-side perf/profile flow:

```sh
scripts/lima-write-profile.sh
```

Important:

```text
Do not use lima-write-profile.sh as a fair Go-vs-Rust regression check.
It enables Go CPU/alloc profiling for the Go server only, so throughput
numbers from that script are intentionally skewed downward on the Go side.
Use scripts/lima-perf-compare.sh for apples-to-apples comparisons.
```

Read the saved CPU profile:

```sh
scripts/lima-pprof-top.sh .tmp/write-profile/go-write.cpu
```

Read the saved allocs profile:

```sh
scripts/lima-pprof-top.sh .tmp/write-profile/go-write.allocs -- --sample_index=alloc_space
```

Useful knobs:

```sh
scripts/lima-write-profile.sh --depth 64 --count 20480 --warmup-count 512
scripts/lima-write-profile.sh --rust-async
scripts/lima-write-profile.sh --out-dir /Users/ramon/src/go-ublk/.tmp/write-profile-depth64
```
