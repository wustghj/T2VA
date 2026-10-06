package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type PrincipalType string

const (
	User   PrincipalType = "user"
	Guest  PrincipalType = "guest"
	System PrincipalType = "system"
)

type Principal struct {
	Type PrincipalType `json:"type"`
	ID   string        `json:"id"`
}
type Session struct {
	Principal Principal `json:"principal"`
	Username  string    `json:"username,omitempty"`
	CSRFToken string    `json:"csrf_token,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Service struct {
	db        *sql.DB
	now       func() time.Time
	dummyHash string
}

var ErrInvalidCredentials = errors.New("invalid credentials")
var ErrInvalidInput = errors.New("invalid input")
var ErrUsernameTaken = errors.New("username unavailable")
var usernamePattern = regexp.MustCompile(`^[\p{L}\p{N}_-]+$`)

const sessionLifetime = 30 * 24 * time.Hour
const argonTime uint32 = 3
const argonMemory uint32 = 64 * 1024
const argonThreads uint8 = 2
const argonKeyLen uint32 = 32

func NewService(ctx context.Context, db *sql.DB) (*Service, error) {
	for _, q := range []string{
		`SELECT id, username, username_key, password_hash, created_at FROM users LIMIT 0`,
		`SELECT token_hash, principal_type, principal_id, username, csrf_token, created_at, expires_at FROM sessions LIMIT 0`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("identity schema is not migrated: %w", err)
		}
	}
	dummyHash, err := hashPassword("authentication timing equalization password")
	if err != nil {
		return nil, fmt.Errorf("prepare credential verifier: %w", err)
	}
	return &Service{db: db, now: time.Now, dummyHash: dummyHash}, nil
}
func normalizeUsername(name string) (display, key string, err error) {
	display = strings.TrimSpace(norm.NFC.String(name))
	n := len([]rune(display))
	if n < 3 || n > 32 || !usernamePattern.MatchString(display) {
		return "", "", ErrInvalidInput
	}
	return display, cases.Fold().String(display), nil
}
func hashPassword(password string) (string, error) {
	if len([]byte(password)) < 12 || len([]byte(password)) > 1024 {
		return "", ErrInvalidInput
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum)), nil
}
func verifyPassword(encoded, password string) bool {
	if len(password) < 12 || len(password) > 1024 {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" || parts[1] != "v=19" {
		return false
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil || memory < 8*1024 || memory > 256*1024 || iterations < 1 || iterations > 10 || threads < 1 || threads > 8 {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[3])
	want, e2 := base64.RawStdEncoding.DecodeString(parts[4])
	if e1 != nil || e2 != nil || len(salt) < 8 || len(salt) > 64 || len(want) != int(argonKeyLen) {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
func token() (raw, digest string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(raw))
	return raw, base64.RawURLEncoding.EncodeToString(h[:]), nil
}
func (s *Service) createSession(ctx context.Context, p Principal, username string) (Session, string, error) {
	raw, digest, err := token()
	if err != nil {
		return Session{}, "", err
	}
	csrfRaw := make([]byte, 32)
	if _, err := rand.Read(csrfRaw); err != nil {
		return Session{}, "", err
	}
	now := s.now().UTC()
	expires := now.Add(sessionLifetime)
	csrf := base64.RawURLEncoding.EncodeToString(csrfRaw)
	_, err = s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash,principal_type,principal_id,username,csrf_token,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`, digest, p.Type, p.ID, username, csrf, now.Format(time.RFC3339Nano), expires.Format(time.RFC3339Nano))
	if err != nil {
		return Session{}, "", err
	}
	return Session{Principal: p, Username: username, CSRFToken: csrf, ExpiresAt: expires}, raw, nil
}
func (s *Service) CreateGuest(ctx context.Context) (Session, string, error) {
	return s.createSession(ctx, Principal{Type: Guest, ID: uuid.NewString()}, "")
}
func (s *Service) Register(ctx context.Context, username, password string) (Session, string, error) {
	display, key, err := normalizeUsername(username)
	if err != nil {
		return Session{}, "", err
	}
	encoded, err := hashPassword(password)
	if err != nil {
		return Session{}, "", err
	}
	id := uuid.NewString()
	_, err = s.db.ExecContext(ctx, `INSERT INTO users(id,username,username_key,password_hash,created_at) VALUES(?,?,?,?,?)`, id, display, key, encoded, s.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return Session{}, "", ErrUsernameTaken
		}
		return Session{}, "", err
	}
	session, raw, err := s.createSession(ctx, Principal{Type: User, ID: id}, display)
	if err != nil {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
		return Session{}, "", err
	}
	return session, raw, nil
}
func (s *Service) Login(ctx context.Context, username, password string) (Session, string, error) {
	_, key, err := normalizeUsername(username)
	if err != nil {
		_ = verifyPassword(s.dummyHash, password)
		return Session{}, "", ErrInvalidCredentials
	}
	var id, display, encoded string
	err = s.db.QueryRowContext(ctx, `SELECT id,username,password_hash FROM users WHERE username_key=?`, key).Scan(&id, &display, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		_ = verifyPassword(s.dummyHash, password)
		return Session{}, "", ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, "", err
	}
	if !verifyPassword(encoded, password) {
		return Session{}, "", ErrInvalidCredentials
	}
	return s.createSession(ctx, Principal{Type: User, ID: id}, display)
}
func (s *Service) ResolveSession(ctx context.Context, raw string) (Session, error) {
	if raw == "" {
		return Session{}, ErrInvalidCredentials
	}
	sum := sha256.Sum256([]byte(raw))
	digest := base64.RawURLEncoding.EncodeToString(sum[:])
	var result Session
	var typ string
	var expiry string
	err := s.db.QueryRowContext(ctx, `SELECT principal_type,principal_id,username,csrf_token,expires_at FROM sessions WHERE token_hash=?`, digest).Scan(&typ, &result.Principal.ID, &result.Username, &result.CSRFToken, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, err
	}
	result.Principal.Type = PrincipalType(typ)
	result.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiry)
	if err != nil || !result.ExpiresAt.After(s.now()) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, digest)
		return Session{}, ErrInvalidCredentials
	}
	return result, nil
}
func (s *Service) RevokeSession(ctx context.Context, raw string) error {
	if raw == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(raw))
	digest := base64.RawURLEncoding.EncodeToString(sum[:])
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, digest)
	return err
}

func (s *Service) CleanupExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, s.now().UTC().Format(time.RFC3339Nano))
	return err
}
