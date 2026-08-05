// Package names assigns human-readable word-pair handles to devices.
//
// A handle is "adjective-noun" (e.g. "amber-falcon"), unique per agent. On
// the rare collision-exhaustion a third word (a second noun) is appended.
// Handles are presentation-layer only: the durable identity of a device is
// its UUID, and knowing a handle grants nothing by itself.
package names

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// Space returns the number of distinct two-word handles available.
func Space() int { return len(adjectives) * len(nouns) }

// Valid reports whether s looks like a handle this package could have
// generated (two or three known words joined by hyphens).
func Valid(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	if !contains(adjectives, parts[0]) || !contains(nouns, parts[1]) {
		return false
	}
	if len(parts) == 3 && !contains(nouns, parts[2]) {
		return false
	}
	return true
}

func contains(list []string, w string) bool {
	for _, x := range list {
		if x == w {
			return true
		}
	}
	return false
}

func pick(list []string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		return "", err
	}
	return list[n.Int64()], nil
}

// Generate returns a new handle not present in taken. It tries two-word
// pairs a bounded number of times, then falls back to appending a third
// word rather than looping forever in a crowded namespace.
func Generate(taken func(string) bool) (string, error) {
	const tries = 64
	for i := 0; i < tries; i++ {
		a, err := pick(adjectives)
		if err != nil {
			return "", err
		}
		n, err := pick(nouns)
		if err != nil {
			return "", err
		}
		id := a + "-" + n
		if !taken(id) {
			return id, nil
		}
	}
	for i := 0; i < tries; i++ {
		a, err := pick(adjectives)
		if err != nil {
			return "", err
		}
		n1, err := pick(nouns)
		if err != nil {
			return "", err
		}
		n2, err := pick(nouns)
		if err != nil {
			return "", err
		}
		if n1 == n2 {
			continue
		}
		id := a + "-" + n1 + "-" + n2
		if !taken(id) {
			return id, nil
		}
	}
	return "", fmt.Errorf("names: could not find a free handle after %d tries", tries*2)
}
