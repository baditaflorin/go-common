package validate

import (
	"fmt"
	"reflect"
)

func checkMin(v reflect.Value, name string, n float64) string {
	switch v.Kind() {
	case reflect.String:
		if float64(len(v.String())) < n {
			return fmt.Sprintf("%s: must be at least %g characters", name, n)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if float64(v.Int()) < n {
			return fmt.Sprintf("%s: must be at least %g", name, n)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if float64(v.Uint()) < n {
			return fmt.Sprintf("%s: must be at least %g", name, n)
		}
	case reflect.Float32, reflect.Float64:
		if v.Float() < n {
			return fmt.Sprintf("%s: must be at least %g", name, n)
		}
	case reflect.Slice, reflect.Map:
		if float64(v.Len()) < n {
			return fmt.Sprintf("%s: must have at least %g items", name, n)
		}
	}
	return ""
}

func checkMax(v reflect.Value, name string, n float64) string {
	switch v.Kind() {
	case reflect.String:
		if float64(len(v.String())) > n {
			return fmt.Sprintf("%s: must be at most %g characters", name, n)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if float64(v.Int()) > n {
			return fmt.Sprintf("%s: must be at most %g", name, n)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if float64(v.Uint()) > n {
			return fmt.Sprintf("%s: must be at most %g", name, n)
		}
	case reflect.Float32, reflect.Float64:
		if v.Float() > n {
			return fmt.Sprintf("%s: must be at most %g", name, n)
		}
	case reflect.Slice, reflect.Map:
		if float64(v.Len()) > n {
			return fmt.Sprintf("%s: must have at most %g items", name, n)
		}
	}
	return ""
}
