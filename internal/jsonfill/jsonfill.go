// Package jsonfill gives the JSON outputs one convention for an empty list: [] (and {} for an empty map), never null.
package jsonfill

import "reflect"

// Of returns a copy of v whose nil slices and maps, wherever they sit, are empty ones; v itself is left as it is.
// A field tagged omitempty is still left out when empty.
func Of(v any) any {
	if v == nil {
		return nil
	}
	return fill(reflect.ValueOf(v)).Interface()
}

func fill(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Elem().Type())
		out.Elem().Set(fill(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(fill(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if f := out.Field(i); f.CanSet() {
				f.Set(fill(v.Field(i)))
			}
		}
		return out
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return v // bytes are a base64 string, not a list
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(fill(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			out.Index(i).Set(fill(v.Index(i)))
		}
		return out
	case reflect.Map:
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(it.Key(), fill(it.Value()))
		}
		return out
	}
	return v
}
