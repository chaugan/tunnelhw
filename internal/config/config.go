// Package config persists agent state: relay pairing, device identity map,
// and expose-list. Writes are atomic (temp file + rename) and private (0600):
// the credential lives here on platforms without a keychain integration yet.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chaugan/tunnelhw/internal/sshtun"
)

// DeviceRecord is the durable state for one fingerprinted device.
type DeviceRecord struct {
	UUID              string `json:"uuid"`
	WordID            string `json:"word_id"`
	Exposed           bool   `json:"exposed"`
	AllowControlLines bool   `json:"allow_control_lines"`
	// AssertLinesOnOpen raises DTR/RTS at open. Off by default because it
	// resets auto-reset boards; some USB-CDC devices need it to transmit.
	AssertLinesOnOpen bool `json:"assert_lines_on_open,omitempty"`
	// Monitored keeps the port open continuously and records output, so that
	// opening a session does not reopen the port. On hardware that resets when
	// the port is opened, this is the difference between one reset and one per
	// access. It also preserves output emitted between sessions.
	Monitored bool `json:"monitored,omitempty"`
	// MonitorBaud is the line rate monitoring uses, since no session is
	// present to supply one.
	MonitorBaud int `json:"monitor_baud,omitempty"`
}

// Config is the agent's persisted state. Note there is deliberately no
// persisted insecure-dev flag: permitting plaintext relay URLs is a
// per-process decision (--insecure-dev) that must be re-made every launch,
// never remembered (design review: a sticky flag silently downgrades TLS
// forever).
type Config struct {
	RelayURL   string                  `json:"relay_url,omitempty"`
	AgentID    string                  `json:"agent_id,omitempty"`
	Credential string                  `json:"credential,omitempty"`
	UIListen   string                  `json:"ui_listen,omitempty"`
	Devices    map[string]DeviceRecord `json:"devices"` // key: fingerprint key

	// SSH, when set, makes the agent reach the relay through an SSH server
	// instead of over the open network. RelayURL is then resolved *on the
	// SSH host*, so a relay bound to its loopback (ws://127.0.0.1:8443/ws)
	// needs no public address at all.
	SSH *sshtun.Config `json:"ssh,omitempty"`
}

// DefaultUIListen is loopback-only by design; see ARCHITECTURE.md §7.
const DefaultUIListen = "127.0.0.1:8787"

// Dir returns the per-user config directory, creating it if needed.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "tunnelhw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func pathIn(dir string) string { return filepath.Join(dir, "agent.json") }

// Load reads the config from dir, returning a usable zero config if absent.
func Load(dir string) (*Config, error) {
	c := &Config{Devices: map[string]DeviceRecord{}, UIListen: DefaultUIListen}
	raw, err := os.ReadFile(pathIn(dir))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", pathIn(dir), err)
	}
	if c.Devices == nil {
		c.Devices = map[string]DeviceRecord{}
	}
	if c.UIListen == "" {
		c.UIListen = DefaultUIListen
	}
	return c, nil
}

// Save writes the config atomically with private permissions.
func Save(dir string, c *Config) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "agent-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, pathIn(dir))
}
