// Package health implements the `healthcheck` subcommand both binaries
// expose for container health checks (the image has no curl or wget).
package health

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// LocalURL turns a listen address like ":8080" or "0.0.0.0:8080" into a
// loopback URL for path.
func LocalURL(listen, path string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}

// Probe GETs url and exits the process: 0 when it answers 200, 1 otherwise.
// The response body is printed either way so `docker inspect` shows why.
func Probe(url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Println(strings.TrimSpace(string(body)))
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}
