//go:build !windows

package sshtun

import (
	"errors"
	"net"
	"os"
)

func agentAddr() string { return os.Getenv("SSH_AUTH_SOCK") }

func dialAgent() (net.Conn, error) {
	sock := agentAddr()
	if sock == "" {
		return nil, errors.New("SSH_AUTH_SOCK is not set")
	}
	return net.Dial("unix", sock)
}
