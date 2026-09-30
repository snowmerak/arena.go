package arena

import (
	"reflect"
	"unsafe"
)

type allocationKind uint8

const (
	objectAllocation allocationKind = iota
	byteAllocation
	stringAllocation
)

// location identifies storage, independently of allocation generation. Different
// segments may use the same offset. The default byte backend has segment 0.
type location struct {
	segment uint64
	offset  int
}

type allocationRequest struct {
	size  int
	align int
	kind  allocationKind
	typ   reflect.Type // Exact type for objects; nil for bytes and strings.
}

type memoryBlock struct {
	location location
	pointer  unsafe.Pointer // A real GC root, never a pointer reconstructed from uintptr.
}

// backend is the internal storage extension boundary. Arena serializes every
// call and owns the backend exclusively. Methods must not call back into Arena.
//
// allocate must either return an aligned, zeroed, stable block of at least
// max(size, 1) bytes, or fail without consuming storage. Live blocks must have
// distinct locations and addresses. Pointer-bearing types require typed storage
// that the GC scans; a cast does not qualify. Retain all storage roots here.
//
// release receives the exact request used to allocate the block. release, reset,
// and close cannot fail and must remove outgoing references using typed writes
// when necessary. reset invalidates all blocks but may retain empty capacity.
//
// locate translates a pointer to a storage location; Arena also checks the exact
// allocation start and type. stats excludes metadata and leaves Active to Arena.
// check must reconcile physical storage with the read-only live allocation map,
// including layout, pointers, supported types, free space, and usage counters.
type backend interface {
	allocate(allocationRequest) (memoryBlock, error)
	locate(unsafe.Pointer) (location, bool)
	release(memoryBlock, allocationRequest)
	reset()
	close()
	stats() Stats
	check(map[location]allocation) error
}

// blockBytes is only for byte/string requests, after kind and lifetime checks.
// Object storage must be read, written, and cleared using its actual Go type.
func blockBytes(rec allocation) []byte {
	return unsafe.Slice((*byte)(rec.block.pointer), rec.buffer.size)
}
