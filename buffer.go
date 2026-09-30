package arena

// Buffer is a pointer-free allocation identity. Copies share ownership; freeing
// one invalidates all copies. It does not keep its Arena alive. Its zero value
// is invalid. A Buffer may itself be stored in arena memory.
type Buffer struct {
	owner uint64
	id    uint64
	location
	size int
}

// Len reports the requested size in bytes, even after the buffer is freed.
func (b Buffer) Len() int { return b.size }

// Segment reports the storage segment ID. New's byte backend uses segment 0.
func (b Buffer) Segment() uint64 { return b.segment }

// Offset reports the byte offset within the segment, for diagnostics.
func (b Buffer) Offset() int { return b.offset }

// AllocBuffer reserves zeroed bytes. Even a zero-length buffer reserves one byte.
func (a *Arena) AllocBuffer(size int) (Buffer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.allocate(allocationRequest{size: size, align: 1, kind: byteAllocation})
	return rec.buffer, err
}

// Bytes borrows a mutable view of a buffer created by AllocBuffer. Its capacity
// is restricted to its length. The view is valid only until FreeBuffer, Reset,
// or Close; the caller must prevent these operations during use of the view.
// Typed allocations and strings cannot be accessed through this method.
func (a *Arena) Bytes(b Buffer) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.lookup(b)
	if err != nil {
		return nil, err
	}
	if rec.request.kind != byteAllocation {
		return nil, ErrTypeMismatch
	}
	return blockBytes(rec), nil
}

// FreeBuffer releases any allocation using its identity. Unlike Free with a raw
// pointer, it detects stale handles even after the same address is reused.
func (a *Arena) FreeBuffer(b Buffer) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.lookup(b)
	if err != nil {
		return err
	}
	a.release(rec)
	return nil
}

// InUse reports whether this exact allocation is live in this arena. This is a
// snapshot, not a lease: it does not protect subsequent pointer or slice access.
func (a *Arena) InUse(b Buffer) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.lookup(b)
	return err == nil
}

func (a *Arena) lookup(b Buffer) (allocation, error) {
	if a.isClosed() {
		return allocation{}, ErrClosed
	}
	if b.owner != a.owner || b.id == 0 {
		return allocation{}, ErrInvalidBuffer
	}
	rec, ok := a.live[b.location]
	if !ok || rec.buffer != b {
		return allocation{}, ErrInvalidBuffer
	}
	return rec, nil
}
