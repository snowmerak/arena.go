package arena

// String is an immutable, pointer-free reference to bytes in an Arena. It may
// be embedded in types passed to Alloc. It does not implement fmt.Stringer:
// resolving its bytes requires the owning arena. Its zero value is invalid.
type String struct {
	buffer Buffer
}

// Len reports the byte length (not the rune count), without checking liveness.
func (s String) Len() int { return s.buffer.Len() }

// Buffer returns the identity for InUse or generation-checked FreeBuffer.
// Bytes refuses string allocations, preserving immutability through this API.
func (s String) Buffer() Buffer { return s.buffer }

// NewString copies value into arena storage. Empty strings reserve one byte.
func (a *Arena) NewString(value string) (String, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.allocate(allocationRequest{size: len(value), align: 1, kind: stringAllocation})
	if err != nil {
		return String{}, err
	}
	copy(blockBytes(rec), value)
	return String{buffer: rec.buffer}, nil
}

// String copies a live arena string into an ordinary Go string. The result is
// independent of the arena and remains valid after FreeString, Reset, or Close.
func (a *Arena) String(s String) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.lookup(s.buffer)
	if err != nil {
		return "", err
	}
	if rec.request.kind != stringAllocation {
		return "", ErrTypeMismatch
	}
	return string(blockBytes(rec)), nil
}

// FreeString releases the string's bytes and invalidates every copy of s.
// Freeing an object that contains a String does not free the string itself.
func (a *Arena) FreeString(s String) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, err := a.lookup(s.buffer)
	if err != nil {
		return err
	}
	if rec.request.kind != stringAllocation {
		return ErrTypeMismatch
	}
	a.release(rec)
	return nil
}
