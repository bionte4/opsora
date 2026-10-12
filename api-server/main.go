package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"devops-allinone/shared/queue"
	"devops-allinone/shared/store"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	healthTimeout   = 2 * time.Second
	requestTimeout  = 5 * time.Second
	maxWebhookBytes = 25 * 1024 * 1024
	signaturePrefix = "sha256="
	zeroCommit      = "0000000000000000000000000000000000000000"
	defaultPageSize = 20
	maxPageSize     = 100
	defaultLogLines = 1000
	maxLogLines     = 5000
)

type app struct {
	db     *pgxpool.Pool
	store  *store.Store
	redis  *redis.Client
	secret string
}

type githubPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	PullRequest struct {
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

type healthResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
	Redis    string `json:"redis"`
}

func main() {
	databaseURL := mustEnv("DATABASE_URL")
	redisURL := mustEnv("REDIS_URL")
	webhookSecret := mustEnv("GITHUB_WEBHOOK_SECRET")
	port := envOr("PORT", "8080")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		log.Fatalf("postgres ping: %v", err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	redisOpts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("redis url: %v", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}

	srv := &app{db: db, store: store.New(db), redis: rdb, secret: webhookSecret}

	fiberApp := fiber.New(fiber.Config{
		BodyLimit: maxWebhookBytes,
	})
	fiberApp.Use(recover.New())
	fiberApp.Use(logger.New())
	fiberApp.Get("/", srv.health)
	fiberApp.Get("/health", srv.health)
	fiberApp.Post("/api/webhook/github", srv.githubWebhook)
	fiberApp.Get("/api/builds", srv.listBuilds)
	fiberApp.Get("/api/builds/:id", srv.getBuild)
	fiberApp.Get("/api/builds/:id/logs", srv.getBuildLogs)

	log.Printf("listening on :%s", port)
	if err := fiberApp.Listen(":" + port); err != nil {
		log.Fatal(err)
	}
}

func (a *app) health(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()

	body := healthResponse{Status: "ok", Database: "ok", Redis: "ok"}
	if err := a.db.Ping(ctx); err != nil {
		body.Database = "error"
	}
	if err := a.redis.Ping(ctx).Err(); err != nil {
		body.Redis = "error"
	}
	status := fiber.StatusOK
	if body.Database != "ok" || body.Redis != "ok" {
		body.Status = "degraded"
		status = fiber.StatusServiceUnavailable
	}
	return c.Status(status).JSON(body)
}

func (a *app) githubWebhook(c *fiber.Ctx) error {
	body := append([]byte(nil), c.Body()...)
	if !validGitHubSignature(a.secret, c.Get("X-Hub-Signature-256"), body) {
		return jsonError(c, fiber.StatusUnauthorized, "invalid signature")
	}

	event := c.Get("X-GitHub-Event")
	deliveryID := c.Get("X-GitHub-Delivery")
	if event == "" || deliveryID == "" {
		return jsonError(c, fiber.StatusBadRequest, "missing X-GitHub-Event or X-GitHub-Delivery")
	}

	var payload githubPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return jsonError(c, fiber.StatusBadRequest, "invalid json payload")
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	nb, reason := buildFromEvent(event, payload)
	if reason != "" {
		created, err := a.store.RecordDelivery(ctx, deliveryID, event, body)
		if err != nil {
			log.Printf("record delivery %s: %v", deliveryID, err)
			return jsonError(c, fiber.StatusInternalServerError, "failed to record delivery")
		}
		status := "ignored"
		if !created {
			status = "duplicate"
		}
		return c.JSON(fiber.Map{"status": status, "delivery_id": deliveryID, "reason": reason})
	}
	if payload.Repository.FullName == "" {
		return jsonError(c, fiber.StatusBadRequest, "missing repository.full_name")
	}

	nb.DeliveryID = deliveryID
	nb.Event = event
	nb.Payload = body
	build, created, err := a.store.CreateBuild(ctx, nb)
	if err != nil {
		log.Printf("create build for delivery %s: %v", deliveryID, err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to record build")
	}

	if !created {
		resp := fiber.Map{"status": "duplicate", "delivery_id": deliveryID}
		if build.ID != 0 {
			resp["build_id"] = build.ID
		}
		// Redelivery re-enqueues a build that was saved while Redis was down.
		// Workers skip IDs they cannot claim, so a second entry is harmless.
		if build.Status == store.StatusQueued {
			if err := queue.Enqueue(ctx, a.redis, build.ID); err != nil {
				log.Printf("re-enqueue build %d: %v", build.ID, err)
				return jsonError(c, fiber.StatusBadGateway, "failed to enqueue build")
			}
		}
		return c.JSON(resp)
	}

	if err := queue.Enqueue(ctx, a.redis, build.ID); err != nil {
		log.Printf("enqueue build %d: %v", build.ID, err)
		return jsonError(c, fiber.StatusBadGateway, "failed to enqueue build")
	}

	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"status":      "queued",
		"delivery_id": deliveryID,
		"build_id":    build.ID,
	})
}

// buildFromEvent returns a non-empty reason when the event should be recorded
// without starting a build.
func buildFromEvent(event string, p githubPayload) (store.NewBuild, string) {
	switch event {
	case "push":
		if p.Deleted || p.After == zeroCommit {
			return store.NewBuild{}, "branch deleted"
		}
		actor := p.Pusher.Name
		if actor == "" {
			actor = p.Sender.Login
		}
		return store.NewBuild{
			Repository: p.Repository.FullName,
			Ref:        p.Ref,
			CommitSHA:  p.After,
			Actor:      actor,
		}, ""
	case "pull_request":
		switch p.Action {
		case "opened", "synchronize", "reopened":
		default:
			return store.NewBuild{}, "pull_request action " + p.Action
		}
		return store.NewBuild{
			Repository: p.Repository.FullName,
			Ref:        p.PullRequest.Head.Ref,
			CommitSHA:  p.PullRequest.Head.SHA,
			Actor:      p.Sender.Login,
		}, ""
	default:
		return store.NewBuild{}, "event " + event + " does not trigger builds"
	}
}

func (a *app) listBuilds(c *fiber.Ctx) error {
	limit := c.QueryInt("limit", defaultPageSize)
	if limit < 1 || limit > maxPageSize {
		return jsonError(c, fiber.StatusBadRequest, "limit must be between 1 and 100")
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	builds, err := a.store.ListBuilds(ctx, limit)
	if err != nil {
		log.Printf("list builds: %v", err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to list builds")
	}
	return c.JSON(fiber.Map{"builds": builds})
}

func (a *app) getBuild(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id < 1 {
		return jsonError(c, fiber.StatusBadRequest, "invalid build id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	build, err := a.store.GetBuild(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return jsonError(c, fiber.StatusNotFound, "build not found")
	}
	if err != nil {
		log.Printf("get build %d: %v", id, err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to load build")
	}
	return c.JSON(build)
}

// getBuildLogs returns log lines oldest first. Pass the returned next_after
// as ?after= to fetch only lines written since the previous call.
func (a *app) getBuildLogs(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id < 1 {
		return jsonError(c, fiber.StatusBadRequest, "invalid build id")
	}
	step := c.QueryInt("step", 0)
	if step < 0 {
		return jsonError(c, fiber.StatusBadRequest, "step must be a step position")
	}
	after, err := strconv.ParseInt(c.Query("after", "0"), 10, 64)
	if err != nil || after < 0 {
		return jsonError(c, fiber.StatusBadRequest, "after must be a log line id")
	}
	limit := c.QueryInt("limit", defaultLogLines)
	if limit < 1 || limit > maxLogLines {
		return jsonError(c, fiber.StatusBadRequest, "limit must be between 1 and 5000")
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := a.store.Status(ctx, id); errors.Is(err, store.ErrNotFound) {
		return jsonError(c, fiber.StatusNotFound, "build not found")
	} else if err != nil {
		log.Printf("get build %d: %v", id, err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to load build")
	}
	lines, err := a.store.Logs(ctx, id, step, after, limit)
	if err != nil {
		log.Printf("get logs for build %d: %v", id, err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to load logs")
	}

	nextAfter := after
	if len(lines) > 0 {
		nextAfter = lines[len(lines)-1].ID
	}
	c.Set("X-Next-After", strconv.FormatInt(nextAfter, 10))

	if c.Query("format") == "text" {
		var b strings.Builder
		for _, l := range lines {
			if l.Stream == store.StreamSystem {
				b.WriteString("[opsora] ")
			}
			b.WriteString(l.Line)
			b.WriteByte('\n')
		}
		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		return c.SendString(b.String())
	}
	if lines == nil {
		lines = []store.LogLine{}
	}
	return c.JSON(fiber.Map{"build_id": id, "lines": lines, "next_after": nextAfter})
}

func jsonError(c *fiber.Ctx, status int, message string) error {
	return c.Status(status).JSON(fiber.Map{"error": message})
}

func validGitHubSignature(secret, header string, body []byte) bool {
	if secret == "" || !strings.HasPrefix(header, signaturePrefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func mustEnv(key string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		log.Fatalf("missing required environment variable %s", key)
	}
	return value
}

func envOr(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
