package arena

import (
	"cmp"
	"fmt"
	"slices"
)

// Stats describes backing storage only; heap-allocated metadata is excluded.
type Stats struct {
	Capacity         int
	Used             int // Reserved bytes, including the byte reserved for zero-sized values.
	Free             int // Includes alignment gaps and the unallocated tail.
	Active           int
	Head             int // Sum of occupied prefix lengths across storage segments.
	FreeBlocks       int
	LargestFreeBlock int // Before accounting for the next allocation's alignment.
}

// Stats returns a synchronized snapshot of storage usage.
func (a *Arena) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isClosed() {
		return Stats{}
	}
	s := a.storage.stats()
	s.Active = len(a.live)
	return s
}

// Allocation describes a live allocation for leak and usage diagnostics.
type Allocation struct {
	Buffer    Buffer
	Type      string // Go type name, "[]byte", or "arena.String".
	Alignment int
}

// Allocations returns live allocations ordered by segment and then offset.
// With New's single byte segment, this is also address order.
func (a *Arena) Allocations() []Allocation {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]Allocation, 0, len(a.live))
	for _, rec := range a.live {
		name := "[]byte"
		switch rec.request.kind {
		case objectAllocation:
			name = rec.request.typ.String()
		case stringAllocation:
			name = "arena.String"
		}
		result = append(result, Allocation{Buffer: rec.buffer, Type: name, Alignment: rec.request.align})
	}
	slices.SortFunc(result, func(x, y Allocation) int {
		if order := cmp.Compare(x.Buffer.segment, y.Buffer.segment); order != 0 {
			return order
		}
		return cmp.Compare(x.Buffer.offset, y.Buffer.offset)
	})
	return result
}

// Check verifies the allocator's partition, alignment, identities, and counters.
// It does not detect out-of-bounds writes, arbitrary unsafe memory corruption,
// or dereferences of stale raw pointers and slices.
func (a *Arena) Check() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isClosed() {
		return ErrClosed
	}
	ids := make(map[uint64]bool, len(a.live))
	for where, rec := range a.live {
		b := rec.buffer
		r := rec.request
		if b.owner != a.owner || b.id == 0 || b.id > a.nextID || ids[b.id] || b.location != where || rec.block.location != where {
			return fmt.Errorf("arena: invalid allocation identity at %v", where)
		}
		ids[b.id] = true
		if b.size < 0 || b.size != r.size || rec.block.pointer == nil {
			return fmt.Errorf("arena: invalid allocation size or pointer at %v", where)
		}
		if r.align < 1 || r.align&(r.align-1) != 0 || uintptr(rec.block.pointer)%uintptr(r.align) != 0 {
			return fmt.Errorf("arena: invalid alignment at %v", where)
		}
		switch r.kind {
		case objectAllocation:
			if r.typ == nil || r.typ.Size() != uintptr(b.size) || r.typ.Align() != r.align {
				return fmt.Errorf("arena: invalid object type at %v", where)
			}
		case byteAllocation, stringAllocation:
			if r.typ != nil || r.align != 1 {
				return fmt.Errorf("arena: invalid byte allocation at %v", where)
			}
		default:
			return fmt.Errorf("arena: invalid allocation kind at %v", where)
		}
	}
	return a.storage.check(a.live)
}
