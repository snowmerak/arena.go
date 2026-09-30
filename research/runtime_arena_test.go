//go:build arenaresearch && goexperiment.arenas

package research_test

import (
	stdarena "arena"
	"runtime"
	"testing"
	"weak"
)

//go:noinline
func experimentalNode(a *stdarena.Arena) (*node, weak.Pointer[payload]) {
	p := stdarena.New[node](a)
	child := &payload{}
	p.child = child
	return p, weak.Make(child)
}

func TestRuntimeArenaTracesChild(t *testing.T) {
	a := stdarena.NewArena()
	defer a.Free()
	p, child := experimentalNode(a)
	expectLive(t, child)
	p.child = nil
	observeCollection(t, child)
	runtime.KeepAlive(p)
	runtime.KeepAlive(a)
}
