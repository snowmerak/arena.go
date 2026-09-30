package arena

import (
	"reflect"
	"sync"
)

var pointerFreeTypes sync.Map // reflect.Type -> bool

func isPointerFree(t reflect.Type) bool {
	if cached, ok := pointerFreeTypes.Load(t); ok {
		return cached.(bool)
	}
	valid := true
	switch t.Kind() {
	case reflect.Pointer, reflect.UnsafePointer, reflect.String, reflect.Slice,
		reflect.Map, reflect.Interface, reflect.Func, reflect.Chan:
		valid = false
	case reflect.Array:
		valid = isPointerFree(t.Elem())
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if !isPointerFree(t.Field(i).Type) {
				valid = false
				break
			}
		}
	}
	pointerFreeTypes.Store(t, valid)
	return valid
}
