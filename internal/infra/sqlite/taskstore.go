package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/pjunod/monarr/internal/infra/scheduler"
	sqlitegen "github.com/pjunod/monarr/internal/infra/sqlite/gen"
)

// TaskStore adapts the scheduled_tasks table to scheduler.Store.
type TaskStore struct {
	db *DB
}

// NewTaskStore returns a Store backed by db.
func NewTaskStore(db *DB) *TaskStore { return &TaskStore{db: db} }

var _ scheduler.Store = (*TaskStore)(nil)

// LoadAll implements scheduler.Store.
func (s *TaskStore) LoadAll(ctx context.Context) ([]scheduler.PersistedTask, error) {
	rows, err := s.db.Read.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.PersistedTask, 0, len(rows))
	for _, r := range rows {
		p := scheduler.PersistedTask{Name: r.Name}
		if r.LastRunAt.Valid {
			p.LastRunAt = time.UnixMilli(r.LastRunAt.Int64)
		}
		if r.LastDurationMs.Valid {
			p.LastDuration = time.Duration(r.LastDurationMs.Int64) * time.Millisecond
		}
		if r.LastError.Valid {
			p.LastError = r.LastError.String
		}
		out = append(out, p)
	}
	return out, nil
}

// RecordRun implements scheduler.Store.
func (s *TaskStore) RecordRun(ctx context.Context, rec scheduler.RunRecord) error {
	return s.db.Write.UpsertTaskRun(ctx, sqlitegen.UpsertTaskRunParams{
		Name:            rec.Name,
		IntervalSeconds: rec.IntervalSeconds,
		LastRunAt:       sql.NullInt64{Int64: rec.StartedAt.UnixMilli(), Valid: true},
		LastDurationMs:  sql.NullInt64{Int64: rec.Duration.Milliseconds(), Valid: true},
		LastError:       sql.NullString{String: rec.Err, Valid: rec.Err != ""},
		NextRunAt:       sql.NullInt64{Int64: rec.NextRunAt.UnixMilli(), Valid: true},
		UpdatedAt:       time.Now().UnixMilli(),
	})
}
