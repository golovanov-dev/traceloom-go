package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	traceloom "github.com/golovanov-dev/traceloom-go"
)

func main() {
	events := 1000
	if len(os.Args) > 1 {
		if parsed, err := strconv.Atoi(os.Args[1]); err == nil && parsed > 0 {
			events = parsed
		}
	}
	directory, err := os.MkdirTemp("", "traceloom-benchmark-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(directory)

	tracer, err := traceloom.New(directory, traceloom.WithFailOnError(true))
	if err != nil {
		log.Fatal(err)
	}
	trace, err := tracer.Start("benchmark-trace")
	if err != nil {
		log.Fatal(err)
	}
	started := time.Now()
	for index := 0; index < events; index++ {
		if err := trace.Event("benchmark", traceloom.Data{"index": index, "value": "small payload"}); err != nil {
			log.Fatal(err)
		}
	}
	if err := tracer.Close(); err != nil {
		log.Fatal(err)
	}
	elapsed := time.Since(started)
	fmt.Printf("%d events in %s\n", events, elapsed.Round(time.Millisecond))
	fmt.Printf("%.0f events/sec\n", float64(events)/elapsed.Seconds())
}
