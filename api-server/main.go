package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"devops-allinone/shared/metrics"
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
	db           *pgxpool.Pool
	store        *store.Store
	redis        *redis.Client
	secret       string
	chatopsToken string
	allowedChats map[string]struct{}
	views        *template.Template
	ai           *aiIntegration
	apiReadToken string
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
	metricsAddr := envOr("METRICS_ADDR", ":9101")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	store.EnableVector(poolConfig)
	db, err := pgxpool.NewWithConfig(ctx, poolConfig)
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

	srv := &app{
		db: db, store: store.New(db), redis: rdb, secret: webhookSecret,
		chatopsToken: strings.TrimSpace(os.Getenv("CHATOPS_TOKEN")),
		allowedChats: allowedChatSet(os.Getenv("CHATOPS_ALLOWED_CHATS")),
		apiReadToken: strings.TrimSpace(os.Getenv("API_READ_TOKEN")),
	}
	if srv.chatopsToken == "" {
		log.Printf("CHATOPS_TOKEN not set: chat commands are disabled")
	}
	if srv.apiReadToken == "" {
		log.Printf("API_READ_TOKEN not set: the builds API requires a signed-in session")
	}
	if err := srv.ensureAdmin(ctx); err != nil {
		log.Fatalf("admin console: %v", err)
	}
	views, err := parseViews()
	if err != nil {
		log.Fatalf("templates: %v", err)
	}
	srv.views = views

	registerQueueGauges(rdb)
	go metrics.Serve(context.Background(), metricsAddr)

	fiberApp := fiber.New(fiber.Config{
		BodyLimit: maxWebhookBytes,
	})
	fiberApp.Use(recover.New())
	fiberApp.Use(metricsMiddleware)
	fiberApp.Use(logger.New())
	fiberApp.Use(func(c *fiber.Ctx) error {
		c.Set("X-Content-Type-Options", "nosniff")
		return c.Next()
	})
	if err := mountStatic(fiberApp); err != nil {
		log.Fatalf("static files: %v", err)
	}
	fiberApp.Get("/", srv.pageLanding)
	fiberApp.Get("/health", srv.health)
	fiberApp.Get("/login", srv.pageLogin)
	fiberApp.Post("/login", srv.submitLogin)
	fiberApp.Post("/logout", srv.submitLogout)
	fiberApp.Post("/api/webhook/github", srv.githubWebhook)

	srv.ai = newAIIntegration(os.Getenv("AI_SERVICE_URL"))
	fiberApp.Get("/api/integrations/ai", srv.ai.status)
	fiberApp.Post("/api/integrations/ai/test", srv.requireAPI("integrations.test"), srv.ai.test)

	builds := fiberApp.Group("/api/builds", srv.requireAPI("builds.read"))
	builds.Get("/", srv.listBuilds)
	builds.Get("/:id", srv.getBuild)
	builds.Get("/:id/logs", srv.getBuildLogs)
	builds.Get("/:id/analysis", srv.getBuildAnalysis)
	builds.Get("/:id/similar", srv.similarFailures)

	chat := fiberApp.Group("/api/chatops", srv.authorizeChat)
	chat.Get("/builds", srv.chatopsStatus)
	chat.Get("/builds/:id/logs", srv.chatopsLogs)
	chat.Get("/builds/:id", srv.chatopsBuild)
	chat.Post("/builds/:id/rerun", srv.chatopsRerun)
	chat.Post("/rollback", srv.chatopsRollback)
	chat.Get("/jobs/:id", srv.chatopsJob)

	ui := fiberApp.Group("/app", srv.requireUser)
	ui.Get("/", srv.pageDashboard)
	ui.Get("/builds", srv.requirePerm("builds.read"), srv.pageBuilds)
	ui.Get("/builds/:id", srv.requirePerm("builds.read"), srv.pageBuild)
	ui.Post("/builds/:id/rerun", srv.requirePerm("builds.rerun"), srv.submitRerun)
	ui.Get("/users", srv.requirePerm("users.read"), srv.pageUsers)
	ui.Get("/users/new", srv.requirePerm("users.write"), srv.pageUserNew)
	ui.Post("/users", srv.requirePerm("users.write"), srv.submitUser)
	ui.Get("/users/:id", srv.requirePerm("users.write"), srv.pageUserEdit)
	ui.Post("/users/:id", srv.requirePerm("users.write"), srv.submitUser)
	ui.Post("/users/:id/delete", srv.requirePerm("users.write"), srv.deleteUser)
	ui.Get("/roles", srv.requirePerm("roles.read"), srv.pageRoles)
	ui.Get("/roles/new", srv.requirePerm("roles.write"), srv.pageRoleNew)
	ui.Post("/roles", srv.requirePerm("roles.write"), srv.submitRole)
	ui.Get("/roles/:id", srv.requirePerm("roles.read"), srv.pageRoleEdit)
	ui.Post("/roles/:id", srv.requirePerm("roles.write"), srv.submitRole)
	ui.Post("/roles/:id/delete", srv.requirePerm("roles.write"), srv.deleteRole)
	ui.Get("/integrations", srv.requirePerm("integrations.read"), srv.pageIntegrations)
	ui.Post("/integrations/ai/test", srv.requirePerm("integrations.test"), srv.submitAITest)

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
	// event stays "unknown" until the signature checks out, so unsigned
	// requests cannot mint arbitrary label values.
	event, result := "unknown", "error"
	defer func() { webhooksTotal.WithLabelValues(event, result).Inc() }()

	body := append([]byte(nil), c.Body()...)
	if !validGitHubSignature(a.secret, c.Get("X-Hub-Signature-256"), body) {
		result = "invalid_signature"
		return jsonError(c, fiber.StatusUnauthorized, "invalid signature")
	}

	deliveryID := c.Get("X-GitHub-Delivery")
	if h := c.Get("X-GitHub-Event"); h != "" {
		event = h
	}
	if event == "unknown" || deliveryID == "" {
		result = "bad_request"
		return jsonError(c, fiber.StatusBadRequest, "missing X-GitHub-Event or X-GitHub-Delivery")
	}

	var payload githubPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		result = "bad_request"
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
		result = "ignored"
		if !created {
			result = "duplicate"
		}
		return c.JSON(fiber.Map{"status": result, "delivery_id": deliveryID, "reason": reason})
	}
	if payload.Repository.FullName == "" {
		result = "bad_request"
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
		result = "duplicate"
		return c.JSON(resp)
	}

	if err := queue.Enqueue(ctx, a.redis, build.ID); err != nil {
		log.Printf("enqueue build %d: %v", build.ID, err)
		return jsonError(c, fiber.StatusBadGateway, "failed to enqueue build")
	}

	result = "queued"
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
