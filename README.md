# arena.go

하나의 큰 `[]byte` 안에 포인터 없는 Go 값을 배치하는 고정 용량 메모리 아레나.
Go **1.27.1 이상**을 사용하며, `Alloc`과 `Free`는 독립 타입 매개변수를 갖는
**제네릭 메서드**다. 외부 의존성은 없다.

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
    if err := a.Free(u); err != nil { panic(err) } // T는 인수에서 추론
    fmt.Println(a.Stats().Active) // 0
}
```

## 할당과 공간 재활용

- `Alloc[T]()`는 정렬과 크기를 계산하고, 0으로 초기화된 `*T`를 반환한다.
- 재사용 가능한 구간을 주소 순서로 먼저 찾고, 없으면 버퍼 끝의 offset을 전진시킨다.
- `Free(ptr)`와 `FreeBuffer(handle)`는 공간을 지우고 인접한 빈 구간을 합친다.
  마지막 구간이 비면 offset도 되돌린다. 정렬 때문에 남은 바이트도 재사용한다.
- 용량을 자동 확장하거나 살아 있는 객체를 이동시키지 않는다. 총 여유 공간이 있어도
  정렬된 연속 공간이 부족하면 `ErrOutOfMemory`를 반환한다.
- 크기가 0인 객체, 버퍼, 문자열도 식별 가능한 위치를 위해 1바이트를 예약한다.
- `Reset()`은 모든 할당을 무효화하고 같은 backing buffer를 재사용한다.
  메타데이터를 비우며, 재할당 시 해당 바이트를 0으로 초기화한다.
- `Close()`는 모든 할당을 무효화하고 아레나가 보유한 메모리 참조를 놓는다.
  실제 회수 시점은 GC가 결정한다. 여러 번 호출해도 된다.

## GC와 수명 규칙

`[]byte`의 backing array 내부는 GC가 포인터 저장 영역으로 스캔하지 않는다.
따라서 `*T`로 재해석했다고 해서 T의 필드가 GC에 알려지지는 않는다.
이 구현은 문자열, 슬라이스, 포인터, `unsafe.Pointer`, map, interface, func, chan을
포함한 타입을 `ErrPointerType`으로 거부한다. 배열과 구조체 내부까지 재귀 검사하며,
보수적으로 길이가 0인 포인터 배열도 거부한다. 숫자, bool, 포인터 없는 배열/구조체와
`arena.String`, `arena.Buffer`는 허용한다. `uintptr`도 숫자로 허용하지만
Go 객체 주소를 저장해도 그 객체를 살려 두지는 못한다.

반환된 `*T`와 `[]byte`는 빌린 뷰다. 해당 할당의 해제, `Reset`, `Close` 이후에는
읽거나 쓰면 안 된다. API는 이미 반환된 포인터나 슬라이스를 회수할 수 없다.
일반 Go 포인터나 슬라이스 자체는 backing array를 GC로부터 유지하지만,
그 사실이 해제된 구간의 사용을 유효하게 만들지는 않는다.

`Arena`의 메타데이터 메서드는 mutex로 보호된다. 반환된 포인터/슬라이스를 통한
데이터 접근과 그 할당의 해제·초기화 사이 동기화는 호출자가 책임진다.
`InUse` 결과는 확인 시점의 상태이며 사용 중 해제를 막는 대여 잠금이 아니다.
`Arena`를 값으로 복사하지 말고 `New`가 반환하는 포인터를 사용한다.

## String과 바이트 버퍼

| API | 동작 |
| --- | --- |
| `a.NewString(value)` | 문자열 바이트를 아레나에 복사하고 `arena.String` 반환 |
| `a.String(s)` | 유효성을 확인하고 독립적인 Go `string`으로 복사 |
| `a.FreeString(s)` | 문자열 저장 공간 해제 |
| `s.Len()` / `s.Buffer()` | 바이트 길이 / 추적용 핸들 |
| `a.AllocBuffer(n)` | 초기화된 n바이트 공간의 `Buffer` 반환 |
| `a.Bytes(b)` | 길이와 capacity가 n인 변경 가능한 뷰 반환 |
| `a.FreeBuffer(b)` | 핸들의 소유자와 할당 번호를 확인하여 공간 해제 |

`String`과 `Buffer`는 소유 아레나 ID, 할당 ID, offset, 길이만 보관한다.
Go 포인터가 없으므로 아레나 안의 구조체에 넣을 수 있다. 대신 핸들 자체는 아레나를
살려 두지 않는다. 읽을 때 소유 아레나를 명시적으로 전달해야 한다.
두 타입의 제로 값은 유효한 할당이 아니다. 빈 문자열도 `NewString("")`으로 만든다.
`Len`과 `Offset`은 메타데이터를 반환할 뿐 생존 여부는 검사하지 않는다.

`Bytes`는 `AllocBuffer`로 만든 버퍼에만 허용된다. 문자열이나 typed object의
바이트를 이 API로 수정할 수 없다. `a.String(s)`의 결과는 복사본이므로
아레나를 해제해도 유효하다. `String` 핸들을 복사하면 같은 할당을 공유하며,
한 번 해제하면 모든 복사본이 무효화된다. 이를 포함한 구조체를 `Free`해도
문자열은 자동으로 해제되지 않는다. 각각 해제하거나 `Reset`을 사용한다.

## 사용 중인 버퍼 확인

- `BufferOf(ptr)`로 **살아 있는 동안** typed object의 할당 핸들을 얻는다.
- `InUse(handle)`은 아레나와 할당 ID를 함께 확인한다. 주소 재사용이나 `Reset`
  이후에도 오래된 핸들이 새 할당으로 오인되지 않는다.
- `Allocations()`는 주소 순서의 현재 할당 목록과 타입, 정렬 정보를 반환한다.
- `Stats()`는 용량, 예약된 바이트, 여유 공간, 활성 할당 수, offset, 빈 구간 수,
  최대 연속 빈 공간을 반환한다. 정렬 패딩은 여유 공간에 포함된다.
- `Check()`는 영역의 겹침/누락, 병합 상태, 정렬, ID, 사용량 집계가 일치하는지 검사한다.

원시 포인터에는 할당 번호가 없다. `Free(ptr)`는 nil, 다른 아레나의 포인터,
중간 주소, 타입 불일치, 아직 재사용되지 않은 해제 주소를 거부하지만,
**같은 주소에 같은 타입이 재할당되면 이전 포인터와 구별할 수 없다(ABA)**.
이 경우까지 해제 오류를 검출하려면 처음에 `BufferOf`로 핸들을 얻어 보관하고
`FreeBuffer`로 해제한다. 이미 무효화된 포인터에 `BufferOf`를 다시 호출해서는 안 된다.
체커는 원시 포인터의 해제 후 접근이나 임의의 `unsafe` 메모리 손상을 막지 못한다.

## 비용과 검증

객체 데이터는 하나의 backing buffer에 놓지만 map, 빈 구간 목록, 타입 검사 캐시 등의
메타데이터는 일반 Go heap을 사용한다. 진단용 스냅샷과 `Check`도 메모리를 할당한다.
빈 구간 탐색/삽입은 구간 수에 비례하고, `Reset`은 활성 메타데이터 정리 비용이 있다.
따라서 모든 연산이 O(1)이거나 heap allocation이 전혀 없는 구현은 아니다.
속도가 일반 `new`보다 빠르다는 보장도 없다. 실제 사용 패턴으로 측정해야 한다.

```text
go test ./...
go vet ./...
go test -gcflags=all=-d=checkptr=2 ./...
go test -race ./...
go test '-run=^$' -fuzz=FuzzArena -fuzztime=10s
go test '-run=^$' '-bench=.' -benchmem
```

race 검사는 Go 도구 체인이 지원하는 OS/아키텍처에서 실행한다.
벤치마크의 `BenchmarkBatch` 한 연산은 128개 객체 할당이다. 일반 heap 측도
포인터를 보관해 컴파일러가 할당을 스택으로 제거하지 못하게 한다.

초기 검증 환경은 Go 1.27.1, Windows/arm64,
Snapdragon X Plus X1P42100(Qualcomm Oryon)이다. 빌드, 단위/예제 테스트,
`go vet`, `checkptr=2`가 통과했으며 코드 커버리지는 95.2%였다.
10초 퍼징은 4개 worker로 102,019회를 실행해 통과했다.
golangci-lint 2.14.0의 기본 검사(`errcheck`, `govet`, `ineffassign`,
`staticcheck`, `unused`)도 통과했다. 이 환경은 `-race`를 지원하지 않으며,
`govulncheck`는 설치되어 있지 않아 실행하지 못했다.

200ms 벤치마크 단일 실행에서는 128개 객체 배치당 일반 heap이 4,951ns /
12,288B / 128 allocations, 아레나 Reset 방식이 5,349ns / 0B /
0 allocations로 측정됐다. 개별 Alloc/Free 반복은 100.8ns / 0B /
0 allocations였다. 초기화 비용은 제외한 반복 구간의 측정값이며,
반복 측정에 따른 분포나 애플리케이션 전체 성능을 의미하지 않는다.

설계 참고: [Go 1.27 제네릭 메서드](https://go.dev/doc/go1.27),
[unsafe 패키지의 포인터 변환 규칙](https://pkg.go.dev/unsafe),
[Go GC 가이드](https://go.dev/doc/gc-guide).
