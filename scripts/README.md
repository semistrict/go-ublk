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
