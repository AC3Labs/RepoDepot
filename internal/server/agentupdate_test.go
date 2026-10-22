package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AC3Labs/RepoDepot/internal/agent"
	"github.com/AC3Labs/RepoDepot/internal/spec"
)

// updatableAgent answers like a real agent: it checks an update's
// signature and checksum the same way, then comes back as the new version.
type updatableAgent struct {
	mu      sync.Mutex
	version string
	pub     ed25519.PublicKey
	got     []byte
	srv     *httptest.Server
}

func newUpdatableAgent(t *testing.T, version string, pub ed25519.PublicKey, token string) *updatableAgent {
	a := &updatableAgent{version: version, pub: pub}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		switch r.URL.Path {
		case "/api/health":
			h := spec.Health{Hostname: "box", OS: "linux", Arch: "amd64", Version: a.version, Admin: true}
			if agent.CompareVersions(a.version, selfUpdateFrom) >= 0 {
				h.Features = []string{"self-update"}
			}
			json.NewEncoder(w).Encode(h)
		case "/api/self-update":
			body, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(body)
			sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-RepoDepot-Signature"))
			v := r.Header.Get("X-RepoDepot-Version")
			if hex.EncodeToString(sum[:]) != r.Header.Get("X-RepoDepot-SHA256") || !ed25519.Verify(a.pub, agent.UpdateMessage(v, "linux", "amd64", hex.EncodeToString(sum[:])), sig) {
				w.WriteHeader(403)
				json.NewEncoder(w).Encode(map[string]string{"message": "That agent isn't signed with The Repo Depot's release key, so it wasn't installed."})
				return
			}
			a.got = body
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
			go func() { // "restarts" as the new version
				time.Sleep(50 * time.Millisecond)
				a.mu.Lock()
				a.version = v
				a.mu.Unlock()
			}()
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// writeAgents puts a signed (or unsigned) agent build and its manifest in
// the server's agents folder, like scripts/build.sh does.
func writeAgents(t *testing.T, dir, version string, program []byte, key ed25519.PrivateKey) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "repodepot-agent-linux-amd64"), program, 0o644)
	sum := sha256.Sum256(program)
	b := AgentBuild{OS: "linux", Arch: "amd64", File: "repodepot-agent-linux-amd64", Size: int64(len(program)), SHA256: hex.EncodeToString(sum[:])}
	if key != nil {
		b.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, agent.UpdateMessage(version, "linux", "amd64", b.SHA256)))
	}
	raw, _ := json.Marshal(AgentManifest{Version: version, Builds: []AgentBuild{b}})
	os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644)
}

func TestAgentUpdatesFromTheMachinesPage(t *testing.T) {
	old := agentUpdatePoll
	agentUpdatePoll = 20 * time.Millisecond
	defer func() { agentUpdatePoll = old }()

	s, _, admin := testServer(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	program := []byte("the new agent")
	writeAgents(t, s.cfg.AgentsDir, "0.6.0.0", program, priv)

	add := func(name string, a *updatableAgent, token string) int64 {
		u, _ := url.Parse(a.srv.URL)
		port, _ := strconv.Atoi(u.Port())
		var made struct{ Machine Machine }
		admin.call("POST", "/api/machines", map[string]any{"name": name, "host": u.Hostname(), "port": port, "token": token}, &made)
		return made.Machine.ID
	}
	status := func(id int64) agentUpdateInfo {
		var st struct {
			Update agentUpdateInfo `json:"agent_update"`
		}
		admin.call("GET", fmt.Sprintf("/api/machines/%d/status", id), nil, &st)
		return st.Update
	}
	tokNew, tokOld := strings.Repeat("n", 32), strings.Repeat("o", 32)
	recent := newUpdatableAgent(t, "0.5.0.0", pub, tokNew)
	legacy := newUpdatableAgent(t, "0.4.0.1", pub, tokOld)
	idRecent, idLegacy := add("Recent", recent, tokNew), add("Legacy", legacy, tokOld)

	if u := status(idRecent); u.To != "0.6.0.0" || !u.CanPush {
		t.Fatalf("recent agent: %+v", u)
	}
	if u := status(idLegacy); u.To != "0.6.0.0" || u.CanPush || !strings.Contains(u.Note, "install command once more") {
		t.Fatalf("legacy agent: %+v", u)
	}
	if page := admin.page("/machines"); !strings.Contains(page, "Update agent") || !strings.Contains(page, "/agent/install.sh | sudo sh") {
		t.Fatal("the Machines page lacks the update controls")
	}

	// Too old to update itself: says so, sends nothing.
	if code := admin.call("POST", fmt.Sprintf("/api/machines/%d/update-agent", idLegacy), nil, nil); code != 422 || legacy.got != nil {
		t.Fatalf("legacy update: %d", code)
	}
	// The real thing.
	var res struct {
		Machine Machine
		Message string
	}
	if code := admin.call("POST", fmt.Sprintf("/api/machines/%d/update-agent", idRecent), nil, &res); code != 200 {
		t.Fatalf("update: %d %+v", code, res)
	}
	if string(recent.got) != string(program) || res.Machine.AgentVersion != "0.6.0.0" || !strings.Contains(res.Message, "now 0.6.0.0") {
		t.Fatalf("after update: got %q, %+v", recent.got, res)
	}
	if u := status(idRecent); u.To != "" {
		t.Fatalf("still offered after updating: %+v", u)
	}
	if ev := s.events(10); len(ev) == 0 || !strings.Contains(ev[0].Detail, "from 0.5.0.0 to 0.6.0.0") {
		t.Fatalf("audit log: %+v", ev)
	}

	// Unsigned builds aren't pushed; builds signed with another key are
	// refused by the agent.
	writeAgents(t, s.cfg.AgentsDir, "0.7.0.0", program, nil)
	if u := status(idRecent); u.CanPush || !strings.Contains(u.Note, "aren't signed") {
		t.Fatalf("unsigned: %+v", u)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	writeAgents(t, s.cfg.AgentsDir, "0.7.0.0", program, other)
	var fail struct{ Message string }
	if code := admin.call("POST", fmt.Sprintf("/api/machines/%d/update-agent", idRecent), nil, &fail); code != 422 || !strings.Contains(fail.Message, "release key") {
		t.Fatalf("foreign key: %d %+v", code, fail)
	}
}

func TestAgentsUpdateAutomatically(t *testing.T) {
	old := agentUpdatePoll
	agentUpdatePoll = 20 * time.Millisecond
	defer func() { agentUpdatePoll = old }()

	s, _, admin := testServer(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	writeAgents(t, s.cfg.AgentsDir, "0.6.0.0", []byte("the new agent"), priv)
	tok := strings.Repeat("a", 32)
	ag := newUpdatableAgent(t, "0.5.0.0", pub, tok)
	u, _ := url.Parse(ag.srv.URL)
	port, _ := strconv.Atoi(u.Port())
	var made struct{ Machine Machine }
	admin.call("POST", "/api/machines", map[string]any{"name": "Box", "host": u.Hostname(), "port": port, "token": tok}, &made)

	// Switched off: nothing happens.
	s.settings.Set("agent_auto_update", "false")
	s.checkMachines(context.Background())
	s.autoUpdateAgents(context.Background())
	if ag.got != nil {
		t.Fatal("updated with automatic updates off")
	}

	// On (the default): the machine check brings it up to date, no click.
	s.settings.Set("agent_auto_update", "true")
	s.autoUpdateAgents(context.Background())
	if m, _ := s.store.Machine(made.Machine.ID); m.AgentVersion != "0.6.0.0" || ag.got == nil {
		t.Fatalf("not updated automatically: %+v", m)
	}
	if ev := s.events(5); !strings.Contains(ev[0].Detail, "from 0.5.0.0 to 0.6.0.0 automatically") || ev[0].User != "System" {
		t.Fatalf("audit log: %+v", ev[0])
	}

	// A version the agent refuses is tried once, not at every check.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	writeAgents(t, s.cfg.AgentsDir, "0.7.0.0", []byte("the new agent"), other)
	s.autoUpdateAgents(context.Background())
	s.autoUpdateAgents(context.Background())
	failed := 0
	for _, e := range s.events(10) {
		if strings.Contains(e.Detail, "to 0.7.0.0 failed") {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("refused version tried %d times", failed)
	}
}
