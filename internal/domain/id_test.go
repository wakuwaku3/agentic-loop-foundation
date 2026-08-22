package domain

import "testing"

func TestNewIDRejectsBlank(t *testing.T) {
	for _, value := range []string{"", " ", "\t"} {
		if _, err := NewID(value); err == nil {
			t.Fatalf("NewID(%q) accepted blank", value)
		}
	}
}

func TestFencingTokenMonotonicity(t *testing.T) {
	next, err := FencingToken(41).Next()
	if err != nil || next != 42 {
		t.Fatalf("got %d, %v", next, err)
	}
	if !FencingToken(42).Allows(42) || FencingToken(42).Allows(41) {
		t.Fatal("stale token was allowed")
	}
}

func TestFencingTokenOverflow(t *testing.T) {
	if _, err := (^FencingToken(0)).Next(); err == nil {
		t.Fatal("overflow accepted")
	}
}
