package traceloom

import (
	"regexp"
	"testing"
)

func TestGenerateTraceID(t *testing.T) {
	first, err := GenerateTraceID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateTraceID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(first) {
		t.Fatalf("invalid generated ID: %q", first)
	}
	if first == second {
		t.Fatal("generated IDs must differ")
	}
}

func TestSanitizeTraceID(t *testing.T) {
	if value, ok := SanitizeTraceID("  request:123_abc  "); !ok || value != "request:123_abc" {
		t.Fatalf("valid ID was rejected: %q %v", value, ok)
	}
	invalid := []string{"", "short", "contains space", "trace\nforged", string(make([]byte, 129))}
	for _, value := range invalid {
		if _, ok := SanitizeTraceID(value); ok {
			t.Fatalf("invalid ID was accepted: %q", value)
		}
	}
}
