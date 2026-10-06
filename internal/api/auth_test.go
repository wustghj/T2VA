package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
	"t2va/internal/identity"
	"t2va/internal/task"
)

func newAuthenticatedHandler(t *testing.T) (*Handler, *identity.Service) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := task.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	repo, err := task.NewRepository(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := identity.NewService(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := NewFileRepository(db)
	h := New(repo, storage, "worker-secret", "")
	h.SetIdentity(ids, true)
	h.SetFileRepository(files)
	d := NewDispatcher(repo)
	d.SetFileRepository(files)
	h.SetDispatcher(d)
	return h, ids
}

func authenticatedRequest(method, path string, body *bytes.Reader, s identity.Session, rawToken string) *http.Request {
	r := httptest.NewRequest(method, "http://example.test"+path, body)
	r.Header.Set("Origin", "http://example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: rawToken})
	if method != http.MethodGet {
		r.Header.Set("X-CSRF-Token", s.CSRFToken)
	}
	return r
}

func TestGuestSessionCookieAndCSRFChecks(t *testing.T) {
	h, _ := newAuthenticatedHandler(t)
	r := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/auth/guest", nil)
	r.Header.Set("Origin", "http://example.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("guest status=%d body=%s", w.Code, w.Body)
	}
	cookie := w.Result().Cookies()[0]
	if cookie.Name != sessionCookie || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("unsafe session cookie: %#v", cookie)
	}
	if strings.Contains(w.Body.String(), cookie.Value) {
		t.Fatal("raw session token leaked in response")
	}

	sessionReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/auth/session", nil)
	sessionReq.AddCookie(cookie)
	sessionResp := httptest.NewRecorder()
	h.ServeHTTP(sessionResp, sessionReq)
	if sessionResp.Code != http.StatusOK || !strings.Contains(sessionResp.Body.String(), `"csrf_token"`) {
		t.Fatalf("session status=%d body=%s", sessionResp.Code, sessionResp.Body)
	}

	badOrigin := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/auth/guest", nil)
	badOrigin.Header.Set("Origin", "https://attacker.test")
	badOriginResp := httptest.NewRecorder()
	h.ServeHTTP(badOriginResp, badOrigin)
	if badOriginResp.Code != http.StatusForbidden {
		t.Fatalf("cross-origin guest status=%d", badOriginResp.Code)
	}

	badCSRF := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/tasks", strings.NewReader(`{"prompt":"x","input_file_id":"y"}`))
	badCSRF.Header.Set("Origin", "http://example.test")
	badCSRF.AddCookie(cookie)
	badCSRFResp := httptest.NewRecorder()
	h.ServeHTTP(badCSRFResp, badCSRF)
	if badCSRFResp.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", badCSRFResp.Code)
	}
}

func TestOwnerScopedFileTaskAndHistoryAPIs(t *testing.T) {
	h, ids := newAuthenticatedHandler(t)
	ctx := context.Background()
	alice, aliceToken, err := ids.CreateGuest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var image bytes.Buffer
	form := multipart.NewWriter(&image)
	part, err := form.CreateFormFile("file", "input.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10})
	_ = form.Close()
	uploadReq := authenticatedRequest(http.MethodPost, "/api/v1/files", bytes.NewReader(image.Bytes()), alice, aliceToken)
	uploadReq.Header.Set("Content-Type", form.FormDataContentType())
	upload := httptest.NewRecorder()
	h.ServeHTTP(upload, uploadReq)
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", upload.Code, upload.Body)
	}
	var uploaded struct {
		FileID string `json:"file_id"`
	}
	if err := json.Unmarshal(upload.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}

	createReq := authenticatedRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader([]byte(`{"prompt":"a test","input_file_id":"`+uploaded.FileID+`"}`)), alice, aliceToken)
	createReq.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	h.ServeHTTP(created, createReq)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body)
	}
	var createdBody struct {
		Task task.Task `json:"task"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatal(err)
	}

	bob, bobToken, err := ids.CreateGuest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/tasks/" + createdBody.Task.ID, "/api/v1/tasks/" + createdBody.Task.ID + "/result", "/api/v1/files/" + uploaded.FileID} {
		req := authenticatedRequest(http.MethodGet, path, bytes.NewReader(nil), bob, bobToken)
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusNotFound {
			t.Errorf("%s: status=%d body=%s; want 404", path, resp.Code, resp.Body)
		}
	}
	bobHistory := httptest.NewRecorder()
	h.ServeHTTP(bobHistory, authenticatedRequest(http.MethodGet, "/api/v1/tasks?limit=20", bytes.NewReader(nil), bob, bobToken))
	if bobHistory.Code != http.StatusOK || !strings.Contains(bobHistory.Body.String(), `"tasks":[]`) {
		t.Fatalf("other principal history=%d %s", bobHistory.Code, bobHistory.Body)
	}
	if err := h.storage.Save("orphan-result", strings.NewReader("unclaimed")); err != nil {
		t.Fatal(err)
	}
	if err := h.files.RegisterUnclaimedResult(ctx, "orphan-result"); err != nil {
		t.Fatal(err)
	}
	for _, principal := range []identity.Session{alice, bob} {
		raw := aliceToken
		if principal.Principal.ID == bob.Principal.ID {
			raw = bobToken
		}
		orphan := httptest.NewRecorder()
		h.ServeHTTP(orphan, authenticatedRequest(http.MethodGet, "/api/v1/files/orphan-result", bytes.NewReader(nil), principal, raw))
		if orphan.Code != http.StatusNotFound {
			t.Errorf("unclaimed output visible to %s: status=%d", principal.Principal.ID, orphan.Code)
		}
	}

	foreignInput := authenticatedRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader([]byte(`{"prompt":"steal","input_file_id":"`+uploaded.FileID+`"}`)), bob, bobToken)
	foreignInput.Header.Set("Content-Type", "application/json")
	foreignCreate := httptest.NewRecorder()
	h.ServeHTTP(foreignCreate, foreignInput)
	if foreignCreate.Code != http.StatusNotFound {
		t.Fatalf("foreign input create status=%d body=%s", foreignCreate.Code, foreignCreate.Body)
	}
}

func TestRegisterAndLoginResponsesDoNotExposeCredentials(t *testing.T) {
	h, _ := newAuthenticatedHandler(t)
	payload := []byte(`{"username":"Sample_User","password":"correct horse battery"}`)
	register := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/auth/register", bytes.NewReader(payload))
	register.Header.Set("Origin", "http://example.test")
	register.Header.Set("Content-Type", "application/json")
	registered := httptest.NewRecorder()
	h.ServeHTTP(registered, register)
	if registered.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", registered.Code, registered.Body)
	}
	if strings.Contains(registered.Body.String(), "correct horse battery") || strings.Contains(registered.Body.String(), "argon2id") {
		t.Fatal("credential material leaked in registration response")
	}
	cookie := registered.Result().Cookies()[0]
	var body map[string]any
	if err := json.Unmarshal(registered.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["csrf_token"] == "" {
		t.Fatal("registration did not return CSRF token")
	}

	login := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/auth/login", bytes.NewReader(payload))
	login.Header.Set("Origin", "http://example.test")
	login.Header.Set("Content-Type", "application/json")
	loggedIn := httptest.NewRecorder()
	h.ServeHTTP(loggedIn, login)
	if loggedIn.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loggedIn.Code, loggedIn.Body)
	}
	if cookie.Value == loggedIn.Result().Cookies()[0].Value {
		t.Fatal("login reused the previous session token")
	}
}
