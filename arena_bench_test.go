package arena_test

import "testing"

// Keeping each batch in an escaping slice forces the heap baseline to allocate
// real objects, rather than benchmarking allocations optimized onto the stack.
var benchmarkRecords []*record

func BenchmarkBatch(b *testing.B) {
	const batchSize = 128
	b.Run("Heap", func(b *testing.B) {
		values := make([]*record, batchSize)
		benchmarkRecords = values
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			for i := range values {
				values[i] = new(record)
				values[i].ID = uint64(i)
			}
		}
		if values[batchSize-1].ID != batchSize-1 {
			b.Fatal("incorrect batch")
		}
	})
	b.Run("ArenaReset", func(b *testing.B) {
		a := newArena(b, 32*1024)
		values := make([]*record, batchSize)
		benchmarkRecords = values
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			if err := a.Reset(); err != nil {
				b.Fatal(err)
			}
			for i := range values {
				var err error
				values[i], err = a.Alloc[record]()
				if err != nil {
					b.Fatal(err)
				}
				values[i].ID = uint64(i)
			}
		}
		if values[batchSize-1].ID != batchSize-1 {
			b.Fatal("incorrect batch")
		}
		clear(values)
	})
}

func BenchmarkAllocFree(b *testing.B) {
	a := newArena(b, 1024)
	b.ReportAllocs()
	for b.Loop() {
		p, err := a.Alloc[record]()
		if err != nil {
			b.Fatal(err)
		}
		p.ID = 42
		if err := a.Free(p); err != nil {
			b.Fatal(err)
		}
	}
}
