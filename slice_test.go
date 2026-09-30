package arena_test

import (
	"errors"
	"testing"
	"unsafe"

	arena "github.com/snowmerak/arena.go"
)

func TestSliceLifecycle(t *testing.T) {
	a := newArena(t, 1024)
	prefix, err := a.AllocBuffer(1)
	if err != nil {
		t.Fatal(err)
	}
	values, err := a.AllocSlice[uint64](8)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 8 || cap(values) != 8 || uintptr(unsafe.Pointer(&values[0]))%unsafe.Alignof(values[0]) != 0 {
		t.Fatal("incorrect slice shape or alignment")
	}
	for i, v := range values {
		if v != 0 {
			t.Fatal("allocation was not zeroed")
		}
		values[i] = uint64(i + 1)
	}
	handle, err := a.BufferOfSlice(values)
	if err != nil || handle.Len() != 64 || a.Stats().Active != 2 {
		t.Fatalf("incorrect batch tracking: %v", err)
	}
	if _, err := a.Bytes(handle); !errors.Is(err, arena.ErrTypeMismatch) {
		t.Fatalf("exposed typed bytes: %v", err)
	}
	if err := a.Free(&values[0]); !errors.Is(err, arena.ErrTypeMismatch) {
		t.Fatalf("freed one element: %v", err)
	}
	if _, err := a.BufferOf(&values[0]); !errors.Is(err, arena.ErrTypeMismatch) {
		t.Fatalf("accepted element: %v", err)
	}
	for _, sub := range [][]uint64{values[:4], values[:4:4], values[1:], values[:0]} {
		if err := a.FreeSlice(sub); !errors.Is(err, arena.ErrInvalidBuffer) {
			t.Fatalf("accepted subslice: %v", err)
		}
	}
	wrong := unsafe.Slice((*int64)(unsafe.Pointer(&values[0])), len(values))
	if err := a.FreeSlice(wrong); !errors.Is(err, arena.ErrTypeMismatch) {
		t.Fatalf("accepted wrong type: %v", err)
	}
	if got := a.Allocations()[1]; got.Type != "[]uint64" || got.Buffer != handle {
		t.Fatalf("incorrect diagnostics: %+v", got)
	}
	if err := a.Check(); err != nil {
		t.Fatal(err)
	}
	if err := a.FreeSlice(values); err != nil {
		t.Fatal(err)
	}
	if err := a.FreeSlice(values); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("double free accepted: %v", err)
	}
	reused, err := a.AllocSlice[uint64](8)
	if err != nil || unsafe.SliceData(reused) != unsafe.SliceData(values) {
		t.Fatalf("space not reused: %v", err)
	}
	for _, v := range reused {
		if v != 0 {
			t.Fatal("reused storage was not zeroed")
		}
	}
	if a.InUse(handle) {
		t.Fatal("stale handle revived")
	}
	if err := a.FreeBuffer(handle); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("stale free accepted: %v", err)
	}
	current, err := a.BufferOfSlice(reused)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.FreeBuffer(current); err != nil {
		t.Fatal(err)
	}
	if err := a.FreeBuffer(prefix); err != nil {
		t.Fatal(err)
	}
	if stats := a.Stats(); stats.Active != 0 || stats.Used != 0 || stats.Head != 0 {
		t.Fatalf("leaked batch: %+v", stats)
	}
	if err := a.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestSliceInvalidRequests(t *testing.T) {
	a := newArena(t, 64)
	before := a.Stats()
	for _, count := range []int{-1, int(^uint(0) >> 1)} {
		if _, err := a.AllocSlice[uint64](count); !errors.Is(err, arena.ErrInvalidSize) {
			t.Fatalf("size %d: %v", count, err)
		}
	}
	if _, err := a.AllocSlice[uint64](9); !errors.Is(err, arena.ErrOutOfMemory) {
		t.Fatalf("oversize: %v", err)
	}
	for _, n := range []int{0, 1, 8} {
		if _, err := a.AllocSlice[*uint64](n); !errors.Is(err, arena.ErrPointerType) {
			t.Fatalf("pointer batch %d: %v", n, err)
		}
	}
	if a.Stats() != before {
		t.Fatal("failed requests consumed storage")
	}
	if err := a.FreeSlice[uint64](nil); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("nil: %v", err)
	}
	foreign := make([]uint64, 1)
	if err := a.FreeSlice(foreign); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("foreign slice: %v", err)
	}
	p, err := a.Alloc[uint64]()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.FreeSlice(unsafe.Slice(p, 1)); !errors.Is(err, arena.ErrTypeMismatch) {
		t.Fatalf("accepted object as slice: %v", err)
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	values, err := a.AllocSlice[uint64](8)
	if err != nil {
		t.Fatal(err)
	}
	b, err := a.BufferOfSlice(values)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocSlice[uint64](8); err != nil {
		t.Fatal(err)
	}
	if a.InUse(b) {
		t.Fatal("reset revived handle")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocSlice[uint64](0); !errors.Is(err, arena.ErrClosed) {
		t.Fatalf("closed allocation: %v", err)
	}
	if err := a.FreeSlice(values); !errors.Is(err, arena.ErrClosed) {
		t.Fatalf("closed free: %v", err)
	}
	if _, err := a.BufferOfSlice(values); !errors.Is(err, arena.ErrClosed) {
		t.Fatalf("closed lookup: %v", err)
	}
}

func TestSliceZeroSize(t *testing.T) {
	a := newArena(t, 8)
	empty, err := a.AllocSlice[uint64](0)
	if err != nil || empty == nil || len(empty) != 0 || cap(empty) != 0 {
		t.Fatalf("empty batch: %v", err)
	}
	first, err := a.AllocSlice[struct{}](100)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.AllocSlice[struct{}](100)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 100 || cap(first) != 100 || unsafe.SliceData(first) == unsafe.SliceData(second) {
		t.Fatal("zero-sized batches share identity")
	}
	if err := a.FreeSlice(first[:99:99]); !errors.Is(err, arena.ErrInvalidBuffer) {
		t.Fatalf("wrong zero-sized length accepted: %v", err)
	}
	if a.Stats().Used != 3 {
		t.Fatal("zero-sized batch should reserve one byte")
	}
	if err := a.Check(); err != nil {
		t.Fatal(err)
	}
	if err := a.FreeSlice(empty); err != nil {
		t.Fatal(err)
	}
	if err := a.FreeSlice(first); err != nil {
		t.Fatal(err)
	}
	if err := a.FreeSlice(second); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(); err != nil {
		t.Fatal(err)
	}
}
