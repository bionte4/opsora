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

type telegram struct {
	endpoint string
	chatID   string
	client   *http.Client
}

type rateLimited struct {
	retryAfter time.Duration
}

func (e *rateLimited) Error() string {
	return fmt.Sprintf("telegram rate limit: retry after %s", e.retryAfter)
}

func newTelegram(apiURL, token, chatID string) *telegram {
	return &telegram{
		endpoint: strings.TrimRight(apiURL, "/") + "/bot" + token + "/sendMessage",
		chatID:   chatID,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *telegram) Send(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":              t.chatID,
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("telegram: invalid TELEGRAM_API_URL")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		// url.Error embeds the request URL, which contains the bot token.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("telegram request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("telegram: HTTP %d with unreadable body", resp.StatusCode)
	}
	if result.OK {
		return nil
	}
	if resp.StatusCode == http.StatusTooManyRequests && result.Parameters.RetryAfter > 0 {
		return &rateLimited{retryAfter: time.Duration(result.Parameters.RetryAfter) * time.Second}
	}
	err = fmt.Errorf("telegram: HTTP %d: %s", resp.StatusCode, result.Description)
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden {
		return &rejected{err}
	}
	return err
}

// rejected means Telegram refused this message for good (bad markup, unknown
// chat, bot blocked); retrying the same text will not help. A bad token
// answers 401 or 404 and is retried, so fixing it delivers the backlog.
type rejected struct{ err error }

func (e *rejected) Error() string { return e.err.Error() }
func (e *rejected) Unwrap() error { return e.err }
