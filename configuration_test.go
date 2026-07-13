package traceloom

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestConfigurationValidatesAndClampsLimits(t *testing.T) {
	_, err := NewConfiguration("   ")
	var configurationErr *ConfigurationError
	if !errors.As(err, &configurationErr) {
		t.Fatalf("expected ConfigurationError, got %v", err)
	}

	_, err = NewConfiguration("logs", WithMaxDepth(0))
	if !errors.As(err, &configurationErr) {
		t.Fatalf("expected max-depth error, got %v", err)
	}

	configuration, err := NewConfiguration(
		"logs/",
		WithMaxFileBytes(500),
		WithMaxRecordBytes(100_000),
		WithMaxStringBytes(100_000),
	)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.LogDirectory() != filepath.Clean("logs") {
		t.Fatalf("unexpected directory: %q", configuration.LogDirectory())
	}
	if configuration.MaxRecordBytes() != 500 || configuration.MaxStringBytes() != 500 {
		t.Fatalf("limits were not clamped: record=%d string=%d", configuration.MaxRecordBytes(), configuration.MaxStringBytes())
	}
}

func TestConfigurationPreservesFilesystemRoot(t *testing.T) {
	root := string(filepath.Separator)
	configuration, err := NewConfiguration(root)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.LogDirectory() != filepath.Clean(root) {
		t.Fatalf("filesystem root changed: %q", configuration.LogDirectory())
	}
}

func TestSensitiveKeysAreCanonicalAndOptionsAreCloned(t *testing.T) {
	keys := []string{"payment-token"}
	configuration, err := NewConfiguration("logs", WithSensitiveKeys(keys...))
	if err != nil {
		t.Fatal(err)
	}
	keys[0] = "changed"

	if CanonicalizeKey("X-Api-Key") != "xapikey" || CanonicalizeKey("apiKey") != "apikey" {
		t.Fatal("key canonicalization mismatch")
	}
	set := make(map[string]bool)
	for _, key := range configuration.SensitiveKeys() {
		set[key] = true
	}
	if !set["paymenttoken"] || !set["xapikey"] || set["changed"] {
		t.Fatalf("unexpected sensitive keys: %#v", set)
	}
}

func TestConfigurationRejectsNegativeRetention(t *testing.T) {
	_, err := NewConfiguration("logs", WithRetentionDays(-1))
	if err == nil {
		t.Fatal("expected retention error")
	}
}
