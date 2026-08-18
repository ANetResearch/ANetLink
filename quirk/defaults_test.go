package quirk

import "testing"

// TestShippedDefaultsAreValid: the defaults must load, and every one must
// carry a stated reason — a correction nobody can justify is one nobody can
// review six months later.
func TestShippedDefaultsAreValid(t *testing.T) {
	r := NewRegistry()
	n, err := r.LoadDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no defaults shipped")
	}
	for _, q := range r.All() {
		if q.Note == "" {
			t.Errorf("%s has no note explaining why it exists", q.ID)
		}
		if err := q.Validate(); err != nil {
			t.Errorf("%s: %v", q.ID, err)
		}
	}
	t.Logf("%d shipped quirks", n)
}
