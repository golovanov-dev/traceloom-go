package traceloom

import (
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	MaxDepthExceeded   = "[MAX_DEPTH_EXCEEDED]"
	CircularReference  = "[CIRCULAR_REFERENCE]"
	binaryPreviewBytes = 32
	// A truncated key ends with this separator and a digest of the original key.
	keyDigestSeparator = "~"
	keyDigestBytes     = 8
)

type visit struct {
	typeOf  reflect.Type
	pointer uintptr
}

type sanitizeState struct {
	budget int
	seen   map[visit]struct{}
}

// Sanitizer normalizes one event payload without retaining per-call mutable state.
// It is safe for concurrent use.
type Sanitizer struct {
	configuration Configuration
}

func NewSanitizer(configuration Configuration) *Sanitizer {
	return &Sanitizer{configuration: configuration}
}

func (sanitizer *Sanitizer) Sanitize(data Data) (result Data) {
	if data == nil {
		return Data{}
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			result = Data{"_encoding_error": fmt.Sprintf("payload sanitization panic: %v", recovered)}
		}
	}()

	state := &sanitizeState{
		budget: sanitizer.configuration.maxPayloadNodes,
		seen:   make(map[visit]struct{}),
	}

	root := reflect.ValueOf(data)
	if reference, ok := referenceOf(root); ok {
		state.seen[reference] = struct{}{}
	}

	normalized := sanitizer.normalizeStringMap(root, 1, state)
	return normalized
}

func (sanitizer *Sanitizer) normalize(value reflect.Value, depth int, state *sanitizeState) any {
	if !value.IsValid() {
		return nil
	}

	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}

	if isNilable(value.Kind()) && value.IsNil() {
		return nil
	}

	if value.Kind() == reflect.String {
		if value.Type() == reflect.TypeOf(json.Number("")) {
			number := json.Number(value.String())
			if _, err := number.Float64(); err == nil {
				return number
			}
		}
		return sanitizer.normalizeString(value.String())
	}

	switch value.Kind() {
	case reflect.Bool:
		return value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint()
	case reflect.Float32, reflect.Float64:
		float := value.Float()
		if math.IsNaN(float) || math.IsInf(float, 0) {
			return fmt.Sprint(float)
		}
		return float
	}

	if bytesValue, ok := byteSequence(value); ok {
		return binaryMarker(bytesValue)
	}

	if depth > sanitizer.configuration.maxDepth {
		return MaxDepthExceeded
	}

	if normalized, ok := sanitizer.normalizeViaInterfaces(value, depth, state); ok {
		return normalized
	}

	if reference, ok := referenceOf(value); ok {
		if _, exists := state.seen[reference]; exists {
			return CircularReference
		}
		state.seen[reference] = struct{}{}
		defer delete(state.seen, reference)
	}

	switch value.Kind() {
	case reflect.Pointer:
		return sanitizer.normalize(value.Elem(), depth, state)
	case reflect.Map:
		if value.Type().Key().Kind() == reflect.String {
			return sanitizer.normalizeStringMap(value, depth, state)
		}
		return sanitizer.normalizeViaJSON(value, depth, state)
	case reflect.Slice, reflect.Array:
		return sanitizer.normalizeSlice(value, depth, state)
	case reflect.Struct:
		return sanitizer.normalizeStruct(value, depth, state)
	case reflect.Invalid:
		return nil
	default:
		return fmt.Sprintf("[UNSUPPORTED_TYPE: %s]", value.Type())
	}
}

func (sanitizer *Sanitizer) normalizeStringMap(value reflect.Value, depth int, state *sanitizeState) Data {
	result := make(Data)
	keys := value.MapKeys()
	sort.Slice(keys, func(left, right int) bool { return keys[left].String() < keys[right].String() })
	omitted := 0

	for index, key := range keys {
		if state.budget <= 0 {
			omitted = len(keys) - index
			break
		}
		state.budget--

		name := key.String()
		if sanitizer.isSensitiveKey(name) {
			sanitizer.assign(result, name, Redacted)
			continue
		}
		sanitizer.assign(result, name, sanitizer.normalize(value.MapIndex(key), depth+1, state))
	}

	if omitted > 0 {
		result["_omitted_items"] = omitted
	}
	return result
}

func (sanitizer *Sanitizer) normalizeSlice(value reflect.Value, depth int, state *sanitizeState) []any {
	capacity := value.Len()
	if capacity > sanitizer.configuration.maxArrayItems {
		capacity = sanitizer.configuration.maxArrayItems + 1
	}
	result := make([]any, 0, capacity)
	omitted := 0

	for index := 0; index < value.Len(); index++ {
		// Two independent limits: this array's own length, and the node budget for
		// the payload as a whole, which stops a wide or deeply nested bomb.
		if index >= sanitizer.configuration.maxArrayItems || state.budget <= 0 {
			omitted = value.Len() - index
			break
		}
		state.budget--
		result = append(result, sanitizer.normalize(value.Index(index), depth+1, state))
	}

	if omitted > 0 {
		result = append(result, Data{"_omitted_items": omitted})
	}
	return result
}

func (sanitizer *Sanitizer) normalizeStruct(value reflect.Value, depth int, state *sanitizeState) Data {
	typeOf := value.Type()
	fields := make(map[string]reflect.Value)
	omitEmpty := make(map[string]bool)

	for _, field := range reflect.VisibleFields(typeOf) {
		if field.PkgPath != "" {
			continue
		}

		tag := field.Tag.Get("json")
		name, options := parseJSONTag(tag)
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" && embeddedStruct(field.Type) {
			continue
		}
		if name == "" {
			name = field.Name
		}

		fieldValue, err := value.FieldByIndexErr(field.Index)
		if err != nil {
			continue
		}
		fields[name] = fieldValue
		omitEmpty[name] = options["omitempty"]
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	filtered := keys[:0]
	for _, key := range keys {
		if omitEmpty[key] && fields[key].IsZero() {
			continue
		}
		filtered = append(filtered, key)
	}
	keys = filtered

	result := make(Data)
	omitted := 0
	for index, key := range keys {
		field := fields[key]
		if state.budget <= 0 {
			omitted = len(keys) - index
			break
		}
		state.budget--
		if sanitizer.isSensitiveKey(key) {
			sanitizer.assign(result, key, Redacted)
			continue
		}
		sanitizer.assign(result, key, sanitizer.normalize(field, depth+1, state))
	}
	if omitted > 0 {
		result["_omitted_items"] = omitted
	}
	return result
}

func (sanitizer *Sanitizer) normalizeViaInterfaces(value reflect.Value, depth int, state *sanitizeState) (any, bool) {
	if !value.CanInterface() {
		return nil, false
	}
	interfaceValue := value.Interface()

	if marshaler, ok := interfaceValue.(json.Marshaler); ok {
		encoded, err := safeMarshalJSON(marshaler)
		if err != nil {
			return serializationFailed(value), true
		}
		return sanitizer.normalizeJSONBytes(encoded, depth, state, value), true
	}
	if marshaler, ok := interfaceValue.(encoding.TextMarshaler); ok {
		encoded, err := safeMarshalText(marshaler)
		if err != nil {
			return serializationFailed(value), true
		}
		return sanitizer.normalizeString(string(encoded)), true
	}
	if stringer, ok := interfaceValue.(fmt.Stringer); ok {
		text, err := safeString(stringer)
		if err != nil {
			return serializationFailed(value), true
		}
		return sanitizer.normalizeString(text), true
	}
	return nil, false
}

func (sanitizer *Sanitizer) normalizeViaJSON(value reflect.Value, depth int, state *sanitizeState) any {
	if !value.CanInterface() {
		return fmt.Sprintf("[UNSUPPORTED_TYPE: %s]", value.Type())
	}
	encoded, err := safeMarshalAny(value.Interface())
	if err != nil {
		return serializationFailed(value)
	}
	return sanitizer.normalizeJSONBytes(encoded, depth, state, value)
}

func (sanitizer *Sanitizer) normalizeJSONBytes(encoded []byte, depth int, state *sanitizeState, original reflect.Value) any {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return serializationFailed(original)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return serializationFailed(original)
	}
	return sanitizer.normalize(reflect.ValueOf(decoded), depth, state)
}

func (sanitizer *Sanitizer) normalizeString(value string) any {
	if !utf8.ValidString(value) {
		return binaryMarker([]byte(value))
	}
	if len(value) <= sanitizer.configuration.maxStringBytes {
		return value
	}
	return Data{
		"_truncated": true,
		"size_bytes": len(value),
		"preview":    truncateUTF8(value, sanitizer.configuration.maxStringBytes),
	}
}

func (sanitizer *Sanitizer) isSensitiveKey(key string) bool {
	canonical := CanonicalizeKey(key)
	if canonical == "" {
		return false
	}
	if _, exists := sanitizer.configuration.sensitiveKeySet[canonical]; exists {
		return true
	}
	if sanitizer.configuration.strictSensitiveKeys {
		return false
	}
	for _, fragment := range sensitiveKeyFragments {
		if strings.Contains(canonical, fragment) {
			return true
		}
	}
	return false
}

// reservedKeyPattern matches the keys the sanitizer writes itself. A payload may not
// spell them: a record carrying its own `_truncated` would be indistinguishable from
// one the sanitizer produced, which is both a reporting bug and a way to forge a log.
var reservedKeyPattern = regexp.MustCompile(`^_+(binary|truncated|encoding_error|omitted_items)$`)

// assign writes a payload key, bounding its length and escaping one that would be
// mistaken for a sanitizer marker. A colliding key gains a leading underscore, which is
// itself escaped on the way in, so the mapping stays unambiguous.
func (sanitizer *Sanitizer) assign(target Data, key string, value any) {
	key = sanitizer.normalizeKey(key)
	if reservedKeyPattern.MatchString(key) {
		target["_"+key] = value
		return
	}
	target[key] = value
}

// normalizeKey bounds a key the way normalizeString bounds a value. Keys are the only
// unbounded dimension left in a payload, and one long key is enough to push a record
// past maxRecordBytes and degrade the whole event to _encoding_error.
//
// Plain truncation would let two distinct long keys collapse into one and silently
// overwrite each other's value, so a truncated key carries a digest of the original.
// The mapping stays collision-free and deterministic across processes and languages.
func (sanitizer *Sanitizer) normalizeKey(key string) string {
	if len(key) <= sanitizer.configuration.maxKeyBytes {
		return key
	}
	digest := sha256.Sum256([]byte(key))
	suffix := keyDigestSeparator + hex.EncodeToString(digest[:keyDigestBytes])
	return truncateUTF8(key, sanitizer.configuration.maxKeyBytes-len(suffix)) + suffix
}

func parseJSONTag(tag string) (string, map[string]bool) {
	parts := strings.Split(tag, ",")
	options := make(map[string]bool, len(parts)-1)
	for _, option := range parts[1:] {
		options[option] = true
	}
	return parts[0], options
}

func embeddedStruct(typeOf reflect.Type) bool {
	if typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	return typeOf.Kind() == reflect.Struct
}

func referenceOf(value reflect.Value) (visit, bool) {
	switch value.Kind() {
	case reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return visit{}, false
		}
		return visit{typeOf: value.Type(), pointer: value.Pointer()}, true
	default:
		return visit{}, false
	}
}

func byteSequence(value reflect.Value) ([]byte, bool) {
	if (value.Kind() != reflect.Slice && value.Kind() != reflect.Array) || value.Type().Elem().Kind() != reflect.Uint8 {
		return nil, false
	}
	bytesValue := make([]byte, value.Len())
	for index := range bytesValue {
		bytesValue[index] = byte(value.Index(index).Uint())
	}
	return bytesValue, true
}

func binaryMarker(value []byte) Data {
	preview := value
	if len(preview) > binaryPreviewBytes {
		preview = preview[:binaryPreviewBytes]
	}
	return Data{
		"_binary":    true,
		"size_bytes": len(value),
		"preview":    hex.EncodeToString(preview),
	}
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func serializationFailed(value reflect.Value) string {
	return fmt.Sprintf("[SERIALIZATION_FAILED: %s]", value.Type())
}

func safeMarshalJSON(marshaler json.Marshaler) (data []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("MarshalJSON panic: %v", recovered)
		}
	}()
	return marshaler.MarshalJSON()
}

func safeMarshalText(marshaler encoding.TextMarshaler) (data []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("MarshalText panic: %v", recovered)
		}
	}()
	return marshaler.MarshalText()
}

func safeString(stringer fmt.Stringer) (text string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("String panic: %v", recovered)
		}
	}()
	return stringer.String(), nil
}

func safeMarshalAny(value any) (data []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("json marshal panic: %v", recovered)
		}
	}()
	return json.Marshal(value)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return err
}

func isNilable(kind reflect.Kind) bool {
	switch kind {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return true
	default:
		return false
	}
}
