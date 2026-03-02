//go:build goexperiment.jsonv2

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// defaultFetch makes an HTTP GET to the upstream URL and returns the raw body.
// It uses the shared HTTP client defined in main.go for connection pooling.
func defaultFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream %s returned %d", url, resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
