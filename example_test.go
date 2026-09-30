package arena_test

import (
	"fmt"

	arena "github.com/snowmerak/arena.go"
)

func ExampleArena_Alloc() {
	type User struct {
		ID   uint64
		Name arena.String
	}
	a, err := arena.New(4096)
	if err != nil {
		panic(err)
	}
	defer func() { _ = a.Close() }()
	u, err := a.Alloc[User]()
	if err != nil {
		panic(err)
	}
	u.ID = 7
	u.Name, err = a.NewString("snowmerak")
	if err != nil {
		panic(err)
	}
	name, err := a.String(u.Name)
	if err != nil {
		panic(err)
	}
	fmt.Println(u.ID, name)
	if err := a.FreeString(u.Name); err != nil {
		panic(err)
	}
	if err := a.Free(u); err != nil {
		panic(err)
	}
	fmt.Println(a.Stats().Active)
	// Output:
	// 7 snowmerak
	// 0
}

func ExampleArena_BufferOf() {
	a, err := arena.New(64)
	if err != nil {
		panic(err)
	}
	defer func() { _ = a.Close() }()
	p, err := a.Alloc[uint64]()
	if err != nil {
		panic(err)
	}
	b, err := a.BufferOf(p)
	if err != nil {
		panic(err)
	}
	fmt.Println(a.InUse(b))
	if err := a.FreeBuffer(b); err != nil {
		panic(err)
	}
	if _, err := a.Alloc[uint64](); err != nil {
		panic(err)
	}
	fmt.Println(a.InUse(b))
	// Output:
	// true
	// false
}

func ExampleArena_MakePool() {
	type Item struct{ ID uint64 }
	a, err := arena.New(4096)
	if err != nil {
		panic(err)
	}
	defer func() { _ = a.Close() }()
	pool, err := a.MakePool[Item](128)
	if err != nil {
		panic(err)
	}
	item, err := pool.Alloc()
	if err != nil {
		panic(err)
	}
	item.ID = 42
	fmt.Println(item.ID, pool.Stats().Active)
	if err := pool.Free(item); err != nil {
		panic(err)
	}
	reused, err := pool.Alloc()
	if err != nil {
		panic(err)
	}
	fmt.Println(reused == item, reused.ID)
	if err := pool.Close(); err != nil {
		panic(err)
	}
	fmt.Println(a.Stats().Used)
	// Output:
	// 42 1
	// true 0
	// 0
}
