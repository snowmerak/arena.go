package arena

import (
	"fmt"
	"slices"
	"unsafe"
)

type span struct{ start, end int }

// byteBackend owns bump allocation and hole reuse in a single noscan buffer.
type byteBackend struct {
	data []byte
	head int
	used int
	free []span // Sorted, disjoint holes strictly below head.
}

var _ backend = (*byteBackend)(nil)

func (s *byteBackend) allocate(request allocationRequest) (memoryBlock, error) {
	if request.typ != nil && !isPointerFree(request.typ) {
		return memoryBlock{}, fmt.Errorf("%w: %v", ErrPointerType, request.typ)
	}
	reserved := max(request.size, 1)
	offset := -1
	for i, hole := range s.free {
		start, ok := s.fit(hole, reserved, request.align)
		if !ok {
			continue
		}
		offset = start
		s.free = slices.Delete(s.free, i, i+1)
		s.addFree(span{hole.start, start})
		s.addFree(span{start + reserved, hole.end})
		break
	}
	if offset < 0 {
		start, ok := s.fit(span{s.head, len(s.data)}, reserved, request.align)
		if !ok {
			return memoryBlock{}, ErrOutOfMemory
		}
		previousHead := s.head
		s.head = start + reserved
		s.addFree(span{previousHead, start})
		offset = start
	}
	s.used += reserved
	clear(s.data[offset : offset+reserved])
	return memoryBlock{location: location{offset: offset}, pointer: unsafe.Pointer(&s.data[offset])}, nil
}

func (s *byteBackend) locate(p unsafe.Pointer) (location, bool) {
	if p == nil || len(s.data) == 0 {
		return location{}, false
	}
	base, address := uintptr(unsafe.Pointer(&s.data[0])), uintptr(p)
	if address < base || address-base >= uintptr(len(s.data)) {
		return location{}, false
	}
	return location{offset: int(address - base)}, true
}

func (s *byteBackend) fit(hole span, size, align int) (int, bool) {
	if size > hole.end-hole.start {
		return 0, false
	}
	address := uintptr(unsafe.Pointer(&s.data[hole.start]))
	padding := int((-address) & uintptr(align-1))
	// Subtraction avoids overflow for excessively large requests.
	if padding > hole.end-hole.start-size {
		return 0, false
	}
	return hole.start + padding, true
}

func (s *byteBackend) release(block memoryBlock, request allocationRequest) {
	reserved := max(request.size, 1)
	offset := block.location.offset
	clear(s.data[offset : offset+reserved])
	s.used -= reserved
	s.addFree(span{offset, offset + reserved})
}

func (s *byteBackend) addFree(hole span) {
	if hole.start == hole.end {
		return
	}
	i, _ := slices.BinarySearchFunc(s.free, hole.start, func(existing span, start int) int {
		return existing.start - start
	})
	s.free = slices.Insert(s.free, i, hole)
	if i > 0 && s.free[i-1].end == s.free[i].start {
		s.free[i-1].end = s.free[i].end
		s.free = slices.Delete(s.free, i, i+1)
		i--
	}
	if i+1 < len(s.free) && s.free[i].end == s.free[i+1].start {
		s.free[i].end = s.free[i+1].end
		s.free = slices.Delete(s.free, i+1, i+2)
	}
	if n := len(s.free); n > 0 && s.free[n-1].end == s.head {
		s.head = s.free[n-1].start
		s.free = s.free[:n-1]
	}
}

func (s *byteBackend) reset() {
	s.free = s.free[:0]
	s.head, s.used = 0, 0
	// No outgoing pointers exist; bytes are zeroed on the next allocation.
}

func (s *byteBackend) close() {
	s.data, s.free = nil, nil
	s.head, s.used = 0, 0
}

func (s *byteBackend) stats() Stats {
	result := Stats{Capacity: len(s.data), Used: s.used, Free: len(s.data) - s.used,
		Head: s.head, FreeBlocks: len(s.free), LargestFreeBlock: len(s.data) - s.head}
	if s.head < len(s.data) {
		result.FreeBlocks++
	}
	for _, hole := range s.free {
		result.LargestFreeBlock = max(result.LargestFreeBlock, hole.end-hole.start)
	}
	return result
}

func (s *byteBackend) check(live map[location]allocation) error {
	if s.head < 0 || s.head > len(s.data) || s.used < 0 || s.used > s.head {
		return fmt.Errorf("arena: invalid head or usage")
	}
	regions := make([]span, 0, len(live)+len(s.free))
	used := 0
	for where, rec := range live {
		reserved := max(rec.request.size, 1)
		if where.segment != 0 || where.offset < 0 || where.offset >= s.head || reserved > s.head-where.offset {
			return fmt.Errorf("arena: allocation outside byte storage at %v", where)
		}
		if rec.block.pointer != unsafe.Pointer(&s.data[where.offset]) {
			return fmt.Errorf("arena: wrong byte storage pointer at %v", where)
		}
		if rec.request.typ != nil && !isPointerFree(rec.request.typ) {
			return fmt.Errorf("arena: pointer-bearing type in byte storage at %v", where)
		}
		used += reserved
		regions = append(regions, span{where.offset, where.offset + reserved})
	}
	for i, hole := range s.free {
		if hole.start < 0 || hole.start >= hole.end || hole.end >= s.head || (i > 0 && s.free[i-1].end >= hole.start) {
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
	if end != s.head || used != s.used {
		return fmt.Errorf("arena: inconsistent occupied storage or usage")
	}
	return nil
}
