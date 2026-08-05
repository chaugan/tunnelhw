package names

import (
	"strings"
	"testing"
)

func TestWordlistHygiene(t *testing.T) {
	for _, list := range [][]string{adjectives, nouns} {
		seen := map[string]bool{}
		for _, w := range list {
			if w != strings.ToLower(w) || strings.ContainsAny(w, " -_") {
				t.Errorf("word %q: must be lowercase, no separators", w)
			}
			if len(w) < 3 || len(w) > 8 {
				t.Errorf("word %q: length %d outside 3..8", w, len(w))
			}
			if seen[w] {
				t.Errorf("duplicate word %q", w)
			}
			seen[w] = true
		}
	}
	if Space() < 20000 {
		t.Errorf("handle space %d too small", Space())
	}
}

func TestGenerateUniqueAndValid(t *testing.T) {
	taken := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := Generate(func(s string) bool { return taken[s] })
		if err != nil {
			t.Fatal(err)
		}
		if taken[id] {
			t.Fatalf("duplicate handle %q", id)
		}
		if !Valid(id) {
			t.Fatalf("generated invalid handle %q", id)
		}
		taken[id] = true
	}
}

func TestGenerateFallsBackToThirdWord(t *testing.T) {
	// Everything two-word is taken; only three-word handles remain free.
	id, err := Generate(func(s string) bool { return strings.Count(s, "-") == 1 })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(id, "-") != 2 {
		t.Fatalf("expected three-word fallback, got %q", id)
	}
	if !Valid(id) {
		t.Fatalf("fallback handle %q not valid", id)
	}
}

func TestValidRejectsJunk(t *testing.T) {
	for _, s := range []string{"", "amber", "amber-falcon-", "notaword-falcon", "amber-notaword", "amber_falcon", "amber-falcon-notaword"} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}
