package traceloom

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

var traceIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

// GenerateTraceID returns a cryptographically random 32-character lowercase hex ID.
func GenerateTraceID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

// SanitizeTraceID returns the accepted ID and true, or an empty string and false.
func SanitizeTraceID(traceID string) (string, bool) {
	trimmed := strings.TrimSpace(traceID)
	if !traceIDPattern.MatchString(trimmed) {
		return "", false
	}
	return trimmed, true
}
