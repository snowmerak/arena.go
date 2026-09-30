package arena

import (
	"errors"
	"runtime"
	"testing"
)

func TestPoolTypedBackend(t *testing.T) {
	storage := new(heapBackend)
	a := newArena(storage)
	t.Cleanup(func() { _ = a.Close() })
	p, err := a.MakePool[backendNode](4)
	if err != nil {
		t.Fatal(err)
	}
	value, err := p.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	value.Child = new([1024]byte)
	value.Child[0] = 42
	runtime.GC()
	if value.Child[0] != 42 {
		t.Fatal("typed slot lost reference")
	}
	if err := p.Free(value); err != nil {
		t.Fatal(err)
	}
	if !storage.slots[0].value.Elem().IsZero() {
		t.Fatal("Free retained typed reference")
	}
	value, err = p.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	value.Child = new([1024]byte)
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	}
	if !storage.slots[0].value.Elem().IsZero() {
		t.Fatal("Reset retained typed reference")
	}
	value, err = p.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	value.Child = new([1024]byte)
	if err := a.Check(); err != nil {
		t.Fatal(err)
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	if !storage.slots[0].value.Elem().IsZero() || p.state.base != nil || p.state.slots != nil {
		t.Fatal("arena reset retained roots")
	}
}

func TestPoolGenerationExhaustion(t *testing.T) {
	a, _ := New(16)
	t.Cleanup(func() { _ = a.Close() })
	p, err := a.MakePool[uint64](2)
	if err != nil {
		t.Fatal(err)
	}
	p.state.serial = slotActive - 2
	value, err := p.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Free(value); err != nil {
		t.Fatal(err)
	}
	before := p.Stats()
	if _, err := p.Alloc(); !errors.Is(err, ErrIDExhausted) {
		t.Fatalf("generation wrapped: %v", err)
	}
	if p.Stats() != before {
		t.Fatal("failed allocation consumed slot")
	}
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Alloc(); !errors.Is(err, ErrIDExhausted) {
		t.Fatalf("reset revived generations: %v", err)
	}
	if err := a.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestPoolChecker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(*poolState)
	}{
		{"usage", func(s *poolState) { s.active++ }},
		{"cycle", func(s *poolState) { s.slots[0] = 1 }},
		{"range", func(s *poolState) { s.freeHead = 100 }},
		{"lost", func(s *poolState) { s.freeHead = 0 }},
		{"unissued", func(s *poolState) { s.slots[3] = slotActive | 1 }},
		{"generation", func(s *poolState) { s.slots[1] = slotActive }},
		{"stride", func(s *poolState) { s.stride++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := New(32)
			t.Cleanup(func() { _ = a.Close() })
			p, err := a.MakePool[uint64](4)
			if err != nil {
				t.Fatal(err)
			}
			value, err := p.Alloc()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Alloc(); err != nil {
				t.Fatal(err)
			}
			if err := p.Free(value); err != nil {
				t.Fatal(err)
			}
			tc.corrupt(p.state)
			if err := a.Check(); err == nil {
				t.Fatal("accepted pool corruption")
			}
		})
	}
}
