package traceloom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const directoryLockFile = ".traceloom.lock"

// JSONLFileWriter appends events to UTC date shards with size rotation.
type JSONLFileWriter struct {
	configuration      Configuration
	gate               chan struct{}
	handle             *os.File
	currentDate        string
	currentSize        int64
	retentionAppliedTo string
	directoryReady     bool
	closed             bool
}

func NewJSONLFileWriter(configuration Configuration) *JSONLFileWriter {
	return &JSONLFileWriter{
		configuration: configuration,
		gate:          make(chan struct{}, 1),
	}
}

func (writer *JSONLFileWriter) Write(ctx context.Context, event TraceEvent) (err error) {
	line, err := writer.encodeLine(event)
	if err != nil {
		return &TracingError{Message: "encode trace event", Err: err}
	}
	if err := writer.acquireGate(ctx); err != nil {
		return &TracingError{Message: "wait for local writer", Err: err}
	}
	defer writer.releaseGate()

	// Any failure below may mean the log directory was moved or removed underneath us, so
	// the cached check is dropped and the next event re-establishes it.
	defer func() {
		if err != nil {
			writer.directoryReady = false
		}
	}()

	// Close is terminal: a later event is rejected and counted as dropped rather than
	// lazily reopening a shard that nobody will close again.
	if writer.closed {
		return &TracingError{Message: "write to a closed writer"}
	}

	if err := writer.ensureDirectory(); err != nil {
		return &TracingError{Message: "prepare log directory", Err: err}
	}

	for {
		if writer.handle == nil || writer.currentDate != event.utcDate() {
			if err := writer.rotate(ctx, event.utcDate(), int64(len(line))); err != nil {
				return err
			}
		}

		unlock, err := acquireFileLock(ctx, writer.handle)
		if err != nil {
			return &TracingError{Message: "lock log shard", Err: err}
		}

		info, statErr := writer.handle.Stat()
		if statErr != nil {
			_ = unlock()
			return &TracingError{Message: "read log shard size", Err: statErr}
		}
		writer.currentSize = info.Size()

		if writer.currentSize > 0 && writer.currentSize+int64(len(line)) > writer.configuration.maxFileBytes {
			if unlockErr := unlock(); unlockErr != nil {
				return &TracingError{Message: "unlock full log shard", Err: unlockErr}
			}
			if err := writer.rotate(ctx, event.utcDate(), int64(len(line))); err != nil {
				return err
			}
			continue
		}

		written, writeErr := writeAll(writer.handle, line)
		if writeErr != nil && written > 0 && written < len(line) {
			writer.restoreLineBoundary(writer.currentSize)
		}
		unlockErr := unlock()
		if writeErr != nil {
			return &TracingError{Message: "append trace event", Err: writeErr}
		}
		if unlockErr != nil {
			return &TracingError{Message: "unlock log shard", Err: unlockErr}
		}
		writer.currentSize += int64(len(line))
		return nil
	}
}

func (writer *JSONLFileWriter) Flush(ctx context.Context) error {
	if err := writer.acquireGate(ctx); err != nil {
		return err
	}
	defer writer.releaseGate()
	if writer.closed {
		return &TracingError{Message: "flush a closed writer"}
	}
	if writer.handle == nil {
		return nil
	}
	return writer.handle.Sync()
}

// Close releases the active shard. It is terminal and idempotent: a later write is
// rejected instead of reopening the file.
func (writer *JSONLFileWriter) Close() error {
	if err := writer.acquireGate(context.Background()); err != nil {
		return err
	}
	defer writer.releaseGate()
	if writer.closed {
		return nil
	}
	writer.closed = true
	return writer.closeHandle()
}

func (writer *JSONLFileWriter) acquireGate(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case writer.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (writer *JSONLFileWriter) releaseGate() { <-writer.gate }

// ensureDirectory verifies the log directory once and then remembers it. Stat'ing it on
// every event is the single most expensive step in the write path — 25 of 57 microseconds
// on Windows — and the directory does not change under a healthy process. Any write
// failure clears the flag, so a directory that disappears at runtime is still recreated.
//
// The caller must hold the gate: directoryReady is writer state.
func (writer *JSONLFileWriter) ensureDirectory() error {
	if writer.directoryReady {
		return nil
	}
	info, err := os.Stat(writer.configuration.logDirectory)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("log path exists but is not a directory: %s", writer.configuration.logDirectory)
		}
		writer.directoryReady = true
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(writer.configuration.logDirectory, writer.configuration.directoryMode); err != nil {
		return err
	}
	if os.PathSeparator != '\\' {
		if err := os.Chmod(writer.configuration.logDirectory, writer.configuration.directoryMode); err != nil {
			return err
		}
	}
	writer.directoryReady = true
	return nil
}

func (writer *JSONLFileWriter) rotate(ctx context.Context, date string, lineBytes int64) error {
	lockPath := filepath.Join(writer.configuration.logDirectory, directoryLockFile)
	unlockDirectory, err := acquirePathLock(ctx, lockPath, 0o600)
	if err != nil {
		return &TracingError{Message: "lock log directory", Err: err}
	}
	defer func() { _ = unlockDirectory() }()

	if err := writer.closeHandle(); err != nil {
		return &TracingError{Message: "close previous log shard", Err: err}
	}

	index, err := writer.discoverShardIndex(date)
	if err != nil {
		return &TracingError{Message: "discover log shards", Err: err}
	}

	for ; ; index++ {
		path := shardPath(writer.configuration.logDirectory, date, index)
		size, exists, err := fileSize(path)
		if err != nil {
			return &TracingError{Message: "read log shard size", Err: err}
		}
		if size != 0 && size+lineBytes > writer.configuration.maxFileBytes {
			continue
		}

		// O_RDWR rather than O_WRONLY: on Windows an append-only handle carries neither
		// read nor write access, and LockFileEx refuses to lock it.
		handle, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, writer.configuration.fileMode)
		if err != nil {
			return &TracingError{Message: "open log shard", Err: err}
		}
		if !exists && os.PathSeparator != '\\' {
			if err := os.Chmod(path, writer.configuration.fileMode); err != nil {
				_ = handle.Close()
				return &TracingError{Message: "set log shard permissions", Err: err}
			}
		}

		writer.handle = handle
		writer.currentDate = date
		writer.currentSize = size
		if err := writer.applyRetention(date); err != nil {
			_ = writer.closeHandle()
			return &TracingError{Message: "apply log retention", Err: err}
		}
		return nil
	}
}

func (writer *JSONLFileWriter) discoverShardIndex(date string) (int, error) {
	entries, err := os.ReadDir(writer.configuration.logDirectory)
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, entry := range entries {
		shardDate, index, ok := parseShardName(entry.Name())
		if ok && shardDate == date && index > highest {
			highest = index
		}
	}
	return highest, nil
}

// parseShardName recognizes only names this writer produces: "<date>.jsonl" and
// "<date>-<index>.jsonl". Anything else in the log directory belongs to somebody else
// and must never be rotated into or deleted.
func parseShardName(name string) (date string, index int, ok bool) {
	rest, found := strings.CutSuffix(name, ".jsonl")
	if !found || len(rest) < 10 {
		return "", 0, false
	}
	date, suffix := rest[:10], rest[10:]
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", 0, false
	}
	if suffix == "" {
		return date, 0, true
	}
	indexText, isIndexed := strings.CutPrefix(suffix, "-")
	if !isIndexed {
		return "", 0, false
	}
	index, err := strconv.Atoi(indexText)
	if err != nil || index < 1 {
		return "", 0, false
	}
	return date, index, true
}

// applyRetention runs on the first rotation into each UTC date, so a long-lived process
// keeps expiring old shards instead of sweeping once at startup and never again.
func (writer *JSONLFileWriter) applyRetention(date string) error {
	if writer.retentionAppliedTo == date || writer.configuration.retentionDays == 0 {
		return nil
	}
	writer.retentionAppliedTo = date
	cutoff := time.Now().UTC().AddDate(0, 0, -writer.configuration.retentionDays).Format("2006-01-02")
	entries, err := os.ReadDir(writer.configuration.logDirectory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		shardDate, _, ok := parseShardName(name)
		if !ok || shardDate >= cutoff {
			continue
		}
		if err := os.Remove(filepath.Join(writer.configuration.logDirectory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// restoreLineBoundary repairs the shard after a write died mid-record, which is what a
// full disk looks like. Without this the fragment has no trailing newline and the next
// event is appended straight onto it, corrupting a second record on top of the lost one.
//
// Truncating back to the last complete record is preferred. On Windows a handle opened
// for append cannot be truncated, so the fragment is instead closed with a newline: the
// reader loses the one broken record rather than two.
func (writer *JSONLFileWriter) restoreLineBoundary(complete int64) {
	if err := writer.handle.Truncate(complete); err == nil {
		return
	}
	_, _ = writer.handle.Write([]byte("\n"))
}

func (writer *JSONLFileWriter) closeHandle() error {
	handle := writer.handle
	writer.handle = nil
	writer.currentDate = ""
	writer.currentSize = 0
	if handle == nil {
		return nil
	}
	return handle.Close()
}

func (writer *JSONLFileWriter) encodeLine(event TraceEvent) ([]byte, error) {
	line, err := encodeJSONLine(event.record())
	if err != nil {
		line, err = encodeJSONLine(event.degradedRecord(err.Error()))
		if err != nil {
			return nil, err
		}
	}
	if len(line) > writer.configuration.maxRecordBytes {
		reason := fmt.Sprintf("record_too_large: %d bytes exceeds %d", len(line), writer.configuration.maxRecordBytes)
		return encodeJSONLine(event.degradedRecord(reason))
	}
	return line, nil
}

func encodeJSONLine(record wireRecord) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(record); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func fileSize(path string) (size int64, exists bool, err error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return info.Size(), true, nil
}

func shardPath(directory, date string, index int) string {
	suffix := ""
	if index > 0 {
		suffix = "-" + strconv.Itoa(index)
	}
	return filepath.Join(directory, date+suffix+".jsonl")
}

func writeAll(target io.Writer, data []byte) (int, error) {
	total := 0
	for total < len(data) {
		written, err := target.Write(data[total:])
		total += written
		if err != nil {
			return total, err
		}
		if written == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}
