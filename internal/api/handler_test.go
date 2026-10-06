package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
	"t2va/internal/task"
)

func TestCreateTaskRequiresPromptAndInput(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := task.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	repo, err := task.NewRepository(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	h := New(repo, nil, "", "")
	for _, body := range []string{`{}`, `{"prompt":"hello"}`, `{"prompt":"  ","input_file_id":"x"}`} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(body)))
		if r.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status=%d", body, r.Code)
		}
	}
}

func TestCreateAndGetTask(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := task.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	repo, err := task.NewRepository(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Save("image", strings.NewReader("image data")); err != nil {
		t.Fatal(err)
	}
	h := New(repo, storage, "", "")
	created := httptest.NewRecorder()
	h.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{"prompt":"hello","input_file_id":"image"}`)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body)
	}
	var response struct {
		Task task.Task `json:"task"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	got := httptest.NewRecorder()
	h.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+response.Task.ID, nil))
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"PENDING"`) {
		t.Fatalf("get status=%d body=%s", got.Code, got.Body)
	}
}
