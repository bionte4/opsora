package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openAI speaks the chat-completions API used by OpenAI and Groq.
// The API key stays in the Authorization header and is never logged.
type openAI struct {
	baseURL    string
	apiKey     string
	model      string
	embedModel string
	client     *http.Client
}

func newOpenAI(baseURL, apiKey, model, embedModel string) *openAI {
	return &openAI{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		model:      model,
		embedModel: embedModel,
		client:     &http.Client{Timeout: 10 * time.Minute},
	}
}

func (o *openAI) Name() string     { return "openai" }
func (o *openAI) Model() string    { return o.model }
func (o *openAI) Endpoint() string { return o.baseURL }

func (o *openAI) Check(ctx context.Context) (providerInfo, error) {
	var info providerInfo
	var listed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := o.get(ctx, "/models", &listed); err != nil {
		return info, err
	}
	info.Version = "ok"
	for _, m := range listed.Data {
		if m.ID == o.model {
			info.ModelAvailable = true
			return info, nil
		}
	}
	return info, fmt.Errorf("model %q is not offered by %s", o.model, o.baseURL)
}

func (o *openAI) Chat(ctx context.Context, system, user string, schema any) (string, error) {
	req := map[string]any{
		"model":       o.model,
		"temperature": 0.1,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	if schema != nil {
		req["response_format"] = map[string]string{"type": "json_object"}
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := o.post(ctx, "/chat/completions", req, &resp); err != nil {
		return "", err
	}
	if resp.Error.Message != "" {
		return "", fmt.Errorf("openai: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai: empty completion")
	}
	return resp.Choices[0].Message.Content, nil
}

func (o *openAI) Embed(ctx context.Context, text string) ([]float32, error) {
	if o.embedModel == "" {
		return nil, errEmbedDisabled
	}
	req := map[string]any{
		"model":      o.embedModel,
		"input":      text,
		"dimensions": embedDimensions,
	}
	var resp struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := o.post(ctx, "/embeddings", req, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 || len(resp.Data[0].Embedding) != embedDimensions {
		n := 0
		if len(resp.Data) > 0 {
			n = len(resp.Data[0].Embedding)
		}
		return nil, fmt.Errorf("embedding length %d, want %d", n, embedDimensions)
	}
	return resp.Data[0].Embedding, nil
}

func (o *openAI) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+path, nil)
	if err != nil {
		return err
	}
	return o.do(req, out)
}

func (o *openAI) post(ctx context.Context, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return o.do(req, out)
}

func (o *openAI) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("openai unreachable at %s: %w", o.baseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("openai: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("openai: %s", e.Error.Message)
		}
		return fmt.Errorf("openai: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("openai: unexpected response: %w", err)
	}
	return nil
}
