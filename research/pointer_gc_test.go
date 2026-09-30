//go:build arenaresearch

// These are opt-in GC observations, not production allocator implementations.
// The deliberately untraced pointers in negative cases are never dereferenced
// after GC. Collection timing is not a portable language guarantee.
package research_test

import (
	"runtime"
	"testing"
	"unsafe"
	"weak"
)

// Avoid tiny-allocation batching, which can keep otherwise dead weak targets
// alive alongside unrelated live objects.
type payload struct{ data [1024]byte }
type node struct{ child *payload }

//go:noinline
func rawNode() ([]byte, *node, weak.Pointer[payload]) {
	storage := make([]byte, unsafe.Sizeof(node{}))
	p := (*node)(unsafe.Pointer(&storage[0]))
	child := &payload{}
	child.data[0] = 42
	p.child = child
	return storage, p, weak.Make(child)
}

//go:noinline
func rootedRawNode() ([]byte, []any, weak.Pointer[payload]) {
	storage, p, observed := rawNode()
	return storage, []any{p.child}, observed
}

//go:noinline
func replacedRawNode() ([]byte, []any, weak.Pointer[payload], weak.Pointer[payload]) {
	storage, p, original := rawNode()
	roots := []any{p.child}
	replacement := &payload{}
	p.child = replacement // Deliberately omit updating the separate root table.
	return storage, roots, original, weak.Make(replacement)
}

//go:noinline
func pinnedRawNode(pinBacking bool) ([]byte, *runtime.Pinner, weak.Pointer[payload]) {
	storage, p, observed := rawNode()
	pinner := new(runtime.Pinner)
	if pinBacking {
		pinner.Pin(&storage[0])
	} else {
		pinner.Pin(p.child)
	}
	return storage, pinner, observed
}

//go:noinline
func typedNodes() ([]node, weak.Pointer[payload]) {
	slab := make([]node, 128)
	child := &payload{}
	slab[0].child = child
	return slab, weak.Make(child)
}

//go:noinline
func replaceTypedNode(p *node) weak.Pointer[payload] {
	child := &payload{}
	p.child = child
	return weak.Make(child)
}

//go:noinline
func alive(p weak.Pointer[payload]) bool { return p.Value() != nil }

func expectLive(t *testing.T, p weak.Pointer[payload]) {
	t.Helper()
	for range 3 {
		runtime.GC()
		if !alive(p) {
			t.Fatal("GC lost a target that should have a strong reference")
		}
	}
	t.Log("target stayed live across 3 forced GC cycles")
}

func observeCollection(t *testing.T, p weak.Pointer[payload]) {
	t.Helper()
	for i := range 20 {
		runtime.GC()
		if !alive(p) {
			t.Logf("weak target cleared after %d forced GC cycle(s)", i+1)
			return
		}
	}
	// weak.Pointer explicitly does not promise eventual collection. A skipped
	// observation is inconclusive, not proof that a hidden pointer is safe.
	t.Skip("collection not observed; weak-pointer clearing has no deadline")
}

func TestByteBackingKeepAliveDoesNotTraceChild(t *testing.T) {
	storage, _, child := rawNode()
	observeCollection(t, child)
	runtime.KeepAlive(storage)
}

func TestTypedPointerToByteBackingDoesNotTraceChild(t *testing.T) {
	_, p, child := rawNode()
	roots := []any{p} // Retain the container pointer, not its actual child.
	observeCollection(t, child)
	runtime.KeepAlive(roots)
}

func TestExplicitRootTableRetainsAndReleasesChild(t *testing.T) {
	storage, roots, child := rootedRawNode()
	expectLive(t, child)
	runtime.KeepAlive(roots)
	clear(roots)
	observeCollection(t, child)
	runtime.KeepAlive(roots)
	runtime.KeepAlive(storage)
}

func TestDirectAssignmentBypassesRootTable(t *testing.T) {
	storage, roots, original, replacement := replacedRawNode()
	expectLive(t, original)
	observeCollection(t, replacement)
	runtime.KeepAlive(roots)
	runtime.KeepAlive(storage)
}

func TestPinningBackingDoesNotTraceChild(t *testing.T) {
	storage, pinner, child := pinnedRawNode(true)
	observeCollection(t, child)
	pinner.Unpin()
	runtime.KeepAlive(storage)
}

func TestPinningActualTargetRetainsAndReleasesChild(t *testing.T) {
	storage, pinner, child := pinnedRawNode(false)
	expectLive(t, child)
	pinner.Unpin()
	observeCollection(t, child)
	runtime.KeepAlive(pinner)
	runtime.KeepAlive(storage)
}

func TestTypedSlabTracesDirectFieldAssignments(t *testing.T) {
	slab, original := typedNodes()
	expectLive(t, original)
	replacement := replaceTypedNode(&slab[0])
	expectLive(t, replacement)
	observeCollection(t, original)
	runtime.KeepAlive(slab)
	clear(slab) // Typed clearing removes outgoing GC references.
	observeCollection(t, replacement)
	runtime.KeepAlive(slab)
}

func TestTypedSlabReslicingDoesNotReleaseChild(t *testing.T) {
	slab, child := typedNodes()
	slab = slab[:0]
	expectLive(t, child)
	runtime.KeepAlive(slab)
	clear(slab[:cap(slab)])
	observeCollection(t, child)
	runtime.KeepAlive(slab)
}
