//go:build windows

package sshtun

import (
	"net"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
)

// windowsAgentPipe is where the OpenSSH-for-Windows agent listens. Unlike
// unix, there is no SSH_AUTH_SOCK by default — the pipe path is fixed.
const windowsAgentPipe = `\\.\pipe\openssh-ssh-agent`

func agentAddr() string {
	if s := os.Getenv("SSH_AUTH_SOCK"); s != "" {
		return s
	}
	return windowsAgentPipe
}

func dialAgent() (net.Conn, error) {
	timeout := 2 * time.Second
	return winio.DialPipe(agentAddr(), &timeout)
}
