package task

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"t2va/internal/identity"
)

func newTestRepository(t *testing.T, db *sql.DB) *Repository {
	t.Helper()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	repo, err := NewRepository(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestTaskStateTransitionsArePersistedAndEnforced(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := newTestRepository(t, db)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if err := repo.Create(context.Background(), Task{ID: "task-1", Prompt: "A red kite", InputFileID: "input-1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	created, err := repo.Get(context.Background(), "task-1")
	if err != nil || created.Status != Pending {
		t.Fatalf("expected pending task, got %#v, err=%v", created, err)
	}
	if err := repo.Complete(context.Background(), "task-1", "output-1", time.Second, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("completion before running should be rejected, got %v", err)
	}
	if err := repo.SetRunning(context.Background(), "task-1", "worker-1", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRunning(context.Background(), "task-1", "worker-1", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("duplicate assignment should be rejected, got %v", err)
	}
	if err := repo.Complete(context.Background(), "task-1", "output-1", 1500*time.Millisecond, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := repo.Get(context.Background(), "task-1")
	if err != nil || result.Status != Success || result.OutputFileID != "output-1" || result.InferenceDuration != 1500 || result.FinishedAt == nil {
		t.Fatalf("unexpected completed task: %#v, err=%v", result, err)
	}
}

func TestRequeueRunningForWorkerReleasesTasks(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := newTestRepository(t, db)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if err := repo.Create(context.Background(), Task{ID: "task-1", Prompt: "A red kite", InputFileID: "input-1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRunning(context.Background(), "task-1", "worker-1", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.RequeueRunningForWorker(context.Background(), "worker-1"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(context.Background(), "task-1")
	if err != nil || got.Status != Pending || got.WorkerID != "" || got.StartedAt != nil {
		t.Fatalf("unexpected requeued task: %#v, err=%v", got, err)
	}
	if err := repo.RequeueRunningForWorker(context.Background(), "worker-1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second release should report no running tasks, got %v", err)
	}
}

func TestRequeueAllRunningTasksOnCloudRestart(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := newTestRepository(t, db)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"a", "b"} {
		if err := repo.Create(context.Background(), Task{ID: id, Prompt: id, InputFileID: "input", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetRunning(context.Background(), id, "worker-1", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.RequeueAllRunning(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		got, err := repo.Get(context.Background(), id)
		if err != nil || got.Status != Pending || got.WorkerID != "" || got.StartedAt != nil {
			t.Fatalf("recovered task %s = %#v err=%v", id, got, err)
		}
	}
}
func TestGetMissingTaskReturnsNotFound(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := newTestRepository(t, db)
	if _, err := repo.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestOwnedTaskIsolationAndStablePagination(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	repo := newTestRepository(t, db)
	ctx := context.Background()
	alice := identity.Principal{Type: identity.User, ID: "alice"}
	bob := identity.Principal{Type: identity.User, ID: "bob"}
	guest := identity.Principal{Type: identity.Guest, ID: "guest"}
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"a", "b", "c"} {
		if err := repo.CreateOwned(ctx, alice, Task{ID: id, Prompt: id, InputFileID: "image", CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.CreateOwned(ctx, bob, Task{ID: "bob-task", Prompt: "private", InputFileID: "image", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, Task{ID: "legacy-pending", Prompt: "old", InputFileID: "old-input", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	pending, err := repo.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, queued := range pending {
		if queued.ID == "legacy-pending" {
			t.Fatal("legacy task was made available to a Worker")
		}
	}
	for _, p := range []identity.Principal{bob, guest, {Type: identity.System, ID: "legacy"}} {
		if _, err := repo.GetOwned(ctx, p, "a"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("principal %#v read another task: %v", p, err)
		}
	}
	if _, err := repo.GetOwned(ctx, alice, "a"); err != nil {
		t.Fatalf("owner could not read task: %v", err)
	}
	first, err := repo.ListOwned(ctx, alice, ListFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Tasks) != 2 || first.Tasks[0].ID != "c" || first.Tasks[1].ID != "b" || first.NextCursor == "" {
		t.Fatalf("first page = %#v", first)
	}
	second, err := repo.ListOwned(ctx, alice, ListFilter{Limit: 2, Before: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Tasks) != 1 || second.Tasks[0].ID != "a" || second.NextCursor != "" {
		t.Fatalf("second page = %#v", second)
	}
	if _, err := repo.ListOwned(ctx, alice, ListFilter{Limit: 51}); err == nil {
		t.Fatal("accepted oversized page")
	}
	if _, err := repo.ListOwned(ctx, alice, ListFilter{Limit: 10, Status: "UNKNOWN"}); err == nil {
		t.Fatal("accepted unknown status")
	}
}

func TestCompleteWithResultClaimsFileForTaskOwnerAtomically(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	repo := newTestRepository(t, db)
	ctx := context.Background()
	p := identity.Principal{Type: identity.Guest, ID: "guest-1"}
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if err := repo.CreateOwned(ctx, p, Task{ID: "task-1", Prompt: "test", InputFileID: "input", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRunning(ctx, "task-1", "worker-1", at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO files(id,owner_type,owner_id,kind,status,created_at) VALUES('result-1','','','result','unclaimed',?)`, at.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteWithResult(ctx, "task-1", "result-1", time.Second, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetOwned(ctx, p, "task-1")
	if err != nil || got.Status != Success || got.OutputFileID != "result-1" {
		t.Fatalf("completed task=%#v err=%v", got, err)
	}
	var ownerType, ownerID, status, taskID string
	if err := db.QueryRowContext(ctx, `SELECT owner_type,owner_id,status,task_id FROM files WHERE id='result-1'`).Scan(&ownerType, &ownerID, &status, &taskID); err != nil {
		t.Fatal(err)
	}
	if ownerType != string(p.Type) || ownerID != p.ID || status != "owned" || taskID != "task-1" {
		t.Fatalf("claimed file owner=%s/%s status=%s task=%s", ownerType, ownerID, status, taskID)
	}
}
