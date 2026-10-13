package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"devops-allinone/shared/env"
	"devops-allinone/shared/metrics"
	"devops-allinone/shared/store"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	defaultDatabaseURL = "postgresql://opsora:opsora@localhost:5432/opsora"
	defaultTelegramAPI = "https://api.telegram.org"
	pollInterval       = 2 * time.Second
	// Builds that finished longer ago than this are skipped, so enabling the
	// bot on an old database does not flood the chat.
	maxAge     = time.Hour
	tailLines  = 15
	dbTimeout  = 5 * time.Second
	sendBudget = 15 * time.Second
)

var notifications = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "opsora_notifications_total",
	Help: "Telegram build notifications, by result.",
}, []string{"result"})

func init() {
	for _, r := range []string{"sent", "failed", "dropped"} {
		notifications.WithLabelValues(r)
	}
}

type notifier struct {
	store     *store.Store
	telegram  *telegram
	statuses  []string
	publicURL string
	ai        *aiClient
	aiWait    time.Duration
}

func main() {
	env.LoadFiles()
	databaseURL := env.Or("DATABASE_URL", defaultDatabaseURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go metrics.Serve(ctx, env.Or("METRICS_ADDR", ":9103"))

	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	chatID := strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	if token == "" || chatID == "" {
		log.Printf("TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID not set: notifier idle")
		<-ctx.Done()
		return
	}

	statuses := []string{store.StatusSuccess, store.StatusFailed}
	switch mode := env.Or("TELEGRAM_NOTIFY", "all"); mode {
	case "all":
	case "failures":
		statuses = []string{store.StatusFailed}
	default:
		log.Fatalf("TELEGRAM_NOTIFY must be all or failures, got %q", mode)
	}

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatalf("parse DATABASE_URL: %v", err)
	}
	poolConfig.ConnConfig.ConnectTimeout = dbTimeout
	poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	poolConfig.MaxConns = 2
	db, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	n := &notifier{
		store:     store.New(db),
		telegram:  newTelegram(env.Or("TELEGRAM_API_URL", defaultTelegramAPI), token, chatID),
		statuses:  statuses,
		publicURL: strings.TrimRight(os.Getenv("OPSORA_PUBLIC_URL"), "/"),
		ai:        newAIClient(strings.TrimSpace(os.Getenv("AI_SERVICE_URL"))),
	}
	// Slightly longer than ai-service's own AI_TIMEOUT so its timeout error,
	// not ours, ends a slow analysis.
	n.aiWait, err = time.ParseDuration(env.Or("AI_WAIT", "130s"))
	if err != nil || n.aiWait <= 0 {
		log.Fatalf("AI_WAIT must be a positive duration like 130s")
	}
	if n.ai == nil {
		log.Printf("AI_SERVICE_URL not set: failure messages have no AI analysis")
	}
	log.Printf("notifier started: chat=%s statuses=%v", chatID, statuses)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		wait := n.drain(ctx)
		if wait > 0 {
			log.Printf("telegram rate limited, waiting %s", wait)
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
		select {
		case <-ctx.Done():
			log.Printf("notifier stopped")
			return
		case <-ticker.C:
		}
	}
}

// drain sends every pending notification and returns how long Telegram asked
// us to back off, if it did.
func (n *notifier) drain(ctx context.Context) time.Duration {
	for ctx.Err() == nil {
		found, err := n.store.NotifyNext(ctx, n.statuses, maxAge, func(b store.Build) error {
			return n.send(ctx, b)
		})
		if err != nil {
			notifications.WithLabelValues("failed").Inc()
			log.Printf("notify: %v", err)
			var rl *rateLimited
			if errors.As(err, &rl) {
				return rl.retryAfter
			}
			return 0
		}
		if !found {
			return 0
		}
	}
	return 0
}

func (n *notifier) send(ctx context.Context, b store.Build) error {
	var tail []store.LogLine
	failed := failedStep(b.Steps)
	if failed != nil {
		lines, err := n.store.TailLogs(ctx, b.ID, failed.Position, tailLines)
		if err != nil {
			log.Printf("build=%d tail logs: %v", b.ID, err)
		}
		tail = lines
	}

	var analysis *store.Analysis
	if failed != nil && n.ai != nil {
		aiCtx, cancel := context.WithTimeout(ctx, n.aiWait)
		a, err := n.ai.Analyze(aiCtx, b.ID)
		cancel()
		if err != nil {
			log.Printf("build=%d sending without AI analysis: %v", b.ID, err)
		}
		analysis = a
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendBudget)
	defer cancel()
	if err := n.telegram.Send(sendCtx, formatMessage(b, failed, tail, analysis, n.publicURL)); err != nil {
		var rej *rejected
		if !errors.As(err, &rej) {
			return err
		}
		// Marking the build announced keeps one bad message from blocking
		// every build queued behind it.
		notifications.WithLabelValues("dropped").Inc()
		log.Printf("build=%d notification dropped: %v", b.ID, err)
		return nil
	}
	notifications.WithLabelValues("sent").Inc()
	log.Printf("build=%d status=%s notified", b.ID, b.Status)
	return nil
}

func failedStep(steps []store.Step) *store.Step {
	for i := range steps {
		if steps[i].Status == store.StepFailed || steps[i].Status == store.StepTimedOut {
			return &steps[i]
		}
	}
	return nil
}
