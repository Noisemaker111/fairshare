package windows

import (
	"testing"
	"unsafe"
)

// Windows reads these structs by size and offset; the sizes are the 64-bit SDK's.
func TestStructSizes(t *testing.T) {
	for name, got := range map[string]uintptr{
		"JOBOBJECT_BASIC_LIMIT_INFORMATION":    unsafe.Sizeof(JOBOBJECT_BASIC_LIMIT_INFORMATION{}),
		"JOBOBJECT_EXTENDED_LIMIT_INFORMATION": unsafe.Sizeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}),
		"IO_COUNTERS":                          unsafe.Sizeof(IO_COUNTERS{}),
		"PROCESS_BASIC_INFORMATION":            unsafe.Sizeof(PROCESS_BASIC_INFORMATION{}),
	} {
		want := map[string]uintptr{
			"JOBOBJECT_BASIC_LIMIT_INFORMATION":    64,
			"JOBOBJECT_EXTENDED_LIMIT_INFORMATION": 144,
			"IO_COUNTERS":                          48,
			"PROCESS_BASIC_INFORMATION":            48,
		}[name]
		if got != want {
			t.Errorf("%s is %d bytes, Windows expects %d", name, got, want)
		}
	}
}

// CreateMutex on a name that exists returns a usable handle and ERROR_ALREADY_EXISTS, which adopt
// uses to tell it is the second copy.
func TestCreateMutexAlreadyExists(t *testing.T) {
	name := StringToUTF16Ptr(`Local\fairshare-test-mutex`)
	a, err := CreateMutex(nil, false, name)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseHandle(a)
	b, err := CreateMutex(nil, false, name)
	if err != ERROR_ALREADY_EXISTS || b == 0 {
		t.Fatalf("second CreateMutex: handle %v, err %v", b, err)
	}
	CloseHandle(b)
}
