package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

const (
	embedDimensions = 768
	similarFloor    = 0.75
)

// EnableVector teaches pool connections how to read and write pgvector values.
func EnableVector(cfg *pgxpool.Config) {
	prev := cfg.AfterConnect
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if prev != nil {
			if err := prev(ctx, conn); err != nil {
				return err
			}
		}
		return pgxvec.RegisterTypes(ctx, conn)
	}
}

// SimilarFailure is an older failed build whose stored explanation is close
// to this one. Match is cosine similarity as a percentage.
type SimilarFailure struct {
	BuildID    int64  `json:"build_id"`
	Repository string `json:"repository"`
	Summary    string `json:"summary"`
	Match      int    `json:"match"`
}

// SaveEmbedding stores the first vector for a build. A wrong length is rejected
// so the column stays vector(768).
func (s *Store) SaveEmbedding(ctx context.Context, buildID int64, values []float32) error {
	if len(values) != embedDimensions {
		return fmt.Errorf("embedding length %d, want %d", len(values), embedDimensions)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE build_analyses SET embedding = $2
		WHERE build_id = $1 AND embedding IS NULL`,
		buildID, pgvector.NewVector(values))
	return err
}

// SimilarFailures lists up to three earlier failures whose vectors are at
// least 75% similar. An analysis without a vector returns an empty list.
func (s *Store) SimilarFailures(ctx context.Context, buildID int64) ([]SimilarFailure, error) {
	var vec pgvector.Vector
	err := s.pool.QueryRow(ctx, `
		SELECT embedding FROM build_analyses
		WHERE build_id = $1 AND embedding IS NOT NULL`, buildID).Scan(&vec)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT b.id, b.repository, a.summary, (1 - (a.embedding <=> $1)) AS sim
		FROM build_analyses a
		JOIN builds b ON b.id = a.build_id
		WHERE a.build_id <> $2
		  AND a.embedding IS NOT NULL
		  AND (1 - (a.embedding <=> $1)) >= $3
		ORDER BY a.embedding <=> $1
		LIMIT 3`, vec, buildID, similarFloor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SimilarFailure
	for rows.Next() {
		var item SimilarFailure
		var sim float64
		if err := rows.Scan(&item.BuildID, &item.Repository, &item.Summary, &sim); err != nil {
			return nil, err
		}
		item.Match = int(sim*100 + 0.5)
		out = append(out, item)
	}
	return out, rows.Err()
}
