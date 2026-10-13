package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"devops-allinone/shared/store"
)

// aiClient asks ai-service to explain a failed build. Notifications never
// depend on it: any error means the message goes out without an analysis.
type aiClient struct {
	baseURL string
	client  *http.Client
}

func newAIClient(baseURL string) *aiClient {
	if baseURL == "" {
		return nil
	}
	return &aiClient{baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{}}
}

func (ai *aiClient) Analyze(ctx context.Context, buildID int64) (*store.Analysis, error) {
	endpoint := ai.baseURL + "/v1/builds/" + strconv.FormatInt(buildID, 10) + "/analysis"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := ai.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("ai-service: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, fmt.Errorf("ai-service: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, fmt.Errorf("ai-service: HTTP %d: %s", resp.StatusCode, e.Error)
	}
	var a store.Analysis
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("ai-service: unexpected response: %w", err)
	}
	return &a, nil
}
