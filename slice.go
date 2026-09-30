package arena

import (
	"reflect"
	"unsafe"
)

// AllocSlice returns n aligned, zeroed elements in one tracked allocation.
// With New's byte storage, T must be pointer-free. Length and capacity are n.
// The elements share a lifetime: release the whole slice with FreeSlice or
// FreeBuffer, or reset the arena. Free cannot release individual elements.
// Empty slices and slices of zero-sized elements reserve one byte per batch.
func (a *Arena) AllocSlice[T any](n int) ([]T, error) {
	t := reflect.TypeFor[T]()
	maxInt := uintptr(int(^uint(0) >> 1))
	if n < 0 || (t.Size() != 0 && uintptr(n) > maxInt/t.Size()) {
		return nil, ErrInvalidSize
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isClosed() {
		return nil, ErrClosed
	}
	// Preserve the exact array type at the backend boundary. A future scanned
	// backend can allocate [n]T with its actual GC layout, without byte casts.
	array := reflect.ArrayOf(n, t)
	rec, err := a.allocate(allocationRequest{
		size: int(array.Size()), align: array.Align(), kind: sliceAllocation, typ: array,
	})
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*T)(rec.block.pointer), n), nil
}

// FreeSlice clears and releases a complete slice returned by AllocSlice.
// Pass its original length and capacity, not a subslice. Like Free, this cannot
// distinguish an old view from a new allocation of the same shape and address.
// Capture BufferOfSlice while live and use FreeBuffer for generation checking.
func (a *Arena) FreeSlice[T any](values []T) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.findSlice(unsafe.Pointer(unsafe.SliceData(values)), reflect.TypeFor[T](), len(values), cap(values))
	if err != nil {
		return err
	}
	a.release(rec)
	return nil
}

// BufferOfSlice captures the identity of a complete, currently live slice.
// The returned Buffer.Len is its byte size, not its element count.
func (a *Arena) BufferOfSlice[T any](values []T) (Buffer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.findSlice(unsafe.Pointer(unsafe.SliceData(values)), reflect.TypeFor[T](), len(values), cap(values))
	return rec.buffer, err
}

func (a *Arena) findSlice(p unsafe.Pointer, t reflect.Type, length, capacity int) (allocation, error) {
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
	if rec.request.kind != sliceAllocation || rec.request.typ.Elem() != t {
		return allocation{}, ErrTypeMismatch
	}
	if rec.request.typ.Len() != length || capacity != length {
		return allocation{}, ErrInvalidBuffer
	}
	return rec, nil
}
