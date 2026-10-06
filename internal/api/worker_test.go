package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
	"t2va/internal/identity"
	"t2va/internal/task"
)

func TestDispatcherRequiresWorkerToken(t *testing.T) {
	db, e := sql.Open("sqlite", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e := task.Migrate(context.Background(), db); e != nil {
		t.Fatal(e)
	}
	repo, e := task.NewRepository(context.Background(), db)
	if e != nil {
		t.Fatal(e)
	}
	d := NewDispatcher(repo)
	w := httptest.NewRecorder()
	d.Serve(w, httptest.NewRequest(http.MethodGet, "/ws/worker", nil), "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
}

func TestDispatcherRunsOneTaskAndRejectsLateCompletion(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := task.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	repo, err := task.NewRepository(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	files := NewFileRepository(db)
	now := time.Now().UTC()
	for i, id := range []string{"task-a", "task-b"} {
		if err := repo.CreateOwned(context.Background(), identity.Principal{Type: identity.Guest, ID: "worker-test"}, task.Task{ID: id, Prompt: id, InputFileID: "input", CreatedAt: now.Add(time.Duration(i))}); err != nil {
			t.Fatal(err)
		}
	}
	d := NewDispatcher(repo)
	d.SetFileRepository(files)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); d.Serve(w, r, "worker-secret") }))
	defer server.Close()
	d.SetAllowedOrigin(server.URL)
	endpoint := strings.Replace(server.URL, "http://", "ws://", 1) + "/ws/worker"
	header := http.Header{"Authorization": []string{"Bearer worker-secret"}}
	conn, _, err := websocket.DefaultDialer.Dial(endpoint, header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]string{"type": "REGISTER", "worker_id": "worker-1"}); err != nil {
		t.Fatal(err)
	}
	var message struct {
		Type   string `json:"type"`
		TaskID string `json:"task_id"`
	}
	if err := conn.ReadJSON(&message); err != nil || message.Type != "REGISTER_ACK" {
		t.Fatalf("register ack=%#v err=%v", message, err)
	}
	if err := conn.ReadJSON(&message); err != nil || message.Type != "TASK_ASSIGN" || message.TaskID != "task-a" {
		t.Fatalf("first assignment=%#v err=%v", message, err)
	}
	if !d.authorizedInput("task-a", "input") || d.authorizedInput("task-b", "input") || d.authorizedInput("task-a", "another-input") {
		t.Fatal("Worker input authorization was not limited to its current task input")
	}
	first, err := repo.Get(context.Background(), "task-a")
	if err != nil || first.Status != task.Running {
		t.Fatalf("first task=%#v err=%v", first, err)
	}
	second, err := repo.Get(context.Background(), "task-b")
	if err != nil || second.Status != task.Pending {
		t.Fatalf("second task started concurrently: %#v err=%v", second, err)
	}
	if err := files.RegisterUnclaimedResult(context.Background(), "result-a"); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]string{"type": "TASK_SUCCESS", "task_id": "task-a", "output_file_id": "result-a"}); err != nil {
		t.Fatal(err)
	}
	if err := conn.ReadJSON(&message); err != nil || message.Type != "TASK_ASSIGN" || message.TaskID != "task-b" {
		t.Fatalf("next assignment=%#v err=%v", message, err)
	}
	if err := conn.WriteJSON(map[string]string{"type": "TASK_FAILED", "task_id": "task-a", "error": "late message"}); err != nil {
		t.Fatal(err)
	}
	second, err = repo.Get(context.Background(), "task-b")
	if err != nil || second.Status != task.Running {
		t.Fatalf("late message changed next task: %#v err=%v", second, err)
	}
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not close after Worker disconnect")
	}
	second, err = repo.Get(context.Background(), "task-b")
	if err != nil || second.Status != task.Pending {
		t.Fatalf("disconnect did not requeue current task: %#v err=%v", second, err)
	}
}
