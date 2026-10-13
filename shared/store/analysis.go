package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrNoAnalysis = errors.New("analysis not found")

// Analysis is an AI explanation of why a build failed.
type Analysis struct {
	BuildID    int64     `json:"build_id"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Summary    string    `json:"summary"`
	Cause      string    `json:"cause,omitempty"`
	LogLineIDs []int64   `json:"log_line_ids"`
	Lines      []LogLine `json:"lines"`
	DurationMS int       `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

// Analysis returns the stored analysis of a build with the log lines it cites.
func (s *Store) Analysis(ctx context.Context, buildID int64) (Analysis, error) {
	var a Analysis
	err := s.pool.QueryRow(ctx, `
		SELECT build_id, provider, model, summary, cause, log_line_ids, duration_ms, created_at
		FROM build_analyses WHERE build_id = $1`, buildID).
		Scan(&a.BuildID, &a.Provider, &a.Model, &a.Summary, &a.Cause, &a.LogLineIDs, &a.DurationMS, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Analysis{}, ErrNoAnalysis
	}
	if err != nil {
		return Analysis{}, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, position, stream, line, created_at FROM build_logs
		WHERE build_id = $1 AND id = ANY($2) ORDER BY id`, buildID, a.LogLineIDs)
	if err != nil {
		return Analysis{}, err
	}
	a.Lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (LogLine, error) {
		var l LogLine
		err := row.Scan(&l.ID, &l.Position, &l.Stream, &l.Line, &l.CreatedAt)
		return l, err
	})
	return a, err
}

// SaveAnalysis keeps the first analysis stored for a build, so concurrent
// requests cannot overwrite each other.
func (s *Store) SaveAnalysis(ctx context.Context, a Analysis) error {
	if a.LogLineIDs == nil {
		a.LogLineIDs = []int64{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO build_analyses (build_id, provider, model, summary, cause, log_line_ids, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (build_id) DO NOTHING`,
		a.BuildID, a.Provider, a.Model, a.Summary, a.Cause, a.LogLineIDs, a.DurationMS)
	return err
}
