package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"devops-allinone/shared/store"
)

const (
	statusContext       = "opsora/build"
	statusPending       = "pending"
	statusSuccess       = "success"
	statusFailure       = "failure"
	statusError         = "error"
	maxStatusDescLength = 140
)

// statusReporter posts commit statuses to GitHub. With no token it does
// nothing, so local runs and public repositories work without credentials.
type statusReporter struct {
	token     string
	publicURL string
	client    *http.Client
}

func newStatusReporter(token, publicURL string) *statusReporter {
	return &statusReporter{
		token:     token,
		publicURL: strings.TrimRight(publicURL, "/"),
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (r *statusReporter) Report(ctx context.Context, b store.Build, state, description string) {
	if r.token == "" || b.CommitSHA == "" || b.Repository == "" {
		return
	}
	if len(description) > maxStatusDescLength {
		description = description[:maxStatusDescLength-3] + "..."
	}
	body := map[string]string{
		"state":       state,
		"context":     statusContext,
		"description": description,
	}
	if r.publicURL != "" {
		body["target_url"] = fmt.Sprintf("%s/app/builds/%d", r.publicURL, b.ID)
	}
	payload, _ := json.Marshal(body)

	url := fmt.Sprintf("https://api.github.com/repos/%s/statuses/%s", b.Repository, b.CommitSHA)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		log.Printf("build=%d github status: %v", b.ID, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		log.Printf("build=%d github status: %v", b.ID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		log.Printf("build=%d github status %s: HTTP %d %s", b.ID, state, resp.StatusCode, strings.TrimSpace(string(snippet)))
		return
	}
	log.Printf("build=%d github status %s", b.ID, state)
}
