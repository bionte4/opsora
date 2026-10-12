package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"devops-allinone/shared/queue"
	"devops-allinone/shared/store"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisURL    = "redis://:opsora@localhost:6379/0"
	defaultDatabaseURL = "postgresql://opsora:opsora@localhost:5432/opsora"
	dequeueTimeout     = 5 * time.Second
	heartbeatInterval  = 5 * time.Second
	staleAfter         = 30 * time.Second
	reapInterval       = 10 * time.Second
	maxAttempts        = 3
	dbTimeout          = 5 * time.Second
)

var pipelineSteps = []string{"checkout", "install dependencies", "test", "build"}

// detached is used for store and Redis calls so status writes still land after
// SIGTERM. Calls are bounded by statement_timeout and the Redis client timeouts.
var detached = context.Background()

type worker struct {
	id           string
	store        *store.Store
	redis        *redis.Client
	stepDuration time.Duration
}

func main() {
	loadEnvFiles()
	redisURL := envOr("REDIS_URL", defaultRedisURL)
	databaseURL := envOr("DATABASE_URL", defaultDatabaseURL)
	stepDuration, err := time.ParseDuration(envOr("STEP_DURATION", "2s"))
	if err != nil {
		log.Fatalf("STEP_DURATION: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatalf("postgres url: %v", err)
	}
	poolConfig.ConnConfig.ConnectTimeout = dbTimeout
	poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(dbTimeout.Milliseconds(), 10)
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("postgres ping: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("redis url: %v", err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}

	host, _ := os.Hostname()
	w := &worker{
		id:           fmt.Sprintf("%s-%d", host, os.Getpid()),
		store:        store.New(pool),
		redis:        rdb,
		stepDuration: stepDuration,
	}

	go w.reapLoop(ctx)

	log.Printf("worker %s listening on queue %s", w.id, queue.BuildQueue)
	for ctx.Err() == nil {
		raw, err := queue.Dequeue(ctx, rdb, dequeueTimeout)
		if err != nil {
			if errors.Is(err, redis.Nil) || ctx.Err() != nil {
				continue
			}
			log.Printf("dequeue: %v", err)
			time.Sleep(time.Second)
			continue
		}
		w.handle(ctx, raw)
	}
	log.Printf("worker %s stopped", w.id)
}

func (w *worker) handle(ctx context.Context, raw string) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		log.Printf("dropping malformed queue entry %q", raw)
		w.ack(raw)
		return
	}

	build, claimed, err := w.store.Claim(detached, id, w.id)
	if err != nil {
		// Left in build_processing; the reaper returns it to the queue.
		log.Printf("claim build=%d: %v", id, err)
		return
	}
	if !claimed {
		log.Printf("skip build=%d: not queued", id)
		w.ack(raw)
		return
	}

	log.Printf("build=%d started attempt=%d repo=%s ref=%s commit=%s actor=%s",
		build.ID, build.Attempts, build.Repository, build.Ref, build.CommitSHA, build.Actor)
	if err := w.store.ResetSteps(detached, build.ID, pipelineSteps); err != nil {
		log.Printf("build=%d reset steps: %v", build.ID, err)
		w.finish(build.ID, raw, store.StatusFailed, "could not record steps")
		return
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	leaseLost := make(chan struct{})
	go w.heartbeat(runCtx, build.ID, cancelRun, leaseLost)

	started := time.Now()
	status, errText := w.runPipeline(runCtx, build.ID)
	cancelRun()

	select {
	case <-leaseLost:
		// The reaper already handed this build to another worker; acking now
		// could remove that worker's in-flight entry.
		log.Printf("build=%d abandoned: lease lost", build.ID)
		return
	default:
	}

	if ctx.Err() != nil {
		w.release(build.ID)
		return
	}

	log.Printf("build=%d finished status=%s duration=%s", build.ID, status, time.Since(started).Round(time.Millisecond))
	w.finish(build.ID, raw, status, errText)
}

func (w *worker) runPipeline(ctx context.Context, buildID int64) (string, string) {
	for i, name := range pipelineSteps {
		position := i + 1
		if err := w.store.SetStepStatus(detached, buildID, position, store.StepRunning); err != nil {
			log.Printf("build=%d step=%s: %v", buildID, name, err)
			return store.StatusFailed, "could not update step " + name
		}
		log.Printf("build=%d step=%s status=running", buildID, name)

		select {
		case <-ctx.Done():
			w.setStep(buildID, position, store.StepCancelled)
			log.Printf("build=%d step=%s status=cancelled", buildID, name)
			return store.StatusFailed, "cancelled during " + name
		case <-time.After(w.stepDuration):
		}

		w.setStep(buildID, position, store.StepSuccess)
		log.Printf("build=%d step=%s status=success", buildID, name)
	}
	return store.StatusSuccess, ""
}

func (w *worker) heartbeat(ctx context.Context, buildID int64, cancelRun context.CancelFunc, leaseLost chan<- struct{}) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := w.store.Heartbeat(detached, buildID, w.id)
			if err != nil {
				log.Printf("build=%d heartbeat: %v", buildID, err)
				continue
			}
			if !ok {
				close(leaseLost)
				cancelRun()
				return
			}
		}
	}
}

func (w *worker) finish(buildID int64, raw, status, errText string) {
	ok, err := w.store.Finish(detached, buildID, w.id, status, errText)
	if err != nil {
		log.Printf("build=%d finish: %v", buildID, err)
		return
	}
	if !ok {
		log.Printf("build=%d finish skipped: lease lost", buildID)
		return
	}
	w.ack(raw)
	log.Printf("build=%d recorded status=%s", buildID, status)
}

func (w *worker) release(buildID int64) {
	ok, err := w.store.Release(detached, buildID, w.id)
	if err != nil {
		log.Printf("build=%d release: %v", buildID, err)
		return
	}
	if !ok {
		return
	}
	if err := queue.Requeue(detached, w.redis, buildID); err != nil {
		log.Printf("build=%d requeue on shutdown: %v", buildID, err)
		return
	}
	log.Printf("build=%d released back to %s", buildID, queue.BuildQueue)
}

func (w *worker) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	// Entries seen as queued-but-in-flight on the previous pass. A live worker
	// claims within milliseconds, so two passes in a row means it crashed.
	suspects := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			suspects = w.reap(ctx, suspects)
		}
	}
}

func (w *worker) reap(ctx context.Context, suspects map[string]bool) map[string]bool {
	requeued, failed, err := w.store.ReapStale(detached, staleAfter, maxAttempts)
	if err != nil {
		log.Printf("reap stale builds: %v", err)
	}
	for _, id := range requeued {
		if err := queue.Requeue(detached, w.redis, id); err != nil {
			log.Printf("build=%d requeue: %v", id, err)
			continue
		}
		log.Printf("build=%d requeued: worker stopped heartbeating", id)
	}
	for _, id := range failed {
		if err := queue.Forget(detached, w.redis, id); err != nil {
			log.Printf("build=%d forget: %v", id, err)
		}
		log.Printf("build=%d failed after %d attempts", id, maxAttempts)
	}

	entries, err := queue.ListInFlight(detached, w.redis)
	if err != nil {
		log.Printf("list in-flight: %v", err)
		return suspects
	}
	next := map[string]bool{}
	for _, raw := range entries {
		if ctx.Err() != nil {
			return next
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			w.ack(raw)
			continue
		}
		status, err := w.store.Status(detached, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			w.ack(raw)
		case err != nil:
			log.Printf("build=%d status: %v", id, err)
		case status == store.StatusSuccess || status == store.StatusFailed:
			w.ack(raw)
		case status == store.StatusQueued && suspects[raw]:
			if returned, err := queue.ReturnIfInFlight(detached, w.redis, raw); err != nil {
				log.Printf("build=%d return to queue: %v", id, err)
			} else if returned {
				log.Printf("build=%d returned to queue: taken but never claimed", id)
			}
		case status == store.StatusQueued:
			next[raw] = true
		}
	}
	return next
}

func (w *worker) setStep(buildID int64, position int, status string) {
	if err := w.store.SetStepStatus(detached, buildID, position, status); err != nil {
		log.Printf("build=%d step %d: %v", buildID, position, err)
	}
}

func (w *worker) ack(raw string) {
	if err := queue.Ack(detached, w.redis, raw); err != nil {
		log.Printf("ack %s: %v", raw, err)
	}
}

func loadEnvFiles() {
	for _, path := range []string{".env", "../.env"} {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if err := godotenv.Load(path); err != nil {
			log.Printf("env file %s: %v", path, err)
			continue
		}
		log.Printf("loaded env file %s", path)
	}
}

func envOr(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		log.Printf("%s not set, using default", key)
		return fallback
	}
	return value
}
