package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:12337/healthz")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "healthcheck request: %v\n", err)
		os.Exit(1)
	}
	if err := response.Body.Close(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "close healthcheck response: %v\n", err)
		os.Exit(1)
	}
	if response.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(os.Stderr, "healthcheck returned %s\n", response.Status)
		os.Exit(1)
	}
}
