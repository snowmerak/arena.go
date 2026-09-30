package arena

import (
	"errors"
	"testing"
)

func TestCheckDetectsCorruption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(*Arena)
	}{
		{"usage", func(a *Arena) { a.storage.(*byteBackend).used++ }},
		{"overlap", func(a *Arena) { a.storage.(*byteBackend).free = []span{{0, 4}} }},
		{"gap", func(a *Arena) { a.storage.(*byteBackend).head++ }},
		{"identity", func(a *Arena) { rec := a.live[location{}]; rec.buffer.owner++; a.live[location{}] = rec }},
		{"alignment", func(a *Arena) { rec := a.live[location{}]; rec.request.align = 3; a.live[location{}] = rec }},
		{"segment", func(a *Arena) { rec := a.live[location{}]; rec.buffer.segment++; a.live[location{}] = rec }},
		{"pointer", func(a *Arena) { rec := a.live[location{}]; rec.block.pointer = nil; a.live[location{}] = rec }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := New(64)
			_, _ = a.Alloc[uint64]()
			tc.corrupt(a)
			if err := a.Check(); err == nil {
				t.Fatal("checker accepted corrupt metadata")
			}
		})
	}
}

func TestAllocationIDExhaustion(t *testing.T) {
	a, _ := New(16)
	a.nextID = ^uint64(0)
	_, err := a.AllocBuffer(1)
	if !errors.Is(err, ErrIDExhausted) || a.Stats().Used != 0 {
		t.Fatalf("identifier wrap was not prevented: %v", err)
	}
}
