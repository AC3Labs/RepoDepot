package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AC3Labs/RepoDepot/internal/spec"
)

type Machine struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Token        string `json:"-"`
	UseTLS       bool   `json:"use_tls"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Hostname     string `json:"hostname"`
	AgentVersion string `json:"agent_version"`
	Docker       bool   `json:"docker"`
	Admin        bool   `json:"admin"`
	AppsDir      string `json:"apps_dir"`
	Status       string `json:"status"`
	LastSeen     string `json:"last_seen"`
}

const machineCols = `id, name, host, port, token, use_tls, os, arch, hostname, agent_version, docker, admin, apps_dir, status, last_seen`

func scanMachine(sc interface{ Scan(...any) error }) (*Machine, error) {
	m := &Machine{}
	err := sc.Scan(&m.ID, &m.Name, &m.Host, &m.Port, &m.Token, &m.UseTLS, &m.OS, &m.Arch, &m.Hostname, &m.AgentVersion, &m.Docker, &m.Admin, &m.AppsDir, &m.Status, &m.LastSeen)
	return m, err
}

func (s *Store) Machines() ([]*Machine, error) {
	rows, err := s.db.Query(`SELECT ` + machineCols + ` FROM machines ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Store) Machine(id int64) (*Machine, error) {
	return scanMachine(s.db.QueryRow(`SELECT `+machineCols+` FROM machines WHERE id = ?`, id))
}

func (m *Machine) BaseURL() string {
	scheme := "http"
	if m.UseTLS {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(m.Host, strconv.Itoa(m.Port))
}

// OSLabel is "Linux", "Windows" or "" when not known yet.
func (m *Machine) OSLabel() string {
	switch m.OS {
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	}
	return m.OS
}

// ---- agent client ----

type AgentClient struct {
	m      *Machine
	client *http.Client
}

func (s *Server) agent(m *Machine) *AgentClient {
	timeout := time.Duration(s.settings.Int("agent_timeout_seconds")) * time.Second
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // agents use self-signed certificates on the LAN
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	}
	return &AgentClient{m: m, client: &http.Client{Transport: tr, Timeout: 20 * time.Second}}
}

func (a *AgentClient) do(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.m.BaseURL()+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.m.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach %s: %s", a.m.Name, shortErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		if resp.StatusCode == 401 {
			e.Message = "the agent on " + a.m.Name + " rejected the token; check it matches the agent's config"
		}
		return fmt.Errorf("%s", e.Message)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := err.(net.Error); ok && e.Timeout() {
		return "no answer (asleep, off, or a firewall)"
	}
	s := err.Error()
	if strings.Contains(s, "Client.Timeout") || strings.Contains(s, "i/o timeout") {
		return "no answer (asleep, off, or a firewall)"
	}
	if strings.Contains(s, "connection refused") {
		return "connection refused (is the agent running?)"
	}
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}

func (a *AgentClient) Health(ctx context.Context) (*spec.Health, error) {
	h := &spec.Health{}
	return h, a.do(ctx, http.MethodGet, "/api/health", nil, h)
}

func (a *AgentClient) StartJob(ctx context.Context, j *spec.Job) (*spec.JobState, error) {
	st := &spec.JobState{}
	return st, a.do(ctx, http.MethodPost, "/api/jobs", j, st)
}

func (a *AgentClient) Job(ctx context.Context, id string) (*spec.JobState, error) {
	st := &spec.JobState{}
	return st, a.do(ctx, http.MethodGet, "/api/jobs/"+id, nil, st)
}

// refreshMachine asks the agent how it is and records the answer.
func (s *Server) refreshMachine(ctx context.Context, m *Machine) {
	h, err := s.agent(m).Health(ctx)
	was := m.Status
	if err != nil {
		m.Status = "offline"
		s.store.db.Exec(`UPDATE machines SET status = 'offline' WHERE id = ?`, m.ID)
	} else {
		m.Status, m.OS, m.Arch, m.Hostname, m.AgentVersion, m.Docker, m.Admin, m.AppsDir = "online", h.OS, h.Arch, h.Hostname, h.Version, h.Docker, h.Admin, h.AppsDir
		m.LastSeen = nowDB()
		s.store.db.Exec(`UPDATE machines SET status = 'online', os = ?, arch = ?, hostname = ?, agent_version = ?, docker = ?, admin = ?, apps_dir = ?, last_seen = ? WHERE id = ?`,
			h.OS, h.Arch, h.Hostname, h.Version, h.Docker, h.Admin, h.AppsDir, m.LastSeen, m.ID)
	}
	if was != "" && was != "unknown" && was != m.Status && s.settings.Bool("notify_machines") {
		if m.Status == "online" {
			s.notify(fmt.Sprintf("%s is back online", m.Name), "The agent answered again.", "white_check_mark", "/machines", nil)
		} else {
			s.notify(fmt.Sprintf("%s went offline", m.Name), "Its agent stopped answering: "+shortErr(err), "warning", "/machines", nil)
		}
	}
}
