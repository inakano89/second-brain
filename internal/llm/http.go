package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPClient is shared by all providers (no global timeout: streams can be long).
var HTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
	},
}

// post sends a JSON body with bounded retries on 429/5xx/network errors
// (only before any response body is consumed) and returns the open response.
func post(ctx context.Context, provider, url string, headers map[string]string, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<uint(attempt-1)) * time.Second
			if ae, ok := lastErr.(*retryAfterErr); ok && ae.after > 0 {
				wait = ae.after
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := HTTPClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		apiErr := &APIError{Provider: provider, Status: resp.StatusCode, Body: string(b)}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			ra, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			lastErr = &retryAfterErr{APIError: apiErr, after: time.Duration(min(ra, 30)) * time.Second}
			continue
		}
		return nil, apiErr
	}
	if ra, ok := lastErr.(*retryAfterErr); ok {
		return nil, ra.APIError
	}
	return nil, lastErr
}

type retryAfterErr struct {
	*APIError
	after time.Duration
}

// postJSON posts and decodes a JSON response into out.
func postJSON(ctx context.Context, provider, url string, headers map[string]string, body, out any) error {
	resp, err := post(ctx, provider, url, headers, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// readSSE parses a text/event-stream, invoking fn(event, data) per message.
func readSSE(r io.Reader, fn func(event, data string) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var event string
	var data strings.Builder
	dispatch := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		d := strings.TrimSuffix(data.String(), "\n")
		err := fn(event, d)
		event = ""
		data.Reset()
		return err
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if derr := dispatch(); derr != nil {
					return derr
				}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(line[6:])
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimPrefix(line[5:], " "))
				data.WriteByte('\n')
			}
		}
		if err == io.EOF {
			return dispatch()
		}
		if err != nil {
			return err
		}
	}
}

// getJSON performs an authenticated GET and decodes the JSON body.
func getJSON(ctx context.Context, provider, url string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return &APIError{Provider: provider, Status: resp.StatusCode, Body: string(b)}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
