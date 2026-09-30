package arena

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"unsafe"
)

// heapBackend is a test-only backend with four independently allocated, typed
// slots. All slots use offset 0, so tests cannot accidentally rely on a single
// byte buffer. It is deliberately not a production allocator or slab pool.
type heapBackend struct {
	slots                    [4]heapSlot
	releases, resets, closes int
	checkErr                 error
}

type heapSlot struct {
	value   reflect.Value // Owns the actual typed heap allocation or byte slice.
	block   memoryBlock
	request allocationRequest
	active  bool
}

func (s *heapBackend) allocate(r allocationRequest) (memoryBlock, error) {
	for i := range s.slots {
		slot := &s.slots[i]
		if slot.active {
			continue
		}
		if !slot.value.IsValid() || slot.request != r {
			if r.kind == objectAllocation && r.size > 0 {
				slot.value = reflect.New(r.typ)
				slot.block.pointer = slot.value.UnsafePointer()
			} else {
				data := make([]byte, max(r.size, 1))
				slot.value = reflect.ValueOf(data)
				slot.block.pointer = unsafe.Pointer(&data[0])
			}
		}
		slot.block.location = location{segment: uint64(i + 1)}
		slot.request, slot.active = r, true
		return slot.block, nil
	}
	return memoryBlock{}, ErrOutOfMemory
}

func (s *heapBackend) locate(p unsafe.Pointer) (location, bool) {
	for _, slot := range s.slots {
		if slot.active && slot.block.pointer == p {
			return slot.block.location, true
		}
	}
	return location{}, false
}

func (s *heapBackend) release(block memoryBlock, r allocationRequest) {
	slot := &s.slots[block.location.segment-1]
	if slot.request != r {
		panic("Arena did not forward the exact allocation request")
	}
	if r.kind == objectAllocation && r.size > 0 {
		slot.value.Elem().SetZero() // A real typed write, including the GC barrier.
	} else {
		clear(slot.value.Bytes())
	}
	slot.active = false
	s.releases++
}

func (s *heapBackend) reset() {
	for _, slot := range s.slots {
		if slot.active {
			s.release(slot.block, slot.request)
		}
	}
	s.resets++
}

func (s *heapBackend) close() {
	s.reset()
	s.slots = [4]heapSlot{}
	s.closes++
}

func (s *heapBackend) stats() Stats {
	var result Stats
	for _, slot := range s.slots {
		if !slot.value.IsValid() {
			continue
		}
		size := max(slot.request.size, 1)
		result.Capacity += size
		if slot.active {
			result.Used += size
			result.Head += size
		} else {
			result.Free += size
			result.FreeBlocks++
			result.LargestFreeBlock = max(result.LargestFreeBlock, size)
		}
	}
	return result
}

func (s *heapBackend) check(live map[location]allocation) error {
	if s.checkErr != nil {
		return s.checkErr
	}
	active := 0
	for _, slot := range s.slots {
		if slot.active {
			active++
			rec, ok := live[slot.block.location]
			if !ok || rec.block != slot.block || rec.request != slot.request {
				return fmt.Errorf("test backend: live allocation mismatch")
			}
		}
	}
	if active != len(live) {
		return fmt.Errorf("test backend: unexpected allocations")
	}
	return nil
}

type backendNode struct {
	ID    uint64
	Child *[1024]byte
}

func TestBackendSegmentsAndTypedRelease(t *testing.T) {
	storage := new(heapBackend)
	a := newArena(storage)
	t.Cleanup(func() { _ = a.Close() })
	p, err := a.Alloc[backendNode]()
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.Alloc[backendNode]()
	if err != nil {
		t.Fatal(err)
	}
	p.Child = new([1024]byte)
	p.Child[0] = 42
	runtime.GC()
	if p.Child[0] != 42 {
		t.Fatal("typed backing storage lost its outgoing reference")
	}
	bp, err := a.BufferOf(p)
	if err != nil {
		t.Fatal(err)
	}
	bq, err := a.BufferOf(q)
	if err != nil || bp.Offset() != bq.Offset() || bp.Segment() == bq.Segment() {
		t.Fatalf("independent segments were conflated: %v, %v, %v", bp, bq, err)
	}
	forged := bp
	forged.segment = bq.segment
	if err := a.FreeBuffer(forged); !errors.Is(err, ErrInvalidBuffer) {
		t.Fatalf("cross-segment handle accepted: %v", err)
	}
	if err := a.Free(&p.ID); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("wrong type accepted: %v", err)
	}
	if _, err := a.Bytes(bp); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("typed storage exposed as bytes: %v", err)
	}
	if err := a.Check(); err != nil {
		t.Fatal(err)
	}
	list := a.Allocations()
	if len(list) != 2 || list[0].Buffer != bp || list[1].Buffer != bq || a.Stats().Active != 2 {
		t.Fatal("incorrect multi-segment diagnostics")
	}
	if err := a.Free(p); err != nil {
		t.Fatal(err)
	}
	if storage.releases != 1 || !storage.slots[0].value.Elem().IsZero() || !a.InUse(bq) {
		t.Fatal("typed release did not clear exactly the released slot")
	}
	reused, err := a.Alloc[backendNode]()
	if err != nil || reused != p {
		t.Fatalf("test backend did not reuse the slot: %v", err)
	}
	if err := a.FreeBuffer(bp); !errors.Is(err, ErrInvalidBuffer) {
		t.Fatalf("stale generation accepted after slot reuse: %v", err)
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	if storage.resets != 1 || !storage.slots[1].value.Elem().IsZero() || a.InUse(bq) || a.Stats().Active != 0 {
		t.Fatal("Reset did not clear typed storage and identities")
	}
	if err := a.Check(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Alloc[backendNode](); err != nil {
		t.Fatal(err)
	}
	if a.InUse(bp) || a.InUse(bq) {
		t.Fatal("Reset reused an old allocation identity")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if storage.closes != 1 || storage.slots[0].value.IsValid() || a.Stats() != (Stats{}) {
		t.Fatal("Close did not release storage exactly once")
	}
}

func TestBackendBytesStringsAndFailure(t *testing.T) {
	storage := new(heapBackend)
	a := newArena(storage)
	t.Cleanup(func() { _ = a.Close() })
	b, err := a.AllocBuffer(3)
	if err != nil {
		t.Fatal(err)
	}
	view, err := a.Bytes(b)
	if err != nil || len(view) != 3 || cap(view) != 3 {
		t.Fatal("byte view did not use backend storage")
	}
	copy(view, "abc")
	s, err := a.NewString("hello")
	if err != nil {
		t.Fatal(err)
	}
	text, err := a.String(s)
	if err != nil || text != "hello" {
		t.Fatal("string did not use backend storage")
	}
	if _, err := a.Alloc[uint64](); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Alloc[struct{}](); err != nil {
		t.Fatal(err)
	}
	before, id := a.Stats(), a.nextID
	if _, err := a.AllocBuffer(1); !errors.Is(err, ErrOutOfMemory) {
		t.Fatalf("backend failure was lost: %v", err)
	}
	if a.Stats() != before || a.nextID != id || !a.InUse(b) {
		t.Fatal("failed allocation changed common metadata")
	}
	if err := a.Check(); err != nil {
		t.Fatal(err)
	}
	storage.checkErr = errors.New("physical storage corruption")
	if err := a.Check(); !errors.Is(err, storage.checkErr) {
		t.Fatalf("backend checker failure was lost: %v", err)
	}
	storage.checkErr = nil
	if err := a.FreeString(s); err != nil {
		t.Fatal(err)
	}
	if err := a.FreeBuffer(b); err != nil {
		t.Fatal(err)
	}
	if text != "hello" || a.InUse(s.Buffer()) || a.InUse(b) {
		t.Fatal("invalid string copy or released identity")
	}
}
