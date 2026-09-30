// Package arena provides manually managed storage for pointer-free Go values.
//
// New creates an Arena with a fixed byte buffer. Bookkeeping is synchronized,
// but access through returned pointers or byte slices must be synchronized by
// the caller. Free, Reset, and Close invalidate borrowed pointers and slices.
// Arena values must not be copied; construct them with New.
package arena

import (
	"errors"
	"reflect"
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

type allocation struct {
	buffer  Buffer
	block   memoryBlock
	request allocationRequest
}

// Arena manages allocation identities and lifetimes over an owned storage
// backend. The zero value is closed. Do not copy an Arena.
type Arena struct {
	mu      sync.Mutex
	storage backend
	owner   uint64
	nextID  uint64
	live    map[location]allocation
	pools   map[location]*poolState
}

// New allocates fixed byte storage for pointer-free values. Capacity may be
// zero, but not negative. Runtime allocation failures may panic, as with make.
func New(capacity int) (*Arena, error) {
	if capacity < 0 {
		return nil, ErrInvalidSize
	}
	return newArena(&byteBackend{data: make([]byte, capacity)}), nil
}

// Future storage constructors compose their backend here. Keeping this boundary
// private avoids promising a public unsafe allocator interface prematurely.
func newArena(storage backend) *Arena {
	owner := arenaIDs.Add(1)
	if owner == 0 {
		panic("arena: arena identifiers exhausted")
	}
	return &Arena{storage: storage, owner: owner, live: make(map[location]allocation)}
}

// Alloc returns an aligned, zeroed T. With New's byte storage, T must contain no
// pointers, strings, slices, maps, interfaces, functions, or channels, recursively.
// Zero-sized types reserve one byte so live allocations have distinct addresses.
func (a *Arena) Alloc[T any]() (*T, error) {
	t := reflect.TypeFor[T]()
	if t.Size() > uintptr(int(^uint(0)>>1)) {
		return nil, ErrInvalidSize
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.allocate(allocationRequest{size: int(t.Size()), align: t.Align(), kind: objectAllocation, typ: t})
	if err != nil {
		return nil, err
	}
	return (*T)(rec.block.pointer), nil
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

func (a *Arena) isClosed() bool { return a.storage == nil }

// Unexported state operations require a.mu. The backend owns physical storage;
// Arena owns identity, exact-type validation, and the list of live allocations.
func (a *Arena) findObject(p unsafe.Pointer, t reflect.Type) (allocation, error) {
	if a.isClosed() {
		return allocation{}, ErrClosed
	}
	where, ok := a.storage.locate(p)
	if !ok {
		return allocation{}, ErrInvalidBuffer
	}
	rec, ok := a.live[where]
	if !ok || rec.block.pointer != p {
		return allocation{}, ErrInvalidBuffer
	}
	if rec.request.kind != objectAllocation || rec.request.typ != t {
		return allocation{}, ErrTypeMismatch
	}
	return rec, nil
}

func (a *Arena) allocate(request allocationRequest) (allocation, error) {
	if a.isClosed() {
		return allocation{}, ErrClosed
	}
	if request.size < 0 {
		return allocation{}, ErrInvalidSize
	}
	if a.nextID == ^uint64(0) {
		return allocation{}, ErrIDExhausted
	}
	block, err := a.storage.allocate(request)
	if err != nil {
		return allocation{}, err
	}
	a.nextID++
	rec := allocation{
		buffer:  Buffer{owner: a.owner, id: a.nextID, location: block.location, size: request.size},
		block:   block,
		request: request,
	}
	a.live[block.location] = rec
	return rec, nil
}

func (a *Arena) release(rec allocation) {
	if rec.request.kind == poolAllocation {
		a.pools[rec.block.location].invalidate()
		delete(a.pools, rec.block.location)
	}
	a.storage.release(rec.block, rec.request)
	delete(a.live, rec.block.location)
}

// Reset invalidates every allocation and reuses storage. Existing pointers and
// slices must no longer be accessed. IDs are never reset. The storage backend
// is responsible for dropping any outgoing Go references before reusing slots.
func (a *Arena) Reset() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isClosed() {
		return ErrClosed
	}
	a.storage.reset()
	for _, pool := range a.pools {
		pool.invalidate()
	}
	clear(a.pools)
	clear(a.live)
	return nil
}

// Close invalidates all allocations and releases the arena's storage references.
// It is idempotent. The GC can reclaim storage once borrowed pointers and slices
// are also unreachable; Close does not forcibly return memory to the OS.
func (a *Arena) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isClosed() {
		return nil
	}
	a.storage.close()
	for _, pool := range a.pools {
		pool.invalidate()
	}
	a.pools = nil
	a.storage, a.live = nil, nil
	return nil
}
