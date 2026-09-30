# Bulk allocation through a complete GC cycle

This is the historical investigation before optimization. See the
[optimization report](optimization.md) for the implemented bulk API and new
before/after measurements; the original measurements below are preserved.
The subsequent [typed-pool report](pools.md) covers independent slot lifetimes.

Benchmark source: [cycle_bench_test.go](../cycle_bench_test.go).
Allocator revision: `5d5fc315ebd59d5229b720f45644453afd244374`.
This benchmark adds measurement code without changing the allocator.

## Results

**The arena at the measured revision did not improve performance in this workload.** Even when
its storage was prepared and reused, the median time from object creation to
GC completion exceeded that of ordinary individual heap allocations.

Complete cycle time, median of seven observations, in milliseconds:

| Mode | 10,000 objects | 100,000 objects | 1,000,000 objects |
| --- | ---: | ---: | ---: |
| HeapObjects | 0.681 | 6.689 | 71.155 |
| SliceFresh | 0.346 | 4.804 | 40.888 |
| SliceReuse | 0.321 | 1.574 | 16.879 |
| ArenaFresh | 2.543 | 42.546 | 720.522 |
| ArenaReset | 1.734 | 35.463 | 644.551 |
| ArenaFree | 3.387 | 72.178 | 1,540.445 |

At one million objects, ArenaReset took about **9.1 times** as long as
HeapObjects, and ArenaFree took about **21.6 times** as long. These ratios
compare medians from this local experiment; they are not universal multipliers.

Time ranges and median memory/GC metrics for one million objects:

| Mode | Min–max time, ms | Allocated per cycle, MiB | Allocations | Retained heap delta, MiB | GC cycles |
| --- | ---: | ---: | ---: | ---: | ---: |
| HeapObjects | 53.018–1,895.984 | 61.035 | 1,000,001 | About 0 | 4 |
| SliceFresh | 25.609–59.724 | 61.039 | 1 | About 0 | 2 |
| SliceReuse | 13.687–20.103 | 0 | 0 | 61.039 | 1 |
| ArenaFresh | 705.845–1,186.983 | 572.449 | 8,207 | About 0 | 5 |
| ArenaReset | 595.292–758.455 | 1.250 | 20 | 317.120 | 1 |
| ArenaFree | 1,383.365–1,935.744 | 1.250 | 20 | 317.120 | 1 |

HeapObjects also had a large outlier of about 1.9 seconds, so conclusions do not
rely solely on averages or minimum times. These results do not establish fine
differences between small batches or GC pause times. Reusing the arena reduced
heap allocation and GC counts but increased retained memory and overall time.
Creating a fresh arena also includes the allocation map's growth, raising total
allocated bytes to about 572MiB. Arena reuse did not completely eliminate new
heap allocations in this workload.

## Where time is spent

A separate CPU profile of ArenaReset and ArenaFree at one million objects showed:

- About 36% cumulative time in `isPointerFree`, mostly under `sync.Map.Load`.
  Type validation is cached, but every allocation still performs a cache lookup.
- About 10% cumulative time in `runtime.mapassign`. Each object gets an entry in
  the `live` map containing location, type, pointer, and handle metadata.
- About 11% cumulative time in `byteBackend.addFree`. Individual Free calls
  perform lookup/deletion, memory clearing, and free region merging.

These are cumulative call-tree percentages and must not be added together.
The profile includes warm-up and is a separate run from the unprofiled timing
samples. It supports the finding that bookkeeping beyond bumping an offset is
substantial. Mutex and interface operations are also on the execution path, but
this profile alone does not establish them as the main bottlenecks.

For bulk production of one struct type, possible next steps are an
`AllocSlice[T](count)` API that validates and tracks an entire batch once, or a
`Pool[T]` that computes type information and size once. If individual release is
required, slot arrays with generation counters and occupancy bitmaps are worth
evaluating instead of a large map. These are proposed optimizations, not
implemented or measured improvements. Ordinary `[]T` allocation and reuse were
the most effective approaches in this experiment.

## Environment and workload

- September 30, 2026; Go 1.27.1; Windows/arm64.
- Snapdragon X Plus X1P42100 / Qualcomm Oryon, eight logical CPUs, approximately
  32GiB of RAM.
- `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`, default GOEXPERIMENT.
- Allocation runs on one goroutine. The GC may run in parallel.
- A fixed, pointer-free, 64-byte struct containing one uint64 ID and seven more
  uint64 values.
- 10,000, 100,000, and 1,000,000 objects. Every field is written, then read and
  checked against an expected checksum.
- One warm-up cycle per case followed by seven observations of one measured
  cycle each. Tables show medians; raw output and JSON also retain min/max times.
  Only completed, successful runs are included.
- Local measurements without a fixed power mode, CPU frequency, or process
  affinity. Background work, GC timing, and map layout introduce variation.

The [environment manifest](results/environment.json) also records the allocator
revision, benchmark source hash, configuration, and command.

## What one cycle includes

```text
Create N objects → write every field → read every field and verify checksum
→ release/reset storage → remove all object pointers → wait for runtime.GC()
```

Both automatic GC during creation and the final forced GC are included in the
total. [`runtime.GC`](https://pkg.go.dev/runtime#GC) waits for collection to finish;
the installed runtime source also confirms that it waits for marking and sweeping.
The benchmark does not call `debug.FreeOSMemory` to force physical memory back
to the operating system.

Every mode stores object pointers in the same kind of `[]*cycleRecord` list.
Creating that list is excluded, but filling and clearing it are included. A
global root forces real heap allocation, and all object pointers are removed
before GC. The baseline does not measure `new` calls optimized onto the stack.

| Mode | Creation | End of cycle | Storage retained for the next cycle |
| --- | --- | --- | --- |
| HeapObjects | One `new(T)` per object | Remove pointers + GC | None |
| SliceFresh | One `make([]T, N)` | Remove pointers + GC | None |
| SliceReuse | Use existing `[]T` elements | Typed clear + remove pointers + GC | Array |
| ArenaFresh | `New` + N calls to `Alloc[T]` | `Close` + remove pointers + GC | None |
| ArenaReset | N calls to `Alloc[T]` on an existing arena | `Reset` + remove pointers + GC | Backing buffer and tracking metadata |
| ArenaFree | N calls to `Alloc[T]` on an existing arena | N calls to `Free` + remove pointers + GC | Backing buffer and tracking metadata |

ArenaFree releases objects in allocation order, a favorable and reproducible
coalescing pattern. Random-order release and its fragmentation costs are not
measured. Reuse modes exclude initial arena/array creation and warm-up;
ArenaReset and ArenaFree therefore pay the initial map growth costs before the
measured cycle. ArenaFresh and SliceFresh create new storage inside the cycle.

Fresh and reuse modes have different lifetime semantics. In particular, Reset
invalidates objects but retains backing memory.

## Reading the metrics

- `ns/op`: Wall-clock time for an entire create/use/release/GC cycle.
- `create-use-ns/op`, `release-GC-ns/op`: Diagnostic phase timings using the same
  `testing.B.Elapsed` clock as the total. On Windows, Go's benchmark clock uses
  QueryPerformanceCounter. The sum of phase medians need not equal the median
  total time.
- `B/op`, `allocs/op`: Total new Go heap allocation volume and count during a
  cycle. These are neither peak memory nor the size of already retained storage.
- `retained-heap-B`: HeapAlloc after the final GC minus HeapAlloc in the initial
  state with only the common pointer list prepared. Negative values are shown
  as zero. This is an approximate process-heap delta, not an exact arena-only
  accounting or peak RSS measurement.
- `GCs/op`: Automatic collections plus the final forced collection.
- `GC-pause-ns/op`: Change in MemStats stop-the-world pause time, not total GC CPU
  time. Small pauses sometimes read as zero on this Windows environment, so this
  metric is not used to support conclusions.

## Reproducing the measurements

Run in PowerShell; the environment variables are restored afterward.

```powershell
$priorGOGC = $env:GOGC
$priorLimit = $env:GOMEMLIMIT
$priorProcs = $env:GOMAXPROCS
try {
    $env:GOGC = '100'
    $env:GOMEMLIMIT = 'off'
    $env:GOMAXPROCS = '8'
    go test '-run=^$' '-bench=^BenchmarkLifecycle$' -benchtime=1x -count=7 -cpu=8 -benchmem |
        Set-Content benchmarks/results/lifecycle-windows-arm64.txt
} finally {
    $env:GOGC = $priorGOGC
    $env:GOMEMLIMIT = $priorLimit
    $env:GOMAXPROCS = $priorProcs
}
python benchmarks/summarize.py benchmarks/results/lifecycle-windows-arm64.txt
```

`summarize.py` uses only Python's standard library. It prints medians, ranges,
and allocation metrics and writes the
[JSON summary](results/lifecycle-windows-arm64.summary.json).
The [raw results](results/lifecycle-windows-arm64.txt) are also included.

Run profiling separately with the same environment settings. The comparison
tables use measurements taken without profiling. The profile includes warm-up
and benchmark harness work, so it is not an exact breakdown of timed cycles alone.

```powershell
$profilePath = Join-Path $env:TEMP 'arena-lifecycle-cpu.pprof'
$binaryPath = Join-Path $env:TEMP 'arena-lifecycle.test.exe'
go test '-run=^$' '-bench=^BenchmarkLifecycle$/^N1000000$/^(ArenaReset|ArenaFree)$' `
    -benchtime=5x -count=1 -cpu=8 -benchmem "-cpuprofile=$profilePath" -o $binaryPath
go tool pprof -top -nodecount=30 $binaryPath $profilePath
go tool pprof -top -cum -nodecount=25 $binaryPath $profilePath
```

Saved profile artifacts include the [flat rankings](results/lifecycle-cpu-top.txt),
[cumulative rankings](results/lifecycle-cpu-cumulative.txt), and
[profiled benchmark log](results/lifecycle-profile-run.txt).

The benchmark validates object checksums and the reused arena's active allocation
count and storage invariants. Tests, `go vet`, and golangci-lint 2.14.0's default
checks passed. All six modes at 10,000 objects also completed one separate cycle
with `checkptr=2`; those timings are excluded from the performance comparison.
Race detection is unavailable on Windows/arm64. No dependencies or allocator
behavior were changed for this experiment.
