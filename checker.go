package arena

import (
	"fmt"
	"slices"
	"unsafe"
)

// Stats describes backing storage only; heap-allocated metadata is excluded.
type Stats struct {
	Capacity         int
	Used             int // Reserved bytes, including the byte reserved for zero-sized values.
	Free             int // Includes alignment gaps and the unallocated tail.
	Active           int
	Head             int // End of the occupied prefix, including holes.
	FreeBlocks       int
	LargestFreeBlock int // Before accounting for the next allocation's alignment.
}

// Stats returns a synchronized snapshot of storage usage.
func (a *Arena) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Stats{Capacity: len(a.data), Used: a.used, Free: len(a.data) - a.used,
		Active: len(a.live), Head: a.head, FreeBlocks: len(a.free), LargestFreeBlock: len(a.data) - a.head}
	if a.head < len(a.data) {
		s.FreeBlocks++
	}
	for _, hole := range a.free {
		s.LargestFreeBlock = max(s.LargestFreeBlock, hole.end-hole.start)
	}
	return s
}

// Allocation describes a live allocation for leak and usage diagnostics.
type Allocation struct {
	Buffer    Buffer
	Type      string // Go type name, "[]byte", or "arena.String".
	Alignment int
}

// Allocations returns the currently live allocations in address order.
func (a *Arena) Allocations() []Allocation {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]Allocation, 0, len(a.live))
	for _, rec := range a.live {
		name := "[]byte"
		switch rec.kind {
		case objectAllocation:
			name = rec.typ.String()
		case stringAllocation:
			name = "arena.String"
		}
		result = append(result, Allocation{Buffer: rec.buffer, Type: name, Alignment: rec.align})
	}
	slices.SortFunc(result, func(x, y Allocation) int { return x.Buffer.offset - y.Buffer.offset })
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
	if a.head < 0 || a.head > len(a.data) || a.used < 0 || a.used > a.head {
		return fmt.Errorf("arena: invalid head or usage")
	}
	regions := make([]span, 0, len(a.live)+len(a.free))
	used := 0
	ids := make(map[uint64]bool, len(a.live))
	for offset, rec := range a.live {
		b := rec.buffer
		reserved := max(b.size, 1)
		if b.owner != a.owner || b.id == 0 || b.id > a.nextID || ids[b.id] || b.offset != offset {
			return fmt.Errorf("arena: invalid allocation identity at %d", offset)
		}
		ids[b.id] = true
		if offset < 0 || offset >= a.head || b.size < 0 || reserved > a.head-offset {
			return fmt.Errorf("arena: allocation outside occupied storage at %d", offset)
		}
		if rec.align < 1 || rec.align&(rec.align-1) != 0 || uintptr(unsafe.Pointer(&a.data[offset]))%uintptr(rec.align) != 0 {
			return fmt.Errorf("arena: invalid alignment at %d", offset)
		}
		switch rec.kind {
		case objectAllocation:
			if rec.typ == nil || !isPointerFree(rec.typ) || rec.typ.Size() != uintptr(b.size) || rec.typ.Align() != rec.align {
				return fmt.Errorf("arena: invalid object type at %d", offset)
			}
		case byteAllocation, stringAllocation:
			if rec.typ != nil || rec.align != 1 {
				return fmt.Errorf("arena: invalid byte allocation at %d", offset)
			}
		default:
			return fmt.Errorf("arena: invalid allocation kind at %d", offset)
		}
		used += reserved
		regions = append(regions, span{offset, offset + reserved})
	}
	for i, hole := range a.free {
		if hole.start < 0 || hole.start >= hole.end || hole.end >= a.head || (i > 0 && a.free[i-1].end >= hole.start) {
			return fmt.Errorf("arena: invalid or unmerged free span at %d", hole.start)
		}
		regions = append(regions, hole)
	}
	slices.SortFunc(regions, func(x, y span) int { return x.start - y.start })
	end := 0
	for _, region := range regions {
		if region.start != end {
			return fmt.Errorf("arena: gap or overlap at %d", end)
		}
		end = region.end
	}
	if end != a.head || used != a.used {
		return fmt.Errorf("arena: inconsistent occupied storage or usage")
	}
	return nil
}
