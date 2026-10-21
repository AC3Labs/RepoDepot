package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// POST /api/self-update: the server sends a new agent program as the body.
// The agent only installs it if it's signed with The Repo Depot's release
// key (the public half is below; the private half never leaves the build
// machine), so the agent token alone can't make an agent run a program of
// someone's choosing. It must also be newer, match its checksum, and run
// here (`version`) — Windows Smart App Control blocks unsigned programs it
// hasn't seen. Then the agent swaps it in and exits, and its service
// (systemd Restart=always, or the Windows service's restart on failure)
// starts the new one. The previous program stays beside it as .old.

// UpdatePublicKey verifies agent updates. A build that signs its own agents
// sets its own key here.
var UpdatePublicKey = "jA5f+kaO0k6seIWcofu8Uum3ojB1zEIMUBiWb15bguE="

// UpdateMessage is what the release key signs: the program's identity, so
// a signature can't be reused for another version, platform or file.
func UpdateMessage(version, goos, goarch, sha string) []byte {
	return []byte("repodepot-agent-update\n" + version + "\n" + goos + "/" + goarch + "\n" + sha + "\n")
}

const maxAgentSize = 100 << 20

// Swapped out in tests.
var (
	agentExecutable = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(exe)
	}
	restartAgent = func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(1)
	}
)

func (a *Agent) handleSelfUpdate(w http.ResponseWriter, r *http.Request) {
	sum := strings.ToLower(strings.TrimSpace(r.Header.Get("X-RepoDepot-SHA256")))
	version := strings.TrimSpace(r.Header.Get("X-RepoDepot-Version"))
	sig, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Header.Get("X-RepoDepot-Signature")))
	if len(sum) != 64 || version == "" || len(sig) != ed25519.SignatureSize {
		writeJSON(w, 400, map[string]string{"message": "Send the new agent with its SHA-256, version and signature."})
		return
	}
	pub, err := base64.StdEncoding.DecodeString(UpdatePublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		writeJSON(w, 501, map[string]string{"message": "This agent was built without an update key; run the install command to update it."})
		return
	}
	if !ed25519.Verify(pub, UpdateMessage(version, runtime.GOOS, runtime.GOARCH, sum), sig) {
		writeJSON(w, 403, map[string]string{"message": "That agent isn't signed with The Repo Depot's release key, so it wasn't installed."})
		return
	}
	if CompareVersions(version, Version) <= 0 {
		writeJSON(w, 409, map[string]string{"message": fmt.Sprintf("This agent is already %s; it only updates to a newer version.", Version)})
		return
	}
	if a.jobs.busy("") {
		writeJSON(w, 409, map[string]string{"message": "This agent is in the middle of a job; update it when that's finished."})
		return
	}

	exe, err := agentExecutable()
	if err != nil {
		writeJSON(w, 500, map[string]string{"message": "Can't find this agent's own program: " + err.Error()})
		return
	}
	dir := filepath.Dir(exe)
	pattern := ".repodepot-agent-new-*"
	if runtime.GOOS == "windows" {
		pattern += ".exe" // Windows only runs programs ending in .exe
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		writeJSON(w, 500, map[string]string{"message": fmt.Sprintf("Can't write to %s (%v); run the install command to update this agent.", dir, err)})
		return
	}
	newPath := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			os.Remove(newPath)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r.Body, maxAgentSize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		writeJSON(w, 500, map[string]string{"message": "Couldn't receive the new agent: " + err.Error()})
		return
	case n > maxAgentSize:
		writeJSON(w, 413, map[string]string{"message": "The new agent is too big."})
		return
	case hex.EncodeToString(h.Sum(nil)) != sum:
		writeJSON(w, 400, map[string]string{"message": "The new agent arrived damaged (its checksum doesn't match), so nothing was changed."})
		return
	}
	os.Chmod(newPath, 0o755)
	if err := checkNewAgent(newPath, version); err != nil {
		writeJSON(w, 422, map[string]string{"message": err.Error()})
		return
	}

	old := exe + ".old"
	os.Remove(old)
	// A running program can be renamed (on Windows too), just not
	// overwritten.
	if err := os.Rename(exe, old); err != nil {
		writeJSON(w, 500, map[string]string{"message": fmt.Sprintf("Can't replace %s (%v); run the install command to update this agent.", exe, err)})
		return
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Rename(old, exe)
		writeJSON(w, 500, map[string]string{"message": "Can't put the new agent in place: " + err.Error()})
		return
	}
	keep = true
	writeJSON(w, 200, map[string]any{"ok": true, "from": Version, "to": version})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go restartAgent()
}

// checkNewAgent runs the new program once and expects its version back.
func checkNewAgent(path, want string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(strings.ToLower(msg+" "+err.Error()), "application control") {
			return fmt.Errorf("Windows blocked the new agent (Smart App Control or another Application Control policy), so this agent was left as it is")
		}
		return fmt.Errorf("the new agent wouldn't run on this machine (%s), so this agent was left as it is", msg)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		return fmt.Errorf("the new agent says it's version %q, expected %q, so this agent was left as it is", got, want)
	}
	return nil
}

// CompareVersions compares dotted numeric versions ("0.4.0.1"); anything
// that isn't a number (like "dev") counts as 0.
func CompareVersions(a, b string) int {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
