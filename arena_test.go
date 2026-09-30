package arena_test

import (
	"errors"
	"math"
	"runtime"
	"sync"
	"testing"
	"unsafe"

	arena "github.com/snowmerak/arena.go"
)

func newArena(t testing.TB, capacity int) *arena.Arena {
	t.Helper()
	a, err := arena.New(capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func requireError(t testing.TB, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want %v", got, want)
	}
}

func requireCheck(t testing.TB, a *arena.Arena) {
	t.Helper()
	if err := a.Check(); err != nil {
		t.Fatal(err)
	}
}

type record struct {
	ID       uint64
	Position [3]float64
	Name     arena.String
	Data     arena.Buffer
}

func TestTypedAllocation(t *testing.T) {
	a := newArena(t, 1024)
	_, err := a.AllocBuffer(1) // Exercise alignment after an odd offset.
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Alloc[record]()
	if err != nil {
		t.Fatal(err)
	}
	if *p != (record{}) || uintptr(unsafe.Pointer(p))%unsafe.Alignof(*p) != 0 {
		t.Fatal("allocation is not zeroed and aligned")
	}
	p.ID, p.Position[1] = 42, 3.5
	handle, err := a.BufferOf(p)
	if err != nil || !a.InUse(handle) {
		t.Fatalf("tracking allocation: %v", err)
	}
	if handle.Len() != int(unsafe.Sizeof(*p)) {
		t.Fatal("incorrect requested size")
	}
	runtime.GC()
	if p.ID != 42 || p.Position[1] != 3.5 {
		t.Fatal("values changed across GC")
	}
	if err := a.Free(p); err != nil { // T is inferred on the generic method.
		t.Fatal(err)
	}
	if a.InUse(handle) {
		t.Fatal("freed allocation remains live")
	}
	requireError(t, a.Free(p), arena.ErrInvalidBuffer)
	q, err := a.Alloc[record]()
	if err != nil || *q != (record{}) {
		t.Fatalf("reallocation is not zeroed: %v", err)
	}
	if q != p {
		t.Fatal("released space was not reused")
	}
	requireError(t, a.FreeBuffer(handle), arena.ErrInvalidBuffer)
	requireCheck(t, a)
}

func rejectType[T any](t *testing.T, a *arena.Arena) {
	t.Helper()
	_, err := a.Alloc[T]()
	requireError(t, err, arena.ErrPointerType)
}

func TestPointerTypesRejected(t *testing.T) {
	a := newArena(t, 1024)
	rejectType[*int](t, a)
	rejectType[unsafe.Pointer](t, a)
	rejectType[string](t, a)
	rejectType[[]byte](t, a)
	rejectType[map[int]int](t, a)
	rejectType[any](t, a)
	rejectType[func()](t, a)
	rejectType[chan int](t, a)
	rejectType[struct{ hidden [2]struct{ p *int } }](t, a)
	rejectType[[0]*int](t, a) // Deliberately conservative for zero-length arrays.
	if a.Stats().Active != 0 {
		t.Fatal("rejected types consumed storage")
	}
	requireCheck(t, a)
}

func TestCoalescingAndTailReclamation(t *testing.T) {
	a := newArena(t, 32)
	blocks := make([]arena.Buffer, 4)
	for i := range blocks {
		var err error
		blocks[i], err = a.AllocBuffer(8)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := a.AllocBuffer(1)
	requireError(t, err, arena.ErrOutOfMemory)
	for _, i := range []int{1, 2} {
		if err := a.FreeBuffer(blocks[i]); err != nil {
			t.Fatal(err)
		}
		requireCheck(t, a)
	}
	if s := a.Stats(); s.Free != 16 || s.LargestFreeBlock != 16 || s.Head != 32 {
		t.Fatalf("unexpected fragmented stats: %+v", s)
	}
	joined, err := a.AllocBuffer(16)
	if err != nil || joined.Offset() != 8 {
		t.Fatalf("adjacent holes were not merged: %v, %v", joined, err)
	}
	for _, b := range []arena.Buffer{blocks[0], blocks[3], joined} {
		if err := a.FreeBuffer(b); err != nil {
			t.Fatal(err)
		}
		requireCheck(t, a)
	}
	if s := a.Stats(); s.Head != 0 || s.Used != 0 || s.Active != 0 || s.LargestFreeBlock != 32 {
		t.Fatalf("arena not fully reclaimed: %+v", s)
	}
}

func TestFragmentationAndHoleSplitting(t *testing.T) {
	a := newArena(t, 32)
	var blocks [4]arena.Buffer
	for i := range blocks {
		blocks[i], _ = a.AllocBuffer(8)
	}
	for _, i := range []int{0, 2} {
		if err := a.FreeBuffer(blocks[i]); err != nil {
			t.Fatal(err)
		}
	}
	before := a.Stats()
	_, err := a.AllocBuffer(9)
	requireError(t, err, arena.ErrOutOfMemory)
	if a.Stats() != before {
		t.Fatal("failed allocation changed storage")
	}
	x, err := a.AllocBuffer(3)
	if err != nil || x.Offset() != 0 {
		t.Fatalf("first-fit allocation: %v, %v", x, err)
	}
	y, err := a.AllocBuffer(5)
	if err != nil || y.Offset() != 3 {
		t.Fatalf("split hole allocation: %v, %v", y, err)
	}
	requireCheck(t, a)
}

func TestBufferIdentityAndBounds(t *testing.T) {
	a, other := newArena(t, 64), newArena(t, 64)
	b, _ := a.AllocBuffer(8)
	view, err := a.Bytes(b)
	if err != nil || len(view) != 8 || cap(view) != 8 {
		t.Fatalf("incorrect view: len %d cap %d err %v", len(view), cap(view), err)
	}
	copy(view, "12345678")
	requireError(t, other.FreeBuffer(b), arena.ErrInvalidBuffer)
	if other.InUse(b) || a.InUse(arena.Buffer{}) {
		t.Fatal("invalid handle accepted")
	}
	if err := a.FreeBuffer(b); err != nil {
		t.Fatal(err)
	}
	reused, _ := a.AllocBuffer(8)
	if reused == b || reused.Offset() != b.Offset() || a.InUse(b) {
		t.Fatal("allocation identity did not change on reuse")
	}
	_, err = a.Bytes(b)
	requireError(t, err, arena.ErrInvalidBuffer)
	view, _ = a.Bytes(reused)
	for _, v := range view {
		if v != 0 {
			t.Fatal("reused bytes not zeroed")
		}
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	afterReset, _ := a.AllocBuffer(8)
	if a.InUse(reused) || afterReset == reused {
		t.Fatal("Reset reused allocation identities")
	}
	requireError(t, a.FreeBuffer(reused), arena.ErrInvalidBuffer)
	requireCheck(t, a)
}

func TestFreeRejectsInvalidPointers(t *testing.T) {
	type pair struct{ A, B uint64 }
	a, other := newArena(t, 64), newArena(t, 64)
	p, _ := a.Alloc[pair]()
	requireError(t, a.Free[pair](nil), arena.ErrInvalidBuffer)
	requireError(t, a.Free(new(pair)), arena.ErrInvalidBuffer)
	requireError(t, other.Free(p), arena.ErrInvalidBuffer)
	requireError(t, a.Free(&p.B), arena.ErrInvalidBuffer)
	requireError(t, a.Free(&p.A), arena.ErrTypeMismatch)
	b, _ := a.BufferOf(p)
	_, err := a.Bytes(b)
	requireError(t, err, arena.ErrTypeMismatch)
	if err := a.FreeBuffer(b); err != nil {
		t.Fatal(err)
	}
	requireCheck(t, a)
}

func TestStringInArenaObject(t *testing.T) {
	a := newArena(t, 512)
	p, err := a.Alloc[record]()
	if err != nil {
		t.Fatal(err)
	}
	want := "안녕\x00arena 🌍"
	p.Name, err = a.NewString(want)
	if err != nil {
		t.Fatal(err)
	}
	name := p.Name
	if name.Len() != len(want) || !a.InUse(name.Buffer()) {
		t.Fatal("invalid string length or identity")
	}
	_, err = a.Bytes(name.Buffer())
	requireError(t, err, arena.ErrTypeMismatch)
	runtime.GC()
	got, err := a.String(p.Name)
	if err != nil || got != want {
		t.Fatalf("string = %q, %v", got, err)
	}
	if err := a.Free(p); err != nil {
		t.Fatal(err)
	}
	if !a.InUse(name.Buffer()) {
		t.Fatal("freeing containing object freed independent string")
	}
	if err := a.FreeString(name); err != nil {
		t.Fatal(err)
	}
	requireError(t, a.FreeString(name), arena.ErrInvalidBuffer)
	_, err = a.String(name)
	requireError(t, err, arena.ErrInvalidBuffer)
	_, _ = a.NewString("overwrite the old storage")
	if got != want {
		t.Fatal("ordinary string aliases arena storage")
	}
	empty, err := a.NewString("")
	if err != nil {
		t.Fatal(err)
	}
	if text, err := a.String(empty); text != "" || err != nil {
		t.Fatalf("empty string = %q, %v", text, err)
	}
	_, err = a.String(arena.String{})
	requireError(t, err, arena.ErrInvalidBuffer)
	requireCheck(t, a)
}

func TestZeroSizeAndCapacity(t *testing.T) {
	_, err := arena.New(-1)
	requireError(t, err, arena.ErrInvalidSize)
	empty := newArena(t, 0)
	_, err = empty.Alloc[struct{}]()
	requireError(t, err, arena.ErrOutOfMemory)
	requireCheck(t, empty)
	a := newArena(t, 2)
	p, err := a.Alloc[struct{}]()
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.Alloc[struct{}]()
	if err != nil {
		t.Fatal(err)
	}
	bp, _ := a.BufferOf(p)
	bq, _ := a.BufferOf(q)
	if bp.Offset() == bq.Offset() || a.Stats().Used != 2 {
		t.Fatal("zero-sized objects need distinct reserved bytes")
	}
	_, err = a.AllocBuffer(-1)
	requireError(t, err, arena.ErrInvalidSize)
	_, err = a.AllocBuffer(math.MaxInt)
	requireError(t, err, arena.ErrOutOfMemory)
	if err := a.Free(p); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(q); err != nil {
		t.Fatal(err)
	}
	b, err := a.AllocBuffer(0)
	if err != nil {
		t.Fatal(err)
	}
	view, err := a.Bytes(b)
	if err != nil || len(view) != 0 || cap(view) != 0 || a.Stats().Used != 1 {
		t.Fatal("invalid zero-length buffer")
	}
	requireCheck(t, a)
}

func TestCloseAndZeroValue(t *testing.T) {
	a := newArena(t, 32)
	b, _ := a.AllocBuffer(4)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if a.InUse(b) || a.Stats().Capacity != 0 || len(a.Allocations()) != 0 {
		t.Fatal("closed arena retains allocations")
	}
	_, err := a.Alloc[int]()
	requireError(t, err, arena.ErrClosed)
	requireError(t, a.Reset(), arena.ErrClosed)
	requireError(t, a.Check(), arena.ErrClosed)
	requireError(t, a.FreeBuffer(b), arena.ErrClosed)
	var zero arena.Arena
	_, err = zero.AllocBuffer(1)
	requireError(t, err, arena.ErrClosed)
	if err := zero.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAllocationSnapshot(t *testing.T) {
	a := newArena(t, 128)
	b, _ := a.AllocBuffer(3)
	p, _ := a.Alloc[uint64]()
	s, _ := a.NewString("test")
	pb, _ := a.BufferOf(p)
	list := a.Allocations()
	if len(list) != 3 {
		t.Fatalf("unexpected allocations: %+v", list)
	}
	want := map[arena.Buffer]string{b: "[]byte", pb: "uint64", s.Buffer(): "arena.String"}
	for i, allocation := range list {
		if allocation.Type != want[allocation.Buffer] || (i > 0 && list[i-1].Buffer.Offset() >= allocation.Buffer.Offset()) {
			t.Fatalf("incorrect allocation or address order: %+v", list)
		}
	}
	if err := a.Free(p); err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || a.InUse(pb) {
		t.Fatal("snapshot or liveness is incorrect")
	}
	requireCheck(t, a)
}

func TestPointerRetainsBackingStorage(t *testing.T) {
	p := func() *[16]uint64 {
		a, err := arena.New(128)
		if err != nil {
			t.Fatal(err)
		}
		p, err := a.Alloc[[16]uint64]()
		if err != nil {
			t.Fatal(err)
		}
		p[15] = 99
		return p
	}()
	runtime.GC()
	if p[15] != 99 {
		t.Fatal("typed Go pointer did not retain backing storage")
	}
}

func TestConcurrentIndependentAllocations(t *testing.T) {
	a := newArena(t, 4096)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 200 {
				p, err := a.Alloc[uint64]()
				if err != nil {
					t.Error(err)
					return
				}
				*p = uint64(worker*200 + i)
				b, err := a.BufferOf(p)
				if err != nil || !a.InUse(b) {
					t.Errorf("missing live allocation: %v", err)
					return
				}
				if err := a.FreeBuffer(b); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	requireCheck(t, a)
	if a.Stats().Active != 0 {
		t.Fatal("concurrent allocations leaked")
	}
}

func FuzzArena(f *testing.F) {
	f.Add([]byte{5, 16, 5, 0, 1, 0, 3, 42, 5, 8, 2, 0})
	f.Add([]byte{0, 8, 0, 16, 1, 0, 3, 42, 4, 9, 2, 0})
	f.Add([]byte{0, 0, 1, 0, 1, 0, 0, 255, 3, 1})
	f.Fuzz(func(t *testing.T, operations []byte) {
		a := newArena(t, 512)
		type entry struct {
			buffer arena.Buffer
			ptr    *uint64
			batch  []uint64
			str    arena.String
			value  byte
			kind   byte
		}
		var live []entry
		for i := 0; i+1 < min(len(operations), 2048); i += 2 {
			op, value := operations[i]%6, operations[i+1]
			switch op {
			case 0, 3, 4, 5:
				e := entry{kind: op, value: value}
				var err error
				switch op {
				case 0:
					e.buffer, err = a.AllocBuffer(int(value))
					if err == nil {
						view, readErr := a.Bytes(e.buffer)
						if readErr != nil {
							t.Fatal(readErr)
						}
						for j := range view {
							if view[j] != 0 {
								t.Fatal("new buffer not zeroed")
							}
							view[j] = value
						}
					}
				case 3:
					e.ptr, err = a.Alloc[uint64]()
					if err == nil {
						if *e.ptr != 0 {
							t.Fatal("new object not zeroed")
						}
						*e.ptr = uint64(value)
						e.buffer, err = a.BufferOf(e.ptr)
					}
				case 4:
					e.str, err = a.NewString(string([]byte{value}))
					e.buffer = e.str.Buffer()
				case 5:
					e.batch, err = a.AllocSlice[uint64](int(value % 32))
					if err == nil {
						for j, got := range e.batch {
							if got != 0 {
								t.Fatal("new batch not zeroed")
							}
							e.batch[j] = uint64(value) + uint64(j)
						}
						e.buffer, err = a.BufferOfSlice(e.batch)
					}
				}
				if err == nil {
					live = append(live, e)
				} else {
					requireError(t, err, arena.ErrOutOfMemory)
				}
			case 1:
				if len(live) > 0 {
					index := int(value) % len(live)
					b := live[index].buffer
					var err error
					if live[index].kind == 5 {
						err = a.FreeSlice(live[index].batch)
					} else {
						err = a.FreeBuffer(b)
					}
					if err != nil {
						t.Fatal(err)
					}
					requireError(t, a.FreeBuffer(b), arena.ErrInvalidBuffer)
					live = append(live[:index], live[index+1:]...)
				}
			case 2:
				if err := a.Reset(); err != nil {
					t.Fatal(err)
				}
				for _, e := range live {
					if a.InUse(e.buffer) {
						t.Fatal("Reset retained a handle")
					}
				}
				live = live[:0]
			}
			requireCheck(t, a)
			used := 0
			for _, e := range live {
				if !a.InUse(e.buffer) {
					t.Fatal("lost live allocation")
				}
				used += max(e.buffer.Len(), 1)
				switch e.kind {
				case 0:
					view, err := a.Bytes(e.buffer)
					if err != nil {
						t.Fatal(err)
					}
					for _, got := range view {
						if got != e.value {
							t.Fatal("live buffer was overwritten")
						}
					}
				case 3:
					if *e.ptr != uint64(e.value) {
						t.Fatal("live object was overwritten")
					}
				case 4:
					got, err := a.String(e.str)
					if err != nil || got != string([]byte{e.value}) {
						t.Fatal("live string was overwritten")
					}
				case 5:
					for j, got := range e.batch {
						if got != uint64(e.value)+uint64(j) {
							t.Fatal("live batch was overwritten")
						}
					}
				}
			}
			if stats := a.Stats(); stats.Active != len(live) || stats.Used != used || stats.Free != 512-used {
				t.Fatalf("model disagrees with stats: %+v", stats)
			}
		}
	})
}
