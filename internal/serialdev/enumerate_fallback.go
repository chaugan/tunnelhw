//go:build darwin && !cgo

package serialdev

import (
	"fmt"
	"strings"

	"go.bug.st/serial"
)

// enumerate is the degraded fallback for darwin binaries built without cgo
// (typically cross-compiled): macOS detailed enumeration needs IOKit via cgo.
// Ports are still fully usable; they just lack USB metadata, so fingerprints
// land in the weak tier and the UI shows its usual weak-confidence warning.
// A native macOS build (CGO_ENABLED=1) gets the full enumerator instead.
func enumerate() ([]PortInfo, error) {
	names, err := serial.GetPortsList()
	if err != nil {
		return nil, fmt.Errorf("serialdev: enumerate: %w", err)
	}
	out := make([]PortInfo, 0, len(names))
	for _, name := range names {
		// The callout device (cu.*) is the right node to open; skip the
		// matching tty.* twin so each physical port appears once.
		if strings.HasPrefix(name, "/dev/tty.") {
			continue
		}
		out = append(out, PortInfo{
			Path: name,
			// Name-based hint only: usbserial/usbmodem nodes are USB devices,
			// but without IOKit we cannot read VID/PID/serial.
			IsUSB: strings.Contains(name, "usbserial") || strings.Contains(name, "usbmodem"),
		})
	}
	return out, nil
}
