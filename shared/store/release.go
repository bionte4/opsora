package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	ReleaseActive     = "active"
	ReleaseFailed     = "failed"
	ReleaseSuperseded = "superseded"
	ReleaseRolledBack = "rolled_back"

	JobQueued  = "queued"
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"

	ActionRollback = "rollback"
)

var ErrNoRelease = errors.New("release not found")

// ErrJobPending is returned when a rollback for the repository is already
// queued or running.
var ErrJobPending = errors.New("a rollback is already in progress")

type Release struct {
	ID         int64     `json:"id"`
	Repository string    `json:"repository"`
	Image      string    `json:"image"`
	CommitSHA  string    `json:"commit_sha"`
	BuildID    int64     `json:"build_id"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type DeployJob struct {
	ID         int64      `json:"id"`
	Repository string     `json:"repository"`
	Action     string     `json:"action"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	ChatID     string     `json:"chat_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// SaveRelease inserts a release. A new active release supersedes the previous
// active one in the same transaction, so a repository never has two.
func (s *Store) SaveRelease(ctx context.Context, rel Release) (Release, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Release{}, err
	}
	defer tx.Rollback(ctx)

	if rel.Status == ReleaseActive {
		if _, err := tx.Exec(ctx, `
			UPDATE releases SET status = 'superseded'
			WHERE repository = $1 AND status = 'active'`, rel.Repository); err != nil {
			return Release{}, err
		}
	}
	saved, err := scanRelease(tx.QueryRow(ctx, `
		INSERT INTO releases (repository, image, commit_sha, build_id, status, error)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, repository, image, commit_sha, build_id, status, error, created_at`,
		rel.Repository, rel.Image, rel.CommitSHA, rel.BuildID, rel.Status, rel.Error))
	if err != nil {
		return Release{}, err
	}
	return saved, tx.Commit(ctx)
}

// ReleaseForBuild returns the release produced by a build, if it deployed.
func (s *Store) ReleaseForBuild(ctx context.Context, buildID int64) (Release, error) {
	rel, err := scanRelease(s.pool.QueryRow(ctx, `
		SELECT id, repository, image, commit_sha, build_id, status, error, created_at
		FROM releases WHERE build_id = $1 ORDER BY id DESC LIMIT 1`, buildID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Release{}, ErrNoRelease
	}
	return rel, err
}

// ReleaseRepositories lists repositories that have at least one release.
func (s *Store) ReleaseRepositories(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT repository FROM releases ORDER BY repository`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// ActiveRelease returns the release a rollback restores.
func (s *Store) ActiveRelease(ctx context.Context, repository string) (Release, error) {
	rel, err := scanRelease(s.pool.QueryRow(ctx, `
		SELECT id, repository, image, commit_sha, build_id, status, error, created_at
		FROM releases WHERE repository = $1 AND status = 'active'`, repository))
	if errors.Is(err, pgx.ErrNoRows) {
		return Release{}, ErrNoRelease
	}
	return rel, err
}

// LatestFailedRelease returns the newest failed deploy for a repository.
func (s *Store) LatestFailedRelease(ctx context.Context, repository string) (Release, error) {
	rel, err := scanRelease(s.pool.QueryRow(ctx, `
		SELECT id, repository, image, commit_sha, build_id, status, error, created_at
		FROM releases WHERE repository = $1 AND status = 'failed'
		ORDER BY id DESC LIMIT 1`, repository))
	if errors.Is(err, pgx.ErrNoRows) {
		return Release{}, ErrNoRelease
	}
	return rel, err
}

// MarkRolledBack closes the newest failed release after the previous image is
// running again. The active release stays active.
func (s *Store) MarkRolledBack(ctx context.Context, repository string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE releases SET status = 'rolled_back'
		WHERE id = (
			SELECT id FROM releases WHERE repository = $1 AND status = 'failed'
			ORDER BY id DESC LIMIT 1
		)`, repository)
	return err
}

// CreateRollback queues a rollback. It fails when the repository has no active
// release to return to, or a rollback is already queued.
func (s *Store) CreateRollback(ctx context.Context, repository, chatID string) (DeployJob, error) {
	if _, err := s.ActiveRelease(ctx, repository); err != nil {
		return DeployJob{}, err
	}
	var pending int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM deploy_jobs
		WHERE repository = $1 AND action = 'rollback' AND status IN ('queued', 'running')`,
		repository).Scan(&pending); err != nil {
		return DeployJob{}, err
	}
	if pending > 0 {
		return DeployJob{}, ErrJobPending
	}
	job, err := scanJob(s.pool.QueryRow(ctx, `
		INSERT INTO deploy_jobs (repository, action, chat_id)
		VALUES ($1, 'rollback', $2)
		RETURNING id, repository, action, status, error, chat_id, created_at, finished_at`,
		repository, chatID))
	return job, err
}

// ClaimDeployJob takes the oldest queued job. RequeueDeployJobs puts jobs
// interrupted by a worker restart back in the queue; one worker is assumed.
func (s *Store) RequeueDeployJobs(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE deploy_jobs SET status = 'queued' WHERE status = 'running'`)
	return err
}

func (s *Store) ClaimDeployJob(ctx context.Context) (DeployJob, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeployJob{}, false, err
	}
	defer tx.Rollback(ctx)

	job, err := scanJob(tx.QueryRow(ctx, `
		SELECT id, repository, action, status, error, chat_id, created_at, finished_at
		FROM deploy_jobs WHERE status = 'queued' ORDER BY id LIMIT 1
		FOR UPDATE SKIP LOCKED`))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeployJob{}, false, nil
	}
	if err != nil {
		return DeployJob{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE deploy_jobs SET status = 'running' WHERE id = $1`, job.ID); err != nil {
		return DeployJob{}, false, err
	}
	job.Status = JobRunning
	return job, true, tx.Commit(ctx)
}

func (s *Store) FinishDeployJob(ctx context.Context, id int64, status, errText string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deploy_jobs SET status = $2, error = $3, finished_at = now() WHERE id = $1`,
		id, status, errText)
	return err
}

func (s *Store) DeployJob(ctx context.Context, id int64) (DeployJob, error) {
	job, err := scanJob(s.pool.QueryRow(ctx, `
		SELECT id, repository, action, status, error, chat_id, created_at, finished_at
		FROM deploy_jobs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeployJob{}, ErrNotFound
	}
	return job, err
}

func scanRelease(row pgx.Row) (Release, error) {
	var r Release
	err := row.Scan(&r.ID, &r.Repository, &r.Image, &r.CommitSHA, &r.BuildID, &r.Status, &r.Error, &r.CreatedAt)
	return r, err
}

func scanJob(row pgx.Row) (DeployJob, error) {
	var j DeployJob
	err := row.Scan(&j.ID, &j.Repository, &j.Action, &j.Status, &j.Error, &j.ChatID, &j.CreatedAt, &j.FinishedAt)
	return j, err
}
