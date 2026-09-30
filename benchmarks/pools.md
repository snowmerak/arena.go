# Typed pools: independent slot reuse

`Arena.MakePool[T](capacity)` reserves a complete array once, then returns a
`Pool[T]` that independently allocates and frees its slots. The arena tracks
one backing allocation. Per-slot metadata is one pointer-free uint64 holding
either an active generation or the next free-list index. Alloc uses an unused
slot or pops a free slot; Free validates the slot address, clears T with a typed
write, and pushes the slot. Neither operation searches an arena map, merges
free regions, or grows metadata. Synchronization uses the arena's mutex.

This preserves individual lifetimes, stable addresses, zeroed allocations,
generation-checked handles, and diagnostic checking. It changes capacity
management: free slots belong to this pool until Pool.Close returns the whole
reservation. Arena.Reset and Arena.Close invalidate the pool permanently.

## Complete lifecycle

The existing [lifecycle benchmark](../cycle_bench_test.go) retains its original
workload: create N pointer-free 64-byte records, write and read every word,
verify the checksum, release all borrowed references, and wait for runtime.GC.
The common escaping pointer list is allocated outside timing. Each sample has
one complete warm-up and one measured cycle. Three modes were added:

- `PoolFresh`: creates the arena and pool within the cycle, individually
  allocates and frees every slot, closes the arena, drops views, and forces GC.
- `PoolFree`: prepares and warms the arena/pool outside timing, then individually
  allocates and frees every slot, drops views, and forces GC. Free order is
  allocation order, as in ArenaFree. The free stack produces reverse allocation
  order on the next cycle.
- `PoolReset`: prepares the pool outside timing, individually allocates slots,
  and calls Pool.Reset to clear them together before GC. It is a bulk-release
  comparison, not evidence for individual Free performance.

Seven samples per case, median complete-cycle milliseconds:

| Mode | 10,000 | 100,000 | 1,000,000 |
| --- | ---: | ---: | ---: |
| HeapObjects | 0.339 | 3.222 | 28.760 |
| ArenaFree | 1.819 | 40.945 | 608.367 |
| PoolFresh | 0.649 | 5.928 | 51.492 |
| PoolFree | 0.637 | 5.209 | 49.974 |
| PoolReset | 0.451 | 3.029 | 28.107 |
| ArenaSliceFree | 0.231 | 1.168 | 9.253 |
| SliceReuse | 0.209 | 1.145 | 8.496 |

One-million-record ranges and median memory metrics:

| Mode | Min–max ms | Allocated MiB/cycle | Allocs/cycle | Retained MiB | GCs/cycle |
| --- | ---: | ---: | ---: | ---: | ---: |
| HeapObjects | 26.593–30.868 | 61.035 | 1,000,000 | About 0 | 4 |
| ArenaFree | 598.803–694.543 | 1.250 | 20 | 317.120 | 1 |
| PoolFresh | 51.158–52.120 | 68.673 | 10 | About 0 | 2 |
| PoolFree | 49.143–53.541 | 0 | 0 | 68.674 | 1 |
| PoolReset | 27.682–30.279 | 0 | 0 | 68.674 | 1 |
| ArenaSliceFree | 8.972–9.785 | 0 | 0 | 61.041 | 1 |
| SliceReuse | 8.409–9.537 | 0 | 0 | 61.039 | 1 |

For this million-record lifecycle, PoolFree was about 12.2 times faster than
ArenaFree, with about 78% less retained heap. It still took about 1.7 times as
long as separate heap allocations. The pool removes large tracking costs but
does not establish a CPU-time advantage over the Go allocator. Bulk release
remains cheaper when individual lifetimes are unnecessary.

Retained heap is a post-GC process-heap delta excluding the common pointer list,
not peak RSS. PoolFree retains roughly 64 million bytes of object storage and
8 million bytes of slot metadata, plus allocation rounding and fixed metadata.
The zero-allocation measurement applies to warmed reuse. Construction,
reflection type descriptors, and diagnostic checks can allocate.

## Repeated scattered replacement

[BenchmarkPoolChurn](../pool_bench_test.go) covers continued reuse after a full
initial population. It retains either 1,024 or 65,536 live 64-byte records and
replaces one record per operation, choosing indices with the fixed odd step
7919 modulo the power-of-two capacity. Every old record's checksum is checked,
every new record is fully written, and all final records are checked outside
timing. The escaping pointer list prevents stack-allocation shortcuts.

Setup, initial filling, one setup GC, and final cleanup are outside timing.
Automatic GC during heap churn is included; this is not a forced-GC lifecycle
measurement. Arena and pool modes explicitly free the old slot before acquiring
its replacement. PoolHandles additionally captures a new handle each time and
frees by the previous generation; this includes an extra synchronized HandleOf
operation. Metadata operations have one caller; this does not measure contention.

Seven samples per case, 300ms target duration each:

| Live slots | Mode | Median ns/op | Min–max ns/op | Bytes/op | Allocs/op |
| ---: | --- | ---: | ---: | ---: | ---: |
| 1,024 | HeapObjects | 33.64 | 31.54–35.33 | 64 | 1 |
| 1,024 | ArenaObjects | 164.00 | 160.40–180.10 | 0 | 0 |
| 1,024 | Pool | 55.31 | 54.77–56.08 | 0 | 0 |
| 1,024 | PoolHandles | 77.20 | 76.66–78.00 | 0 | 0 |
| 65,536 | HeapObjects | 42.89 | 41.73–93.33 | 64 | 1 |
| 65,536 | ArenaObjects | 428.30 | 383.00–2,067.00 | 0 | 0 |
| 65,536 | Pool | 562.70 | 504.60–885.50 | 0 | 0 |
| 65,536 | PoolHandles | 1,134.00 | 681.70–2,283.00 | 0 | 0 |

At 1,024 slots, pool operations improve on ArenaObjects but remain slower than
the heap. At 65,536 slots the pool median also exceeds ArenaObjects. Large-case
ranges are wide, especially for later cases; these measurements do not isolate
cache effects, synchronization costs, or machine-state changes. No general churn
speedup over ArenaObjects or HeapObjects is claimed. Allocation volume is lower,
but zero allocations alone do not demonstrate faster execution.

## Environment and reproduction

The base revision is `c3c993b` plus the pool implementation. Exact source and
measured test-executable SHA-256 hashes are recorded in
[pool-environment.json](results/pool-environment.json). Environment: Go 1.27.1,
Windows/arm64, Snapdragon X Plus X1P42100 / Qualcomm Oryon, eight logical CPUs,
GOMAXPROCS=8, GOGC=100, and GOMEMLIMIT=off. No intentional test/build/profile
workload ran concurrently with measurement. Affinity, CPU power/frequency, and
OS background activity were not controlled. Cases ran in the benchmark's
declared order, not randomized or interleaved. All observations are preserved.
Earlier reports were separate runs; use the controls in this run for comparisons.

With that environment, run these commands from the repository root:

```powershell
go test '-run=^$' '-bench=^BenchmarkLifecycle$' -benchtime=1x -count=7 -cpu=8 -benchmem |
    Set-Content -Encoding utf8 benchmarks/results/pool-lifecycle.txt
go test '-run=^$' '-bench=^BenchmarkPoolChurn$' -benchtime=300ms -count=7 -cpu=8 -benchmem |
    Set-Content -Encoding utf8 benchmarks/results/pool-churn.txt
python benchmarks/summarize.py benchmarks/results/pool-lifecycle.txt
python benchmarks/summarize.py benchmarks/results/pool-churn.txt --benchmark PoolChurn
```

The recorded runs used one precompiled `go test -c` executable with equivalent
`-test.*` arguments. Choose other output paths to preserve the checked-in runs.

- [Lifecycle raw output](results/pool-lifecycle.txt) and [summary](results/pool-lifecycle.summary.json)
- [Churn raw output](results/pool-churn.txt) and [summary](results/pool-churn.summary.json)
- [Environment and source manifest](results/pool-environment.json)

Validation passed: unit/example tests (96.4% statement coverage), vet, checkptr=2,
and golangci-lint 2.14.0 default checks. Ten-second four-worker fuzz runs passed:
FuzzPool completed 111,272 executions and FuzzArena completed 57,113 executions.
Tests cover slot reuse, stale/foreign/interior handles and pointers, zero-sized
and empty pools, capacity/overflow failures, generation exhaustion, concurrent
independent slots, pool/arena reset and close, backing-handle release, checker
corruption, and a typed backend's outgoing GC references. The default byte
backend continues to reject pointer-bearing elements. Race detection is not
supported on Windows/arm64; govulncheck was unavailable and was not run.
