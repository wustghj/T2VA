package task

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"t2va/internal/identity"
)

type Status string

const (
	Pending Status = "PENDING"
	Running Status = "RUNNING"
	Success Status = "SUCCESS"
	Failed  Status = "FAILED"
)

type Task struct {
	ID                string     `json:"task_id"`
	Status            Status     `json:"status"`
	Prompt            string     `json:"prompt"`
	InputFileID       string     `json:"input_file_id"`
	OutputFileID      string     `json:"output_file_id,omitempty"`
	WorkerID          string     `json:"-"`
	ErrorMessage      string     `json:"error_message,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	InferenceDuration int64      `json:"inference_duration_ms,omitempty"`
	OwnerType         string     `json:"-"`
	OwnerID           string     `json:"-"`
}

type ListFilter struct {
	Limit  int
	Before string
	Status Status
}

type Page struct {
	Tasks      []Task `json:"tasks"`
	NextCursor string `json:"next_cursor,omitempty"`
}

var ErrNotFound = errors.New("task not found")
var ErrInvalidTransition = errors.New("invalid task state transition")
var ErrInvalidListQuery = errors.New("invalid task list query")

type Repository struct{ db *sql.DB }

func NewRepository(ctx context.Context, db *sql.DB) (*Repository, error) {
	if _, err := db.ExecContext(ctx, `SELECT id,status,prompt,input_file_id,owner_type,owner_id FROM tasks LIMIT 0`); err != nil {
		return nil, fmt.Errorf("task schema is not migrated: %w", err)
	}
	return &Repository{db: db}, nil
}

func (r *Repository) Create(ctx context.Context, t Task) error {
	return r.create(ctx, t, identity.Principal{Type: identity.System, ID: "legacy"})
}

func (r *Repository) CreateOwned(ctx context.Context, p identity.Principal, t Task) error {
	if p.Type != identity.User && p.Type != identity.Guest || p.ID == "" {
		return errors.New("invalid task owner")
	}
	return r.create(ctx, t, p)
}

func (r *Repository) create(ctx context.Context, t Task, p identity.Principal) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO tasks (id,status,prompt,input_file_id,created_at,owner_type,owner_id) VALUES (?,?,?,?,?,?,?)`, t.ID, Pending, t.Prompt, t.InputFileID, t.CreatedAt.UTC().Format(time.RFC3339Nano), p.Type, p.ID)
	return err
}

func (r *Repository) Get(ctx context.Context, id string) (Task, error) {
	return r.scanTask(ctx, `SELECT id,status,prompt,input_file_id,output_file_id,worker_id,error_message,created_at,started_at,finished_at,inference_duration,owner_type,owner_id FROM tasks WHERE id=?`, id)
}

func (r *Repository) GetOwned(ctx context.Context, p identity.Principal, id string) (Task, error) {
	if p.Type != identity.User && p.Type != identity.Guest {
		return Task{}, ErrNotFound
	}
	return r.scanTask(ctx, `SELECT id,status,prompt,input_file_id,output_file_id,worker_id,error_message,created_at,started_at,finished_at,inference_duration,owner_type,owner_id FROM tasks WHERE id=? AND owner_type=? AND owner_id=?`, id, p.Type, p.ID)
}

func (r *Repository) scanTask(ctx context.Context, query string, args ...any) (Task, error) {
	var t Task
	var created string
	var started, finished sql.NullString
	err := r.db.QueryRowContext(ctx, query, args...).Scan(&t.ID, &t.Status, &t.Prompt, &t.InputFileID, &t.OutputFileID, &t.WorkerID, &t.ErrorMessage, &created, &started, &finished, &t.InferenceDuration, &t.OwnerType, &t.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	t.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Task{}, err
	}
	if started.Valid {
		v, e := time.Parse(time.RFC3339Nano, started.String)
		if e != nil {
			return Task{}, e
		}
		t.StartedAt = &v
	}
	if finished.Valid {
		v, e := time.Parse(time.RFC3339Nano, finished.String)
		if e != nil {
			return Task{}, e
		}
		t.FinishedAt = &v
	}
	return t, nil
}

func (r *Repository) ListOwned(ctx context.Context, p identity.Principal, f ListFilter) (Page, error) {
	if (p.Type != identity.User && p.Type != identity.Guest) || p.ID == "" || f.Limit < 1 || f.Limit > 50 {
		return Page{}, ErrInvalidListQuery
	}
	if f.Status != "" && f.Status != Pending && f.Status != Running && f.Status != Success && f.Status != Failed {
		return Page{}, ErrInvalidListQuery
	}
	var beforeTime, beforeID string
	if f.Before != "" {
		if len(f.Before) > 1024 {
			return Page{}, ErrInvalidListQuery
		}
		decoded, err := base64.RawURLEncoding.DecodeString(f.Before)
		if err != nil {
			return Page{}, ErrInvalidListQuery
		}
		parts := strings.SplitN(string(decoded), "\n", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return Page{}, ErrInvalidListQuery
		}
		if _, err := time.Parse(time.RFC3339Nano, parts[0]); err != nil {
			return Page{}, ErrInvalidListQuery
		}
		beforeTime, beforeID = parts[0], parts[1]
	}
	query := `SELECT id,status,prompt,input_file_id,output_file_id,worker_id,error_message,created_at,started_at,finished_at,inference_duration,owner_type,owner_id FROM tasks WHERE owner_type=? AND owner_id=?`
	args := []any{p.Type, p.ID}
	if f.Status != "" {
		query += ` AND status=?`
		args = append(args, f.Status)
	}
	if beforeTime != "" {
		query += ` AND (created_at<? OR (created_at=? AND id<?))`
		args = append(args, beforeTime, beforeTime, beforeID)
	}
	query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, f.Limit+1)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	page := Page{Tasks: make([]Task, 0, f.Limit)}
	for rows.Next() {
		var t Task
		var created string
		var started, finished sql.NullString
		if err := rows.Scan(&t.ID, &t.Status, &t.Prompt, &t.InputFileID, &t.OutputFileID, &t.WorkerID, &t.ErrorMessage, &created, &started, &finished, &t.InferenceDuration, &t.OwnerType, &t.OwnerID); err != nil {
			return Page{}, err
		}
		t.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return Page{}, err
		}
		if started.Valid {
			v, e := time.Parse(time.RFC3339Nano, started.String)
			if e != nil {
				return Page{}, e
			}
			t.StartedAt = &v
		}
		if finished.Valid {
			v, e := time.Parse(time.RFC3339Nano, finished.String)
			if e != nil {
				return Page{}, e
			}
			t.FinishedAt = &v
		}
		page.Tasks = append(page.Tasks, t)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(page.Tasks) > f.Limit {
		page.Tasks = page.Tasks[:f.Limit]
		last := page.Tasks[len(page.Tasks)-1]
		stamp := last.CreatedAt.UTC().Format(time.RFC3339Nano)
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(stamp + "\n" + last.ID))
	}
	return page, nil
}

func (r *Repository) Pending(ctx context.Context) ([]Task, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,status,prompt,input_file_id,output_file_id,worker_id,error_message,created_at,started_at,finished_at,inference_duration FROM tasks WHERE status=? AND owner_type IN ('user','guest') ORDER BY created_at,id`, Pending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var t Task
		var created string
		var started, finished sql.NullString
		if err := rows.Scan(&t.ID, &t.Status, &t.Prompt, &t.InputFileID, &t.OutputFileID, &t.WorkerID, &t.ErrorMessage, &created, &started, &finished, &t.InferenceDuration); err != nil {
			return nil, err
		}
		t.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		if started.Valid {
			v, e := time.Parse(time.RFC3339Nano, started.String)
			if e != nil {
				return nil, e
			}
			t.StartedAt = &v
		}
		if finished.Valid {
			v, e := time.Parse(time.RFC3339Nano, finished.String)
			if e != nil {
				return nil, e
			}
			t.FinishedAt = &v
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (r *Repository) SetRunning(ctx context.Context, id, workerID string, at time.Time) error {
	res, err := r.db.ExecContext(ctx, `UPDATE tasks SET status=?,worker_id=?,started_at=? WHERE id=? AND status=?`, Running, workerID, at.UTC().Format(time.RFC3339Nano), id, Pending)
	return transitionResult(res, err)
}

func (r *Repository) Complete(ctx context.Context, id, outputID string, duration time.Duration, at time.Time) error {
	res, err := r.db.ExecContext(ctx, `UPDATE tasks SET status=?,output_file_id=?,inference_duration=?,finished_at=? WHERE id=? AND status=?`, Success, outputID, duration.Milliseconds(), at.UTC().Format(time.RFC3339Nano), id, Running)
	return transitionResult(res, err)
}

// CompleteWithResult attaches an uploaded, unclaimed result to the task owner
// and transitions the task atomically. A result can only be claimed once.
func (r *Repository) CompleteWithResult(ctx context.Context, id, outputID string, duration time.Duration, at time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ownerType, ownerID string
	if err := tx.QueryRowContext(ctx, `SELECT owner_type,owner_id FROM tasks WHERE id=? AND status=?`, id, Running).Scan(&ownerType, &ownerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidTransition
		}
		return err
	}
	file, err := tx.ExecContext(ctx, `UPDATE files SET owner_type=?,owner_id=?,status='owned',task_id=? WHERE id=? AND kind='result' AND status='unclaimed'`, ownerType, ownerID, id, outputID)
	if err != nil {
		return err
	}
	claimed, err := file.RowsAffected()
	if err != nil {
		return err
	}
	if claimed != 1 {
		return errors.New("result file is missing or already claimed")
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET status=?,output_file_id=?,inference_duration=?,finished_at=? WHERE id=? AND status=?`, Success, outputID, duration.Milliseconds(), at.UTC().Format(time.RFC3339Nano), id, Running)
	if err := transitionResult(res, err); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) Fail(ctx context.Context, id, message string, at time.Time) error {
	res, err := r.db.ExecContext(ctx, `UPDATE tasks SET status=?,error_message=?,finished_at=? WHERE id=? AND status=?`, Failed, message, at.UTC().Format(time.RFC3339Nano), id, Running)
	return transitionResult(res, err)
}

func (r *Repository) FailRunningForWorker(ctx context.Context, workerID, message string, at time.Time) error {
	res, err := r.db.ExecContext(ctx, `UPDATE tasks SET status=?,error_message=?,finished_at=? WHERE status=? AND worker_id=?`, Failed, message, at.UTC().Format(time.RFC3339Nano), Running, workerID)
	return transitionResult(res, err)
}

func (r *Repository) RequeueRunningForWorker(ctx context.Context, workerID string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE tasks SET status=?,worker_id='',started_at=NULL WHERE status=? AND worker_id=?`, Pending, Running, workerID)
	return transitionResult(res, err)
}

func (r *Repository) RequeueAllRunning(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `UPDATE tasks SET status=?,worker_id='',started_at=NULL WHERE status=?`, Pending, Running)
	return err
}

func transitionResult(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInvalidTransition
	}
	return nil
}
