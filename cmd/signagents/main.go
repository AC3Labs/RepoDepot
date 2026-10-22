// Command signagents writes manifest.json beside the built agents: each
// one's size, SHA-256 and, when a key is given, its signature by The Repo
// Depot's release key. Agents only install updates carrying that signature.
//
//	go run ./cmd/signagents -dir dist/agents -version 0.5.0.0
//
// The key is a base64 Ed25519 seed in $REPODEPOT_SIGNING_KEY, or in the file
// named by $REPODEPOT_SIGNING_KEY_FILE. Without one the manifest is written
// unsigned: agents install from it, but the server can't push updates.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/AC3Labs/RepoDepot/internal/agent"
	"github.com/AC3Labs/RepoDepot/internal/server"
)

var platforms = [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}}

func main() {
	dir := flag.String("dir", "dist/agents", "folder with the built agents")
	version := flag.String("version", "", "the agents' version")
	flag.Parse()
	if *version == "" {
		log.Fatal("-version is required")
	}
	key, err := signingKey()
	if err != nil {
		log.Fatal(err)
	}
	if key == nil {
		log.Print("no signing key: the agents are listed unsigned (they can't be pushed as updates)")
	}
	man := server.AgentManifest{Version: *version}
	for _, p := range platforms {
		name := "repodepot-agent-" + p[0] + "-" + p[1]
		if p[0] == "windows" {
			name += ".exe"
		}
		size, sum, err := hashFile(filepath.Join(*dir, name))
		if err != nil {
			log.Fatal(err)
		}
		b := server.AgentBuild{OS: p[0], Arch: p[1], File: name, Size: size, SHA256: sum}
		if key != nil {
			b.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, agent.UpdateMessage(*version, p[0], p[1], sum)))
		}
		man.Builds = append(man.Builds, b)
	}
	raw, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(*dir, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("manifest.json: %d agents, signed: %v\n", len(man.Builds), key != nil)
}

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return n, hex.EncodeToString(h.Sum(nil)), err
}

func signingKey() (ed25519.PrivateKey, error) {
	val := os.Getenv("REPODEPOT_SIGNING_KEY")
	if f := os.Getenv("REPODEPOT_SIGNING_KEY_FILE"); val == "" && f != "" {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		val = string(raw)
	}
	val = strings.TrimSpace(val)
	if val == "" {
		return nil, nil
	}
	seed, err := base64.StdEncoding.DecodeString(val)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("the signing key must be a base64 Ed25519 seed (%d bytes)", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
