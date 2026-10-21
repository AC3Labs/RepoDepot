package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"repodepot/internal/agent"
	"repodepot/internal/spec"
)

// Updating agents from the Machines page. The server sends the agent build
// it carries (with the image's agents, see cmd/signagents) to the agent,
// which installs it only if it's signed with the release key, then waits
// for it to come back as the new version. Agents older than selfUpdateFrom
// can't do this: they need the install command once more.

const selfUpdateFrom = "0.5.0.0"

type AgentBuild struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	File      string `json:"file"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature,omitempty"`
}

type AgentManifest struct {
	Version string       `json:"version"`
	Builds  []AgentBuild `json:"builds"`
}

func (s *Server) agentManifest() *AgentManifest {
	raw, err := os.ReadFile(filepath.Join(s.cfg.AgentsDir, "manifest.json"))
	if err != nil {
		return nil
	}
	m := &AgentManifest{}
	if json.Unmarshal(raw, m) != nil || m.Version == "" {
		return nil
	}
	return m
}

func (m *AgentManifest) find(goos, goarch string) *AgentBuild {
	for i := range m.Builds {
		if m.Builds[i].OS == goos && m.Builds[i].Arch == goarch {
			return &m.Builds[i]
		}
	}
	return nil
}

// agentUpdateInfo: is this machine's agent behind the agents this server
// carries, and can it be updated from here (or why not)?
type agentUpdateInfo struct {
	To      string `json:"to,omitempty"`
	CanPush bool   `json:"can_push"`
	Note    string `json:"note,omitempty"`
}

func (s *Server) agentUpdateFor(m *Machine) agentUpdateInfo {
	man := s.agentManifest()
	if man == nil || m.AgentVersion == "" || m.Status != "online" || agent.CompareVersions(m.AgentVersion, man.Version) >= 0 {
		return agentUpdateInfo{}
	}
	info := agentUpdateInfo{To: man.Version}
	build := man.find(m.OS, m.Arch)
	switch {
	case agent.CompareVersions(m.AgentVersion, selfUpdateFrom) < 0:
		info.Note = "This agent is older than " + selfUpdateFrom + ", so it needs the install command once more; after that it updates from here."
	case build == nil:
		info.Note = "There's no agent build for " + m.OS + "/" + m.Arch + "."
	case build.Signature == "":
		info.Note = "This server's agent builds aren't signed, so agents won't accept them as updates. Run the install command on the machine instead."
	default:
		info.CanPush = true
	}
	return info
}

var errAgentCurrent = errors.New("already up to date")

var agentUpdating sync.Map // machine id -> true while being updated

// POST /api/machines/{id}/update-agent
func (s *Server) apiUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	m, err := s.store.Machine(id)
	if err != nil {
		jsonError(w, 404, "No such machine.")
		return
	}
	if _, busy := agentUpdating.LoadOrStore(m.ID, true); busy {
		jsonError(w, 409, m.Name+"'s agent is already being updated.")
		return
	}
	defer agentUpdating.Delete(m.ID)
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	from := m.AgentVersion
	version, err := s.pushAgentUpdate(ctx, m)
	if errors.Is(err, errAgentCurrent) {
		writeJSON(w, 200, map[string]any{"machine": m, "message": m.Name + "'s agent is already up to date."})
		return
	}
	if err != nil {
		jsonError(w, 422, err.Error())
		return
	}
	s.event(r, "agent.update", fmt.Sprintf("Updated the agent on %s from %s to %s", m.Name, from, version))
	writeJSON(w, 200, map[string]any{"machine": m, "message": m.Name + "'s agent is now " + version + "."})
}

func (s *Server) pushAgentUpdate(ctx context.Context, m *Machine) (string, error) {
	s.refreshMachine(ctx, m)
	if m.Status != "online" {
		return "", fmt.Errorf("%s's agent isn't answering", m.Name)
	}
	man := s.agentManifest()
	if man != nil && agent.CompareVersions(m.AgentVersion, man.Version) >= 0 {
		return m.AgentVersion, errAgentCurrent
	}
	info := s.agentUpdateFor(m)
	if !info.CanPush {
		if info.Note == "" {
			info.Note = "This server has no newer agent to send."
		}
		return "", errors.New(info.Note)
	}
	build := man.find(m.OS, m.Arch)
	f, err := os.Open(filepath.Join(s.cfg.AgentsDir, build.File))
	if err != nil {
		return "", fmt.Errorf("this server's agent build is missing: %w", err)
	}
	defer f.Close()

	sendCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, m.BaseURL()+"/api/self-update", f)
	if err != nil {
		return "", err
	}
	req.ContentLength = build.Size
	req.Header.Set("Authorization", "Bearer "+m.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-RepoDepot-SHA256", build.SHA256)
	req.Header.Set("X-RepoDepot-Version", man.Version)
	req.Header.Set("X-RepoDepot-Signature", build.Signature)
	client := s.agent(m).client
	c := *client
	c.Timeout = 0 // the request's context limits it
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't send the update to %s: %s", m.Name, shortErr(err))
	}
	var answer struct {
		Message string `json:"message"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&answer)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		if answer.Message == "" {
			answer.Message = resp.Status
		}
		return "", errors.New(answer.Message)
	}

	// The agent restarts itself; wait for the new one to answer.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(agentUpdatePoll):
		}
		var h spec.Health
		hctx, hcancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.agent(m).do(hctx, http.MethodGet, "/api/health", nil, &h)
		hcancel()
		if err == nil && h.Version == man.Version {
			s.refreshMachine(ctx, m)
			return man.Version, nil
		}
	}
	return "", fmt.Errorf("%s took the update but didn't come back as %s within 90 seconds; check its service on the machine", m.Name, man.Version)
}

var agentUpdatePoll = 2 * time.Second
