package sshtun

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// hostConfig is the subset of ~/.ssh/config TunnelHW honours.
type hostConfig struct {
	HostName      string
	User          string
	Port          string
	IdentityFiles []string
}

// sshConfigPath is the user's OpenSSH client config.
func sshConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "config")
}

// lookupSSHConfig resolves alias against ~/.ssh/config the way OpenSSH does
// for the handful of keywords we care about: first obtained value wins, and
// a keyword applies when the alias matches one of the enclosing Host
// patterns. Users routinely keep the real hostname and the right key behind
// a short alias, so ignoring this file means rejecting credentials that
// plainly work from a terminal.
func lookupSSHConfig(alias string) hostConfig {
	return lookupSSHConfigIn(sshConfigPath(), alias)
}

func lookupSSHConfigIn(path, alias string) hostConfig {
	var out hostConfig
	if path == "" || alias == "" {
		return out
	}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()

	// Strip any :port the caller typed before matching Host patterns.
	if h, _, ok := splitHostPort(alias); ok {
		alias = h
	}

	applies := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := splitDirective(line)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "host":
			applies = false
			for _, pattern := range strings.Fields(value) {
				if strings.HasPrefix(pattern, "!") {
					if matchPattern(strings.TrimPrefix(pattern, "!"), alias) {
						applies = false
						break
					}
					continue
				}
				if matchPattern(pattern, alias) {
					applies = true
				}
			}
		case "match":
			// Conditional blocks need full OpenSSH evaluation; skip rather
			// than guess and apply the wrong identity.
			applies = false
		case "hostname":
			if applies && out.HostName == "" {
				out.HostName = value
			}
		case "user":
			if applies && out.User == "" {
				out.User = value
			}
		case "port":
			if applies && out.Port == "" {
				out.Port = value
			}
		case "identityfile":
			if applies {
				out.IdentityFiles = append(out.IdentityFiles, expandHome(unquote(value)))
			}
		}
	}
	return out
}

// splitDirective handles both "Key value" and "Key=value" forms.
func splitDirective(line string) (key, value string, ok bool) {
	if i := strings.IndexAny(line, " \t="); i > 0 {
		key = line[:i]
		value = strings.TrimSpace(strings.TrimLeft(line[i:], " \t="))
		return key, value, value != ""
	}
	return "", "", false
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// expandHome resolves a leading ~ (and OpenSSH's %d) to the home directory.
func expandHome(p string) string {
	if p == "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	switch {
	case strings.HasPrefix(p, "~/"), strings.HasPrefix(p, `~\`):
		return filepath.Join(home, p[2:])
	case p == "~":
		return home
	case strings.HasPrefix(p, "%d"):
		return filepath.Join(home, p[2:])
	}
	return p
}

// matchPattern implements OpenSSH host globbing: * and ?.
func matchPattern(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	return globMatch(pattern, s)
}

// globMatch is a plain recursive glob supporting * and ?, which is all
// ssh_config Host patterns use.
func globMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			// Collapse runs of '*' then try every split point.
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if pattern == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		default:
			if s == "" || s[0] != pattern[0] {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		}
	}
	return s == ""
}

// splitHostPort tolerates a bare host (no port) unlike net.SplitHostPort.
func splitHostPort(addr string) (host, port string, ok bool) {
	i := strings.LastIndex(addr, ":")
	if i < 0 || strings.Contains(addr[i+1:], "]") {
		return addr, "", false
	}
	// An IPv6 literal without brackets has several colons; leave it alone.
	if strings.Count(addr, ":") > 1 && !strings.HasPrefix(addr, "[") {
		return addr, "", false
	}
	return strings.Trim(addr[:i], "[]"), addr[i+1:], true
}
