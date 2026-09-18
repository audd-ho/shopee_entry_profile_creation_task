// TODO: Entry point for the HTTP server binary.
//
// Responsibilities:
//   - Load config (port, TCP-server address, etc.) via internal/config
//   - Initialize the internal/httpserver package
//   - Handle SIGINT/SIGTERM for graceful shutdown
//   - Keep the main goroutine simple; all real logic lives in internal/httpserver
//
// Build:  go build ./cmd/http-server
// Run:    ./http-server (or `go run ./cmd/http-server`)
package main

func main() {
	// TODO: implement
}
