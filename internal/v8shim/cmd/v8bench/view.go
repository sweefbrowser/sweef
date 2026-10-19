package main

import "unsafe"

// unsafeInt32s reinterprets a byte view (an Int32Array backing store) as int32s.
func unsafeInt32s(b []byte) []int32 {
	if len(b) < 4 {
		return nil
	}
	return unsafe.Slice((*int32)(unsafe.Pointer(&b[0])), len(b)/4)
}
