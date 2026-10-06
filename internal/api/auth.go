package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"t2va/internal/identity"
)

const sessionCookie = "t2va_session"

type sessionContextKey struct{}

func contextWithSession(ctx context.Context, s identity.Session) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, s)
}

func requestSession(r *http.Request) (identity.Session, bool) {
	s, ok := r.Context().Value(sessionContextKey{}).(identity.Session)
	return s, ok
}

func requestPrincipal(r *http.Request) (identity.Principal, bool) {
	s, ok := requestSession(r)
	return s.Principal, ok
}

func (h *Handler) resolveBrowserSession(r *http.Request) (identity.Session, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return identity.Session{}, identity.ErrInvalidCredentials
	}
	return h.identity.ResolveSession(r.Context(), cookie.Value)
}

func (h *Handler) authAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/auth/session" && r.Method == http.MethodGet:
		s, err := h.resolveBrowserSession(r)
		if err != nil {
			if errors.Is(err, identity.ErrInvalidCredentials) {
				writeAPIError(w, http.StatusUnauthorized, "authentication_required")
			} else {
				writeAPIError(w, http.StatusInternalServerError, "session_lookup_failed")
			}
			return
		}
		writeJSON(w, http.StatusOK, sessionResponse(s))
	case r.URL.Path == "/api/v1/auth/guest" && r.Method == http.MethodPost:
		if !h.validOrigin(r) {
			writeAPIError(w, http.StatusForbidden, "invalid_origin")
			return
		}
		if s, err := h.resolveBrowserSession(r); err == nil {
			writeJSON(w, http.StatusOK, sessionResponse(s))
			return
		} else if !errors.Is(err, identity.ErrInvalidCredentials) {
			writeAPIError(w, http.StatusInternalServerError, "session_lookup_failed")
			return
		}
		s, raw, err := h.identity.CreateGuest(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "session_creation_failed")
			return
		}
		h.setSessionCookie(w, raw, s)
		writeJSON(w, http.StatusCreated, sessionResponse(s))
	case r.URL.Path == "/api/v1/auth/register" && r.Method == http.MethodPost:
		h.authCredentials(w, r, true)
	case r.URL.Path == "/api/v1/auth/login" && r.Method == http.MethodPost:
		h.authCredentials(w, r, false)
	case r.URL.Path == "/api/v1/auth/logout" && r.Method == http.MethodPost:
		if !h.validOrigin(r) {
			writeAPIError(w, http.StatusForbidden, "invalid_origin")
			return
		}
		if s, err := h.resolveBrowserSession(r); err == nil {
			if !h.validCSRF(r, s) {
				writeAPIError(w, http.StatusForbidden, "invalid_csrf_token")
				return
			}
			if c, e := r.Cookie(sessionCookie); e == nil {
				if e := h.identity.RevokeSession(r.Context(), c.Value); e != nil {
					writeAPIError(w, http.StatusInternalServerError, "logout_failed")
					return
				}
			}
		} else if !errors.Is(err, identity.ErrInvalidCredentials) {
			writeAPIError(w, http.StatusInternalServerError, "session_lookup_failed")
			return
		}
		h.clearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) authCredentials(w http.ResponseWriter, r *http.Request, register bool) {
	if !h.validOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "invalid_origin")
		return
	}
	if s, err := h.resolveBrowserSession(r); err == nil {
		if !h.validCSRF(r, s) {
			writeAPIError(w, http.StatusForbidden, "invalid_csrf_token")
			return
		}
	} else if !errors.Is(err, identity.ErrInvalidCredentials) {
		writeAPIError(w, http.StatusInternalServerError, "session_lookup_failed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body struct {
		Username string
		Password string
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.Decode(new(any)) != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var s identity.Session
	var raw string
	var err error
	if register {
		s, raw, err = h.identity.Register(r.Context(), body.Username, body.Password)
	} else {
		s, raw, err = h.identity.Login(r.Context(), body.Username, body.Password)
	}
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrUsernameTaken):
			writeAPIError(w, http.StatusConflict, "username_unavailable")
		case errors.Is(err, identity.ErrInvalidInput):
			writeAPIError(w, http.StatusBadRequest, "invalid_credentials_format")
		case errors.Is(err, identity.ErrInvalidCredentials):
			writeAPIError(w, http.StatusUnauthorized, "invalid_username_or_password")
		default:
			writeAPIError(w, http.StatusInternalServerError, "authentication_failed")
		}
		return
	}
	h.setSessionCookie(w, raw, s)
	writeJSON(w, http.StatusOK, sessionResponse(s))
}

func sessionResponse(s identity.Session) map[string]any {
	return map[string]any{"principal": s.Principal, "username": s.Username, "csrf_token": s.CSRFToken, "expires_at": s.ExpiresAt}
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, raw string, s identity.Session) {
	maxAge := int(s.ExpiresAt.Sub(time.Now()).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: raw, Path: "/", HttpOnly: true, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge, Expires: s.ExpiresAt})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func (h *Handler) validOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	expectedScheme := "http"
	if r.TLS != nil {
		expectedScheme = "https"
	}
	if forwarded := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])); forwarded == "http" || forwarded == "https" {
		expectedScheme = forwarded
	}
	return strings.EqualFold(u.Host, r.Host) && u.Scheme == expectedScheme
}

func (h *Handler) validCSRF(r *http.Request, s identity.Session) bool {
	provided := r.Header.Get("X-CSRF-Token")
	return provided != "" && len(provided) == len(s.CSRFToken) && subtle.ConstantTimeCompare([]byte(provided), []byte(s.CSRFToken)) == 1
}

func writeAPIError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
