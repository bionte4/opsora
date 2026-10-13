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
	apiBase  string
	endpoint string
	chatID   string
	client   *http.Client
	poll     *http.Client
}

type rateLimited struct {
	retryAfter time.Duration
}

func (e *rateLimited) Error() string {
	return fmt.Sprintf("telegram rate limit: retry after %s", e.retryAfter)
}

func newTelegram(apiURL, token, chatID string) *telegram {
	base := strings.TrimRight(apiURL, "/") + "/bot" + token
	return &telegram{
		apiBase:  base,
		endpoint: base + "/sendMessage",
		chatID:   chatID,
		client:   &http.Client{Timeout: 10 * time.Second},
		poll:     &http.Client{Timeout: 35 * time.Second},
	}
}

func (t *telegram) Send(ctx context.Context, text string) error {
	return t.SendTo(ctx, t.chatID, text)
}

func (t *telegram) SendTo(ctx context.Context, chatID, text string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":              chatID,
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

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

// Updates long-polls getUpdates. timeout is Telegram's server-side wait, in
// seconds; the HTTP client allows a few seconds more.
func (t *telegram) Updates(ctx context.Context, offset int64, timeout int) ([]tgUpdate, error) {
	endpoint := fmt.Sprintf("%s/getUpdates?timeout=%d", t.apiBase, timeout)
	if offset > 0 {
		endpoint += fmt.Sprintf("&offset=%d", offset)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("telegram: invalid TELEGRAM_API_URL")
	}
	resp, err := t.poll.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("telegram updates: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		OK          bool       `json:"ok"`
		Result      []tgUpdate `json:"result"`
		Description string     `json:"description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("telegram updates: HTTP %d with unreadable body", resp.StatusCode)
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram updates: HTTP %d: %s", resp.StatusCode, result.Description)
	}
	return result.Result, nil
}
