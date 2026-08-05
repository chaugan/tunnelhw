//go:build windows

package svc

import (
	"testing"
	"unsafe"
)

// ShellExecuteExW validates cbSize against the SHELLEXECUTEINFOW it knows,
// and rejects the call outright if it disagrees. A field of the wrong width
// or a missed padding slot therefore shows up as a flat refusal to elevate,
// with no hint as to why, so pin the size to the documented ABI.
func TestShellExecuteInfoMatchesWindowsABI(t *testing.T) {
	want := map[int]uintptr{
		4: 60,  // 386 / arm: 32-bit pointers, no padding
		8: 112, // amd64 / arm64: 64-bit pointers plus two 4-byte padding slots
	}[int(unsafe.Sizeof(uintptr(0)))]

	if want == 0 {
		t.Skipf("no expected size recorded for %d-bit pointers", unsafe.Sizeof(uintptr(0))*8)
	}
	if got := unsafe.Sizeof(shellExecuteInfo{}); got != want {
		t.Fatalf("sizeof(SHELLEXECUTEINFOW) = %d, want %d: field layout has drifted from the Windows ABI", got, want)
	}
}

func TestOnlyMutatingActionsElevate(t *testing.T) {
	for _, a := range []string{"install", "uninstall", "start", "stop", "restart"} {
		if !needsElevation(a) {
			t.Errorf("%q changes service state and must request elevation", a)
		}
	}
	// Querying status works with an ordinary token; prompting for it would be
	// gratuitous UAC noise.
	if needsElevation("status") {
		t.Error("status must not require elevation")
	}
}
