package traceloom

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

type brokenMarshaler struct{}

func (brokenMarshaler) MarshalJSON() ([]byte, error) { return nil, errors.New("boom") }

type panicStringer struct{}

func (panicStringer) String() string { panic("boom") }

type recursiveNode struct {
	Name     string         `json:"name"`
	Password string         `json:"password"`
	Next     *recursiveNode `json:"next"`
}

type taggedPayload struct {
	APIKey string `json:"apiKey"`
	Safe   string `json:"safe"`
	Hidden string `json:"-"`
	Empty  string `json:"empty,omitempty"`
}

func testSanitizer(t *testing.T, options ...Option) *Sanitizer {
	t.Helper()
	configuration, err := NewConfiguration("logs", options...)
	if err != nil {
		t.Fatal(err)
	}
	return NewSanitizer(configuration)
}

func TestSanitizerMasksSecretsRecursivelyAndInStructTags(t *testing.T) {
	result := testSanitizer(t).Sanitize(Data{
		"password": "one",
		"nested": Data{
			"apiKey":        "two",
			"X-Api-Key":     "three",
			"user_password": "four",
			"safe":          "visible",
		},
		"struct": taggedPayload{APIKey: "five", Safe: "shown", Hidden: "hidden"},
	})

	if result["password"] != Redacted {
		t.Fatalf("password leaked: %#v", result)
	}
	nested := result["nested"].(Data)
	for _, key := range []string{"apiKey", "X-Api-Key", "user_password"} {
		if nested[key] != Redacted {
			t.Fatalf("secret %s leaked: %#v", key, nested)
		}
	}
	structured := result["struct"].(Data)
	if structured["apiKey"] != Redacted || structured["safe"] != "shown" {
		t.Fatalf("struct tags were not normalized: %#v", structured)
	}
	if _, exists := structured["Hidden"]; exists {
		t.Fatalf("json:- field was included: %#v", structured)
	}
}

// Secrets are masked whatever the spelling, and innocent words that merely contain a
// secret-looking substring are not.
func TestSanitizerMasksSuffixedSecretsWithoutRedactingInnocentWords(t *testing.T) {
	secrets := []string{
		"cookies", "Cookie-Header", "authorization", "authorization_header",
		"Authorization-Bearer", "bearer", "session", "session_token", "jwt", "jwt_value",
		"access_key", "access_key_id", "secret_key", "auth", "signature",
	}
	innocent := []string{"author", "keyboard", "monkey", "email", "status", "user_id"}

	payload := Data{}
	for _, key := range append(append([]string{}, secrets...), innocent...) {
		payload[key] = "value"
	}
	result := testSanitizer(t).Sanitize(payload)

	for _, key := range secrets {
		if result[key] != Redacted {
			t.Fatalf("secret key %q leaked", key)
		}
	}
	for _, key := range innocent {
		if result[key] != "value" {
			t.Fatalf("innocent key %q was redacted", key)
		}
	}
}

// Keys are the only unbounded dimension left in a payload, so they are truncated too.
// A digest suffix keeps two long keys from collapsing into one and silently overwriting
// each other's value.
func TestSanitizerTruncatesLongKeysWithoutCollisions(t *testing.T) {
	prefix := strings.Repeat("k", 300)
	sanitized := testSanitizer(t).Sanitize(Data{
		prefix + "-one": 1,
		prefix + "-two": 2,
		"short":         3,
	})

	if len(sanitized) != 3 {
		t.Fatalf("two long keys collapsed into one: %#v", sanitized)
	}
	for key := range sanitized {
		if len(key) > DefaultMaxKeyBytes {
			t.Fatalf("key was not bounded: %d bytes", len(key))
		}
	}
	if sanitized["short"] != int64(3) {
		t.Fatalf("a short key must be untouched: %#v", sanitized)
	}

	// The mapping is deterministic: the same key truncates to the same name every time.
	again := testSanitizer(t).Sanitize(Data{prefix + "-one": 1})
	for key := range again {
		if _, matched := sanitized[key]; !matched {
			t.Fatalf("truncation is not deterministic: %q", key)
		}
	}
}

func TestSanitizerStrictModeMatchesWholeKeysOnly(t *testing.T) {
	result := testSanitizer(t, WithStrictSensitiveKeys(true)).Sanitize(Data{
		"token":         "hidden",
		"payment_token": "visible",
	})
	if result["token"] != Redacted || result["payment_token"] != "visible" {
		t.Fatalf("strict matching failed: %#v", result)
	}
}

func TestSanitizerTruncatesUTF8WithoutSplittingRune(t *testing.T) {
	result := testSanitizer(t, WithMaxStringBytes(5)).Sanitize(Data{"value": "ééé"})
	marker := result["value"].(Data)
	if marker["_truncated"] != true || marker["size_bytes"] != 6 || marker["preview"] != "éé" {
		t.Fatalf("unexpected truncation marker: %#v", marker)
	}
}

func TestSanitizerMarksInvalidUTF8AndByteSlicesAsBinary(t *testing.T) {
	result := testSanitizer(t).Sanitize(Data{
		"invalid": string([]byte{0xff, 0xfe}),
		"bytes":   []byte{1, 2, 3},
	})
	invalid := result["invalid"].(Data)
	bytesMarker := result["bytes"].(Data)
	if invalid["preview"] != "fffe" || bytesMarker["preview"] != "010203" {
		t.Fatalf("unexpected binary markers: %#v %#v", invalid, bytesMarker)
	}
}

func TestSanitizerDetectsCircularMapsAndPointers(t *testing.T) {
	circular := Data{}
	circular["self"] = circular
	node := &recursiveNode{Name: "root", Password: "secret"}
	node.Next = node

	result := testSanitizer(t).Sanitize(Data{"map": circular, "node": node})
	if result["map"].(Data)["self"] != CircularReference {
		t.Fatalf("map cycle missed: %#v", result)
	}
	normalizedNode := result["node"].(Data)
	if normalizedNode["next"] != CircularReference || normalizedNode["password"] != Redacted {
		t.Fatalf("pointer cycle or secret missed: %#v", normalizedNode)
	}
}

func TestSanitizerStopsAtMaxDepthAndGlobalItemBudget(t *testing.T) {
	depth := testSanitizer(t, WithMaxDepth(2)).Sanitize(Data{
		"one": Data{"two": Data{"three": true}},
	})
	if depth["one"].(Data)["two"] != MaxDepthExceeded {
		t.Fatalf("depth marker missing: %#v", depth)
	}

	budget := testSanitizer(t, WithMaxPayloadNodes(3)).Sanitize(Data{
		"first":  Data{"a": 1, "b": 2},
		"second": 2,
		"third":  3,
	})
	if budget["_omitted_items"] != 2 {
		t.Fatalf("global budget mismatch: %#v", budget)
	}
}

// Each array is capped on its own, so one long array no longer starves the fields beside it.
func TestSanitizerCapsEachArraySeparatelyFromThePayloadBudget(t *testing.T) {
	sanitized := testSanitizer(t, WithMaxArrayItems(2)).Sanitize(Data{
		"items": []int{1, 2, 3, 4, 5},
		"kept":  "still here",
	})

	items, ok := sanitized["items"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("array cap mismatch: %#v", sanitized["items"])
	}
	if items[0] != int64(1) || items[1] != int64(2) {
		t.Fatalf("array items mismatch: %#v", items)
	}
	if omitted, isMarker := items[2].(Data); !isMarker || omitted["_omitted_items"] != 3 {
		t.Fatalf("array omission marker missing: %#v", items[2])
	}
	if sanitized["kept"] != "still here" {
		t.Fatalf("a long array starved the field beside it: %#v", sanitized)
	}
	if _, truncated := sanitized["_omitted_items"]; truncated {
		t.Fatalf("array cap must not consume the payload budget: %#v", sanitized)
	}
}

// The payload budget still bounds a payload as a whole, across every array in it.
func TestSanitizerPayloadBudgetBoundsTheWholePayload(t *testing.T) {
	// Budget of 5: the "first" key, its three items, then the "second" key exhaust it.
	sanitized := testSanitizer(t, WithMaxArrayItems(100), WithMaxPayloadNodes(5)).Sanitize(Data{
		"first":  []int{1, 2, 3},
		"second": []int{4, 5, 6},
	})

	first, _ := sanitized["first"].([]any)
	second, _ := sanitized["second"].([]any)
	if len(first) != 3 || len(second) != 1 {
		t.Fatalf("payload budget mismatch: %#v", sanitized)
	}
	if omitted, isMarker := second[0].(Data); !isMarker || omitted["_omitted_items"] != 3 {
		t.Fatalf("exhausted budget must mark the remainder: %#v", second)
	}
}

// A payload may not spell a sanitizer marker: it could neither be forged nor overwritten.
func TestSanitizerEscapesPayloadKeysThatSpellMarkers(t *testing.T) {
	sanitized := testSanitizer(t).Sanitize(Data{
		"_truncated":      true,
		"_binary":         "forged",
		"_encoding_error": "nothing happened",
		"_omitted_items":  999,
		"__truncated":     "already escaped",
		"regular":         "kept",
	})

	expected := Data{
		"__truncated":      true,
		"__binary":         "forged",
		"__encoding_error": "nothing happened",
		"__omitted_items":  int64(999),
		"___truncated":     "already escaped",
		"regular":          "kept",
	}
	for key, value := range expected {
		if sanitized[key] != value {
			t.Fatalf("key %q mismatch: got %#v, want %#v", key, sanitized[key], value)
		}
	}
	for _, marker := range []string{"_truncated", "_binary", "_encoding_error", "_omitted_items"} {
		if _, forged := sanitized[marker]; forged {
			t.Fatalf("payload forged the %q marker: %#v", marker, sanitized)
		}
	}
}

func TestSanitizerHandlesSerializersUnsupportedValuesAndNonFiniteFloats(t *testing.T) {
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	result := testSanitizer(t).Sanitize(Data{
		"broken":   brokenMarshaler{},
		"panic":    panicStringer{},
		"function": func() {},
		"channel":  make(chan int),
		"complex":  complex(1, 2),
		"nan":      math.NaN(),
		"infinity": math.Inf(1),
		"time":     now,
	})
	if result["broken"] != "[SERIALIZATION_FAILED: traceloom.brokenMarshaler]" {
		t.Fatalf("broken marshaler mismatch: %#v", result["broken"])
	}
	if result["panic"] != "[SERIALIZATION_FAILED: traceloom.panicStringer]" {
		t.Fatalf("panic stringer mismatch: %#v", result["panic"])
	}
	if result["function"] != "[UNSUPPORTED_TYPE: func()]" || result["channel"] != "[UNSUPPORTED_TYPE: chan int]" {
		t.Fatalf("unsupported markers mismatch: %#v", result)
	}
	if result["nan"] != "NaN" || result["infinity"] != "+Inf" {
		t.Fatalf("non-finite floats mismatch: %#v", result)
	}
	if result["time"] != "2026-07-10T10:00:00Z" {
		t.Fatalf("time serialization mismatch: %#v", result["time"])
	}
}
