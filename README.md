# arena.go

A fixed-capacity memory arena that stores pointer-free Go values in one large
`[]byte` buffer. Requires **Go 1.27.1 or later**. `Alloc` and `Free` are
**generic methods** with their own type parameters. No external dependencies.

```go
package main

import (
    "fmt"

    arena "github.com/snowmerak/arena.go"
)

type User struct {
    ID   uint64
    Name arena.String
}

func main() {
    a, err := arena.New(1 << 20)
    if err != nil { panic(err) }
    defer func() { _ = a.Close() }()

    u, err := a.Alloc[User]()
    if err != nil { panic(err) }
    u.ID = 7
    u.Name, err = a.NewString("snowmerak")
    if err != nil { panic(err) }

    name, err := a.String(u.Name)
    if err != nil { panic(err) }
    fmt.Println(u.ID, name)

    if err := a.FreeString(u.Name); err != nil { panic(err) }
    if err := a.Free(u); err != nil { panic(err) } // T is inferred from u.
    fmt.Println(a.Stats().Active) // 0
}
```

## Allocation and reuse

- `Alloc[T]()` computes size and alignment and returns a zero-initialized `*T`.
- `AllocSlice[T](n)` returns n zero-initialized elements with length and capacity
  n, tracked as **one allocation**. `FreeSlice(values)` releases the whole batch.
- Allocation searches reusable regions in address order, then advances the bump
  offset if no suitable region is available.
- `Free(ptr)` and `FreeBuffer(handle)` clear storage and merge adjacent free
  regions. Free space at the end moves the bump offset back. Alignment gaps are
  also available for reuse.
- Capacity is fixed, and live objects never move. Allocation returns
  `ErrOutOfMemory` when no suitably aligned contiguous region fits, even if total
  free space is sufficient.
- Zero-sized objects, buffers, and strings reserve one byte to give each live
  allocation a distinct location.
- `Reset()` invalidates every allocation and reuses the backing buffer. It clears
  allocation metadata; storage is zeroed when allocated again.
- `Close()` invalidates every allocation and releases the arena's references to
  storage. The GC determines when memory is reclaimed. Closing is idempotent.

## Bulk allocation

When many objects share a lifetime, allocate them together to avoid a mutex,
type-cache lookup, and live-map insertion for every object:

```go
users, err := a.AllocSlice[User](10_000)
if err != nil { panic(err) }
for i := range users {
    users[i].ID = uint64(i)
}
if err := a.FreeSlice(users); err != nil { panic(err) }
```

Pass the original slice to `FreeSlice`, with its original length and capacity.
Elements cannot be freed individually. `Reset` also releases the batch.
`BufferOfSlice(values)` captures a generation-checked handle for `FreeBuffer`;
its `Len()` is the byte size of the batch. `Stats().Active` counts the batch once.
Empty batches and batches of zero-sized elements reserve one byte per batch.
Allocations are shown as `[]T` in diagnostic snapshots.

Slice views are borrowed, just like object pointers. Do not access them after
release, and do not store their slice headers inside byte-backed arena objects.
Pointer-bearing element types are still rejected. Raw slices have the same ABA
limitation as raw pointers; capture a handle before release when that matters.

In the measured one-million-object lifecycle workload, bulk allocation with
reused storage took **18.8ms with Reset** or **19.6ms with FreeSlice**, versus
**51.4ms for individual heap allocations**. Creating and closing a fresh bulk
arena took **32.6ms**. A plain reused `[]T` took **14.4ms**. These are medians of
seven local observations including use and forced GC, not universal guarantees.
See the [optimization report](benchmarks/optimization.md) for ranges, memory
costs, comparison with the old API, and reproduction instructions.

## GC and lifetime rules

The GC does not scan a `[]byte` backing array for pointers. Reinterpreting its
contents as `*T` does not register T's fields with the GC. The byte backend
rejects types containing strings, slices, pointers, `unsafe.Pointer`, maps,
interfaces, functions, or channels with `ErrPointerType`. Arrays and structs are
checked recursively; even zero-length arrays of pointers are conservatively
rejected. Numeric values, booleans, pointer-free arrays and structs,
`arena.String`, and `arena.Buffer` are supported. `uintptr` is allowed as a number,
but storing an object's address in it does not keep that object alive.

Returned `*T`, `[]T`, and `[]byte` values are borrowed views. Do not read or write them
after their allocation is freed, or after `Reset` or `Close`. The API cannot
revoke pointers or slices already handed to a caller. An ordinary Go pointer or
slice can keep the backing array alive, but that does not make a freed region
valid to access.

Arena metadata operations are protected by a mutex. Callers must synchronize
access through borrowed pointers and slices with freeing or resetting their
storage. `InUse` is a snapshot, not a lease that prevents concurrent release.
Do not copy an `Arena` value; use the pointer returned by `New`.

## Strings and byte buffers

| API | Behavior |
| --- | --- |
| `a.NewString(value)` | Copies string bytes into the arena and returns an `arena.String` |
| `a.String(s)` | Validates s and copies its contents into an independent Go `string` |
| `a.FreeString(s)` | Releases the string's storage |
| `s.Len()` / `s.Buffer()` | Returns the byte length / allocation handle |
| `a.AllocBuffer(n)` | Returns a `Buffer` for n zero-initialized bytes |
| `a.Bytes(b)` | Returns a mutable view with length and capacity both equal to n |
| `a.FreeBuffer(b)` | Validates ownership and allocation identity, then releases storage |

`String` and `Buffer` contain only an arena ID, allocation ID, storage segment ID,
offset, and length. They contain no Go pointers and can be embedded in
arena-allocated structs. The handles themselves do not keep the arena alive;
resolving their contents requires the owning arena. Their zero values are invalid.
Create empty strings with `NewString("")`. `Len`, `Segment`, and `Offset` expose
metadata without checking whether the allocation is still live.

`Bytes` accepts only buffers created by `AllocBuffer`; it cannot expose string
or typed object storage for mutation. The result of `a.String(s)` is a copy and
remains valid after the arena is released. Copies of a `String` handle share one
allocation, so releasing it invalidates every handle copy. Freeing a struct that
contains a `String` does not free the string automatically. Release each
allocation explicitly or use `Reset`.

## Tracking live allocations

- Call `BufferOf(ptr)` **while the object is live** to capture its allocation handle.
- `InUse(handle)` checks both the owning arena and allocation ID. Reusing an
  address or resetting the arena does not make an old handle valid again.
- `Allocations()` returns live allocations, their types, and alignment, ordered
  by segment ID and offset. The default byte backend uses segment 0, so this is
  also address order.
- `Stats()` reports capacity, reserved bytes, free space, active allocations,
  occupied prefix length, free region count, and the largest contiguous free
  region. Alignment gaps count as free space.
- `Check()` verifies allocation identities, alignment, region coverage, merging,
  and storage accounting.

Raw pointers have no allocation ID. `Free(ptr)` rejects nil, foreign pointers,
interior addresses, type mismatches, and freed addresses that have not been
reused. However, **an old pointer cannot be distinguished from a new allocation
of the same type at the same address (ABA)**. To detect stale releases after
address reuse, capture a handle with `BufferOf` while the pointer is valid and
release it with `FreeBuffer`. Do not call `BufferOf` on an already invalid pointer.
The checker cannot prevent stale pointer dereferences or arbitrary `unsafe`
memory corruption.

## Storage extension boundary

Common allocation management is separate from physical storage.

| Component | Responsibility |
| --- | --- |
| `Arena` | Generic methods, allocation IDs, handle/type validation, synchronization, and lifetimes |
| Internal `backend` | Storage-specific allocation, release, reset, address lookup, statistics, and checking |
| `byteBackend` | The default byte array, alignment, free region merging, and pointer-type rejection |

The public API includes `New`, `Alloc[T]`, `Free`, `AllocSlice[T]`, and `FreeSlice`.
Additional storage strategies
can implement the internal `backend` interface and be composed through a new
constructor. A public backend registration API and a production typed-chunk
allocator are not implemented. The default `New` still rejects pointer-bearing
types.

`Buffer.Segment()` and `Buffer.Offset()` identify locations across storage
segments. Tests connect an alternative backend with separately allocated typed
heap slots to verify identical offsets in different segments, pointer-bearing
objects, string/buffer access, lifecycle delegation, and failure handling.
See the [storage extension guide](docs/extending.md) for implementation locations
and contracts; that guide is currently written in Korean.

## Costs and validation

Object data lives in one backing buffer, but allocation maps, free region lists,
and the type-check cache use the ordinary Go heap. Diagnostic snapshots and
`Check` also allocate. Searching and inserting free regions costs time
proportional to their number, and `Reset` must clear active allocation metadata.
Operations are therefore neither universally O(1) nor guaranteed to perform zero
heap allocations. This implementation is not guaranteed to outperform `new`.
Measure your actual workload.

The byte backend remembers its last validated type under the existing mutex,
and adjacent frees merge before inserting a new free-list entry. These reduce
repeated bookkeeping but leave per-object tracking costs in `Alloc`/`Free`.
`AllocSlice` avoids those costs per element. It passes an exact reflected `[n]T`
type to the backend; Go caches these type descriptors, so use a bounded set of
batch sizes when descriptor retention matters. Fresh sizes/types can allocate
metadata even when a batch ultimately fails to allocate storage.

```text
go test ./...
go vet ./...
go test -gcflags=all=-d=checkptr=2 ./...
go test -race ./...
go test '-run=^$' -fuzz=FuzzArena -fuzztime=10s
go test '-run=^$' '-bench=.' -benchmem
```

Run race detection on an OS/architecture supported by the Go toolchain.
One `BenchmarkBatch` operation allocates 128 objects. Its heap baseline retains
pointers so the compiler cannot optimize those allocations onto the stack.

`BenchmarkLifecycle` covers bulk creation, use, release, and completion of GC.
It compares fresh and reused heap, slice, and arena storage for 10,000, 100,000,
and 1,000,000 objects of 64 bytes each, including the new bulk API. The per-object
arena remains slower than ordinary heap allocation in this workload. See the
[original investigation](benchmarks/README.md) and [optimization results](benchmarks/optimization.md)
for repeated measurements, memory costs, raw logs, and CPU profile analysis.

The initial commit, `e6b57b0`, was validated with Go 1.27.1 on Windows/arm64 using
a Snapdragon X Plus X1P42100 (Qualcomm Oryon). Builds, unit/example tests,
`go vet`, and `checkptr=2` passed, with 95.2% statement coverage. A 10-second fuzz
run with four workers completed 102,019 executions. golangci-lint 2.14.0 passed
with its default checks: `errcheck`, `govet`, `ineffassign`, `staticcheck`, and
`unused`. Race detection is unsupported on this platform, and `govulncheck` was
not installed, so those checks were not completed.

In a single 200ms benchmark run of that initial commit, allocating a batch of
128 objects used 4,951ns / 12,288B / 128 allocations on the heap, versus
5,349ns / 0B / 0 allocations with arena reset. Repeated individual Alloc/Free
operations used 100.8ns / 0B / 0 allocations. These figures exclude setup and do
not establish a latency distribution or application-level performance.

After extracting the storage backend, the existing tests, backend integration
tests, vet, lint, and checkptr passed, with 95.1% statement coverage. Segment IDs
and storage block metadata add memory and execution costs; do not assume the
same layout or performance as the initial version. Handles are not a stable
binary storage or wire format. Compute struct sizes with `unsafe.Sizeof`.

Design references: [Go 1.27 generic methods](https://go.dev/doc/go1.27),
[unsafe pointer conversion rules](https://pkg.go.dev/unsafe), and the
[Go GC guide](https://go.dev/doc/gc-guide).
