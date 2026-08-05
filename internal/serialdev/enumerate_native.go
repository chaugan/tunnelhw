//go:build !darwin || cgo

package serialdev

import (
	"fmt"
	"strings"

	"go.bug.st/serial/enumerator"
)

// enumerate uses the platform's detailed port list, including USB VID/PID,
// serial number, and product string.
func enumerate() ([]PortInfo, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, fmt.Errorf("serialdev: enumerate: %w", err)
	}
	out := make([]PortInfo, 0, len(ports))
	for _, p := range ports {
		out = append(out, PortInfo{
			Path:         p.Name,
			IsUSB:        p.IsUSB,
			VID:          strings.ToLower(p.VID),
			PID:          strings.ToLower(p.PID),
			SerialNumber: p.SerialNumber,
			Product:      p.Product,
		})
	}
	return out, nil
}
