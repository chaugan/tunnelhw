//go:build windows

package svc

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Creating, deleting, starting and stopping a Windows service all require an
// elevated token. Nothing in the service library asks for one, so an
// unelevated `service install` would simply fail with "Access is denied".
// Instead we re-launch ourselves through ShellExecuteEx with the "runas"
// verb, which is what raises the UAC consent prompt, then wait for that
// elevated copy and adopt its exit code.

// elevatedMarker stops an elevated child from trying to elevate again if the
// token check is ever wrong; without it that would be an infinite spawn loop.
const elevatedMarker = "TUNNELHW_SVC_ELEVATED"

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	swShowNormal          = 1
)

type shellExecuteInfo struct {
	cbSize        uint32
	fMask         uint32
	hwnd          windows.Handle
	verb          *uint16
	file          *uint16
	parameters    *uint16
	directory     *uint16
	show          int32
	instApp       windows.Handle
	idList        uintptr
	class         *uint16
	keyClass      windows.Handle
	hotKey        uint32
	iconOrMonitor windows.Handle
	process       windows.Handle
}

var (
	shell32          = windows.NewLazySystemDLL("shell32.dll")
	procShellExecute = shell32.NewProc("ShellExecuteExW")
)

// elevationSupported reports that this platform can request elevation.
func elevationSupported() bool { return true }

// needsElevation reports whether an action mutates service state and so
// requires an elevated token. Reading status does not.
func needsElevation(action string) bool {
	switch action {
	case "install", "uninstall", "start", "stop", "restart":
		return true
	}
	return false
}

// isElevated reports whether this process already holds an elevated token.
func isElevated() bool {
	if os.Getenv(elevatedMarker) != "" {
		return true
	}
	return windows.GetCurrentProcessToken().IsElevated()
}

// relaunchElevated re-runs this executable with the same arguments via UAC and
// returns the elevated process's exit code.
func relaunchElevated() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	// ComposeCommandLine applies Windows' quoting rules, so arguments
	// containing spaces (paths, mostly) survive the round trip.
	params := windows.ComposeCommandLine(os.Args[1:])

	info := shellExecuteInfo{
		fMask: seeMaskNoCloseProcess | seeMaskNoAsync,
		verb:  windows.StringToUTF16Ptr("runas"),
		file:  windows.StringToUTF16Ptr(exe),
		show:  swShowNormal,
	}
	if params != "" {
		info.parameters = windows.StringToUTF16Ptr(params)
	}
	if cwd != "" {
		info.directory = windows.StringToUTF16Ptr(cwd)
	}
	info.cbSize = uint32(unsafe.Sizeof(info))

	ret, _, callErr := procShellExecute.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == windows.ERROR_CANCELLED {
			return 0, fmt.Errorf("elevation was declined at the UAC prompt")
		}
		return 0, fmt.Errorf("could not request elevation: %w", callErr)
	}
	if info.process == 0 {
		// Elevation was granted but no handle came back; we cannot wait, so
		// report rather than silently claim success.
		return 0, fmt.Errorf("elevated process started but its result could not be read; " +
			"re-run this command from an Administrator prompt to see the outcome")
	}
	defer windows.CloseHandle(info.process)

	if _, err := windows.WaitForSingleObject(info.process, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.process, &code); err != nil {
		return 0, err
	}
	return int(code), nil
}
