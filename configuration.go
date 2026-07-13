package traceloom

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// DefaultMaxFileBytes is the soft size limit for one JSONL shard.
	DefaultMaxFileBytes int64 = 50 * 1024 * 1024
	// DefaultMaxStringBytes is the UTF-8 byte limit for one payload string.
	DefaultMaxStringBytes = 64 * 1024
	// DefaultMaxKeyBytes is the UTF-8 byte limit for one payload key.
	DefaultMaxKeyBytes = 256
	// minMaxKeyBytes leaves room for the digest that keeps a truncated key unique.
	minMaxKeyBytes = 32
	// DefaultMaxRecordBytes is the byte limit for one JSONL record.
	DefaultMaxRecordBytes = 256 * 1024
	// DefaultMaxArrayItems is the item limit applied to each array on its own.
	DefaultMaxArrayItems = 1000
	// DefaultMaxPayloadNodes is the node budget shared by an event payload as a whole.
	DefaultMaxPayloadNodes = 10000
	// DefaultMaxDepth limits recursive payload traversal.
	DefaultMaxDepth = 16
	// DefaultDirectoryMode is used for newly created log directories on POSIX.
	DefaultDirectoryMode fs.FileMode = 0o750
	// DefaultFileMode is used for newly created log shards on POSIX.
	DefaultFileMode fs.FileMode = 0o640
	// Redacted replaces values stored under sensitive keys.
	Redacted = "[REDACTED]"
)

var defaultSensitiveKeys = []string{
	"password", "passwd", "pwd", "token", "access_token", "refresh_token",
	"authorization", "proxy_authorization", "cookie", "set_cookie", "api_key",
	"x_api_key", "secret", "client_secret", "private_key", "credentials",
	"signature", "session_id", "csrf", "csrf_token", "access_key", "secret_key",
	"auth",
}

// A fragment is a substring that appears in no ordinary English word, so matching it
// cannot redact an innocent key. That rules out "auth" and "key", which would swallow
// "author", "keyboard", and "monkey"; those spellings are covered by exact keys instead.
var sensitiveKeyFragments = []string{
	"password", "passwd", "secret", "token", "apikey", "privatekey", "credential",
	"authorization", "cookie", "session", "bearer", "signature", "jwt", "accesskey",
}

type configValues struct {
	logDirectory         string
	maxFileBytes         int64
	maxStringBytes       int
	maxKeyBytes          int
	maxRecordBytes       int
	maxArrayItems        int
	maxPayloadNodes      int
	maxDepth             int
	sensitiveKeys        []string
	strictSensitiveKeys  bool
	directoryMode        fs.FileMode
	fileMode             fs.FileMode
	retentionDays        int
	trustIncomingTraceID bool
	failOnError          bool
	onError              func(error)
}

// Option customizes an immutable Configuration.
type Option func(*configValues)

// Configuration is validated and immutable after construction.
type Configuration struct {
	logDirectory         string
	maxFileBytes         int64
	maxStringBytes       int
	maxKeyBytes          int
	maxRecordBytes       int
	maxArrayItems        int
	maxPayloadNodes      int
	maxDepth             int
	sensitiveKeySet      map[string]struct{}
	strictSensitiveKeys  bool
	directoryMode        fs.FileMode
	fileMode             fs.FileMode
	retentionDays        int
	trustIncomingTraceID bool
	failOnError          bool
	onError              func(error)
}

// NewConfiguration validates options and returns an immutable configuration.
func NewConfiguration(logDirectory string, options ...Option) (Configuration, error) {
	values := configValues{
		logDirectory:         strings.TrimSpace(logDirectory),
		maxFileBytes:         DefaultMaxFileBytes,
		maxStringBytes:       DefaultMaxStringBytes,
		maxKeyBytes:          DefaultMaxKeyBytes,
		maxRecordBytes:       DefaultMaxRecordBytes,
		maxArrayItems:        DefaultMaxArrayItems,
		maxPayloadNodes:      DefaultMaxPayloadNodes,
		maxDepth:             DefaultMaxDepth,
		directoryMode:        DefaultDirectoryMode,
		fileMode:             DefaultFileMode,
		trustIncomingTraceID: false,
	}

	for _, option := range options {
		if option != nil {
			option(&values)
		}
	}

	if values.logDirectory == "" {
		return Configuration{}, &ConfigurationError{Message: "log directory must not be empty"}
	}
	if values.maxFileBytes <= 0 {
		return Configuration{}, &ConfigurationError{Message: "max file size must be greater than zero"}
	}
	if values.maxStringBytes <= 0 {
		return Configuration{}, &ConfigurationError{Message: "max string size must be greater than zero"}
	}
	if values.maxKeyBytes < minMaxKeyBytes {
		return Configuration{}, &ConfigurationError{
			Message: fmt.Sprintf("max key size must be at least %d bytes", minMaxKeyBytes),
		}
	}
	if values.maxRecordBytes <= 0 {
		return Configuration{}, &ConfigurationError{Message: "max record size must be greater than zero"}
	}
	if values.maxArrayItems <= 0 {
		return Configuration{}, &ConfigurationError{Message: "max array items must be greater than zero"}
	}
	if values.maxPayloadNodes <= 0 {
		return Configuration{}, &ConfigurationError{Message: "max payload nodes must be greater than zero"}
	}
	if values.maxDepth <= 0 {
		return Configuration{}, &ConfigurationError{Message: "max depth must be greater than zero"}
	}
	if values.retentionDays < 0 {
		return Configuration{}, &ConfigurationError{Message: "retention days must not be negative"}
	}
	if values.directoryMode > 0o7777 {
		return Configuration{}, &ConfigurationError{Message: "directory mode must be a valid permission mode"}
	}
	if values.fileMode > 0o7777 {
		return Configuration{}, &ConfigurationError{Message: "file mode must be a valid permission mode"}
	}

	if int64(values.maxRecordBytes) > values.maxFileBytes {
		values.maxRecordBytes = int(values.maxFileBytes)
	}
	if values.maxStringBytes > values.maxRecordBytes {
		values.maxStringBytes = values.maxRecordBytes
	}

	keySet := make(map[string]struct{}, len(defaultSensitiveKeys)+len(values.sensitiveKeys))
	for _, key := range append(append([]string(nil), defaultSensitiveKeys...), values.sensitiveKeys...) {
		if canonical := CanonicalizeKey(key); canonical != "" {
			keySet[canonical] = struct{}{}
		}
	}

	return Configuration{
		logDirectory:         filepath.Clean(values.logDirectory),
		maxFileBytes:         values.maxFileBytes,
		maxStringBytes:       values.maxStringBytes,
		maxKeyBytes:          values.maxKeyBytes,
		maxRecordBytes:       values.maxRecordBytes,
		maxArrayItems:        values.maxArrayItems,
		maxPayloadNodes:      values.maxPayloadNodes,
		maxDepth:             values.maxDepth,
		sensitiveKeySet:      keySet,
		strictSensitiveKeys:  values.strictSensitiveKeys,
		directoryMode:        values.directoryMode,
		fileMode:             values.fileMode,
		retentionDays:        values.retentionDays,
		trustIncomingTraceID: values.trustIncomingTraceID,
		failOnError:          values.failOnError,
		onError:              values.onError,
	}, nil
}

// WithMaxFileBytes sets the soft size limit for one shard.
func WithMaxFileBytes(value int64) Option {
	return func(config *configValues) { config.maxFileBytes = value }
}

// WithMaxStringBytes sets the UTF-8 byte limit for one payload string.
func WithMaxStringBytes(value int) Option {
	return func(config *configValues) { config.maxStringBytes = value }
}

// WithMaxKeyBytes sets the UTF-8 byte limit for one payload key. A longer key is
// truncated and given a digest suffix, so two long keys cannot collide into one.
func WithMaxKeyBytes(value int) Option {
	return func(config *configValues) { config.maxKeyBytes = value }
}

// WithMaxRecordBytes sets the byte limit for one encoded JSONL record.
func WithMaxRecordBytes(value int) Option {
	return func(config *configValues) { config.maxRecordBytes = value }
}

// WithMaxArrayItems caps each array in a payload on its own, so one long array
// does not starve the fields beside it.
func WithMaxArrayItems(value int) Option {
	return func(config *configValues) { config.maxArrayItems = value }
}

// WithMaxPayloadNodes bounds a payload as a whole, which stops a wide or deeply
// nested value from costing an unbounded amount of work.
func WithMaxPayloadNodes(value int) Option {
	return func(config *configValues) { config.maxPayloadNodes = value }
}

// WithMaxDepth sets the recursive payload depth limit.
func WithMaxDepth(value int) Option {
	return func(config *configValues) { config.maxDepth = value }
}

// WithSensitiveKeys merges custom secret keys with the built-in defaults.
func WithSensitiveKeys(keys ...string) Option {
	cloned := append([]string(nil), keys...)
	return func(config *configValues) { config.sensitiveKeys = append(config.sensitiveKeys, cloned...) }
}

// WithStrictSensitiveKeys disables secret-fragment matching when true.
func WithStrictSensitiveKeys(value bool) Option {
	return func(config *configValues) { config.strictSensitiveKeys = value }
}

// WithDirectoryMode sets permissions for newly created log directories.
func WithDirectoryMode(value fs.FileMode) Option {
	return func(config *configValues) { config.directoryMode = value }
}

// WithFileMode sets permissions for newly created JSONL shards.
func WithFileMode(value fs.FileMode) Option {
	return func(config *configValues) { config.fileMode = value }
}

// WithRetentionDays removes older date shards during the writer's first rotation.
func WithRetentionDays(value int) Option {
	return func(config *configValues) { config.retentionDays = value }
}

// WithTrustIncomingTraceID controls whether accepted inbound IDs are reused.
func WithTrustIncomingTraceID(value bool) Option {
	return func(config *configValues) { config.trustIncomingTraceID = value }
}

// WithFailOnError enables strict runtime tracing errors when true.
func WithFailOnError(value bool) Option {
	return func(config *configValues) { config.failOnError = value }
}

// WithOnError observes fail-safe runtime errors. Handler panics are recovered.
func WithOnError(handler func(error)) Option {
	return func(config *configValues) { config.onError = handler }
}

func (c Configuration) LogDirectory() string       { return c.logDirectory }
func (c Configuration) MaxFileBytes() int64        { return c.maxFileBytes }
func (c Configuration) MaxStringBytes() int        { return c.maxStringBytes }
func (c Configuration) MaxKeyBytes() int           { return c.maxKeyBytes }
func (c Configuration) MaxRecordBytes() int        { return c.maxRecordBytes }
func (c Configuration) MaxArrayItems() int         { return c.maxArrayItems }
func (c Configuration) MaxPayloadNodes() int       { return c.maxPayloadNodes }
func (c Configuration) MaxDepth() int              { return c.maxDepth }
func (c Configuration) StrictSensitiveKeys() bool  { return c.strictSensitiveKeys }
func (c Configuration) DirectoryMode() fs.FileMode { return c.directoryMode }
func (c Configuration) FileMode() fs.FileMode      { return c.fileMode }
func (c Configuration) RetentionDays() int         { return c.retentionDays }
func (c Configuration) TrustIncomingTraceID() bool { return c.trustIncomingTraceID }
func (c Configuration) FailOnError() bool          { return c.failOnError }

func (c Configuration) SensitiveKeys() []string {
	keys := make([]string, 0, len(c.sensitiveKeySet))
	for key := range c.sensitiveKeySet {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// CanonicalizeKey folds case and punctuation for secret-key matching.
func CanonicalizeKey(key string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(key) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}
