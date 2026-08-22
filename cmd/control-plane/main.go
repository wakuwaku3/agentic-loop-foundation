package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/takushi/agentic-loop-foundation/v2/internal/api"
)

func main() {
	mux := http.NewServeMux()
	// Authentication and application wiring are intentionally absent until a
	// production identity provider is configured. API routes fail closed while
	// healthz remains available for platform probes.
	mux.Handle("/", api.New(api.Config{}))
	addr := ":8080"
	if value := os.Getenv("PORT"); value != "" {
		addr = ":" + value
	}
	slog.Info("control plane listening", "address", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
