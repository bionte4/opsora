package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrations embed.FS

var ErrNotFound = errors.New("build not found")

const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"

	StepPending   = "pending"
	StepRunning   = "running"
	StepSuccess   = "success"
	StepFailed    = "failed"
	StepTimedOut  = "timed_out"
	StepCancelled = "cancelled"

	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamSystem = "system"
)

type Build struct {
	ID         int64      `json:"id"`
	DeliveryID string     `json:"delivery_id"`
	Repository string     `json:"repository"`
	Ref        string     `json:"ref"`
	CommitSHA  string     `json:"commit_sha"`
	Actor      string     `json:"actor"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	Attempts   int        `json:"attempts"`
	WorkerID   string     `json:"worker_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Steps      []Step     `json:"steps,omitempty"`
}

type Step struct {
	Position   int        `json:"position"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type LogLine struct {
	ID        int64     `json:"id"`
	Position  int       `json:"position"`
	Stream    string    `json:"stream"`
	Line      string    `json:"line"`
	CreatedAt time.Time `json:"created_at"`
}

type NewBuild struct {
	DeliveryID string
	Event      string
	Payload    []byte
	Repository string
	Ref        string
	CommitSHA  string
	Actor      string
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Migrate is safe to call from several processes at once; goose holds a
// Postgres advisory lock while it applies migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, dir, goose.WithSessionLocker(locker))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}

const buildColumns = `id, delivery_id, repository, ref, commit_sha, actor, status, error,
	attempts, COALESCE(worker_id, ''), created_at, started_at, finished_at`

func scanBuild(row pgx.Row) (Build, error) {
	var b Build
	err := row.Scan(&b.ID, &b.DeliveryID, &b.Repository, &b.Ref, &b.CommitSHA, &b.Actor,
		&b.Status, &b.Error, &b.Attempts, &b.WorkerID, &b.CreatedAt, &b.StartedAt, &b.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Build{}, ErrNotFound
	}
	return b, err
}

// RecordDelivery stores a delivery that does not trigger a build. It reports
// false when GitHub already sent this delivery ID.
func (s *Store) RecordDelivery(ctx context.Context, id, event string, payload []byte) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO deliveries (id, event, payload) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO NOTHING`, id, event, string(payload))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CreateBuild stores the delivery and its build in one transaction. When the
// delivery ID was seen before, it returns the existing build (zero ID if that
// delivery never produced one) and false.
func (s *Store) CreateBuild(ctx context.Context, nb NewBuild) (Build, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Build{}, false, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		INSERT INTO deliveries (id, event, payload) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO NOTHING`, nb.DeliveryID, nb.Event, string(nb.Payload))
	if err != nil {
		return Build{}, false, err
	}
	if tag.RowsAffected() == 0 {
		existing, err := scanBuild(tx.QueryRow(ctx,
			`SELECT `+buildColumns+` FROM builds WHERE delivery_id = $1`, nb.DeliveryID))
		if errors.Is(err, ErrNotFound) {
			return Build{}, false, nil
		}
		return existing, false, err
	}

	build, err := scanBuild(tx.QueryRow(ctx, `
		INSERT INTO builds (delivery_id, repository, ref, commit_sha, actor)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+buildColumns,
		nb.DeliveryID, nb.Repository, nb.Ref, nb.CommitSHA, nb.Actor))
	if err != nil {
		return Build{}, false, err
	}
	return build, true, tx.Commit(ctx)
}

func (s *Store) GetBuild(ctx context.Context, id int64) (Build, error) {
	build, err := scanBuild(s.pool.QueryRow(ctx,
		`SELECT `+buildColumns+` FROM builds WHERE id = $1`, id))
	if err != nil {
		return Build{}, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT position, name, status, started_at, finished_at
		FROM build_steps WHERE build_id = $1 ORDER BY position`, id)
	if err != nil {
		return Build{}, err
	}
	build.Steps, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Step, error) {
		var st Step
		err := row.Scan(&st.Position, &st.Name, &st.Status, &st.StartedAt, &st.FinishedAt)
		return st, err
	})
	return build, err
}

func (s *Store) ListBuilds(ctx context.Context, limit int) ([]Build, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+buildColumns+` FROM builds ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Build, error) {
		return scanBuild(row)
	})
}

func (s *Store) Status(ctx context.Context, id int64) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM builds WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return status, err
}

// Claim moves a queued build to running for this worker. It reports false
// when the build is not queued, e.g. another worker already took it.
func (s *Store) Claim(ctx context.Context, id int64, workerID string) (Build, bool, error) {
	build, err := scanBuild(s.pool.QueryRow(ctx, `
		UPDATE builds SET status = 'running', worker_id = $2, attempts = attempts + 1,
			started_at = now(), heartbeat_at = now(), finished_at = NULL, error = ''
		WHERE id = $1 AND status = 'queued'
		RETURNING `+buildColumns, id, workerID))
	if errors.Is(err, ErrNotFound) {
		return Build{}, false, nil
	}
	return build, err == nil, err
}

// ResetSteps replaces the step list, so a retried build starts from pending.
func (s *Store) ResetSteps(ctx context.Context, id int64, names []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM build_steps WHERE build_id = $1`, id); err != nil {
		return err
	}
	for i, name := range names {
		if _, err := tx.Exec(ctx,
			`INSERT INTO build_steps (build_id, position, name) VALUES ($1, $2, $3)`,
			id, i+1, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) SetStepStatus(ctx context.Context, id int64, position int, status string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE build_steps SET status = $3::text,
			started_at = CASE WHEN $3::text = 'running' THEN now() ELSE started_at END,
			finished_at = CASE WHEN $3::text IN ('success', 'failed', 'timed_out', 'cancelled') THEN now() ELSE finished_at END
		WHERE build_id = $1 AND position = $2`, id, position, status)
	return err
}

// AppendSteps adds pending steps after the existing ones, starting at first.
func (s *Store) AppendSteps(ctx context.Context, id int64, first int, names []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for i, name := range names {
		if _, err := tx.Exec(ctx,
			`INSERT INTO build_steps (build_id, position, name) VALUES ($1, $2, $3)`,
			id, first+i, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// CancelPendingSteps marks steps that never ran as cancelled once a build stops.
func (s *Store) CancelPendingSteps(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE build_steps SET status = 'cancelled', finished_at = now()
		WHERE build_id = $1 AND status = 'pending'`, id)
	return err
}

func (s *Store) AppendLogs(ctx context.Context, buildID int64, lines []LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	_, err := s.pool.CopyFrom(ctx,
		pgx.Identifier{"build_logs"},
		[]string{"build_id", "position", "stream", "line", "created_at"},
		pgx.CopyFromSlice(len(lines), func(i int) ([]any, error) {
			l := lines[i]
			return []any{buildID, l.Position, l.Stream, l.Line, l.CreatedAt}, nil
		}))
	return err
}

// NotifyNext locks the oldest finished build that has not been announced yet,
// calls send, and marks the build announced only if send succeeds. The row
// stays locked while send runs, so concurrent notifiers never announce the
// same build. It reports false when no build is waiting. Builds that finished
// longer ago than maxAge are skipped so a notifier enabled late does not flood
// the chat with old results.
func (s *Store) NotifyNext(ctx context.Context, statuses []string, maxAge time.Duration, send func(Build) error) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	build, err := scanBuild(tx.QueryRow(ctx, `
		SELECT `+buildColumns+` FROM builds
		WHERE notified_at IS NULL AND status = ANY($1)
			AND finished_at > now() - make_interval(secs => $2)
		ORDER BY finished_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED`, statuses, maxAge.Seconds()))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	rows, err := tx.Query(ctx, `
		SELECT position, name, status, started_at, finished_at
		FROM build_steps WHERE build_id = $1 ORDER BY position`, build.ID)
	if err != nil {
		return false, err
	}
	build.Steps, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Step, error) {
		var st Step
		err := row.Scan(&st.Position, &st.Name, &st.Status, &st.StartedAt, &st.FinishedAt)
		return st, err
	})
	if err != nil {
		return false, err
	}

	if err := send(build); err != nil {
		return true, err
	}
	if _, err := tx.Exec(ctx, `UPDATE builds SET notified_at = now() WHERE id = $1`, build.ID); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}

// TailLogs returns the last n lines of one step, oldest first.
func (s *Store) TailLogs(ctx context.Context, buildID int64, position, n int) ([]LogLine, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, position, stream, line, created_at FROM (
			SELECT * FROM build_logs WHERE build_id = $1 AND position = $2
			ORDER BY id DESC LIMIT $3
		) tail ORDER BY id`, buildID, position, n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LogLine, error) {
		var l LogLine
		err := row.Scan(&l.ID, &l.Position, &l.Stream, &l.Line, &l.CreatedAt)
		return l, err
	})
}

// Logs returns lines with an ID greater than after, oldest first, so callers
// can tail a running build by passing the last ID they saw.
func (s *Store) Logs(ctx context.Context, buildID int64, position int, after int64, limit int) ([]LogLine, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, position, stream, line, created_at FROM build_logs
		WHERE build_id = $1 AND id > $2 AND ($3 = 0 OR position = $3)
		ORDER BY id LIMIT $4`, buildID, after, position, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LogLine, error) {
		var l LogLine
		err := row.Scan(&l.ID, &l.Position, &l.Stream, &l.Line, &l.CreatedAt)
		return l, err
	})
}

// Heartbeat extends this worker's lease. False means the lease was lost and
// the build may already be running elsewhere.
func (s *Store) Heartbeat(ctx context.Context, id int64, workerID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE builds SET heartbeat_at = now()
		WHERE id = $1 AND status = 'running' AND worker_id = $2`, id, workerID)
	return tag.RowsAffected() == 1, err
}

func (s *Store) Finish(ctx context.Context, id int64, workerID, status, errText string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE builds SET status = $3, error = $4, finished_at = now(), heartbeat_at = NULL
		WHERE id = $1 AND status = 'running' AND worker_id = $2`, id, workerID, status, errText)
	return tag.RowsAffected() == 1, err
}

// Release hands a build back to the queue on graceful shutdown without
// spending one of its attempts.
func (s *Store) Release(ctx context.Context, id int64, workerID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE builds SET status = 'queued', worker_id = NULL, heartbeat_at = NULL,
			started_at = NULL, attempts = GREATEST(attempts - 1, 0)
		WHERE id = $1 AND status = 'running' AND worker_id = $2`, id, workerID)
	return tag.RowsAffected() == 1, err
}

// ReapStale returns running builds whose worker stopped heartbeating. Builds
// under maxAttempts go back to queued; the rest are marked failed.
func (s *Store) ReapStale(ctx context.Context, staleAfter time.Duration, maxAttempts int) (requeued, failed []int64, err error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE builds SET
			status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'queued' END,
			error = CASE WHEN attempts >= $2 THEN 'worker stopped responding' ELSE error END,
			finished_at = CASE WHEN attempts >= $2 THEN now() ELSE NULL END,
			started_at = CASE WHEN attempts >= $2 THEN started_at ELSE NULL END,
			worker_id = NULL, heartbeat_at = NULL
		WHERE status = 'running' AND heartbeat_at < now() - make_interval(secs => $1)
		RETURNING id, status`, staleAfter.Seconds(), maxAttempts)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id int64
		var status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if status == StatusFailed {
			failed = append(failed, id)
		} else {
			requeued = append(requeued, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if len(failed) > 0 {
		if _, err := s.pool.Exec(ctx, `
			UPDATE build_steps SET status = 'cancelled', finished_at = now()
			WHERE build_id = ANY($1) AND status IN ('pending', 'running')`, failed); err != nil {
			return requeued, failed, fmt.Errorf("cancel steps: %w", err)
		}
	}
	return requeued, failed, nil
}
