package main

import (
	"fmt"
	"log"

	traceloom "github.com/golovanov-dev/traceloom-go"
)

func main() {
	tracer, err := traceloom.New("./logs")
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := tracer.Close(); err != nil {
			log.Printf("close tracer: %v", err)
		}
	}()

	trace, err := tracer.Start("")
	if err != nil {
		log.Fatal(err)
	}

	events := []struct {
		name string
		data traceloom.Data
	}{
		{"request_start", traceloom.Data{"method": "POST", "path": "/orders"}},
		{"auth_success", traceloom.Data{"user_id": 42}},
		{"billing_request", traceloom.Data{"provider": "example"}},
		{"billing_response", traceloom.Data{"status": 200}},
		{"request_end", traceloom.Data{"status": 201}},
	}
	for _, event := range events {
		if err := trace.Event(event.name, event.data); err != nil {
			log.Fatal(err)
		}
	}

	fmt.Println("Trace ID:", trace.ID())
}
