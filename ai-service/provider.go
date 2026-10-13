package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// provider is a chat model backend. Only Ollama exists today; hosted APIs
// can be added behind the same interface.
type provider interface {
	Name() string
	Model() string
	Endpoint() string
	// Check verifies the backend answers and the model is installed, without
	// running a generation.
	Check(ctx context.Context) (providerInfo, error)
	// Chat returns the assistant reply. A non-nil schema asks for JSON that
	// matches it.
	Chat(ctx context.Context, system, user string, schema any) (string, error)
}

type providerInfo struct {
	Version        string
	ModelAvailable bool
}

type ollama struct {
	baseURL   string
	model     string
	keepAlive string
	client    *http.Client
}

func newOllama(baseURL, model, keepAlive string) *ollama {
	return &ollama{
		baseURL:   strings.TrimRight(baseURL, "/"),
		model:     model,
		keepAlive: keepAlive,
		// Generations are bounded by the caller's context; this only caps
		// a server that accepts the connection and never answers.
		client: &http.Client{Timeout: 10 * time.Minute},
	}
}

func (o *ollama) Name() string     { return "ollama" }
func (o *ollama) Model() string    { return o.model }
func (o *ollama) Endpoint() string { return o.baseURL }

func (o *ollama) Check(ctx context.Context) (providerInfo, error) {
	var info providerInfo
	var version struct {
		Version string `json:"version"`
	}
	if err := o.get(ctx, "/api/version", &version); err != nil {
		return info, err
	}
	info.Version = version.Version

	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := o.get(ctx, "/api/tags", &tags); err != nil {
		return info, err
	}
	for _, m := range tags.Models {
		if m.Name == o.model || m.Name == o.model+":latest" {
			info.ModelAvailable = true
			return info, nil
		}
	}
	return info, fmt.Errorf("model %q is not installed; run: ollama pull %s", o.model, o.model)
}

func (o *ollama) Chat(ctx context.Context, system, user string, schema any) (string, error) {
	req := map[string]any{
		"model":      o.model,
		"stream":     false,
		"keep_alive": o.keepAlive,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"options": map[string]any{"temperature": 0.1, "num_ctx": 4096},
	}
	if schema != nil {
		req["format"] = schema
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	var resp struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := o.do(httpReq, &resp); err != nil {
		return "", err
	}
	return resp.Message.Content, nil
}

func (o *ollama) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+path, nil)
	if err != nil {
		return err
	}
	return o.do(req, out)
}

func (o *ollama) do(req *http.Request, out any) error {
	resp, err := o.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("ollama unreachable at %s: %w", o.baseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("ollama: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("ollama: %s", e.Error)
		}
		return fmt.Errorf("ollama: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ollama: unexpected response: %w", err)
	}
	return nil
}
