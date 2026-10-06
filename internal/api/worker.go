package api

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"t2va/internal/task"
)

// Dispatcher deliberately supports a single V0.1 worker while keeping task persistence authoritative.
type Dispatcher struct {
	mu       sync.Mutex
	conn     *websocket.Conn
	repo     *task.Repository
	files    *FileRepository
	workerID string
	origin   string
	current  string
}

func NewDispatcher(repo *task.Repository) *Dispatcher {
	return &Dispatcher{repo: repo}
}

func (d *Dispatcher) SetFileRepository(files *FileRepository) { d.files = files }

func (d *Dispatcher) SetAllowedOrigin(origin string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.origin = strings.TrimRight(strings.TrimSpace(origin), "/")
}

func (d *Dispatcher) Serve(w http.ResponseWriter, r *http.Request, token string) {
	if token == "" || r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, "unauthorized", 401)
		return
	}
	d.mu.Lock()
	allowedOrigin := d.origin
	d.mu.Unlock()
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
		origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/")
		if origin == "" {
			return true
		}
		if allowedOrigin == "" {
			return false
		}
		u, err := url.Parse(origin)
		return err == nil && u.Scheme != "" && u.Host != "" && origin == allowedOrigin
	}}
	c, e := up.Upgrade(w, r, nil)
	if e != nil {
		return
	}
	defer c.Close()
	var registration struct {
		Type     string `json:"type"`
		WorkerID string `json:"worker_id"`
	}
	if c.SetReadDeadline(time.Now().Add(15*time.Second)) != nil || c.ReadJSON(&registration) != nil || registration.Type != "REGISTER" || strings.TrimSpace(registration.WorkerID) == "" {
		return
	}
	d.mu.Lock()
	if d.conn != nil {
		d.mu.Unlock()
		_ = c.WriteJSON(map[string]string{"type": "ERROR", "error": "worker already connected"})
		return
	}
	d.conn = c
	d.workerID = registration.WorkerID
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		if d.conn == c {
			d.conn = nil
			d.current = ""
			_ = d.repo.RequeueRunningForWorker(context.Background(), registration.WorkerID)
		}
		d.mu.Unlock()
	}()
	_ = c.SetReadDeadline(time.Time{})
	_ = c.WriteJSON(map[string]any{"type": "REGISTER_ACK", "protocol_version": 1, "timestamp": time.Now().UTC()})
	d.dispatchNext(r.Context())
	for {
		var msg struct {
			Type         string `json:"type"`
			TaskID       string `json:"task_id"`
			OutputFileID string `json:"output_file_id"`
			Error        string `json:"error"`
		}
		if e := c.ReadJSON(&msg); e != nil {
			return
		}
		ctx := r.Context()
		now := time.Now().UTC()
		switch msg.Type {
		case "HEARTBEAT":
			_ = c.WriteJSON(map[string]any{"type": "HEARTBEAT_ACK", "timestamp": now})
		case "TASK_ACK", "TASK_STARTED":
		case "TASK_SUCCESS":
			d.finishSuccess(ctx, c, msg.TaskID, msg.OutputFileID, now)
		case "TASK_FAILED":
			d.finishFailure(ctx, c, msg.TaskID, msg.Error, now)
		case "PONG":
		}
	}
}

func (d *Dispatcher) finishSuccess(ctx context.Context, c *websocket.Conn, id, outputID string, at time.Time) {
	d.mu.Lock()
	if d.conn != c || id == "" || d.current != id || d.files == nil {
		d.mu.Unlock()
		return
	}
	t, err := d.repo.Get(ctx, id)
	if err != nil || t.Status != task.Running || t.WorkerID != d.workerID {
		d.mu.Unlock()
		return
	}
	err = d.repo.CompleteWithResult(ctx, id, outputID, 0, at)
	if err != nil {
		if failErr := d.repo.Fail(ctx, id, "worker returned an invalid result", at); failErr != nil {
			_ = d.repo.RequeueRunningForWorker(ctx, d.workerID)
		}
	}
	d.current = ""
	d.mu.Unlock()
	d.dispatchNext(ctx)
}

func (d *Dispatcher) finishFailure(ctx context.Context, c *websocket.Conn, id, message string, at time.Time) {
	d.mu.Lock()
	if d.conn != c || id == "" || d.current != id {
		d.mu.Unlock()
		return
	}
	t, err := d.repo.Get(ctx, id)
	if err != nil || t.Status != task.Running || t.WorkerID != d.workerID {
		d.mu.Unlock()
		return
	}
	if message == "" {
		message = "worker failed"
	}
	if runes := []rune(message); len(runes) > 1024 {
		message = string(runes[:1024])
	}
	err = d.repo.Fail(ctx, id, message, at)
	if err != nil {
		_ = d.repo.RequeueRunningForWorker(ctx, d.workerID)
	}
	d.current = ""
	d.mu.Unlock()
	d.dispatchNext(ctx)
}

func (d *Dispatcher) NotifyTaskCreated(ctx context.Context) {
	d.dispatchNext(ctx)
}

func (d *Dispatcher) dispatchNext(ctx context.Context) {
	d.mu.Lock()
	ready := d.conn != nil && d.current == ""
	d.mu.Unlock()
	if !ready {
		return
	}
	pending, e := d.repo.Pending(ctx)
	if e != nil || len(pending) == 0 {
		return
	}
	_ = d.assign(ctx, pending[0])
}

func (d *Dispatcher) assign(ctx context.Context, t task.Task) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil || d.current != "" {
		return errors.New("worker offline")
	}
	if err := d.repo.SetRunning(ctx, t.ID, d.workerID, time.Now().UTC()); err != nil {
		return err
	}
	d.current = t.ID
	conn := d.conn
	if err := conn.WriteJSON(map[string]any{"type": "TASK_ASSIGN", "task": t, "task_id": t.ID, "timestamp": time.Now().UTC()}); err != nil {
		d.conn = nil
		d.current = ""
		_ = d.repo.RequeueRunningForWorker(ctx, d.workerID)
		_ = conn.Close()
		return err
	}
	return nil
}

func (h *Handler) workerAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/worker/files/") {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/worker/files/")
		taskID := r.URL.Query().Get("task_id")
		if h.dispatcher == nil || h.files == nil || !h.dispatcher.authorizedInput(taskID, id) {
			http.NotFound(w, r)
			return
		}
		if _, err := h.files.GetWorkerInput(r.Context(), id); err != nil {
			http.NotFound(w, r)
			return
		}
		h.downloadWorkerInput(w, r, id)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/worker/results" {
		h.uploadResult(w, r)
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) downloadWorkerInput(w http.ResponseWriter, r *http.Request, id string) {
	extendTransferDeadline(w, 15*time.Minute)
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	f, err := h.storage.Open(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, id, time.Time{}, f)
}

func (h *Handler) uploadResult(w http.ResponseWriter, r *http.Request) {
	extendTransferDeadline(w, 15*time.Minute)
	r.Body = http.MaxBytesReader(w, r.Body, 500<<20)
	parseErr := r.ParseMultipartForm(32 << 20)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if parseErr != nil {
		http.Error(w, "result upload must be up to 500 MiB", 400)
		return
	}
	f, head, e := r.FormFile("file")
	if e != nil {
		http.Error(w, "missing file field", 400)
		return
	}
	defer f.Close()
	if head.Size <= 0 || head.Size > 500<<20 {
		http.Error(w, "invalid result size", 400)
		return
	}
	id := uuid.NewString()
	if e = h.storage.Save(id, f); e != nil {
		http.Error(w, "could not store result", 500)
		return
	}
	if h.files == nil || h.files.RegisterUnclaimedResult(r.Context(), id) != nil {
		_ = h.storage.Delete(id)
		http.Error(w, "could not register result", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"file_id": id})
}

func extendTransferDeadline(w http.ResponseWriter, duration time.Duration) {
	deadline := time.Now().Add(duration)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
}

func (d *Dispatcher) authorizedInput(taskID, fileID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil || taskID == "" || d.current != taskID {
		return false
	}
	t, err := d.repo.Get(context.Background(), taskID)
	return err == nil && t.Status == task.Running && t.WorkerID == d.workerID && t.InputFileID == fileID
}
