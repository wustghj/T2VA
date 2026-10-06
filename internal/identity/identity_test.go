package identity

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testService(t *testing.T) *Service {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE users (id TEXT PRIMARY KEY, username TEXT NOT NULL, username_key TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE sessions (token_hash TEXT PRIMARY KEY, principal_type TEXT NOT NULL, principal_id TEXT NOT NULL, username TEXT NOT NULL DEFAULT '', csrf_token TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewService(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGuestSessionResolvesAndStoresOnlyTokenHash(t *testing.T) {
	s := testService(t)
	session, token, err := s.CreateGuest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || session.Principal.Type != Guest {
		t.Fatalf("unexpected guest session: %#v", session)
	}
	resolved, err := s.ResolveSession(context.Background(), token)
	if err != nil || resolved.Principal != session.Principal {
		t.Fatalf("resolve = %#v, %v", resolved, err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT token_hash FROM sessions WHERE principal_id=?`, session.Principal.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token || stored == "" {
		t.Fatal("raw session token must not be stored")
	}
}

func TestRegisterLoginAndRevokeSession(t *testing.T) {
	s := testService(t)
	created, token, err := s.Register(context.Background(), "  Alice_1 ", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if created.Username != "Alice_1" {
		t.Fatalf("username = %q", created.Username)
	}
	loggedIn, loginToken, err := s.Login(context.Background(), "alice_1", "correct horse battery")
	if err != nil || loggedIn.Principal != created.Principal || loginToken == token {
		t.Fatalf("login = %#v, %v", loggedIn, err)
	}
	if err := s.RevokeSession(context.Background(), loginToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveSession(context.Background(), loginToken); err == nil {
		t.Fatal("revoked session resolved")
	}
	if _, _, err := s.Login(context.Background(), "alice_1", "wrong password here"); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestRegisterRejectsCaseFoldedDuplicateConcurrently(t *testing.T) {
	s := testService(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for _, name := range []string{"Sample_User", " sample_user "} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if _, _, err := s.Register(context.Background(), name, "correct horse battery"); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(name)
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("successful registrations = %d, want 1", successes)
	}
}

func TestSessionExpiryAndInputValidation(t *testing.T) {
	s := testService(t)
	_, token, err := s.Register(context.Background(), "valid-user", strings.Repeat("x", 12))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveSession(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Register(context.Background(), "ab", "short"); err == nil {
		t.Fatal("invalid username/password accepted")
	}
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at=?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveSession(context.Background(), token); err == nil {
		t.Fatal("expired session resolved")
	}
}
