// Package jsonview exposes read-only GJSON views over byte slices without
// copying the complete JSON document into a string first.
package jsonview

import (
	"unsafe"

	"github.com/tidwall/gjson"
)

// Get returns a result that may reference data directly. Callers must not retain
// the result or mutate data while the result is in use.
func Get(data []byte, path string) gjson.Result {
	if len(data) == 0 {
		return gjson.Result{}
	}
	return gjson.Get(unsafe.String(unsafe.SliceData(data), len(data)), path)
}

// Parse returns a root result that may reference data directly. Callers must not
// retain the result or mutate data while the result is in use.
func Parse(data []byte) gjson.Result {
	if len(data) == 0 {
		return gjson.Result{}
	}
	return gjson.Parse(unsafe.String(unsafe.SliceData(data), len(data)))
}
