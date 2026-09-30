package arena_test

import (
	"fmt"
	"runtime"
	"testing"
	"unsafe"

	arena "github.com/snowmerak/arena.go"
)

// BenchmarkPoolChurn fills storage before timing, then replaces one scattered
// slot per operation. The odd index step visits every slot in a power-of-two
// capacity. Each old record is checked and every new record is fully written.
// Automatic GC is included; setup and final release/forced GC are excluded.
// Heap records escape via cyclePointers, as in BenchmarkLifecycle.
func BenchmarkPoolChurn(b *testing.B) {
	for _, capacity := range []int{1024, 65536} {
		b.Run(fmt.Sprintf("N%d", capacity), func(b *testing.B) {
			for _, mode := range []string{"HeapObjects", "ArenaObjects", "Pool", "PoolHandles"} {
				b.Run(mode, func(b *testing.B) {
					values := make([]*cycleRecord, capacity)
					expected := make([]uint64, capacity)
					cyclePointers = values
					b.Cleanup(func() { cyclePointers = nil })
					var a *arena.Arena
					var pool *arena.Pool[cycleRecord]
					var handles []arena.PoolHandle
					if mode != "HeapObjects" {
						a = newArena(b, capacity*int(unsafe.Sizeof(cycleRecord{})))
					}
					if mode == "Pool" || mode == "PoolHandles" {
						var err error
						pool, err = a.MakePool[cycleRecord](capacity)
						if err != nil {
							b.Fatal(err)
						}
					}
					if mode == "PoolHandles" {
						handles = make([]arena.PoolHandle, capacity)
					}
					for i := range values {
						var err error
						switch mode {
						case "HeapObjects":
							values[i] = new(cycleRecord)
						case "ArenaObjects":
							values[i], err = a.Alloc[cycleRecord]()
						default:
							values[i], err = pool.Alloc()
						}
						if err != nil {
							b.Fatal(err)
						}
						if handles != nil {
							handles[i], err = pool.HandleOf(values[i])
							if err != nil {
								b.Fatal(err)
							}
						}
						fillCycleRecord(values[i], i)
						expected[i] = uint64(i)
					}
					runtime.GC()
					index, sequence := 0, capacity
					b.ReportAllocs()
					for b.Loop() {
						index = (index + 7919) & (capacity - 1)
						previous := values[index]
						sum := previous.ID
						for _, value := range previous.Values {
							sum += value
						}
						if sum != 8*expected[index]+28 {
							b.Fatal("corrupted live slot")
						}
						var next *cycleRecord
						var err error
						switch mode {
						case "HeapObjects":
							next = new(cycleRecord)
						case "ArenaObjects":
							if err = a.Free(previous); err != nil {
								b.Fatal(err)
							}
							next, err = a.Alloc[cycleRecord]()
						case "Pool":
							if err = pool.Free(previous); err != nil {
								b.Fatal(err)
							}
							next, err = pool.Alloc()
						case "PoolHandles":
							if err = pool.FreeHandle(handles[index]); err != nil {
								b.Fatal(err)
							}
							next, err = pool.Alloc()
						}
						if err != nil {
							b.Fatal(err)
						}
						if handles != nil {
							handles[index], err = pool.HandleOf(next)
							if err != nil {
								b.Fatal(err)
							}
						}
						fillCycleRecord(next, sequence)
						values[index] = next
						expected[index] = uint64(sequence)
						sequence++
					}
					for i, value := range values {
						sum := value.ID
						for _, word := range value.Values {
							sum += word
						}
						if sum != 8*expected[i]+28 {
							b.Fatal("incorrect final record")
						}
					}
					if a != nil {
						requireCheck(b, a)
					}
					runtime.KeepAlive(values)
				})
			}
		})
	}
}
