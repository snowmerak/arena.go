# Bulk allocation optimization

The later [typed-pool implementation and measurements](pools.md) address
individual slot reuse. This report preserves the earlier bulk-API experiment.

`AllocSlice[T](n)` stores and tracks a complete batch once. It removes the
per-element mutex, type validation, map insertion, and release bookkeeping from
the bulk workload. Elements share a lifetime; this is a different release
granularity from `Alloc`/`Free`, not a faster drop-in implementation of individual
object lifetimes. Exact type, alignment, zeroing, allocation IDs, and diagnostics
remain checked. The backend receives `[n]T`, preserving typed-storage extension
support without placing outgoing pointers in byte storage.

Two smaller changes retain the existing API: the byte backend remembers the
last validated type, and free regions merge before insertion. The million-entry
live map still makes individual allocation expensive.

## Measurements

Baseline: `0a36f0ba794e92503b69394f837fa3776b77af3c`. The optimized working-tree
sources and benchmark are identified by SHA-256 in
[optimization-environment.json](results/optimization-environment.json).
Both executables ran on the same Windows/arm64 machine with Go 1.27.1,
Snapdragon X Plus X1P42100, eight logical CPUs, GOMAXPROCS=8, GOGC=100, and
GOMEMLIMIT=off. Each case has seven observations. Before/after execution order
alternates each round. There was no intentional competing test, build, or
profiling process during timing. CPU frequency, affinity, power state, and OS
background activity were not controlled.

One operation still creates 10,000, 100,000, or 1,000,000 pointer-free 64-byte
records, writes and checks all eight words, releases the records, clears the
escaping pointer list, and waits for `runtime.GC()`. The common pointer list is
created outside timing. A complete warm-up cycle precedes every sample,
including fresh modes. Reuse modes prepare their backing storage outside
timing; fresh modes create it within each cycle. Reflected type caches are warm.
No checksum work, pointer-list writes, or forced GC was removed for the new API.

`ArenaSliceFresh` creates an arena, calls `AllocSlice`, and closes the arena.
`ArenaSliceReset` retains the arena and calls `Reset` after each batch.
`ArenaSliceFree` retains the arena and calls `FreeSlice` after each batch.
FreeSlice clears the batch immediately; Reset defers zeroing until allocation.
Each reused measured cycle therefore includes a complete zeroing pass, as does
the existing SliceReuse baseline. Borrowed batch and element views are dropped
before collection. The original six modes remain available and unchanged in
work performed.

Median complete-cycle time in milliseconds:

| Optimized executable mode | 10,000 | 100,000 | 1,000,000 |
| --- | ---: | ---: | ---: |
| HeapObjects | 0.659 | 6.478 | 51.439 |
| SliceFresh | 0.413 | 4.391 | 37.436 |
| SliceReuse | 0.320 | 1.657 | 14.397 |
| ArenaFresh | 2.747 | 39.737 | 694.667 |
| ArenaReset | 1.690 | 31.270 | 719.353 |
| ArenaFree | 3.441 | 65.515 | 1,485.816 |
| ArenaSliceFresh | 0.372 | 4.537 | 32.642 |
| ArenaSliceReset | 0.325 | 1.689 | 18.792 |
| ArenaSliceFree | 0.353 | 1.764 | 19.593 |

One-million-object details, with median memory metrics:

| Mode | Median ms | Min–max ms | Allocated MiB/cycle | Allocs/cycle | Retained MiB | GCs/cycle |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| HeapObjects | 51.439 | 46.354–89.162 | 61.035 | 1,000,000 | About 0 | 4 |
| SliceFresh | 37.436 | 24.840–45.674 | 61.039 | 1 | About 0 | 2 |
| SliceReuse | 14.397 | 13.099–17.345 | 0 | 0 | 61.039 | 1 |
| ArenaReset | 719.353 | 655.680–772.692 | 1.250 | 20 | 317.120 | 1 |
| ArenaFree | 1,485.816 | 1,142.448–1,667.815 | 1.250 | 20 | 317.120 | 1 |
| ArenaSliceFresh | 32.642 | 19.350–52.672 | 61.040 | 5 | About 0 | 2 |
| ArenaSliceReset | 18.792 | 10.667–20.640 | 0 | 0 | 61.041 | 1 |
| ArenaSliceFree | 19.593 | 9.844–21.912 | 0 | 0 | 61.041 | 1 |

The reused bulk arena was about 2.7 times faster than allocating separate heap
objects in this experiment. This comparison includes the benefit of retaining
storage. The fresh bulk arena was about 1.6 times faster than separate heap
objects. Neither result demonstrates an advantage over ordinary slice storage:
plain SliceReuse had the lowest million-object median, and fresh slice/arena
ranges overlap. Use plain slices when arena allocation tracking and mixed
allocation lifetimes are unnecessary.

Replacing individual tracking with batch tracking reduced retained memory from
about 317MiB to 61MiB and measured warmed allocations to zero. Retained memory
is the post-GC heap delta excluding the common pointer list, not peak RSS;
runtime noise and global type caches can affect it. Zero measured allocations
does not mean fresh construction, new reflected types, or diagnostics are free.
Go caches reflected `[n]T` types; unbounded distinct element-count/type pairs can
retain metadata outside the arena even after Close.

The unchanged individual API did not overcome its main limitation. At one
million objects, the before/after medians were 704.899/694.667ms for ArenaFresh,
796.379/719.353ms for ArenaReset, and 1,648.051/1,485.816ms for ArenaFree.
These ranges overlap, and some small cases regressed. Seven noisy observations
do not establish a consistent speedup for the two small fast-path changes.
Independent per-object lifetime workloads still need a different tracking
design if they are to approach heap or slice performance.

## Reproduction and evidence

Build the baseline revision and working tree into separate test executables,
then run the comparison sequentially. Example from the repository root:

```powershell
git worktree add --detach ../arena-baseline 0a36f0ba794e92503b69394f837fa3776b77af3c
$beforeBinary = Join-Path $env:TEMP 'arena-before.test.exe'
$afterBinary = Join-Path $env:TEMP 'arena-after.test.exe'
Push-Location ../arena-baseline
try { go test -c -o $beforeBinary } finally { Pop-Location }
go test -c -o $afterBinary
./benchmarks/compare.ps1 -BeforeBinary $beforeBinary -AfterBinary $afterBinary
python benchmarks/summarize.py benchmarks/results/optimization-before.txt
python benchmarks/summarize.py benchmarks/results/optimization-after.txt
```

The [comparison script](compare.ps1) alternates executable order, runs one
measured cycle per case in each process, and repeats seven times. Each output
file contains repeated benchmark headers and PASS records. Use a different
`-OutputDirectory` to preserve the checked-in observations during reruns.

- [Baseline raw output](results/optimization-before.txt) and [summary](results/optimization-before.summary.json)
- [Optimized raw output](results/optimization-after.txt) and [summary](results/optimization-after.summary.json)
- [Source, executable, and environment manifest](results/optimization-environment.json)
- [Original investigation and CPU profile](README.md)

Correctness validation passed: unit and example tests, `go vet`, `checkptr=2`,
golangci-lint default checks, and a ten-second four-worker fuzz run with 85,376
executions. Fuzzing now mixes typed batches with objects, buffers, strings,
individual releases, and resets while checking contents and the allocator's
partition. Batch tests cover alignment, overflow, pointer-type rejection,
zero-sized values, empty batches, partial/foreign/wrong-type releases, stale
handles, and an alternate backend with real pointer-bearing typed arrays.
Race detection remains unavailable on Windows/arm64.
