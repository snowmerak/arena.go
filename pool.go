package arena

import (
	"fmt"
	"reflect"
	"unsafe"
)

const slotActive = uint64(1) << 63

// Pool manages fixed-size slots for T in storage reserved from an Arena.
// Construct it with Arena.MakePool. The zero value is closed. Pool copies share
// state. Metadata operations use the owning arena's mutex; callers synchronize
// access through borrowed pointers with Free, Reset, Close, and arena lifetimes.
type Pool[T any] struct {
	arena *Arena
	state *poolState
}

// PoolHandle is a pointer-free slot identity. It does not keep a pool alive.
// Capture one with HandleOf while its pointer is live, before releasing it.
type PoolHandle struct {
	owner      uint64
	pool       uint64
	index      int
	generation uint64
}

// PoolStats counts slots, independently of the arena's backing allocations.
// A closed pool reports zero statistics.
type PoolStats struct {
	Capacity int
	Active   int
	Free     int
}

// A slot word is either a live generation with its high bit set, or a one-based
// free-list link. Zero terminates the list. Metadata has no Go pointers and uses
// eight bytes per slot; allocation never grows a map or slice.
type poolState struct {
	buffer   Buffer
	base     unsafe.Pointer
	stride   uintptr
	typ      reflect.Type
	slots    []uint64
	next     int // First slot never issued since construction or pool Reset.
	freeHead uint64
	active   int
	serial   uint64 // Never reset; at most slotActive-1.
}

// Give zero-sized T a distinct, aligned address per slot while retaining T in
// the backend's type information (including conservative pointer rejection).
type zeroPoolSlot[T any] struct {
	_ T
	_ byte
}

// MakePool reserves capacity zeroed slots in one arena allocation. Slots are
// initially free. Capacity is fixed; exhausted Alloc calls return ErrOutOfMemory.
// New's byte backend requires pointer-free T. Metadata is allocated on the Go
// heap. Even an empty pool reserves one byte; zero-sized T uses padded slots.
// Arena.Reset, Arena.Close, or FreeBuffer of the pool's backing allocation
// permanently closes this pool and invalidates all its pointers and handles.
func (a *Arena) MakePool[T any](capacity int) (*Pool[T], error) {
	t := reflect.TypeFor[T]()
	physical := t
	if t.Size() == 0 {
		physical = reflect.TypeFor[zeroPoolSlot[T]]()
	}
	maxInt := uintptr(int(^uint(0) >> 1))
	if capacity < 0 || uintptr(capacity) > maxInt/physical.Size() || uintptr(capacity) > maxInt/8 {
		return nil, ErrInvalidSize
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isClosed() {
		return nil, ErrClosed
	}
	array := reflect.ArrayOf(capacity, physical)
	rec, err := a.allocate(allocationRequest{
		size: int(array.Size()), align: array.Align(), kind: poolAllocation, typ: array,
	})
	if err != nil {
		return nil, err
	}
	s := &poolState{buffer: rec.buffer, base: rec.block.pointer, stride: physical.Size(),
		typ: t, slots: make([]uint64, capacity)}
	if a.pools == nil {
		a.pools = make(map[location]*poolState)
	}
	a.pools[rec.block.location] = s
	return &Pool[T]{arena: a, state: s}, nil
}

// Alloc returns one zeroed slot. It does no per-slot type or arena-map lookup.
// Storage is zeroed on construction, Free, and Reset, so reuse needs no clear.
func (p *Pool[T]) Alloc() (*T, error) {
	if p.arena == nil {
		return nil, ErrClosed
	}
	p.arena.mu.Lock()
	defer p.arena.mu.Unlock()
	s := p.state
	if s.base == nil {
		return nil, ErrClosed
	}
	if s.serial == slotActive-1 {
		return nil, ErrIDExhausted
	}
	index := s.next
	if s.freeHead != 0 {
		index = int(s.freeHead - 1)
		s.freeHead = s.slots[index]
	} else {
		if index == len(s.slots) {
			return nil, ErrOutOfMemory
		}
		s.next++
	}
	s.serial++
	s.slots[index] = slotActive | s.serial
	s.active++
	return (*T)(unsafe.Add(s.base, uintptr(index)*s.stride)), nil
}

// Free clears a live slot and returns it to this pool. It does not return the
// pool's backing space to the arena. Nil, foreign, interior, and already-free
// pointers are rejected. Raw pointers cannot detect reuse at the same address
// (ABA); use HandleOf and FreeHandle when generation checking is required.
func (p *Pool[T]) Free(value *T) error {
	if p.arena == nil {
		return ErrClosed
	}
	p.arena.mu.Lock()
	defer p.arena.mu.Unlock()
	index, err := p.state.find(unsafe.Pointer(value))
	if err != nil {
		return err
	}
	p.release(index)
	return nil
}

// HandleOf captures a currently live slot's identity. The pointer must still be
// valid; this cannot recover the generation of an already-stale raw pointer.
func (p *Pool[T]) HandleOf(value *T) (PoolHandle, error) {
	if p.arena == nil {
		return PoolHandle{}, ErrClosed
	}
	p.arena.mu.Lock()
	defer p.arena.mu.Unlock()
	index, err := p.state.find(unsafe.Pointer(value))
	if err != nil {
		return PoolHandle{}, err
	}
	s := p.state
	return PoolHandle{owner: s.buffer.owner, pool: s.buffer.id, index: index, generation: s.slots[index]}, nil
}

// Get validates a handle and borrows its slot. It is not a lease against Free.
func (p *Pool[T]) Get(handle PoolHandle) (*T, error) {
	if p.arena == nil {
		return nil, ErrClosed
	}
	p.arena.mu.Lock()
	defer p.arena.mu.Unlock()
	if err := p.state.lookup(handle); err != nil {
		return nil, err
	}
	return (*T)(unsafe.Add(p.state.base, uintptr(handle.index)*p.state.stride)), nil
}

// FreeHandle clears and releases a slot, rejecting stale or foreign identities.
func (p *Pool[T]) FreeHandle(handle PoolHandle) error {
	if p.arena == nil {
		return ErrClosed
	}
	p.arena.mu.Lock()
	defer p.arena.mu.Unlock()
	if err := p.state.lookup(handle); err != nil {
		return err
	}
	p.release(handle.index)
	return nil
}

// Reset clears all issued slots and invalidates their handles, retaining the
// pool for reuse. Unlike Arena.Reset, it does not close the pool.
func (p *Pool[T]) Reset() error {
	if p.arena == nil {
		return ErrClosed
	}
	p.arena.mu.Lock()
	defer p.arena.mu.Unlock()
	s := p.state
	if s.base == nil {
		return ErrClosed
	}
	// Nonzero T is contiguous. For zero-sized T this clears no bytes or roots.
	clear(unsafe.Slice((*T)(s.base), s.next))
	clear(s.slots[:s.next])
	s.next, s.active, s.freeHead = 0, 0, 0
	return nil
}

// Close invalidates all slots and returns the pool's backing space to its arena.
// It is idempotent. Borrowed pointers must no longer be used.
func (p *Pool[T]) Close() error {
	if p.arena == nil {
		return nil
	}
	p.arena.mu.Lock()
	defer p.arena.mu.Unlock()
	if p.state.base == nil {
		return nil
	}
	rec, err := p.arena.lookup(p.state.buffer)
	if err != nil {
		return err
	}
	p.arena.release(rec)
	return nil
}

// Stats returns a synchronized snapshot; it does not pin any slot's lifetime.
func (p *Pool[T]) Stats() PoolStats {
	if p.arena == nil {
		return PoolStats{}
	}
	p.arena.mu.Lock()
	defer p.arena.mu.Unlock()
	s := p.state
	return PoolStats{Capacity: len(s.slots), Active: s.active, Free: len(s.slots) - s.active}
}

// Caller holds the arena mutex. Typed clearing preserves the GC write barrier
// for a future backend that supports pointer-bearing T.
func (p *Pool[T]) release(index int) {
	s := p.state
	value := (*T)(unsafe.Add(s.base, uintptr(index)*s.stride))
	var zero T
	*value = zero
	s.slots[index] = s.freeHead
	s.freeHead = uint64(index + 1)
	s.active--
}

func (s *poolState) find(value unsafe.Pointer) (int, error) {
	if s.base == nil {
		return 0, ErrClosed
	}
	address, base := uintptr(value), uintptr(s.base)
	if address < base || address-base >= uintptr(s.next)*s.stride || (address-base)%s.stride != 0 {
		return 0, ErrInvalidBuffer
	}
	index := int((address - base) / s.stride)
	if s.slots[index]&slotActive == 0 {
		return 0, ErrInvalidBuffer
	}
	return index, nil
}

func (s *poolState) lookup(h PoolHandle) error {
	if s.base == nil {
		return ErrClosed
	}
	if h.owner != s.buffer.owner || h.pool != s.buffer.id || h.index < 0 || h.index >= s.next ||
		h.generation&slotActive == 0 || s.slots[h.index] != h.generation {
		return ErrInvalidBuffer
	}
	return nil
}

func (s *poolState) invalidate() {
	s.base, s.slots = nil, nil
	s.next, s.active, s.freeHead = 0, 0, 0
}

// check is diagnostic and may allocate. Hot-path operations do not traverse the
// slot list. The arena has already checked the backing array's physical layout.
func (s *poolState) check(rec allocation) error {
	if s.base != rec.block.pointer || s.buffer != rec.buffer || s.typ == nil ||
		s.stride != rec.request.typ.Elem().Size() || s.stride == 0 ||
		len(s.slots) != rec.request.typ.Len() || s.next < 0 || s.next > len(s.slots) ||
		s.serial >= slotActive || s.active < 0 || s.active > s.next {
		return fmt.Errorf("arena: invalid pool metadata")
	}
	free := make([]bool, s.next)
	for link := s.freeHead; link != 0; {
		if link > uint64(s.next) || free[link-1] || s.slots[link-1]&slotActive != 0 {
			return fmt.Errorf("arena: invalid pool free list")
		}
		free[link-1] = true
		link = s.slots[link-1]
	}
	active := 0
	for i, word := range s.slots {
		if i >= s.next {
			if word != 0 {
				return fmt.Errorf("arena: occupied unissued pool slot")
			}
			continue
		}
		if word&slotActive != 0 {
			id := word &^ slotActive
			if id == 0 || id > s.serial {
				return fmt.Errorf("arena: invalid pool generation")
			}
			active++
		} else if !free[i] {
			return fmt.Errorf("arena: lost pool slot")
		}
	}
	if active != s.active {
		return fmt.Errorf("arena: inconsistent pool usage")
	}
	return nil
}
