package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"devops-allinone/shared/store"

	"github.com/gofiber/fiber/v2"
)

const (
	aiStatusTimeout = 10 * time.Second
	// The test runs a real generation, which needs a cold model load on a
	// laptop; ai-service gives up at 90s.
	aiTestTimeout = 100 * time.Second
)

// aiIntegration proxies the integration card endpoints to ai-service, which
// is the only component that talks to the model.
type aiIntegration struct {
	baseURL string
	client  *http.Client
}

func newAIIntegration(baseURL string) *aiIntegration {
	if baseURL == "" {
		return nil
	}
	return &aiIntegration{baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{}}
}

func (ai *aiIntegration) status(c *fiber.Ctx) error {
	return ai.proxy(c, http.MethodGet, "/v1/status", aiStatusTimeout)
}

func (ai *aiIntegration) test(c *fiber.Ctx) error {
	return ai.proxy(c, http.MethodPost, "/v1/test", aiTestTimeout)
}

// proxy always answers 200 with "configured" and, on failure, "error", so a
// card can render any outcome from one shape.
func (ai *aiIntegration) proxy(c *fiber.Ctx, method, path string, timeout time.Duration) error {
	if ai == nil {
		return c.JSON(fiber.Map{"configured": false, "error": "AI_SERVICE_URL is not set"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	body := map[string]any{}
	if err := ai.call(ctx, method, path, &body); err != nil {
		log.Printf("ai integration %s: %v", path, err)
		return c.JSON(fiber.Map{"configured": true, "connected": false, "ok": false, "error": err.Error()})
	}
	body["configured"] = true
	return c.JSON(body)
}

func (ai *aiIntegration) call(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, ai.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := ai.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("ai-service unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("ai-service: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ai-service: HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

func (a *app) getBuildAnalysis(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id < 1 {
		return jsonError(c, fiber.StatusBadRequest, "invalid build id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	analysis, err := a.store.Analysis(ctx, id)
	if errors.Is(err, store.ErrNoAnalysis) {
		return jsonError(c, fiber.StatusNotFound, "no analysis for this build")
	}
	if err != nil {
		log.Printf("get analysis %d: %v", id, err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to load analysis")
	}
	return c.JSON(analysis)
}
