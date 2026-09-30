// Package arena provides manually managed storage for pointer-free Go values.
//
// An Arena owns a fixed byte buffer. Its bookkeeping is synchronized, but access
// through returned pointers or byte slices must be synchronized by the caller.
// Free, Reset, and Close invalidate borrowed pointers and slices immediately.
// Arena values must not be copied; construct them with New.
package arena

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"
)

var (
	ErrInvalidSize   = errors.New("arena: invalid size")
	ErrOutOfMemory   = errors.New("arena: no contiguous space available")
	ErrPointerType   = errors.New("arena: type contains Go pointers")
	ErrInvalidBuffer = errors.New("arena: foreign, freed, or invalid allocation")
	ErrTypeMismatch  = errors.New("arena: allocation type mismatch")
	ErrClosed        = errors.New("arena: closed")
	ErrIDExhausted   = errors.New("arena: allocation identifiers exhausted")
)

var arenaIDs atomic.Uint64
var pointerFreeTypes sync.Map // reflect.Type -> bool

type span struct{ start, end int }

type allocationKind uint8

const (
	objectAllocation allocationKind = iota
	byteAllocation
	stringAllocation
)

type allocation struct {
	buffer Buffer
	align  int
	kind   allocationKind
	typ    reflect.Type
}

// Arena owns a fixed-capacity byte buffer and separate, GC-visible metadata.
// The zero value is closed. Do not copy an Arena, even before its first use.
type Arena struct {
	mu     sync.Mutex
	data   []byte
	owner  uint64
	nextID uint64
	head   int
	used   int
	free   []span // Sorted, disjoint holes strictly below head.
	live   map[int]allocation
	closed bool
}

// New allocates the backing storage. Capacity may be zero, but not negative.
// As with make, failure to obtain memory from the runtime can panic.
func New(capacity int) (*Arena, error) {
	if capacity < 0 {
		return nil, ErrInvalidSize
	}
	owner := arenaIDs.Add(1)
	if owner == 0 {
		panic("arena: arena identifiers exhausted")
	}
	return &Arena{data: make([]byte, capacity), owner: owner, live: make(map[int]allocation)}, nil
}

// Alloc returns an aligned, zeroed T in the arena. T must contain no pointers,
// strings, slices, maps, interfaces, functions, or channels, recursively.
// Zero-sized types reserve one byte so live allocations have distinct addresses.
func (a *Arena) Alloc[T any]() (*T, error) {
	t := reflect.TypeFor[T]()
	if !isPointerFree(t) {
		return nil, fmt.Errorf("%w: %v", ErrPointerType, t)
	}
	if t.Size() > uintptr(int(^uint(0)>>1)) {
		return nil, ErrInvalidSize
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b, err := a.allocate(int(t.Size()), t.Align(), objectAllocation, t)
	if err != nil {
		return nil, err
	}
	// Keep the conversion rooted in a real pointer, never a saved uintptr.
	return (*T)(unsafe.Pointer(&a.data[b.offset])), nil
}

// Free releases a value returned by Alloc. The pointer must refer to the start
// of a live allocation of exactly T. A raw pointer has no allocation ID: after
// address reuse, an old pointer is indistinguishable from the new one. Use
// BufferOf before freeing and FreeBuffer for generation-checked release.
func (a *Arena) Free[T any](p *T) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.findObject(unsafe.Pointer(p), reflect.TypeFor[T]())
	if err != nil {
		return err
	}
	a.release(rec)
	return nil
}

// BufferOf captures the allocation identity of a currently live typed pointer.
// Capture it while the pointer is valid, before Free or Reset can occur.
func (a *Arena) BufferOf[T any](p *T) (Buffer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.findObject(unsafe.Pointer(p), reflect.TypeFor[T]())
	return rec.buffer, err
}

func isPointerFree(t reflect.Type) bool {
	if cached, ok := pointerFreeTypes.Load(t); ok {
		return cached.(bool)
	}
	valid := true
	switch t.Kind() {
	case reflect.Pointer, reflect.UnsafePointer, reflect.String, reflect.Slice,
		reflect.Map, reflect.Interface, reflect.Func, reflect.Chan:
		valid = false
	case reflect.Array:
		valid = isPointerFree(t.Elem())
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if !isPointerFree(t.Field(i).Type) {
				valid = false
				break
			}
		}
	}
	pointerFreeTypes.Store(t, valid)
	return valid
}

func (a *Arena) isClosed() bool { return a.owner == 0 || a.closed }

func (a *Arena) findObject(p unsafe.Pointer, t reflect.Type) (allocation, error) {
	if a.isClosed() {
		return allocation{}, ErrClosed
	}
	if p == nil || len(a.data) == 0 {
		return allocation{}, ErrInvalidBuffer
	}
	base, address := uintptr(unsafe.Pointer(&a.data[0])), uintptr(p)
	if address < base || address-base >= uintptr(len(a.data)) {
		return allocation{}, ErrInvalidBuffer
	}
	rec, ok := a.live[int(address-base)]
	if !ok {
		return allocation{}, ErrInvalidBuffer
	}
	if rec.kind != objectAllocation || rec.typ != t {
		return allocation{}, ErrTypeMismatch
	}
	return rec, nil
}

// allocate and all other unexported state operations require a.mu.
func (a *Arena) allocate(size, align int, kind allocationKind, t reflect.Type) (Buffer, error) {
	if a.isClosed() {
		return Buffer{}, ErrClosed
	}
	if size < 0 {
		return Buffer{}, ErrInvalidSize
	}
	if a.nextID == ^uint64(0) {
		return Buffer{}, ErrIDExhausted
	}
	reserved := max(size, 1)
	offset := -1
	for i, hole := range a.free {
		start, ok := a.fit(hole, reserved, align)
		if !ok {
			continue
		}
		offset = start
		a.free = slices.Delete(a.free, i, i+1)
		a.addFree(span{hole.start, start})
		a.addFree(span{start + reserved, hole.end})
		break
	}
	if offset < 0 {
		start, ok := a.fit(span{a.head, len(a.data)}, reserved, align)
		if !ok {
			return Buffer{}, ErrOutOfMemory
		}
		previousHead := a.head
		a.head = start + reserved
		a.addFree(span{previousHead, start})
		offset = start
	}
	a.nextID++
	b := Buffer{owner: a.owner, id: a.nextID, offset: offset, size: size}
	a.live[offset] = allocation{buffer: b, align: align, kind: kind, typ: t}
	a.used += reserved
	clear(a.data[offset : offset+reserved])
	return b, nil
}

func (a *Arena) fit(s span, size, align int) (int, bool) {
	if size > s.end-s.start {
		return 0, false
	}
	address := uintptr(unsafe.Pointer(&a.data[s.start]))
	padding := int((-address) & uintptr(align-1))
	// Subtraction avoids overflow for excessively large requests.
	if padding > s.end-s.start-size {
		return 0, false
	}
	return s.start + padding, true
}

func (a *Arena) release(rec allocation) {
	b := rec.buffer
	reserved := max(b.size, 1)
	clear(a.data[b.offset : b.offset+reserved])
	delete(a.live, b.offset)
	a.used -= reserved
	a.addFree(span{b.offset, b.offset + reserved})
}

func (a *Arena) addFree(s span) {
	if s.start == s.end {
		return
	}
	i, _ := slices.BinarySearchFunc(a.free, s.start, func(hole span, start int) int {
		return hole.start - start
	})
	a.free = slices.Insert(a.free, i, s)
	if i > 0 && a.free[i-1].end == a.free[i].start {
		a.free[i-1].end = a.free[i].end
		a.free = slices.Delete(a.free, i, i+1)
		i--
	}
	if i+1 < len(a.free) && a.free[i].end == a.free[i+1].start {
		a.free[i].end = a.free[i+1].end
		a.free = slices.Delete(a.free, i+1, i+2)
	}
	if n := len(a.free); n > 0 && a.free[n-1].end == a.head {
		a.head = a.free[n-1].start
		a.free = a.free[:n-1]
	}
}

// Reset invalidates every allocation and reuses the backing storage. Existing
// pointers and slices must no longer be accessed. IDs are never reset.
// Storage is cleared on the next allocation, so Reset need not sweep the bytes.
func (a *Arena) Reset() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isClosed() {
		return ErrClosed
	}
	clear(a.live)
	a.free = a.free[:0]
	a.head, a.used = 0, 0
	return nil
}

// Close invalidates all allocations and drops the arena's backing reference.
// It is idempotent. The GC can reclaim storage once borrowed pointers and slices
// are also unreachable; Close does not forcibly return memory to the OS.
func (a *Arena) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.data, a.live, a.free = nil, nil, nil
	a.head, a.used = 0, 0
	return nil
}
