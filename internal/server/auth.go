package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID    int64
	Name  string
	Email string
}

type ctxKey int

const (
	userKey ctxKey = iota
	sessKey
)

const sessionCookie = "repodepot_session"

type session struct {
	ID   string
	CSRF string
}

func userFrom(r *http.Request) *User {
	u, _ := r.Context().Value(userKey).(*User)
	return u
}

func sessFrom(r *http.Request) *session {
	s, _ := r.Context().Value(sessKey).(*session)
	return s
}

func hashPassword(p string) string {
	h, _ := bcrypt.GenerateFromPassword([]byte(p), 12)
	return string(h)
}

func (s *Server) userCount() int {
	var n int
	s.store.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n)
	return n
}

func (s *Server) newSession(w http.ResponseWriter, r *http.Request, userID int64) {
	id, csrf := randomHex(32), randomHex(24)
	days := s.settings.Int("session_days")
	exp := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	s.store.db.Exec(`INSERT INTO sessions (id, user_id, csrf, expires_at) VALUES (?, ?, ?, ?)`, id, userID, csrf, exp.UTC().Format(dbTime))
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", Expires: exp})
}

// loadSession attaches the signed-in user, if any.
func (s *Server) loadSession(r *http.Request) *http.Request {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != 64 {
		return r
	}
	u := &User{}
	se := &session{ID: c.Value}
	var exp string
	err = s.store.db.QueryRow(`SELECT u.id, u.name, u.email, s.csrf, s.expires_at FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`, c.Value).
		Scan(&u.ID, &u.Name, &u.Email, &se.CSRF, &exp)
	if err != nil || time.Now().After(parseDB(exp)) {
		return r
	}
	ctx := context.WithValue(r.Context(), userKey, u)
	ctx = context.WithValue(ctx, sessKey, se)
	return r.WithContext(ctx)
}

// requireAuth sends visitors to sign in (pages) or answers 401 (API), and
// checks the CSRF token on everything that changes something.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r) == nil {
			if s.userCount() == 0 {
				http.Redirect(w, r, "/setup", http.StatusFound)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Signed out. Reload the page to sign in again."})
				return
			}
			http.Redirect(w, r, "/login?next="+urlQuery(r.URL.RequestURI()), http.StatusFound)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			tok := r.Header.Get("X-CSRF-Token")
			if tok == "" {
				tok = r.FormValue("_csrf")
			}
			if subtle.ConstantTimeCompare([]byte(tok), []byte(sessFrom(r).CSRF)) != 1 {
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "This page is out of date. Reload it and try again."})
				return
			}
		}
		next(w, r)
	}
}

// loginLimiter locks out an address after too many wrong passwords.
type loginLimiter struct {
	mu   sync.Mutex
	fail map[string][]time.Time
}

func (l *loginLimiter) blocked(ip string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	var recent []time.Time
	for _, t := range l.fail[ip] {
		if time.Since(t) < 15*time.Minute {
			recent = append(recent, t)
		}
	}
	l.fail[ip] = recent
	return len(recent) >= max
}

func (l *loginLimiter) failed(ip string) {
	l.mu.Lock()
	l.fail[ip] = append(l.fail[ip], time.Now())
	l.mu.Unlock()
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.fail, ip)
	l.mu.Unlock()
}

func (s *Server) checkLogin(email, password string) (int64, bool) {
	var id int64
	var hash string
	err := s.store.db.QueryRow(`SELECT id, password FROM users WHERE email = ?`, strings.TrimSpace(email)).Scan(&id, &hash)
	if err != nil {
		bcrypt.CompareHashAndPassword([]byte("$2a$12$C6UzMDM.H6dfI/f/IKcEeO7ZqE3xYqVYYuH8GJZVWJXYBy0eNpqSe"), []byte(password)) // same time either way
		return 0, false
	}
	return id, bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
