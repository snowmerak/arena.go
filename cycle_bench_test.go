package arena_test

import (
	"fmt"
	"runtime"
	"testing"
	"time"
	"unsafe"

	arena "github.com/snowmerak/arena.go"
)

// A fixed 64-byte, pointer-free object keeps this workload independent of the
// arena's handle layout. Every word is written and later read in every mode.
type cycleRecord struct {
	ID     uint64
	Values [7]uint64
}

var cyclePointers []*cycleRecord
var cycleChecksum uint64

// BenchmarkLifecycle measures one complete create/use/release/GC cycle per op.
// Reuse modes retain their warmed storage; fresh modes include creating it.
// Use -benchtime=1x -count=7 to collect seven individual cycle observations.
func BenchmarkLifecycle(b *testing.B) {
	for _, count := range []int{10_000, 100_000, 1_000_000} {
		b.Run(fmt.Sprintf("N%d", count), func(b *testing.B) {
			for _, mode := range []string{"HeapObjects", "SliceFresh", "SliceReuse", "ArenaFresh", "ArenaReset", "ArenaFree", "ArenaSliceFresh", "ArenaSliceReset", "ArenaSliceFree", "PoolFresh", "PoolFree", "PoolReset"} {
				b.Run(mode, func(b *testing.B) { benchmarkLifecycle(b, mode, count) })
			}
		})
	}
}

func benchmarkLifecycle(b *testing.B, mode string, count int) {
	values := make([]*cycleRecord, count)
	cyclePointers = values // Force real heap allocations, including the baseline.
	b.Cleanup(func() { cyclePointers = nil })
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	var reusable *arena.Arena
	var slab []cycleRecord
	var pool *arena.Pool[cycleRecord]
	if mode == "ArenaReset" || mode == "ArenaFree" || mode == "ArenaSliceReset" || mode == "ArenaSliceFree" || mode == "PoolFree" || mode == "PoolReset" {
		var err error
		reusable, err = arena.New(count * int(unsafe.Sizeof(cycleRecord{})))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = reusable.Close() })
		if mode == "PoolFree" || mode == "PoolReset" {
			pool, err = reusable.MakePool[cycleRecord](count)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	if mode == "SliceReuse" {
		slab = make([]cycleRecord, count)
	}

	// Warm all modes once, including the arena's map capacity and type cache.
	runLifecycle(b, mode, reusable, pool, slab, values)
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ReportAllocs()
	b.ResetTimer()
	var allocateUse, releaseGC time.Duration
	for range b.N {
		createTime, releaseTime := runLifecycle(b, mode, reusable, pool, slab, values)
		allocateUse += createTime
		releaseGC += releaseTime
	}
	b.StopTimer()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	b.ReportMetric(float64(allocateUse.Nanoseconds())/float64(b.N), "create-use-ns/op")
	b.ReportMetric(float64(releaseGC.Nanoseconds())/float64(b.N), "release-GC-ns/op")
	b.ReportMetric(float64(after.NumGC-before.NumGC)/float64(b.N), "GCs/op")
	b.ReportMetric(float64(after.PauseTotalNs-before.PauseTotalNs)/float64(b.N), "GC-pause-ns/op")
	// A process-heap delta after the final GC, not peak RSS or allocation volume.
	retained := max(int64(after.HeapAlloc)-int64(baseline.HeapAlloc), 0)
	b.ReportMetric(float64(retained), "retained-heap-B")
	if reusable != nil {
		wantActive := 0
		if pool != nil {
			wantActive = 1 // The empty pool retains its backing allocation.
			if pool.Stats().Active != 0 {
				b.Fatal("cycle retained live pool slots")
			}
		}
		if reusable.Stats().Active != wantActive {
			b.Fatal("cycle retained live arena allocations")
		}
		if err := reusable.Check(); err != nil {
			b.Fatal(err)
		}
	}
	runtime.KeepAlive(reusable)
	runtime.KeepAlive(pool)
	runtime.KeepAlive(slab)
	runtime.KeepAlive(values)
}

func runLifecycle(b *testing.B, mode string, reusable *arena.Arena, pool *arena.Pool[cycleRecord], slab []cycleRecord, values []*cycleRecord) (time.Duration, time.Duration) {
	// Use testing's high-resolution clock (QueryPerformanceCounter on Windows)
	// for phase timings as well as ns/op. time.Now can be too coarse here.
	start := b.Elapsed()
	a, batch, pool := populateCycle(b, mode, reusable, pool, slab, values)
	var sum uint64
	for _, p := range values {
		sum += p.ID
		for _, value := range p.Values {
			sum += value
		}
	}
	n := uint64(len(values))
	if sum != 4*n*n+24*n {
		b.Fatalf("invalid cycle checksum: %d", sum)
	}
	cycleChecksum = sum
	allocated := b.Elapsed()

	switch mode {
	case "ArenaFresh", "ArenaSliceFresh":
		if err := a.Close(); err != nil {
			b.Fatal(err)
		}
		a = nil
	case "ArenaReset", "ArenaSliceReset":
		if err := a.Reset(); err != nil {
			b.Fatal(err)
		}
	case "ArenaFree":
		// Allocation-order Free is a favorable, reproducible coalescing pattern.
		for _, p := range values {
			if err := a.Free(p); err != nil {
				b.Fatal(err)
			}
		}
	case "ArenaSliceFree":
		if err := a.FreeSlice(batch); err != nil {
			b.Fatal(err)
		}
	case "PoolFresh", "PoolFree":
		for _, p := range values {
			if err := pool.Free(p); err != nil {
				b.Fatal(err)
			}
		}
		if mode == "PoolFresh" {
			if err := a.Close(); err != nil {
				b.Fatal(err)
			}
			a, pool = nil, nil
		}
	case "PoolReset":
		if err := pool.Reset(); err != nil {
			b.Fatal(err)
		}
	case "SliceReuse":
		clear(slab)
	}
	batch = nil
	clear(values) // Remove every borrowed pointer before the collection.
	runtime.GC()  // Waits for a full mark/sweep cycle; included for every mode.
	runtime.KeepAlive(a)
	runtime.KeepAlive(slab)
	runtime.KeepAlive(batch)
	runtime.KeepAlive(pool)
	return allocated - start, b.Elapsed() - allocated
}

func populateCycle(b *testing.B, mode string, a *arena.Arena, pool *arena.Pool[cycleRecord], slab []cycleRecord, values []*cycleRecord) (*arena.Arena, []cycleRecord, *arena.Pool[cycleRecord]) {
	switch mode {
	case "HeapObjects":
		for i := range values {
			p := new(cycleRecord)
			fillCycleRecord(p, i)
			values[i] = p
		}
	case "ArenaSliceFresh", "ArenaSliceReset", "ArenaSliceFree":
		var err error
		if mode == "ArenaSliceFresh" {
			a, err = arena.New(len(values) * int(unsafe.Sizeof(cycleRecord{})))
			if err != nil {
				b.Fatal(err)
			}
		}
		batch, err := a.AllocSlice[cycleRecord](len(values))
		if err != nil {
			b.Fatal(err)
		}
		for i := range values {
			p := &batch[i]
			fillCycleRecord(p, i)
			values[i] = p
		}
		return a, batch, nil
	case "PoolFresh", "PoolFree", "PoolReset":
		if mode == "PoolFresh" {
			var err error
			a, err = arena.New(len(values) * int(unsafe.Sizeof(cycleRecord{})))
			if err != nil {
				b.Fatal(err)
			}
			pool, err = a.MakePool[cycleRecord](len(values))
			if err != nil {
				b.Fatal(err)
			}
		}
		for i := range values {
			p, err := pool.Alloc()
			if err != nil {
				b.Fatal(err)
			}
			fillCycleRecord(p, i)
			values[i] = p
		}
	case "SliceFresh", "SliceReuse":
		if mode == "SliceFresh" {
			slab = make([]cycleRecord, len(values))
		}
		for i := range values {
			p := &slab[i]
			fillCycleRecord(p, i)
			values[i] = p
		}
	default:
		if mode == "ArenaFresh" {
			var err error
			a, err = arena.New(len(values) * int(unsafe.Sizeof(cycleRecord{})))
			if err != nil {
				b.Fatal(err)
			}
		}
		for i := range values {
			p, err := a.Alloc[cycleRecord]()
			if err != nil {
				b.Fatal(err)
			}
			fillCycleRecord(p, i)
			values[i] = p
		}
	}
	return a, nil, pool
}

func fillCycleRecord(p *cycleRecord, index int) {
	p.ID = uint64(index)
	for j := range p.Values {
		p.Values[j] = uint64(index + j + 1)
	}
}
