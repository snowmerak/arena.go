package arena_test

import (
	"errors"
	"sync"
	"testing"
	"unsafe"

	arena "github.com/snowmerak/arena.go"
)

func TestPoolSlotsAndHandles(t *testing.T) {
	a := newArena(t, 256)
	if _, err := a.AllocBuffer(1); err != nil {
		t.Fatal(err)
	}
	p, err := a.MakePool[[2]uint64](4)
	if err != nil {
		t.Fatal(err)
	}
	var values [4]*[2]uint64
	var handles [4]arena.PoolHandle
	for i := range values {
		values[i], err = p.Alloc()
		if err != nil {
			t.Fatal(err)
		}
		if *values[i] != [2]uint64{} || uintptr(unsafe.Pointer(values[i]))%8 != 0 {
			t.Fatal("slot not zeroed or aligned")
		}
		*values[i] = [2]uint64{uint64(i + 1), 42}
		handles[i], err = p.HandleOf(values[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Alloc(); !errors.Is(err, arena.ErrOutOfMemory) {
		t.Fatalf("full pool: %v", err)
	}
	if p.Stats() != (arena.PoolStats{Capacity: 4, Active: 4}) || a.Stats().Active != 2 {
		t.Fatal("incorrect tracking")
	}
	if err := a.Free(values[0]); !errors.Is(err, arena.ErrTypeMismatch) {
		t.Fatalf("arena freed a slot: %v", err)
	}
	if err := p.Free(nil); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("nil accepted: %v", err)
	}
	interior := (*[2]uint64)(unsafe.Add(unsafe.Pointer(values[0]), 8))
	if err := p.Free(interior); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("interior accepted: %v", err)
	}
	if err := p.Free(new([2]uint64)); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("foreign accepted: %v", err)
	}
	if err := p.FreeHandle(arena.PoolHandle{}); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("zero handle accepted: %v", err)
	}
	other, err := a.MakePool[[2]uint64](1)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Free(values[0]); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("wrong pool accepted: %v", err)
	}
	if _, err := other.Get(handles[0]); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("foreign handle accepted: %v", err)
	}
	otherArena := newArena(t, 64)
	foreignPool, err := otherArena.MakePool[[2]uint64](1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreignPool.Get(handles[0]); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("foreign arena handle accepted: %v", err)
	}
	for _, i := range []int{1, 3, 0, 2} {
		if err := p.FreeHandle(handles[i]); err != nil {
			t.Fatal(err)
		}
		if err := p.Free(values[i]); !errors.Is(err, arena.ErrInvalidBuffer) {
			t.Fatalf("double free accepted: %v", err)
		}
		requireCheck(t, a)
	}
	for _, i := range []int{2, 0, 3, 1} {
		value, err := p.Alloc()
		if err != nil || value != values[i] || *value != [2]uint64{} {
			t.Fatalf("slot not reused and zeroed: %v", err)
		}
		if _, err := p.Get(handles[i]); !errors.Is(err, arena.ErrInvalidBuffer) {
			t.Fatalf("stale generation revived: %v", err)
		}
		if err := p.FreeHandle(handles[i]); !errors.Is(err, arena.ErrInvalidBuffer) {
			t.Fatalf("stale release accepted: %v", err)
		}
		h, err := p.HandleOf(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.Get(h)
		if err != nil || got != value {
			t.Fatalf("handle did not resolve: %v", err)
		}
		handles[i] = h
	}
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	}
	for _, h := range handles {
		if _, err := p.Get(h); !errors.Is(err, arena.ErrInvalidBuffer) {
			t.Fatalf("reset retained handle: %v", err)
		}
	}
	value, err := p.Alloc()
	if err != nil || *value != [2]uint64{} {
		t.Fatalf("reset slot not zero: %v", err)
	}
	copyOfPool := *p
	if err := copyOfPool.Free(value); err != nil {
		t.Fatal(err)
	}
	if p.Stats().Active != 0 {
		t.Fatal("pool copy did not share state")
	}
	stored, err := a.Alloc[arena.PoolHandle]()
	if err != nil {
		t.Fatal(err)
	}
	*stored = handles[0] // Handles remain pointer-free even when stale.
	if err := a.Free(stored); err != nil {
		t.Fatal(err)
	}
	requireCheck(t, a)
}

func TestPoolLifetime(t *testing.T) {
	for _, end := range []string{"pool close", "arena reset", "arena close", "backing free"} {
		t.Run(end, func(t *testing.T) {
			a := newArena(t, 64)
			p, err := a.MakePool[uint64](8)
			if err != nil {
				t.Fatal(err)
			}
			value, err := p.Alloc()
			if err != nil {
				t.Fatal(err)
			}
			h, err := p.HandleOf(value)
			if err != nil {
				t.Fatal(err)
			}
			list := a.Allocations()
			if len(list) != 1 || list[0].Type != "arena.Pool[uint64]" || list[0].Buffer.Len() != 64 {
				t.Fatal("incorrect pool diagnostics")
			}
			if _, err := a.Bytes(list[0].Buffer); !errors.Is(err, arena.ErrTypeMismatch) {
				t.Fatalf("exposed pool bytes: %v", err)
			}
			switch end {
			case "pool close":
				err = p.Close()
			case "arena reset":
				err = a.Reset()
			case "arena close":
				err = a.Close()
			case "backing free":
				err = a.FreeBuffer(list[0].Buffer)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Alloc(); !errors.Is(err, arena.ErrClosed) {
				t.Fatalf("closed allocation: %v", err)
			}
			if _, err := p.Get(h); !errors.Is(err, arena.ErrClosed) {
				t.Fatalf("closed lookup: %v", err)
			}
			if _, err := p.HandleOf(value); !errors.Is(err, arena.ErrClosed) {
				t.Fatalf("closed handle capture: %v", err)
			}
			if err := p.Free(value); !errors.Is(err, arena.ErrClosed) {
				t.Fatalf("closed free: %v", err)
			}
			if err := p.FreeHandle(h); !errors.Is(err, arena.ErrClosed) {
				t.Fatalf("closed handle free: %v", err)
			}
			if err := p.Reset(); !errors.Is(err, arena.ErrClosed) {
				t.Fatalf("closed reset: %v", err)
			}
			if p.Stats() != (arena.PoolStats{}) {
				t.Fatal("closed stats not empty")
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			if end == "arena close" {
				return
			}
			q, err := a.MakePool[uint64](8)
			if err != nil {
				t.Fatal(err)
			}
			current, err := q.Alloc()
			if err != nil {
				t.Fatal(err)
			}
			*current = 42
			if _, err := q.Get(h); !errors.Is(err, arena.ErrInvalidBuffer) {
				t.Fatalf("new pool revived old handle: %v", err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			if *current != 42 {
				t.Fatal("old pool touched new allocation")
			}
			requireCheck(t, a)
		})
	}
}

func TestPoolConstruction(t *testing.T) {
	a := newArena(t, 128)
	before := a.Stats()
	for _, n := range []int{-1, int(^uint(0) >> 1)} {
		if _, err := a.MakePool[uint64](n); !errors.Is(err, arena.ErrInvalidSize) {
			t.Fatalf("size %d: %v", n, err)
		}
	}
	for _, n := range []int{0, 1} {
		if _, err := a.MakePool[*uint64](n); !errors.Is(err, arena.ErrPointerType) {
			t.Fatalf("pointer type accepted: %v", err)
		}
		if _, err := a.MakePool[[0]*uint64](n); !errors.Is(err, arena.ErrPointerType) {
			t.Fatalf("zero pointer type accepted: %v", err)
		}
	}
	if _, err := a.MakePool[uint64](17); !errors.Is(err, arena.ErrOutOfMemory) {
		t.Fatalf("oversize accepted: %v", err)
	}
	if a.Stats() != before {
		t.Fatal("failed construction consumed space")
	}
	empty, err := a.MakePool[uint64](0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Alloc(); !errors.Is(err, arena.ErrOutOfMemory) {
		t.Fatalf("empty pool: %v", err)
	}
	if err := empty.Reset(); err != nil {
		t.Fatal(err)
	}
	type alignedZero struct{ _ [0]uint64 }
	p, err := a.MakePool[alignedZero](4)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[*alignedZero]bool)
	for range 4 {
		value, err := p.Alloc()
		if err != nil || seen[value] || uintptr(unsafe.Pointer(value))%unsafe.Alignof(*value) != 0 {
			t.Fatalf("zero-size slot identity: %v", err)
		}
		seen[value] = true
	}
	for value := range seen {
		if err := p.Free(value); err != nil {
			t.Fatal(err)
		}
	}
	requireCheck(t, a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.MakePool[uint64](1); !errors.Is(err, arena.ErrClosed) {
		t.Fatalf("closed arena: %v", err)
	}
	var zero arena.Pool[uint64]
	if _, err := zero.Alloc(); !errors.Is(err, arena.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := zero.Get(arena.PoolHandle{}); !errors.Is(err, arena.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := zero.HandleOf(nil); !errors.Is(err, arena.ErrClosed) {
		t.Fatal(err)
	}
	if err := zero.Free(nil); !errors.Is(err, arena.ErrClosed) {
		t.Fatal(err)
	}
	if err := zero.FreeHandle(arena.PoolHandle{}); !errors.Is(err, arena.ErrClosed) {
		t.Fatal(err)
	}
	if err := zero.Reset(); !errors.Is(err, arena.ErrClosed) {
		t.Fatal(err)
	}
	if err := zero.Close(); err != nil {
		t.Fatal(err)
	}
	if zero.Stats() != (arena.PoolStats{}) {
		t.Fatal("nonempty zero pool")
	}
}

func TestPoolConcurrentSlots(t *testing.T) {
	a := newArena(t, 8*64)
	p, err := a.MakePool[[8]uint64](8)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Go(func() {
			for range 500 {
				value, err := p.Alloc()
				if err != nil {
					t.Error(err)
					return
				}
				if *value != [8]uint64{} {
					t.Error("slot not zero")
					return
				}
				for i := range value {
					value[i] = uint64(worker + 1)
				}
				if err := p.Free(value); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	workers.Wait()
	if p.Stats().Active != 0 {
		t.Fatal("slot leak")
	}
	requireCheck(t, a)
}

func FuzzPool(f *testing.F) {
	f.Add([]byte{0, 1, 0, 2, 2, 1, 0, 3, 3, 0, 0, 4, 4, 0})
	f.Add([]byte{2, 0, 1, 255, 3, 0, 4, 1, 0, 2})
	f.Fuzz(func(t *testing.T, operations []byte) {
		a := newArena(t, 256)
		p, err := a.MakePool[[2]uint64](16)
		if err != nil {
			t.Fatal(err)
		}
		type entry struct {
			ptr    *[2]uint64
			handle arena.PoolHandle
			value  [2]uint64
		}
		var live []entry
		for i := 0; i+1 < min(len(operations), 1024); i += 2 {
			op, value := operations[i]%5, operations[i+1]
			switch op {
			case 0, 1:
				ptr, err := p.Alloc()
				if len(live) == 16 {
					requireError(t, err, arena.ErrOutOfMemory)
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if *ptr != [2]uint64{} {
					t.Fatal("slot not zero")
				}
				for _, e := range live {
					if e.ptr == ptr {
						t.Fatal("duplicate live address")
					}
				}
				*ptr = [2]uint64{uint64(value), uint64(i)}
				h, err := p.HandleOf(ptr)
				if err != nil {
					t.Fatal(err)
				}
				live = append(live, entry{ptr, h, *ptr})
			case 2:
				if len(live) == 0 {
					break
				}
				index := int(value) % len(live)
				e := live[index]
				if value%2 == 0 {
					err = p.Free(e.ptr)
				} else {
					err = p.FreeHandle(e.handle)
				}
				if err != nil {
					t.Fatal(err)
				}
				_, err = p.Get(e.handle)
				requireError(t, err, arena.ErrInvalidBuffer)
				live = append(live[:index], live[index+1:]...)
			case 3:
				if err := p.Reset(); err != nil {
					t.Fatal(err)
				}
				for _, e := range live {
					_, err := p.Get(e.handle)
					requireError(t, err, arena.ErrInvalidBuffer)
				}
				live = nil
			case 4:
				if err := a.Reset(); err != nil {
					t.Fatal(err)
				}
				_, err := p.Alloc()
				requireError(t, err, arena.ErrClosed)
				p, err = a.MakePool[[2]uint64](16)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range live {
					_, err := p.Get(e.handle)
					requireError(t, err, arena.ErrInvalidBuffer)
				}
				live = nil
			}
			for _, e := range live {
				ptr, err := p.Get(e.handle)
				if err != nil || ptr != e.ptr || *ptr != e.value {
					t.Fatalf("live slot changed: %v", err)
				}
			}
			if stats := p.Stats(); stats.Active != len(live) || stats.Free != 16-len(live) {
				t.Fatal("incorrect slot accounting")
			}
			requireCheck(t, a)
		}
	})
}
