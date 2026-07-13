package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	traceloom "github.com/golovanov-dev/traceloom-go"
)

func main() {
	tracer, err := traceloom.New(
		"./logs",
		traceloom.WithTrustIncomingTraceID(false),
		traceloom.WithOnError(func(err error) { log.Printf("tracing: %v", err) }),
	)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(response http.ResponseWriter, request *http.Request) {
		trace, err := tracer.Start(request.Header.Get("X-Trace-Id"))
		if err != nil {
			http.Error(response, "unable to start trace", http.StatusInternalServerError)
			return
		}
		if err := trace.EventContext(request.Context(), "request_start", traceloom.Data{
			"method": request.Method,
			"path":   request.URL.Path,
		}); err != nil {
			http.Error(response, "invalid trace event", http.StatusInternalServerError)
			return
		}

		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		response.Header().Set("X-Trace-Id", trace.ID())
		_ = json.NewEncoder(response).Encode(map[string]any{"ok": true, "trace_id": trace.ID()})
		_ = trace.EventContext(request.Context(), "request_end", traceloom.Data{"status": http.StatusOK})
	})

	server := &http.Server{Addr: ":3000", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Println("Listening on http://localhost:3000")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	if err := tracer.Close(); err != nil {
		log.Printf("close tracer: %v", err)
	}
}
