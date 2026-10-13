package store

import (
	"context"
	"time"
)

// BuildTotals counts builds created since a moment, grouped by status.
type BuildTotals struct {
	Queued  int
	Running int
	Success int
	Failed  int
}

// AnalysisNote is a short failure explanation for the dashboard.
type AnalysisNote struct {
	BuildID    int64
	Repository string
	Summary    string
	CreatedAt  time.Time
	Repeated   bool
}

// BuildTotalsSince counts builds created at or after since.
func (s *Store) BuildTotalsSince(ctx context.Context, since time.Time) (BuildTotals, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT status, count(*) FROM builds WHERE created_at >= $1 GROUP BY status`, since)
	if err != nil {
		return BuildTotals{}, err
	}
	defer rows.Close()
	var totals BuildTotals
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return BuildTotals{}, err
		}
		switch status {
		case StatusQueued:
			totals.Queued = int(n)
		case StatusRunning:
			totals.Running = int(n)
		case StatusSuccess:
			totals.Success = int(n)
		case StatusFailed:
			totals.Failed = int(n)
		}
	}
	return totals, rows.Err()
}

// ActiveReleases lists releases that are still the one to roll back to.
func (s *Store) ActiveReleases(ctx context.Context, limit int) ([]Release, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repository, image, commit_sha, COALESCE(build_id, 0), status, error, created_at
		FROM releases WHERE status = 'active'
		ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		rel, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}

// OpenDeployJobs lists rollbacks that have not finished.
func (s *Store) OpenDeployJobs(ctx context.Context, limit int) ([]DeployJob, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repository, action, status, error, chat_id, created_at, finished_at
		FROM deploy_jobs WHERE status IN ('queued', 'running')
		ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeployJob
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// RecentAnalyses returns the newest failure explanations. Repeated is true
// when another stored explanation is at least as close as the build page's
// similar-failure list.
func (s *Store) RecentAnalyses(ctx context.Context, limit int) ([]AnalysisNote, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.build_id, b.repository, a.summary, a.created_at,
			EXISTS (
				SELECT 1 FROM build_analyses other
				WHERE other.build_id <> a.build_id
				  AND other.embedding IS NOT NULL
				  AND a.embedding IS NOT NULL
				  AND (1 - (other.embedding <=> a.embedding)) >= $1
			)
		FROM build_analyses a
		JOIN builds b ON b.id = a.build_id
		ORDER BY a.created_at DESC
		LIMIT $2`, similarFloor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AnalysisNote
	for rows.Next() {
		var note AnalysisNote
		if err := rows.Scan(&note.BuildID, &note.Repository, &note.Summary, &note.CreatedAt, &note.Repeated); err != nil {
			return nil, err
		}
		out = append(out, note)
	}
	return out, rows.Err()
}
