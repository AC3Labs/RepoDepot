// Package server is The Repo Depot's web app: it finds software on
// GitHub, sends install jobs to agents, tracks versions and notifies.
package server

import (
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var Version = "dev"

//go:embed web
var webFS embed.FS

type Config struct {
	DataDir   string // database, logos
	Listen    string // ":8470"
	AgentsDir string // agent binaries and install scripts offered to machines
	Scheduler bool
}

func ConfigFromEnv() Config {
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return Config{
		DataDir:   get("REPODEPOT_DATA", "/data"),
		Listen:    get("REPODEPOT_LISTEN", ":8470"),
		AgentsDir: get("REPODEPOT_AGENTS", "/agents"),
		Scheduler: get("REPODEPOT_SCHEDULER", "true") == "true",
	}
}

type Server struct {
	cfg      Config
	store    *Store
	settings *Settings
	gh       *GitHub
	runner   *Runner
	checker  Checker
	tmpl     map[string]*template.Template
	limiter  *loginLimiter
	seenBase atomic.Value // the address the app was last opened at, for links
	static   http.Handler
}

func New(cfg Config) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("can't create the data folder %s: %w", cfg.DataDir, err)
	}
	st, err := openStore(filepath.Join(cfg.DataDir, "repodepot.db"))
	if err != nil {
		return nil, err
	}
	set, err := loadSettings(st)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, settings: set, runner: newRunner(), limiter: &loginLimiter{fail: map[string][]time.Time{}}}
	s.gh = newGitHub(set, st)
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	sub, _ := fs.Sub(webFS, "web/static")
	s.static = http.StripPrefix("/static/", http.FileServerFS(sub))
	return s, nil
}

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	s.resumeJobs()
	if s.cfg.Scheduler {
		go s.scheduler(ctx)
	}
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	log.Printf("The Repo Depot %s listening on %s (data in %s)", Version, s.cfg.Listen, s.cfg.DataDir)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	a := s.requireAuth

	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		s.static.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFileFS(w, r, webFS, "web/static/favicon.ico")
	})
	mux.HandleFunc("GET /logos/{file}", s.serveLogo)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })

	mux.HandleFunc("GET /setup", s.showSetup)
	mux.HandleFunc("POST /setup", s.doSetup)
	mux.HandleFunc("GET /login", s.showLogin)
	mux.HandleFunc("POST /login", s.doLogin)
	mux.HandleFunc("POST /logout", a(s.doLogout))
	mux.HandleFunc("GET /invite/{token}", s.showInvite)
	mux.HandleFunc("POST /invite/{token}", s.doInvite)
	mux.HandleFunc("GET /forgot", s.showForgot)
	mux.HandleFunc("POST /forgot", s.doForgot)
	mux.HandleFunc("GET /reset/{token}", s.showReset)
	mux.HandleFunc("POST /reset/{token}", s.doReset)

	// Pages: everyone signed in can look; what they can change depends on
	// their role (admin, member, viewer), checked again by each API call.
	mux.HandleFunc("GET /{$}", a(s.pageDepot))
	mux.HandleFunc("GET /discover", a(s.pageDiscover))
	mux.HandleFunc("GET /repo/{owner}/{name}", a(s.pageRepo))
	mux.HandleFunc("GET /apps/{id}", a(s.pageApp))
	mux.HandleFunc("GET /machines", a(s.pageMachines))
	mux.HandleFunc("GET /activity", a(s.pageActivity))
	mux.HandleFunc("GET /settings", a(s.pageSettings))

	member := func(h http.HandlerFunc) http.HandlerFunc { return s.need(RoleMember, h) }
	admin := func(h http.HandlerFunc) http.HandlerFunc { return s.need(RoleAdmin, h) }

	mux.HandleFunc("GET /api/status", a(s.apiStatus))
	mux.HandleFunc("GET /api/search", a(s.apiSearch))
	mux.HandleFunc("GET /api/starred", a(s.apiStarred))
	mux.HandleFunc("GET /api/repo/{owner}/{name}/inspect", a(s.apiInspect))
	mux.HandleFunc("GET /api/repo/{owner}/{name}/readme", a(s.apiReadme))
	mux.HandleFunc("GET /api/repo/{owner}/{name}/file", a(s.apiRepoFile))
	mux.HandleFunc("POST /api/install", member(s.apiInstall))
	mux.HandleFunc("GET /api/apps", a(s.apiApps))
	mux.HandleFunc("POST /api/apps/update", member(s.apiUpdateMany))
	mux.HandleFunc("GET /api/apps/{id}/versions", a(s.apiVersions))
	mux.HandleFunc("GET /api/apps/{id}/changes", a(s.apiChanges))
	mux.HandleFunc("POST /api/apps/{id}/update", member(s.apiUpdate))
	mux.HandleFunc("POST /api/apps/{id}/reinstall", member(s.apiReinstall))
	mux.HandleFunc("POST /api/apps/{id}/uninstall", member(s.apiUninstall))
	mux.HandleFunc("POST /api/apps/{id}/unregister", member(s.apiUnregister))
	mux.HandleFunc("POST /api/apps/{id}/settings", member(s.apiAppSettings))
	mux.HandleFunc("POST /api/apps/{id}/mark", member(s.apiMarkUpdated))
	mux.HandleFunc("POST /api/apps/{id}/skip", member(s.apiSkip))
	mux.HandleFunc("POST /api/apps/{id}/logo", member(s.apiFindLogo))
	mux.HandleFunc("POST /api/check", member(s.apiCheck))
	mux.HandleFunc("GET /api/jobs", a(s.apiJobs))
	mux.HandleFunc("GET /api/jobs/{id}", a(s.apiJob))
	mux.HandleFunc("GET /api/events", a(s.apiEvents))
	mux.HandleFunc("GET /api/machines", a(s.apiMachines))
	mux.HandleFunc("POST /api/machines", admin(s.apiSaveMachine))
	mux.HandleFunc("POST /api/machines/{id}", admin(s.apiSaveMachine))
	mux.HandleFunc("POST /api/machines/{id}/delete", admin(s.apiDeleteMachine))
	mux.HandleFunc("POST /api/machines/{id}/token", admin(s.apiNewToken))
	mux.HandleFunc("GET /api/machines/{id}/status", a(s.apiMachineStatus))
	mux.HandleFunc("POST /api/settings", admin(s.apiSaveSettings))
	mux.HandleFunc("POST /api/settings/test-ntfy", admin(s.apiTestNtfy))
	mux.HandleFunc("POST /api/settings/test-email", admin(s.apiTestEmail))
	mux.HandleFunc("POST /api/settings/test-webhook", admin(s.apiTestWebhook))
	mux.HandleFunc("GET /api/backup", admin(s.apiBackup))
	mux.HandleFunc("GET /api/users", admin(s.apiUsers))
	mux.HandleFunc("POST /api/users/invite", admin(s.apiInvite))
	mux.HandleFunc("POST /api/users/{id}/resend", admin(s.apiResendInvite))
	mux.HandleFunc("POST /api/users/{id}/role", admin(s.apiUserRole))
	mux.HandleFunc("POST /api/users/{id}/delete", admin(s.apiDeleteUser))
	mux.HandleFunc("POST /api/account", a(s.apiAccount))

	mux.HandleFunc("POST /hook/update", s.hookUpdate)
	mux.HandleFunc("GET /agent/install.sh", s.agentScript("install.sh"))
	mux.HandleFunc("GET /agent/install.ps1", s.agentScript("install.ps1"))
	mux.HandleFunc("GET /agent/download/{platform}", s.agentDownload)

	return s.middleware(mux)
}

// middleware: sessions, security headers, gzip, and remembering the
// address the app is opened at (for links in notifications).
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		if !strings.HasPrefix(r.URL.Path, "/static/") && !strings.HasPrefix(r.URL.Path, "/logos/") {
			h.Set("Cache-Control", "no-store")
		}
		if r.Host != "" && !strings.HasPrefix(r.URL.Path, "/hook/") && !strings.HasPrefix(r.URL.Path, "/agent/") {
			scheme := "http"
			if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			s.seenBase.Store(scheme + "://" + r.Host)
		}
		r = s.loadSession(r)
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && compressible(r.URL.Path) {
			h.Set("Content-Encoding", "gzip")
			h.Add("Vary", "Accept-Encoding")
			gz := gzPool.Get().(*gzip.Writer)
			gz.Reset(w)
			defer func() { gz.Close(); gzPool.Put(gz) }()
			w = &gzWriter{ResponseWriter: w, w: gz}
		}
		next.ServeHTTP(w, r)
	})
}

func compressible(p string) bool {
	return !strings.HasPrefix(p, "/logos/") && !strings.HasPrefix(p, "/agent/download/") && !strings.HasSuffix(p, ".png") && !strings.HasSuffix(p, ".ico")
}

var gzPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return w }}

type gzWriter struct {
	http.ResponseWriter
	w io.Writer
}

func (g *gzWriter) Write(b []byte) (int, error) { return g.w.Write(b) }
func (g *gzWriter) WriteHeader(code int) {
	g.ResponseWriter.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func urlQuery(s string) string { return url.QueryEscape(s) }

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) serveLogo(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("file"))
	p := filepath.Join(s.logoDir(), name)
	if strings.HasSuffix(name, ".svg") {
		// An SVG opened directly can't run scripts against this app.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, p)
}
