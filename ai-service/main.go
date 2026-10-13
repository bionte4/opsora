package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"devops-allinone/shared/env"
	"devops-allinone/shared/metrics"
	"devops-allinone/shared/store"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	defaultDatabaseURL = "postgresql://opsora:opsora@localhost:5432/opsora"
	checkTimeout       = 5 * time.Second
	testTimeout        = 90 * time.Second
	dbTimeout          = 5 * time.Second
)

var (
	aiRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opsora_ai_requests_total",
		Help: "AI requests by kind (analysis, test) and result.",
	}, []string{"kind", "result"})
	aiDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "opsora_ai_duration_seconds",
		Help:    "Model call duration, including time spent waiting for a free slot.",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 10),
	}, []string{"kind"})
)

func init() {
	for _, r := range []string{"ok", "cached", "error"} {
		aiRequests.WithLabelValues("analysis", r)
	}
	for _, r := range []string{"ok", "error"} {
		aiRequests.WithLabelValues("test", r)
	}
	aiDuration.WithLabelValues("analysis")
	aiDuration.WithLabelValues("test")
}

type service struct {
	store    *store.Store
	provider provider
	language string
	timeout  time.Duration
	// slots bounds concurrent generations; a local model on a laptop serves
	// one request at a time anyway, and queueing here keeps timeouts honest.
	slots chan struct{}
}

func main() {
	env.LoadFiles()
	databaseURL := env.Or("DATABASE_URL", defaultDatabaseURL)
	port := env.Or("PORT", "8090")

	if name := env.Or("AI_PROVIDER", "ollama"); name != "ollama" {
		log.Fatalf("AI_PROVIDER %q is not supported; available: ollama", name)
	}
	// A loaded phi3 holds about 4 GB; on small machines unload it soon after
	// the last request rather than keep the host swapping.
	keepAlive := env.Or("AI_KEEP_ALIVE", "2m")
	if _, err := time.ParseDuration(keepAlive); err != nil {
		log.Fatalf("AI_KEEP_ALIVE must be a duration like 2m")
	}
	p := newOllama(env.Or("OLLAMA_URL", "http://localhost:11434"), env.Or("AI_MODEL", "phi3"), keepAlive)

	timeout, err := time.ParseDuration(env.Or("AI_TIMEOUT", "120s"))
	if err != nil || timeout <= 0 {
		log.Fatalf("AI_TIMEOUT must be a positive duration like 120s")
	}
	concurrency, err := strconv.Atoi(env.Or("AI_CONCURRENCY", "1"))
	if err != nil || concurrency < 1 {
		log.Fatalf("AI_CONCURRENCY must be a positive integer")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go metrics.Serve(ctx, env.Or("METRICS_ADDR", ":9104"))

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatalf("parse DATABASE_URL: %v", err)
	}
	poolConfig.ConnConfig.ConnectTimeout = dbTimeout
	poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	db, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	s := &service{
		store:    store.New(db),
		provider: p,
		language: env.Or("AI_LANGUAGE", "English"),
		timeout:  timeout,
		slots:    make(chan struct{}, concurrency),
	}

	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	if _, err := p.Check(checkCtx); err != nil {
		log.Printf("warning: %v (analyses fail until this is fixed)", err)
	}
	cancel()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(recover.New())
	app.Get("/health", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"status": "ok"}) })
	app.Get("/v1/status", s.status)
	app.Post("/v1/test", s.test)
	app.Post("/v1/builds/:id/analysis", s.analyze)

	go func() {
		<-ctx.Done()
		_ = app.ShutdownWithTimeout(5 * time.Second)
	}()
	log.Printf("ai-service listening on :%s provider=%s model=%s endpoint=%s",
		port, p.Name(), p.Model(), p.Endpoint())
	if err := app.Listen(":" + port); err != nil {
		log.Fatal(err)
	}
}

type statusResponse struct {
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	Endpoint       string    `json:"endpoint"`
	Connected      bool      `json:"connected"`
	ModelAvailable bool      `json:"model_available"`
	Version        string    `json:"version,omitempty"`
	LatencyMS      int64     `json:"latency_ms"`
	Error          string    `json:"error,omitempty"`
	CheckedAt      time.Time `json:"checked_at"`
}

// status is a cheap reachability check: it lists installed models but does
// not run the model.
func (s *service) status(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	start := time.Now()
	info, err := s.provider.Check(ctx)
	resp := statusResponse{
		Provider:       s.provider.Name(),
		Model:          s.provider.Model(),
		Endpoint:       s.provider.Endpoint(),
		Connected:      info.Version != "",
		ModelAvailable: info.ModelAvailable,
		Version:        info.Version,
		LatencyMS:      time.Since(start).Milliseconds(),
		CheckedAt:      start.UTC(),
	}
	if err != nil {
		resp.Error = err.Error()
	}
	return c.JSON(resp)
}

type testResponse struct {
	OK        bool   `json:"ok"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	LatencyMS int64  `json:"latency_ms"`
	Reply     string `json:"reply,omitempty"`
	Error     string `json:"error,omitempty"`
}

// test runs a tiny generation, proving the model loads and answers.
func (s *service) test(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	start := time.Now()
	resp := testResponse{Provider: s.provider.Name(), Model: s.provider.Model()}

	reply, err := s.generate(ctx, "test", "Reply with the single word OK.", "ping", nil)
	resp.LatencyMS = time.Since(start).Milliseconds()
	if err == nil && reply == "" {
		err = errors.New("model returned an empty reply")
	}
	if err != nil {
		aiRequests.WithLabelValues("test", "error").Inc()
		resp.Error = err.Error()
		return c.JSON(resp)
	}
	aiRequests.WithLabelValues("test", "ok").Inc()
	resp.OK = true
	resp.Reply = clip(reply, 100)
	return c.JSON(resp)
}

func (s *service) analyze(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id < 1 {
		return jsonError(c, fiber.StatusBadRequest, "invalid build id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	b, err := s.store.GetBuild(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return jsonError(c, fiber.StatusNotFound, "build not found")
	}
	if err != nil {
		log.Printf("build=%d load: %v", id, err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to load build")
	}
	if b.Status != store.StatusFailed {
		return jsonError(c, fiber.StatusConflict, "only failed builds are analyzed")
	}
	if a, err := s.store.Analysis(ctx, id); err == nil {
		aiRequests.WithLabelValues("analysis", "cached").Inc()
		return c.JSON(a)
	}

	step, ok := failedStep(b.Steps)
	if !ok {
		return jsonError(c, fiber.StatusUnprocessableEntity, "build has no failed step to analyze")
	}
	lines, err := s.store.TailLogs(ctx, id, step.Position, tailLines)
	if err != nil {
		log.Printf("build=%d tail logs: %v", id, err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to load logs")
	}

	start := time.Now()
	raw, err := s.generate(ctx, "analysis", systemPrompt(s.language), failurePrompt(b, step, lines), answerSchema)
	if err == nil {
		var a store.Analysis
		a.Summary, a.Cause, a.LogLineIDs, err = parseAnswer(raw, lines)
		if err == nil {
			a.BuildID, a.Provider, a.Model = id, s.provider.Name(), s.provider.Model()
			a.DurationMS = int(time.Since(start).Milliseconds())
			err = s.store.SaveAnalysis(ctx, a)
		}
	}
	if err != nil {
		aiRequests.WithLabelValues("analysis", "error").Inc()
		log.Printf("build=%d analysis: %v", id, err)
		status := fiber.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = fiber.StatusGatewayTimeout
		}
		return jsonError(c, status, err.Error())
	}
	aiRequests.WithLabelValues("analysis", "ok").Inc()

	// Read back so a concurrent request that saved first wins, and the cited
	// lines come with the response.
	a, err := s.store.Analysis(ctx, id)
	if err != nil {
		return jsonError(c, fiber.StatusInternalServerError, "failed to load analysis")
	}
	log.Printf("build=%d analyzed in %dms, cited %d lines", id, a.DurationMS, len(a.LogLineIDs))
	return c.JSON(a)
}

// generate waits for a free model slot, then calls the provider. Time spent
// waiting counts against ctx.
func (s *service) generate(ctx context.Context, kind, system, user string, schema any) (string, error) {
	start := time.Now()
	defer func() { aiDuration.WithLabelValues(kind).Observe(time.Since(start).Seconds()) }()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return s.provider.Chat(ctx, system, user, schema)
}

func failedStep(steps []store.Step) (store.Step, bool) {
	for _, st := range steps {
		if st.Status == store.StepFailed || st.Status == store.StepTimedOut {
			return st, true
		}
	}
	return store.Step{}, false
}

func jsonError(c *fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"error": msg})
}
