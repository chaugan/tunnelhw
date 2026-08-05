// Package mux pins the yamux configuration for both ends of the tunnel.
// Defaults are never accepted implicitly (design review finding: pin
// keepalive and window sizes from day one).
package mux

import (
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

// Config is the pinned yamux configuration shared by agent and relay.
func Config() *yamux.Config {
	return &yamux.Config{
		AcceptBacklog:          64,
		EnableKeepAlive:        true,
		KeepAliveInterval:      30 * time.Second,
		ConnectionWriteTimeout: 15 * time.Second,
		MaxStreamWindowSize:    256 * 1024,
		StreamOpenTimeout:      15 * time.Second,
		StreamCloseTimeout:     10 * time.Second,
		LogOutput:              io.Discard,
	}
}

// Client wraps the dialing side (the agent).
func Client(conn net.Conn) (*yamux.Session, error) { return yamux.Client(conn, Config()) }

// Server wraps the accepting side (the relay).
func Server(conn net.Conn) (*yamux.Session, error) { return yamux.Server(conn, Config()) }
