//go:build !windows

package svc

import "errors"

// Unix service managers report a plain permission error and the user re-runs
// under sudo, so there is nothing to escalate to here.

func elevationSupported() bool { return false }

func needsElevation(string) bool { return false }

func isElevated() bool { return true }

func relaunchElevated() (int, error) {
	return 0, errors.New("elevation is only available on Windows")
}
