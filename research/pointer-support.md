# 포인터 지원 연구

조사일: 2026-09-30. 기준 구현: `e6b57b067f8d158ae29c5a271366cebe78c355e9`.
환경: Go 1.27.1, Windows/arm64, Snapdragon X Plus X1P42100.
이 문서의 API는 설계 제안이며 아직 라이브러리에 구현하지 않았다.

## 결론

일반 구조체처럼 `p.Child = child`를 자유롭게 쓰는 것이 목표라면,
포인터 없는 타입은 기존 바이트 아레나에 두고 **포인터가 있는 타입은 타입별
`[]T` 청크에 할당하는 방식**을 권장한다. `Alloc[T]()`와 `Free(ptr)`라는
제네릭 메서드 호출 형태는 유지할 수 있다. 다만 단일 backing buffer라는
구현 조건과 현재 용량 집계 계약은 변경해야 한다.

단일 `[]byte`와 그 영역의 GC 스캔 회피를 유지하는 것이 우선이면,
**아레나 내부 연결은 offset 핸들**, **외부 Go 객체는 강한 참조 테이블과
포인터 없는 `Ref[T]` 핸들**로 표현하는 것이 적합하다. 이 경우 `*Child`
필드를 `Ref[*Child]`로 바꾸고 조회 API를 사용해야 한다.

이는 아래 런타임 소스와 실험에 근거한 설계 판단이다. 표준 공개 API만으로
기존 `[]byte`의 임의 구간을 다른 타입의 GC 스캔 대상으로 재등록하는 방법은
찾지 못했다. 단일 noscan 버퍼, 임의 타입, 자유로운 필드 쓰기를 모두 유지하려면
현재 구현 범위를 넘어선 런타임 통합이 필요하다.

## 문제가 발생하는 지점

Go 런타임은 할당 시 타입에 따라 스캔 대상 메모리와 noscan 메모리를 구분한다.
`[]byte`의 backing array는 noscan 경로로 할당된다. 나중에 `*T`로 변환하거나
그 `*T`를 `any`에 담아도 원래 메모리의 포인터 배치 정보는 변경되지 않는다.
이는 [malloc 구현](https://go.dev/src/runtime/malloc.go)과 이번 실험에서 확인했다.

다음 두 참조는 역할이 다르다.

```text
일반 Go 루트 → 바이트 버퍼 안의 *Node → backing array 자체는 살아 있음
backing array 내부에 숨긴 Node.Child → 대상 heap 객체는 추적되지 않음
```

포인터 필드 쓰기에 컴파일러의 write barrier가 발생할 수 있다는 사실도
영구적인 스캔 정보를 만들어 주지는 않는다. 특정 GC 주기에 우연히 살아남은
객체가 다음 주기에도 유지된다고 판단할 수 없다.
[write barrier 구현](https://go.dev/src/runtime/mbarrier.go)은 GC 진행 중의
참조 변경을 처리하는 장치이지 바이트 버퍼의 타입 등록 API가 아니다.

`reflect.NewAt` 역시 기존 주소를 주어진 타입의 값으로 표현할 뿐이다.
[공식 설명](https://pkg.go.dev/reflect#NewAt)에 GC 레이아웃을 다시 등록하는
계약은 없으며, 런타임 할당 정보를 바꾸는 해결책으로 사용할 수 없다.

## 대안 비교

| 방식 | 단일 바이트 저장소 유지 | 일반 포인터 필드 대입 | 비용과 제약 |
| --- | --- | --- | --- |
| 내부 offset 핸들 | 가능 | 조회 API 필요 | 외부 객체는 별도 보관 필요; 현재 String/Buffer와 같은 방향 |
| 외부 강한 참조 테이블 + Ref | 데이터 영역은 유지 | 참조 핸들 API 필요 | 실제 참조의 GC 스캔, 등록/조회/해제 비용 |
| 타입별 `[]T` 청크 | 불가 | 가능 | 타입별 여유 공간, GC 스캔, 청크 단위 메모리 유지 |
| 포인터 포함 타입만 `new(T)` | 불가 | 가능 | 가장 간단하지만 해당 객체의 개별 heap 할당은 유지 |
| 명시적 `Pinner.Pin(target)` | 가능하나 대상은 별도 heap | 등록된 대상만 조건부 가능 | 참조 변경마다 관리 필요; pin 해제·수명 관리 |
| 실험적 표준 `arena` | 현재 `[]byte` 대신 런타임 청크 | 가능 | 실험 플래그, 보류된 API, 개별 Free 미제공 |
| GC bitmap 직접 수정 / runtime 내부 호출 | 런타임 개조 수준 | 정확한 통합 시 가능 | 비공개 ABI, GC 동기화, 메모리 재사용 프로토콜 책임 |

## 1. 타입별 청크: 일반적인 *T 사용을 위한 권장안

청크는 처음부터 `make([]T, count)`로 만든다. 런타임이 T의 포인터 필드를
알고 있으므로 `&chunk[i]`에 수행하는 일반 대입을 GC가 정상적으로 추적한다.
[슬라이스 할당 구현](https://go.dev/src/runtime/slice.go)도 원소 타입을
런타임 할당에 전달한다. 아래는 동작 원리를 보여 주는 예다.

```go
type Node struct {
    Next  *Node
    Name  string
    Data  []byte
}

chunk := make([]Node, 1024)
p := &chunk[0]
p.Next = &chunk[1]
p.Name = "hello"
p.Data = make([]byte, 128)
```

이 방식은 개별 Node의 `new` 호출을 청크 할당으로 묶는다.
`Data = make(...)`나 새로운 문자열 생성까지 자동으로 아레나에 넣어 주지는 않는다.
다른 typed chunk나 일반 heap 객체를 가리키는 것도 정상적인 GC 참조가 된다.

현재 코드에 통합할 때 필요한 변경은 다음과 같다.

1. `Alloc[T]()`에서 타입 분류 결과에 따라 바이트 저장소 또는 `typedPool[T]`를 선택한다.
   풀은 일반 heap의 `map[reflect.Type]any` 등에 보관할 수 있다.
2. 한 번 포인터를 반환한 청크는 크기나 주소를 바꾸지 않는다. 용량이 부족하면
   새 `[]T` 청크를 추가하고 이전 청크를 보관한다. 기존 slice에 append해서 새 주소에
   복사한 뒤 그 복사본을 동일한 슬롯으로 취급해서는 안 된다.
3. `Free`는 타입을 아는 코드에서 `*p = zeroT`로 참조를 지우고 슬롯을 재활용한다.
   `Reset`도 `clear(chunk)` 같은 typed 연산으로 사용한 슬롯의 참조를 지운다.
   기존 `clear([]byte)`를 pointerful 저장소에 그대로 적용하면 안 된다.
4. offset 하나로 주소를 식별할 수 없으므로 핸들/메타데이터에 저장소 종류,
   청크 ID, 슬롯 ID, 할당 세대를 포함한다. `Free`, `BufferOf`, `Check`도 이를 검증한다.
5. byte capacity와 typed chunk capacity를 구분해서 집계한다. 현재 `New(n)`을
   전체 저장소 예산으로 확대할지, `NewHybrid` 같은 별도 생성자를 둘지 결정한다.
   기존 용량 한도를 조용히 넘기는 fallback은 피한다.

특히 `chunk = chunk[:0]`만으로는 참조가 사라지지 않는다. backing array의
사용했던 슬롯을 지워야 자식 객체를 놓을 수 있다. 이번 실험에서도 길이 0인
슬라이스가 유지되는 동안 자식은 살아 있었고, 전체 슬롯을 clear한 뒤 수거됐다.
또한 일부 슬롯만 살아 있어도 해당 청크 전체의 저장 공간이 유지될 수 있다.

이 설계는 **할당 횟수 감소**를 목표로 삼을 수 있지만, pointerful 청크와 연결된
객체 그래프를 스캔하는 비용은 남는다. 실제 GC CPU, heap scan bytes, 청크 이용률,
처리 시간은 별도로 측정해야 한다. raw pointer의 Free 이후 접근과 ABA 문제도 남는다.

## 2. 외부 참조 테이블: 바이트 아레나를 유지하는 권장안

구조체에는 포인터 없는 ID를 저장하고 실제 값은 일반 Go heap의 테이블에 둔다.
이것은 강한 참조이므로 `weak.Pointer`나 `uintptr`를 참조 보관용으로 쓰면 안 된다.
정수 핸들로 Go 값을 유지하는 공개 API의 사례로
[`runtime/cgo.Handle`](https://pkg.go.dev/runtime/cgo#Handle)이 있다.
이 라이브러리에서는 아레나별 생명주기와 세대 검사를 위해 자체 테이블을 두는 편이 적합하다.

아래는 아직 구현하지 않은 API 구상이다.

```go
type Entry struct {
    ID    uint64
    Child Ref[*ExternalObject]
}

ref, err := a.Retain(child)       // 실제 child를 GC-visible 테이블에 저장
entry.Child = ref                 // 바이트 영역에는 정수 식별자만 복사
child, err = a.Resolve(entry.Child)
err = a.Release(entry.Child)
```

`Ref[T]`는 owner ID, slot ID, generation처럼 숫자 필드만 가져야 한다.
현재 타입 검사기는 `[0]*T` 같은 타입 표시용 필드도 포인터 포함 타입으로 거부한다.
또한 숫자 필드만 가진 제네릭 핸들은 서로 다른 T 사이 명시적 변환이 가능할 수 있으므로,
조회 때 테이블의 실제 등록 타입과 `reflect.TypeFor[T]()`도 반드시 비교해야 한다.
nil interface와 typed nil도 명시적으로 정의해야 한다.

테이블은 `[]any`/`map[ID]any`처럼 실제 값의 참조를 GC가 볼 수 있는 형태로 보관한다.
문자열·슬라이스·map도 테이블의 값으로 유지할 수 있다. `Resolve`로 얻은 실제 Go 값이
다른 곳에 남아 있으면 `Release` 후에도 그 값의 일반적인 GC 수명은 유지된다.

`Release`와 `Reset`은 테이블의 슬롯 자체를 nil로 지워 참조를 끊어야 한다.
테이블 slice의 길이만 줄이거나 ID 메타데이터만 지우면 기존 참조가 남을 수 있다.
핸들 복사본의 소유권은 현재 `Buffer`처럼 한 번 해제하면 모두 무효화하는 계약이 단순하다.
핸들을 포함한 객체를 Free할 때 참조도 자동으로 해제하려면 별도의 소유 관계를 설계해야 한다.

등록된 외부 객체의 필드 변경은 그 객체가 원래의 typed Go heap에 있으므로 GC가 추적한다.
반면 바이트 영역에 실제 포인터를 넣고 참조 테이블에는 복사본만 두면 다음 문제가 생긴다.

```go
roots = append(roots, p.Child) // 최초 대상만 유지
p.Child = other              // 테이블을 갱신하지 않으면 other는 유지되지 않음
```

이번 실험에서 최초 대상은 살아 있고 `other`는 수거되는 현상을 재현했다.
`roots = append(roots, p)`는 바이트 버퍼만 살려 둘 뿐이며,
`roots = append(roots, *p)`로 구조체를 복사해도 이후 변경을 추적하지 못한다.
setter에서 항상 참조를 함께 갱신하는 제한된 API는 가능하지만, 외부로 자유로운
`*T`를 내보내면 라이브러리가 모든 쓰기를 가로챌 수 없다. 따라서 포인터 필드를
계속 허용하면서 자동 해결된다고 주장할 수는 없다.

별도 테이블을 유지하는 Arena 자체도 핸들 사용 기간 동안 강하게 참조되어야 한다.
바이트 영역을 가리키는 raw pointer만 남으면 그 pointer가 테이블까지 유지하지는 않는다.

## 3. 아레나 내부 링크는 별도로 생각할 수 있음

하나의 backing array 자체가 살아 있고, 모든 링크가 반드시 그 배열 내부를 가리키며,
개별 슬롯의 Free/재사용 규칙을 지킨다면 링크가 GC edge가 아니어도 내부 데이터의
물리적 생존에는 문제가 없다. 참조 대상이 이미 같은 backing array에 있기 때문이다.
그러나 일반 `*Node` 필드에는 외부 heap 포인터나 다른 아레나 포인터도 대입할 수 있어
이 조건을 현재 API로 강제할 수 없다.

현재 `Buffer`/`String`을 확장한 `ArenaRef[T]` 같은 offset·세대 핸들을 사용하면
소유 아레나, 타입, 슬롯 생존 여부를 조회 시 검사할 수 있다. 이는 외부 객체를
보관하는 `Ref[T]` 테이블과 역할이 다르다. 다른 아레나를 참조한다면 그 아레나의
backing storage와 참조 테이블 수명까지 별도로 관리해야 한다.

## 단독으로 해결하지 못하는 방법

- **`KeepAlive(buffer)` 또는 `KeepAlive(typedPointer)`**: backing storage의 수명만
  연장한다. 숨겨진 필드를 스캔 대상으로 바꾸지 않는다. 실제 참조 대상을 정상적인
  강한 참조 변수로 유지하고 마지막 사용 뒤 `KeepAlive(target)`하는 제한된 범위의
  사용은 가능하지만, 모든 쓰기를 자동 관리하는 일반 아레나 해법은 아니다.
  GC 이후 바이트 영역에서 대상을 다시 읽어 KeepAlive에 넘기는 것은 늦을 수 있다.
  [KeepAlive 문서](https://pkg.go.dev/runtime#KeepAlive).
- **`Pinner.Pin(&buffer[0])`**: backing array만 pin하므로 외부 대상을 유지하지 않는다.
  실제 `target`을 각각 pin하면 유지할 수 있으나 여전히 명시적 등록과 변경 관리가
  필요하다. `Pin(&p.Child)` 역시 필드의 저장소를 pin하는 것이지 자식을 pin하지 않는다.
  pin은 재귀적인 주소 고정도 아니다. C에 노출할 주소 고정이 필요하지 않다면
  강한 참조 테이블이 더 직접적인 설계다.
  [Pinner 문서](https://pkg.go.dev/runtime#Pinner), [실제 참조 보관 구현](https://go.dev/src/runtime/pinner.go).
- **`[]unsafe.Pointer`를 범용 바이트 저장소로 재해석**: 모든 word가 포인터로 스캔되는
  곳에 정수·길이·임의 바이트를 쓰면 잘못된 포인터를 GC에 노출한다. 타입별 레이아웃을
  갖춘 `[]T`와 다르다. 실제 포인터만 담는 별도 테이블로는 사용할 수 있다.
- **주기적인 수동 스캔 / GC 직전 복사**: 자동 GC, concurrent marking, 사용자 필드
  변경과의 동기화가 해결되지 않는다. GC에 사용자 scanner를 등록하는 안정적인 공개
  API를 찾지 못했다. 전체 GC를 끄는 것은 앱 전체의 메모리 관리 계약을 바꾸므로 제외한다.
- **mmap 또는 C malloc으로 이전**: Go GC가 외부 메모리의 숨겨진 참조를 자동으로
  추적하게 되지 않는다. 외부 저장소의 할당/반납 기능과 Go 객체의 참조 보존은 별개다.

## 실험적 표준 arena와 내부 bitmap

설치된 Go 1.27.1에는 `GOEXPERIMENT=arenas`로 활성화하는 표준 `arena` 패키지가 있다.
이 패키지의 런타임 구현은 pointerful 할당 때 실제 타입별 heap bitmap을 기록하고
GC에 보이도록 publication ordering도 처리한다. 따라서 단순한 `unsafe` 캐스팅과
동작 원리가 다르다. [런타임 arena 구현](https://go.dev/src/runtime/arena.go).

이 환경에서 실험 패키지로 만든 Node의 외부 heap 자식이 여러 GC 동안 유지되고,
필드를 nil로 바꾸면 수거되는 것을 확인했다. 다만
[공식 제안 #51317](https://github.com/golang/go/issues/51317)은 확인 시점에도
Proposal-Hold 상태이며 운영 사용을 권장하지 않는다.
[공개 실험 API](https://go.dev/src/arena/arena.go)는 전체 Free를 제공하고
현재 라이브러리 같은 객체별 Free/공간 재활용 API는 제공하지 않는다.
기본 backend로 채택하기보다 별도 실험 대상으로 두는 것이 적절하다.

같은 기법을 따라 runtime 내부 bitmap을 직접 바꾸려면 타입 메타데이터, GC 상태,
publication barrier, span 분류, 주소 재사용을 함께 처리해야 한다. `go:linkname`으로
함수 하나를 호출하는 것으로 현재 noscan 배열을 안전하게 바꾸는 설계는 성립하지 않는다.
공개 API 기반의 타입별 청크보다 유지보수 범위가 크게 넓어진다.

## 재현 실험과 한계

`pointer_gc_test.go`는 `arenaresearch` 태그가 있을 때만 실행한다.
잘못된 참조 보존 방식도 의도적으로 포함하지만, 수거 후 숨겨진 포인터를 역참조하지는 않는다.
1,024바이트 외부 객체와 `weak.Pointer`로 생존 여부만 관측한다.
tiny allocator의 공유로 인한 거짓 생존 관측을 피하고자 작은 대상을 사용하지 않았다.

| 실험 | 이 환경에서 관측한 결과 |
| --- | --- |
| 바이트 버퍼 KeepAlive | 자식의 weak pointer가 첫 강제 GC 뒤 nil |
| byte-backed `*Node`를 `[]any`에 저장 | 자식의 weak pointer가 첫 강제 GC 뒤 nil |
| 실제 자식을 `[]any`에 저장 | 3회 GC 동안 생존; 테이블 clear 후 첫 GC에서 nil |
| 필드만 교체하고 테이블은 그대로 유지 | 기존 자식 생존, 새 자식은 첫 GC에서 nil |
| backing buffer만 pin | 자식은 첫 GC에서 nil |
| 실제 자식을 pin | 3회 GC 동안 생존; Unpin 후 첫 GC에서 nil |
| `[]Node`의 필드를 직접 교체 | 새 자식 생존, 이전 자식 수거; typed clear 뒤 새 자식도 수거 |
| typed slab을 길이 0으로 reslice | 자식 생존; 전체 슬롯 clear 후 수거 |
| 실험적 표준 arena | 자식 생존; 필드를 nil로 변경한 뒤 수거 |

실행 명령:

```powershell
go test -tags=arenaresearch -v -count=1 ./research
go test -tags=arenaresearch '-gcflags=all=-d=checkptr=2' -count=3 ./research
go vet -tags=arenaresearch ./research
golangci-lint run --no-config --build-tags=arenaresearch ./research

$previousExperiment = $env:GOEXPERIMENT
try {
    $env:GOEXPERIMENT = 'arenas'
    go test -tags=arenaresearch '-run=TestRuntimeArena' -v ./research
} finally {
    $env:GOEXPERIMENT = $previousExperiment
}
```

위 명령은 모두 통과했다. lint는 2.14.0의 기본 5개 검사였다.
`weak.Pointer`는 대상의 최종 수거 시점을 보장하지 않으므로, 수거를 관측하지 못하면
실험은 해당 항목을 skip한다. 이번 실행에는 skip이 없었다.
[weak.Pointer 계약](https://pkg.go.dev/weak#Pointer)에 따라 이 결과는 관측값이며,
GC 한 번이면 항상 수거된다는 계약이나 전체 메모리 안전성의 증명은 아니다.
`checkptr` 통과도 GC edge가 올바르게 등록되었다는 증명이 아니다.

Windows/arm64에서는 race detector를 사용할 수 없다. 이 실험은 동시성, 성능,
모든 Go 타입을 검증한 allocator 구현이 아니며, 기존 라이브러리 API를 확장하지 않는다.

## 다음 구현을 선택한다면

기존 목적 중 **일반 구조체를 그대로 쓰기**를 우선하면 `NewHybrid`와 타입별 청크를
먼저 구현하는 것이 좋다. 원래 바이트 모드는 유지하고, typed 저장소에 대해 용량,
슬롯 해제, Reset의 typed clear, 체크 핸들을 추가하는 범위다.

반대로 **하나의 큰 바이트 영역에 데이터를 모으고 스캔을 줄이기**가 우선이면
기존 String/Buffer와 같은 방식의 내부 링크 핸들, 외부 참조용 `Retain/Resolve/Release`를
추가하는 편이 일관된다. 이 경우 외부 객체의 heap 할당과 참조 테이블 스캔은 남는다.
두 방향 모두 데이터 수명과 API 편의성의 선택이며, 성능 우위는 아직 측정하지 않았다.
