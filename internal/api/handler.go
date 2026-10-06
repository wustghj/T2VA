package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"t2va/internal/identity"
	"t2va/internal/task"
)

type Handler struct {
	repo         *task.Repository
	storage      *Storage
	workerToken  string
	userKey      string
	dispatcher   *Dispatcher
	identity     *identity.Service
	files        *FileRepository
	cookieSecure bool
}

func New(repo *task.Repository, storage *Storage, workerToken, userKey string) *Handler {
	return &Handler{repo: repo, storage: storage, workerToken: workerToken, userKey: userKey}
}
func (h *Handler) SetDispatcher(d *Dispatcher) { h.dispatcher = d }
func (h *Handler) SetIdentity(service *identity.Service, secureCookie bool) {
	h.identity, h.cookieSecure = service, secureCookie
}
func (h *Handler) SetFileRepository(files *FileRepository) { h.files = files }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(r.URL.Path, "/api/v1/") {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Vary", "Cookie, Authorization")
	}
	if r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/" && r.Method == http.MethodGet {
		http.ServeFile(w, r, "web/index.html")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/worker/") {
		if h.workerToken == "" || r.Header.Get("Authorization") != "Bearer "+h.workerToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.workerAPI(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
		if h.identity == nil {
			http.NotFound(w, r)
			return
		}
		h.authAPI(w, r)
		return
	}
	if h.identity != nil && (r.URL.Path == "/api/v1/files" || strings.HasPrefix(r.URL.Path, "/api/v1/files/") || r.URL.Path == "/api/v1/tasks" || strings.HasPrefix(r.URL.Path, "/api/v1/tasks/")) {
		session, err := h.resolveBrowserSession(r)
		if err != nil {
			if errors.Is(err, identity.ErrInvalidCredentials) {
				writeAPIError(w, http.StatusUnauthorized, "authentication_required")
			} else {
				writeAPIError(w, http.StatusInternalServerError, "session_lookup_failed")
			}
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !h.validOrigin(r) {
			writeAPIError(w, http.StatusForbidden, "invalid_origin")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !h.validCSRF(r, session) {
			writeAPIError(w, http.StatusForbidden, "invalid_csrf_token")
			return
		}
		r = r.WithContext(contextWithSession(r.Context(), session))
	}
	if h.identity == nil && h.userKey != "" && strings.HasPrefix(r.URL.Path, "/api/v1/") && r.Header.Get("Authorization") != "Bearer "+h.userKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/api/v1/files" && r.Method == http.MethodPost {
		h.upload(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/files/") && r.Method == http.MethodGet {
		h.download(w, r, strings.TrimPrefix(r.URL.Path, "/api/v1/files/"))
		return
	}
	if r.URL.Path == "/api/v1/tasks" && r.Method == http.MethodGet {
		h.listTasks(w, r)
		return
	}
	if r.URL.Path == "/api/v1/tasks" && r.Method == http.MethodPost {
		h.createTask(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/tasks/") && r.Method == http.MethodGet {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
		if strings.HasSuffix(id, "/result") {
			h.downloadResult(w, r, strings.TrimSuffix(id, "/result"))
			return
		}
		h.getTask(w, r, id)
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) createTask(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in struct {
		Prompt      string `json:"prompt"`
		InputFileID string `json:"input_file_id"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(new(any)) != io.EOF || strings.TrimSpace(in.Prompt) == "" || len(in.Prompt) > 4000 || in.InputFileID == "" {
		http.Error(w, "prompt and input_file_id are required; prompt max length is 4000", http.StatusBadRequest)
		return
	}
	if h.identity != nil {
		p, ok := requestPrincipal(r)
		if !ok || h.files == nil {
			http.NotFound(w, r)
			return
		}
		if _, err := h.files.GetOwned(r.Context(), in.InputFileID, p); err != nil {
			http.NotFound(w, r)
			return
		}
	} else if _, err := h.storage.Stat(in.InputFileID); err != nil {
		http.Error(w, "input file not found", http.StatusBadRequest)
		return
	}
	t := task.Task{ID: uuid.NewString(), Prompt: strings.TrimSpace(in.Prompt), InputFileID: in.InputFileID, CreatedAt: time.Now().UTC()}
	t.Status = task.Pending
	var createErr error
	if h.identity != nil {
		p, _ := requestPrincipal(r)
		createErr = h.repo.CreateOwned(r.Context(), p, t)
	} else {
		createErr = h.repo.Create(r.Context(), t)
	}
	if createErr != nil {
		http.Error(w, "could not create task", http.StatusInternalServerError)
		return
	}
	if h.dispatcher != nil {
		h.dispatcher.NotifyTaskCreated(r.Context())
	}
	writeJSON(w, http.StatusCreated, map[string]any{"task": t})
}
func (h *Handler) getTask(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	var t task.Task
	var err error
	if h.identity != nil {
		p, ok := requestPrincipal(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		t, err = h.repo.GetOwned(r.Context(), p, id)
	} else {
		t, err = h.repo.Get(r.Context(), id)
	}
	if errors.Is(err, task.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not load task", 500)
		return
	}
	writeJSON(w, 200, map[string]any{"task": t})
}
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	extendTransferDeadline(w, 2*time.Minute)
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	parseErr := r.ParseMultipartForm(20 << 20)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if parseErr != nil {
		http.Error(w, "upload must be a file up to 20 MiB", http.StatusBadRequest)
		return
	}
	f, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", 400)
		return
	}
	defer f.Close()
	if header.Size <= 0 || header.Size > 20<<20 {
		http.Error(w, "file must be between 1 byte and 20 MiB", 400)
		return
	}
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	kind := http.DetectContentType(buf[:n])
	if !strings.HasPrefix(kind, "image/") {
		http.Error(w, "only image uploads are allowed", http.StatusUnsupportedMediaType)
		return
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "invalid upload", 400)
		return
	}
	id := uuid.NewString()
	if err := h.storage.Save(id, f); err != nil {
		http.Error(w, "could not store file", 500)
		return
	}
	if h.identity != nil {
		p, ok := requestPrincipal(r)
		if !ok || h.files == nil || h.files.RegisterOwned(r.Context(), id, p, fileInput) != nil {
			_ = h.storage.Delete(id)
			http.Error(w, "could not register uploaded file", http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]string{"file_id": id, "content_type": kind, "name": filepath.Base(header.Filename)})
}
func (h *Handler) download(w http.ResponseWriter, r *http.Request, id string) {
	extendTransferDeadline(w, 15*time.Minute)
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	if h.identity != nil {
		p, ok := requestPrincipal(r)
		if !ok || h.files == nil {
			http.NotFound(w, r)
			return
		}
		if _, err := h.files.GetOwned(r.Context(), id, p); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	f, err := h.storage.Open(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, id, time.Time{}, f)
}
func (h *Handler) downloadResult(w http.ResponseWriter, r *http.Request, id string) {
	var t task.Task
	var err error
	if h.identity != nil {
		p, ok := requestPrincipal(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		t, err = h.repo.GetOwned(r.Context(), p, id)
	} else {
		t, err = h.repo.Get(r.Context(), id)
	}
	if err != nil || t.Status != task.Success || t.OutputFileID == "" {
		http.NotFound(w, r)
		return
	}
	h.download(w, r, t.OutputFileID)
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	p, ok := requestPrincipal(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &limit); err != nil || strconv.Itoa(limit) != raw {
			writeAPIError(w, http.StatusBadRequest, "invalid_limit")
			return
		}
	}
	var status task.Status
	if raw := r.URL.Query().Get("status"); raw != "" {
		status = task.Status(raw)
	}
	page, err := h.repo.ListOwned(r.Context(), p, task.ListFilter{Limit: limit, Before: r.URL.Query().Get("before"), Status: status})
	if err != nil {
		if errors.Is(err, task.ErrInvalidListQuery) {
			writeAPIError(w, http.StatusBadRequest, "invalid_task_query")
		} else {
			writeAPIError(w, http.StatusInternalServerError, "task_history_failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func validID(s string) bool {
	return s != "" && filepath.Base(s) == s && !strings.ContainsAny(s, "\\/")
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Storage confines generated identifiers beneath its configured root.
type Storage struct{ root string }

func NewStorage(root string) (*Storage, error) {
	if err := os.MkdirAll(root, 0750); err != nil {
		return nil, err
	}
	return &Storage{root: root}, nil
}
func (s *Storage) path(id string) (string, error) {
	if !validID(id) {
		return "", errors.New("invalid file id")
	}
	return filepath.Join(s.root, id), nil
}
func (s *Storage) Save(id string, src io.Reader) error {
	p, e := s.path(id)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if e != nil {
		return e
	}
	if _, e = io.Copy(f, src); e != nil {
		f.Close()
		os.Remove(p)
		return e
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(p)
		return err
	}
	return nil
}
func (s *Storage) Open(id string) (*os.File, error) {
	p, e := s.path(id)
	if e != nil {
		return nil, e
	}
	return os.Open(p)
}
func (s *Storage) Stat(id string) (os.FileInfo, error) {
	p, e := s.path(id)
	if e != nil {
		return nil, e
	}
	return os.Stat(p)
}
func (s *Storage) Delete(id string) error {
	p, e := s.path(id)
	if e != nil {
		return e
	}
	return os.Remove(p)
}
