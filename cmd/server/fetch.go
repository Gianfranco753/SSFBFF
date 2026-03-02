//go:build goexperiment.jsonv2

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// httpClient is pre-configured for upstream calls with sensible timeouts.
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	},
}

var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// defaultFetch makes an HTTP GET to the upstream URL and returns the raw body.
// It reuses buffers from a sync.Pool to reduce allocations on the hot path.
func defaultFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream %s returned %d", url, resp.StatusCode)
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	if _, err := io.Copy(buf, resp.Body); err != nil {
		return nil, fmt.Errorf("reading upstream response: %w", err)
	}

	// Return a copy so the pooled buffer can be safely reused.
	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}
