package traceloom

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
)

const (
	concurrencyWorkers         = 4
	concurrencyEventsPerWorker = 150
)

// The directory check is cached because stat'ing the log directory on every event was the
// most expensive step in the write path. The cache must not survive a failure: a directory
// that goes away at runtime has to be noticed and recreated.
func TestDirectoryCheckIsCachedButClearedOnFailure(t *testing.T) {
	configuration, err := NewConfiguration(t.TempDir(), WithFailOnError(true))
	if err != nil {
		t.Fatal(err)
	}
	writer := NewJSONLFileWriter(configuration)
	tracer, err := NewWithDependencies(configuration, writer, newSystemClock())
	if err != nil {
		t.Fatal(err)
	}
	trace, _ := tracer.Start("cache-trace")

	if err := trace.Event("first", nil); err != nil {
		t.Fatal(err)
	}
	if !writer.directoryReady {
		t.Fatal("the directory check was not cached after a successful write")
	}

	// Break the shard underneath the writer: the next append fails.
	if err := writer.handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := trace.Event("second", nil); err == nil {
		t.Fatal("a write to a closed handle must fail")
	}
	if writer.directoryReady {
		t.Fatal("a failed write must clear the cached directory check")
	}
}

func TestWriterHelperProcess(t *testing.T) {
	if os.Getenv("TRACELOOM_HELPER") != "1" {
		return
	}
	directory := os.Getenv("TRACELOOM_DIRECTORY")
	worker := os.Getenv("TRACELOOM_WORKER")
	events, _ := strconv.Atoi(os.Getenv("TRACELOOM_EVENTS"))
	maxFileBytes, _ := strconv.ParseInt(os.Getenv("TRACELOOM_MAX_FILE_BYTES"), 10, 64)

	tracer, err := New(directory, WithMaxFileBytes(maxFileBytes), WithFailOnError(true))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	trace, err := tracer.Start("worker-" + worker + "-trace")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for index := 0; index < events; index++ {
		if err := trace.Event("tick", Data{"worker": worker, "index": index, "pad": string(bytes.Repeat([]byte{'x'}, 64))}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := tracer.Close(); err != nil || tracer.DroppedEventCount() != 0 {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestConcurrentProcessesDoNotLoseOrInterleaveLines(t *testing.T) {
	directory := t.TempDir()
	runConcurrentWorkers(t, directory, 1024*1024)
	events := readTestEvents(t, directory)
	if len(events) != concurrencyWorkers*concurrencyEventsPerWorker {
		t.Fatalf("events lost: got %d", len(events))
	}
	counts := make(map[string]int)
	for _, event := range events {
		counts[event.Data["worker"].(string)]++
	}
	for worker := 0; worker < concurrencyWorkers; worker++ {
		if counts[strconv.Itoa(worker)] != concurrencyEventsPerWorker {
			t.Fatalf("worker %d count = %d", worker, counts[strconv.Itoa(worker)])
		}
	}
}

func TestConcurrentRotationPreservesEveryEvent(t *testing.T) {
	directory := t.TempDir()
	runConcurrentWorkers(t, directory, 4096)
	if got := len(readTestEvents(t, directory)); got != concurrencyWorkers*concurrencyEventsPerWorker {
		t.Fatalf("events lost during rotation: %d", got)
	}
	if len(testLogFiles(t, directory)) < 2 {
		t.Fatal("rotation did not create multiple shards")
	}
}

func runConcurrentWorkers(t *testing.T, directory string, maxFileBytes int64) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		worker int
		output string
		err    error
	}
	results := make(chan result, concurrencyWorkers)
	var wait sync.WaitGroup
	for worker := 0; worker < concurrencyWorkers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			command := exec.Command(executable, "-test.run=^TestWriterHelperProcess$")
			command.Env = append(os.Environ(),
				"TRACELOOM_HELPER=1",
				"TRACELOOM_DIRECTORY="+directory,
				"TRACELOOM_WORKER="+strconv.Itoa(worker),
				"TRACELOOM_EVENTS="+strconv.Itoa(concurrencyEventsPerWorker),
				"TRACELOOM_MAX_FILE_BYTES="+strconv.FormatInt(maxFileBytes, 10),
			)
			output, err := command.CombinedOutput()
			results <- result{worker: worker, output: string(output), err: err}
		}(worker)
	}
	wait.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			t.Fatalf("worker %d failed: %v\n%s", result.worker, result.err, result.output)
		}
	}
}
