// Package agent is The Repo Depot agent: a small service on each machine
// that installs, updates and removes software when the server asks.
package agent

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"repodepot/internal/spec"
)

// Version is set at build time (-ldflags "-X repodepot/internal/agent.Version=...").
var Version = "dev"

type Config struct {
	Token   string `json:"token"`
	Listen  string `json:"listen"`   // default ":8471"
	AppsDir string `json:"apps_dir"` // default /opt/repodepot/apps or C:\RepoDepot\apps
	Cert    string `json:"cert_file,omitempty"`
	Key     string `json:"key_file,omitempty"`
}

// DefaultConfigPath is where the installers put config.json.
func DefaultConfigPath() string {
	if p := os.Getenv("REPODEPOT_AGENT_CONFIG"); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\RepoDepot\agent\config.json`
	}
	return "/etc/repodepot-agent/config.json"
}

func defaultAppsDir() string {
	if runtime.GOOS == "windows" {
		return `C:\RepoDepot\apps`
	}
	return "/opt/repodepot/apps"
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no agent config at %s: %w", path, err)
	}
	// Windows PowerShell 5.1 writes UTF-8 with a byte-order mark.
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	c := &Config{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("agent config %s isn't valid JSON: %w", path, err)
	}
	if len(c.Token) < 16 {
		return nil, fmt.Errorf("agent config %s needs a token of at least 16 characters", path)
	}
	if c.Listen == "" {
		c.Listen = ":8471"
	}
	if c.AppsDir == "" {
		c.AppsDir = defaultAppsDir()
	}
	return c, nil
}

type Agent struct {
	cfg  *Config
	jobs *jobBook
	srv  *http.Server
}

func New(cfg *Config) *Agent {
	a := &Agent{cfg: cfg, jobs: newJobBook()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.handleHealth)
	mux.HandleFunc("POST /api/jobs", a.handleCreateJob)
	mux.HandleFunc("GET /api/jobs/{id}", a.handleGetJob)
	a.srv = &http.Server{Addr: cfg.Listen, Handler: a.auth(mux), ReadHeaderTimeout: 10 * time.Second}
	return a
}

func (a *Agent) Handler() http.Handler { return a.srv.Handler }

// Run serves until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.srv.Shutdown(c)
	}()
	log.Printf("The Repo Depot agent %s listening on %s (apps in %s)", Version, a.cfg.Listen, a.cfg.AppsDir)
	var err error
	if a.cfg.Cert != "" && a.cfg.Key != "" {
		err = a.srv.ListenAndServeTLS(a.cfg.Cert, a.cfg.Key)
	} else {
		err = a.srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a *Agent) auth(next http.Handler) http.Handler {
	want := []byte("Bearer " + a.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Invalid or missing agent token."})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, 200, spec.Health{
		Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: Version,
		Docker: dockerUsable(r.Context()), AppsDir: a.cfg.AppsDir, Admin: isAdmin(),
	})
}

// dockerUsable checks `docker compose version` once a minute at most.
var dockerCheck struct {
	at time.Time
	ok bool
}

func dockerUsable(ctx context.Context) bool {
	if time.Since(dockerCheck.at) < time.Minute {
		return dockerCheck.ok
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	dockerCheck.ok = exec.CommandContext(ctx, "docker", "compose", "version").Run() == nil
	dockerCheck.at = time.Now()
	return dockerCheck.ok
}

func (a *Agent) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var j spec.Job
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&j); err != nil {
		writeJSON(w, 400, map[string]string{"message": "invalid job: " + err.Error()})
		return
	}
	if err := a.validate(&j); err != nil {
		writeJSON(w, 400, map[string]string{"message": err.Error()})
		return
	}
	if a.jobs.busy(j.App) {
		writeJSON(w, 409, map[string]string{"message": j.App + " already has a job running on this machine."})
		return
	}
	st := a.jobs.start(j.App, func(l *jobLog) (string, error) { return a.run(context.Background(), &j, l) })
	writeJSON(w, 202, st)
}

func (a *Agent) handleGetJob(w http.ResponseWriter, r *http.Request) {
	st, ok := a.jobs.get(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"message": "Unknown job."})
		return
	}
	writeJSON(w, 200, st)
}

// validate keeps a job inside sane bounds: a plain app name, a known
// method and action, and an install folder that is absolute.
func (a *Agent) validate(j *spec.Job) error {
	if !validAppName(j.App) {
		return fmt.Errorf("app name %q must be 1-64 letters, digits, dots, dashes or underscores", j.App)
	}
	switch j.Action {
	case spec.ActionInstall, spec.ActionUpdate, spec.ActionUninstall:
	default:
		return fmt.Errorf("unknown action %q", j.Action)
	}
	switch j.Method {
	case spec.MethodCompose, spec.MethodRelease, spec.MethodSource:
	default:
		return fmt.Errorf("unknown method %q", j.Method)
	}
	if j.Dir == "" {
		j.Dir = filepath.Join(a.cfg.AppsDir, j.App)
	}
	if !filepath.IsAbs(j.Dir) {
		return fmt.Errorf("install folder %q must be an absolute path", j.Dir)
	}
	j.Dir = filepath.Clean(j.Dir)
	if isRootish(j.Dir) {
		return fmt.Errorf("refusing to install into %s", j.Dir)
	}
	if j.Action != spec.ActionUninstall {
		switch j.Method {
		case spec.MethodRelease:
			if j.AssetURL == "" {
				return errors.New("a release install needs asset_url")
			}
		default:
			if j.SourceURL == "" {
				return errors.New("this install needs source_url")
			}
		}
	}
	if j.ComposeFile != "" && (filepath.IsAbs(j.ComposeFile) || strings.Contains(filepath.ToSlash(j.ComposeFile), "..")) {
		return fmt.Errorf("compose_file %q must be a path inside the repo", j.ComposeFile)
	}
	return nil
}

func validAppName(s string) bool {
	if s == "" || len(s) > 64 || s[0] == '.' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// isRootish refuses folders whose removal would be a disaster.
func isRootish(dir string) bool {
	d := strings.TrimRight(filepath.ToSlash(dir), "/")
	if d == "" || len(strings.Split(strings.Trim(d, "/"), "/")) < 3 {
		return true // "/", "/opt/apps", "C:", "C:/Users/me": at least three levels deep
	}
	for _, bad := range []string{"/usr", "/etc", "/bin", "/sbin", "/lib", "/var", "/boot", "/home", "/root", "/proc", "/sys", "/dev"} {
		if d == bad {
			return true
		}
	}
	return false
}
