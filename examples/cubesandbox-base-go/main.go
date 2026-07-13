// Minimal HTTP server used by the cubesandbox-base-go demo template.
//
// Uses only the Go standard library (net/http) so the image needs no
// third-party dependencies. Listens on :8080 (override via APP_PORT) and
// serves a tiny landing page plus a /health endpoint. The Cube readiness
// probe is served by envd on :49983 — this server is the "real" application
// traffic endpoint, mirroring how nginx serves :80 in cubesandbox-base-nginx.
package main

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"
)

func main() {
	port := configuredPort()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { handleRootForPort(w, r, port) })
	mux.HandleFunc("/health", handleHealth)

	fmt.Printf("helloserver listening on :%s (Go %s)\n", port, runtime.Version())
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "server error:", err)
		os.Exit(1)
	}
}

func configuredPort() string {
	port := os.Getenv("APP_PORT")
	if port == "" {
		port = "8080"
	}
	return port
}

func handleRootForPort(w http.ResponseWriter, r *http.Request, port string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!doctype html>\n"+
		"<title>cubesandbox-base-go</title>\n"+
		"<h1>Hello from Go inside a CubeSandbox MicroVM</h1>\n"+
		"<p>Go runtime: %s</p>\n"+
		"<p>envd is running on :49983, this Go server on :%s.</p>\n",
		runtime.Version(), port)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
