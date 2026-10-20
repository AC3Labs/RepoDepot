package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"repodepot/internal/agent"
)

// fakeGitHub serves just enough of GitHub's API for one repo, me/tool,
// with ETags so the cache's 304 path is used.
type fakeGitHub struct {
	mu       sync.Mutex
	srv      *httptest.Server
	releases []string // newest first
	hits     map[string]int
	badSum   map[string]bool // versions whose published checksum is wrong
	pre      map[string]bool // versions that are pre-releases
}

func tarball(t *testing.T, version string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "#!/bin/sh\necho tool " + version + "\n"
	tw.WriteHeader(&tar.Header{Name: "tool-" + version + "/tool", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{releases: []string{"v1.0.0"}, hits: map[string]int{}, badSum: map[string]bool{}, pre: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[r.URL.Path]++
		rels := append([]string(nil), f.releases...)
		badSum, pre := map[string]bool{}, map[string]bool{}
		for k, v := range f.badSum {
			badSum[k] = v
		}
		for k, v := range f.pre {
			pre[k] = v
		}
		f.mu.Unlock()
		base := f.srv.URL
		reply := func(v any) {
			raw, _ := json.Marshal(v)
			etag := fmt.Sprintf(`"%x"`, len(raw)*31+int(raw[len(raw)/2]))
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			w.Header().Set("X-RateLimit-Remaining", "4999")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Write(raw)
		}
		repo := map[string]any{"full_name": "me/tool", "name": "tool", "owner": map[string]string{"login": "me", "avatar_url": base + "/avatar?v=1"},
			"description": "A tiny tool", "html_url": "https://github.com/me/tool", "stargazers_count": 1234, "default_branch": "main", "topics": []string{"cli"}}
		switch {
		case r.URL.Path == "/repos/me/tool":
			reply(repo)
		case r.URL.Path == "/search/repositories":
			reply(map[string]any{"total_count": 1, "items": []any{repo}})
		case r.URL.Path == "/repos/me/tool/releases":
			var out []any
			for _, v := range rels {
				out = append(out, map[string]any{"tag_name": v, "name": v, "html_url": "https://github.com/me/tool/releases/" + v, "published_at": "2026-10-01T00:00:00Z",
					"prerelease": pre[v], "body_html": "<p>Notes for " + v + "</p><script>x()</script>",
					"assets": []any{
						map[string]any{"name": "tool-" + v + "-linux-amd64.tar.gz", "size": 100, "browser_download_url": base + "/dl/" + v + "/tool-linux-amd64.tar.gz"},
						map[string]any{"name": "tool-" + v + "-windows-amd64.zip", "size": 100, "browser_download_url": base + "/dl/" + v + "/tool-windows-amd64.zip"},
						map[string]any{"name": "checksums.txt", "size": 10, "browser_download_url": base + "/dl/" + v + "/checksums.txt"},
					}})
			}
			reply(out)
		case strings.HasPrefix(r.URL.Path, "/repos/me/tool/releases/tags/"):
			v := strings.TrimPrefix(r.URL.Path, "/repos/me/tool/releases/tags/")
			reply(map[string]any{"tag_name": v, "html_url": "https://github.com/me/tool/releases/" + v, "assets": []any{
				map[string]any{"name": "tool-" + v + "-linux-amd64.tar.gz", "size": 100, "browser_download_url": base + "/dl/" + v + "/tool-linux-amd64.tar.gz"}}})
		case strings.HasPrefix(r.URL.Path, "/repos/me/tool/commits/"):
			reply(map[string]any{"sha": "abcdef1234567890", "html_url": "https://github.com/me/tool/commit/abcdef1", "commit": map[string]any{"message": "Fix things"}})
		case r.URL.Path == "/repos/me/tool/contents/":
			reply([]any{map[string]any{"name": "logo.png", "path": "logo.png", "type": "file", "size": 70, "download_url": base + "/raw/logo.png"},
				map[string]any{"name": "main.go", "path": "main.go", "type": "file"}})
		case strings.HasPrefix(r.URL.Path, "/repos/me/tool/git/trees/"):
			reply(map[string]any{"tree": []any{map[string]any{"path": "main.go", "type": "blob", "size": 10},
				map[string]any{"path": "assets/logo.png", "type": "blob", "size": 70}}})
		case r.URL.Path == "/repos/me/tool/contents/assets/logo.png":
			reply(map[string]any{"name": "logo.png", "path": "assets/logo.png", "type": "file", "size": 70, "download_url": base + "/raw/logo.png"})
		case r.URL.Path == "/repos/me/tool/readme":
			w.Write([]byte(`<article class="markdown-body"><h1>Tool</h1><p>Does things.</p><script>bad()</script></article>`))
		case r.URL.Path == "/raw/logo.png":
			w.Write([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 60)))
		case strings.HasPrefix(r.URL.Path, "/repos/me/tool/compare/"):
			reply(map[string]any{"commits": []any{
				map[string]any{"sha": "1111111aaaa", "html_url": "https://github.com/me/tool/commit/1111111", "commit": map[string]any{"message": "Older change\n\nbody", "author": map[string]any{"name": "Ann"}}},
				map[string]any{"sha": "2222222bbbb", "html_url": "https://github.com/me/tool/commit/2222222", "commit": map[string]any{"message": "Newer change", "author": map[string]any{"name": "Bob"}}}}})
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			v := strings.Split(r.URL.Path, "/")[2]
			sum := sha256.Sum256(tarball(t, v))
			if badSum[v] {
				sum[0] ^= 0xff
			}
			fmt.Fprintf(w, "%x  tool-%s-linux-amd64.tar.gz\n%x  tool-%s-windows-amd64.zip\n", sum, v, sha256.Sum256(nil), v)
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			v := strings.Split(r.URL.Path, "/")[2]
			w.Write(tarball(t, v))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type browser struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

var csrfMeta = regexp.MustCompile(`name="csrf" content="([^"]+)"`)

func (b *browser) page(path string) string {
	b.t.Helper()
	resp, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if m := csrfMeta.FindSubmatch(body); m != nil {
		b.csrf = string(m[1])
	}
	return string(body)
}

func (b *browser) call(method, path string, in any, out any) int {
	b.t.Helper()
	var r io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		r = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, b.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", b.csrf)
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEndToEndInstallUpdateAndUninstall(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the fake release only has a linux/amd64 file")
	}
	gh := newFakeGitHub(t)

	var ntfyMu sync.Mutex
	var ntfyMsgs []map[string]any
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		ntfyMu.Lock()
		ntfyMsgs = append(ntfyMsgs, m)
		ntfyMu.Unlock()
	}))
	defer ntfy.Close()

	// A real agent, in-process.
	appsDir := t.TempDir()
	token := strings.Repeat("a", 32)
	ag := agent.New(&agent.Config{Token: token, AppsDir: filepath.Join(appsDir, "apps")})
	agentSrv := httptest.NewServer(ag.Handler())
	defer agentSrv.Close()
	au, _ := url.Parse(agentSrv.URL)
	port, _ := strconv.Atoi(au.Port())

	s, err := New(Config{DataDir: t.TempDir(), Listen: ":0", AgentsDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.settings.Set("github_api", gh.srv.URL)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	b := &browser{t: t, base: srv.URL, c: &http.Client{Jar: jar}}

	// First run: everything sends you to setup.
	if body := b.page("/"); !strings.Contains(body, "Set up your depot") {
		t.Fatal("expected the setup page")
	}
	resp, _ := b.c.PostForm(srv.URL+"/setup", url.Values{"name": {"Andrew"}, "email": {"a@example.com"}, "password": {"pass-word-1"}, "password_confirm": {"pass-word-1"}})
	resp.Body.Close()
	if body := b.page("/"); !strings.Contains(body, "Your depot is empty") {
		t.Fatalf("expected an empty depot after setup")
	}

	// Writes need the CSRF token.
	saved := b.csrf
	b.csrf = "wrong"
	if code := b.call("POST", "/api/check", nil, nil); code != 403 {
		t.Fatalf("missing CSRF accepted: %d", code)
	}
	b.csrf = saved

	s.settings.Set("ntfy_enabled", "true")
	s.settings.Set("ntfy_server", ntfy.URL)

	var made struct {
		Machine  Machine           `json:"machine"`
		Commands map[string]string `json:"commands"`
	}
	b.call("POST", "/api/machines", map[string]any{"name": "Test box", "host": au.Hostname(), "port": port, "token": token}, &made)
	if made.Machine.ID == 0 || !strings.Contains(made.Commands["linux"], token) {
		t.Fatalf("add machine: %+v", made)
	}
	var st struct{ Machine Machine }
	b.call("GET", fmt.Sprintf("/api/machines/%d/status", made.Machine.ID), nil, &st)
	if st.Machine.Status != "online" || st.Machine.OS != "linux" || st.Machine.AgentVersion == "" {
		t.Fatalf("machine status: %+v", st.Machine)
	}

	var found struct{ Items []searchItem }
	b.call("GET", "/api/search?q=tool", nil, &found)
	if len(found.Items) != 1 || found.Items[0].FullName != "me/tool" {
		t.Fatalf("search: %+v", found)
	}

	var ins struct {
		Inspect  Inspection   `json:"inspect"`
		Machines []machineFit `json:"machines"`
	}
	b.call("GET", "/api/repo/me/tool/inspect", nil, &ins)
	if ins.Inspect.Recommended != "release" || len(ins.Machines) != 1 || ins.Machines[0].Asset != "tool-v1.0.0-linux-amd64.tar.gz" {
		t.Fatalf("inspect: rec=%s machines=%+v", ins.Inspect.Recommended, ins.Machines)
	}
	var readme map[string]string
	b.call("GET", "/api/repo/me/tool/readme", nil, &readme)
	if !strings.Contains(readme["html"], "Does things") || strings.Contains(readme["html"], "script") {
		t.Fatalf("readme: %s", readme["html"])
	}

	var res struct{ Results []installResult }
	b.call("POST", "/api/install", map[string]any{"repo": "me/tool", "machines": []int64{made.Machine.ID}, "method": "release", "track": "release", "notify": true,
		"options": map[string]any{"link_binary": false}}, &res)
	if len(res.Results) != 1 || res.Results[0].Error != "" {
		t.Fatalf("install: %+v", res.Results)
	}
	appID := res.Results[0].AppID
	app := func() *App { a, _ := s.store.App(appID); return a }
	waitFor(t, "install", func() bool { return app().Status == "installed" })
	a := app()
	if a.InstalledRef != "v1.0.0" || a.Options.AssetPattern != "tool-*-linux-amd64.tar.gz" {
		t.Fatalf("after install: %+v", a)
	}
	if jobs, _ := s.store.Jobs(appID, 1); len(jobs) != 1 || !strings.Contains(jobs[0].Log, "Checksum matches") || jobs[0].StartedBy != "Andrew" {
		t.Fatalf("install job didn't verify the checksum or record who started it: %+v", jobs)
	}
	toolPath := filepath.Join(appsDir, "apps", "tool", "app", "tool")
	if b, _ := os.ReadFile(toolPath); !strings.Contains(string(b), "v1.0.0") {
		t.Fatalf("installed file: %q", b)
	}
	waitFor(t, "logo", func() bool { return app().Logo != "" })

	// Nothing new yet.
	var chk struct{ Updates int }
	b.call("POST", "/api/check", nil, &chk)
	if chk.Updates != 0 {
		t.Fatalf("phantom update: %+v", chk)
	}

	// A new release comes out: found, notified with a one-tap button.
	gh.mu.Lock()
	gh.releases = []string{"v1.1.0", "v1.0.0"}
	gh.mu.Unlock()
	b.call("POST", "/api/check", nil, &chk)
	if chk.Updates != 1 || !app().UpdateAvailable() {
		t.Fatalf("update not found: %+v %+v", chk, app())
	}
	var hook string
	waitFor(t, "ntfy message", func() bool {
		ntfyMu.Lock()
		defer ntfyMu.Unlock()
		for _, m := range ntfyMsgs {
			if strings.HasPrefix(fmt.Sprint(m["title"]), "Update for tool") {
				for _, act := range m["actions"].([]any) {
					if a := act.(map[string]any); a["label"] == "Update now" {
						hook = a["url"].(string)
					}
				}
			}
		}
		return hook != ""
	})
	// A second check doesn't notify again.
	b.call("POST", "/api/check", nil, &chk)
	time.Sleep(100 * time.Millisecond)
	ntfyMu.Lock()
	n := len(ntfyMsgs)
	ntfyMu.Unlock()
	if n != 1 {
		t.Fatalf("expected one notification, got %d", n)
	}

	// A tampered link is refused; the real one starts the update.
	if r, _ := http.Post(strings.Replace(hook, "v1.1.0", "v9.9.9", 1), "", nil); r.StatusCode != 403 {
		t.Fatalf("tampered hook: %d", r.StatusCode)
	}
	if r, _ := http.Post(hook, "", nil); r.StatusCode != 200 {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("hook: %d %s", r.StatusCode, body)
	}
	waitFor(t, "update", func() bool { a := app(); return a.Status == "installed" && a.InstalledRef == "v1.1.0" })
	if b, _ := os.ReadFile(toolPath); !strings.Contains(string(b), "v1.1.0") {
		t.Fatalf("updated file: %q", b)
	}
	if s.updateCount() != 0 {
		t.Fatal("still shows an update")
	}

	if a := app(); a.PreviousRef != "v1.0.0" {
		t.Fatalf("previous version not remembered: %+v", a)
	}

	// Roll back: v1.0.0 goes back on, and v1.1.0 is skipped so it isn't
	// offered (or auto-installed) again straight away.
	var rb struct {
		JobID   int64  `json:"job_id"`
		Skipped string `json:"skipped"`
	}
	b.call("POST", fmt.Sprintf("/api/apps/%d/update", appID), map[string]string{"ref": "v1.0.0"}, &rb)
	if rb.JobID == 0 || rb.Skipped != "v1.1.0" {
		t.Fatalf("roll back: %+v", rb)
	}
	waitFor(t, "roll back", func() bool { a := app(); return a.Status == "installed" && a.InstalledRef == "v1.0.0" })
	if b, _ := os.ReadFile(toolPath); !strings.Contains(string(b), "v1.0.0") {
		t.Fatalf("rolled-back file: %q", b)
	}
	if a := app(); a.UpdateAvailable() || a.SkippedRef != "v1.1.0" || a.PreviousRef != "v1.1.0" || s.updateCount() != 0 {
		t.Fatalf("after roll back: %+v", a)
	}
	b.call("POST", fmt.Sprintf("/api/apps/%d/skip", appID), map[string]bool{"undo": true}, nil)
	if !app().UpdateAvailable() {
		t.Fatal("un-skipping didn't offer the update again")
	}

	// A download that doesn't match its published checksum is never
	// installed; the app stays on the version it had.
	gh.mu.Lock()
	gh.releases = []string{"v1.2.0", "v1.1.0", "v1.0.0"}
	gh.badSum["v1.2.0"] = true
	gh.mu.Unlock()
	b.call("POST", "/api/check", nil, &chk)
	b.call("POST", fmt.Sprintf("/api/apps/%d/update", appID), map[string]any{}, nil)
	waitFor(t, "failed update", func() bool { a := app(); return a.Status != "updating" && a.ActiveJob == 0 })
	if jobs, _ := s.store.Jobs(appID, 1); jobs[0].Status != "error" || !strings.Contains(jobs[0].Error, "checksum") {
		t.Fatalf("tampered download wasn't refused: %+v", jobs[0])
	}
	if a := app(); a.InstalledRef != "v1.0.0" || a.Status != "installed" {
		t.Fatalf("after a refused download: %+v", a)
	}
	if b, _ := os.ReadFile(toolPath); !strings.Contains(string(b), "v1.0.0") {
		t.Fatalf("file changed by a refused download: %q", b)
	}

	// Pages render.
	for _, p := range []string{"/", "/discover?q=tool", "/repo/me/tool", fmt.Sprintf("/apps/%d", appID), "/machines", "/activity", "/settings"} {
		if body := b.page(p); !strings.Contains(body, "The Repo Depot") {
			t.Errorf("%s didn't render", p)
		}
	}

	// Uninstall with data: the app and its folder are gone.
	if code := b.call("POST", fmt.Sprintf("/api/apps/%d/uninstall", appID), map[string]bool{"remove_data": true}, nil); code != 200 {
		t.Fatalf("uninstall: %d", code)
	}
	waitFor(t, "uninstall", func() bool { _, err := s.store.App(appID); return err != nil })
	if _, err := os.Stat(filepath.Join(appsDir, "apps", "tool")); err == nil {
		t.Fatal("folder still there")
	}
	jobs, _ := s.store.Jobs(0, 10)
	if len(jobs) != 5 || jobs[0].Action != "uninstall" || jobs[0].Status != "done" {
		t.Fatalf("jobs: %+v", jobs)
	}

	// The cache answered repeat lookups without re-downloading.
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if gh.hits["/search/repositories"] != 1 {
		t.Errorf("search hit GitHub %d times", gh.hits["/search/repositories"])
	}
}

func TestTrackOnlyAppsAreWatchedButNeverTouched(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.releases = []string{"v1.1.0", "v1.0.0"}
	s, err := New(Config{DataDir: t.TempDir(), Listen: ":0", AgentsDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.settings.Set("github_api", gh.srv.URL)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	b := &browser{t: t, base: srv.URL, c: &http.Client{Jar: jar}}
	resp, _ := b.c.PostForm(srv.URL+"/setup", url.Values{"name": {"A"}, "email": {"a@example.com"}, "password": {"pass-word-1"}, "password_confirm": {"pass-word-1"}})
	resp.Body.Close()
	b.page("/")
	// An offline machine is fine: nothing is ever sent to it.
	var made struct{ Machine Machine }
	b.call("POST", "/api/machines", map[string]any{"name": "Elsewhere", "host": "192.0.2.1", "port": 8471}, &made)

	var res struct{ Results []installResult }
	b.call("POST", "/api/install", map[string]any{"repo": "me/tool", "machines": []int64{made.Machine.ID}, "method": "track", "track": "release",
		"version": "v1.0.0", "dirs": map[string]string{fmt.Sprint(made.Machine.ID): "/srv/tool"}, "options": map[string]string{"open_url": "http://192.0.2.1:9000"}, "notify": true}, &res)
	if len(res.Results) != 1 || res.Results[0].Error != "" || res.Results[0].JobID != 0 {
		t.Fatalf("track: %+v", res.Results)
	}
	id := res.Results[0].AppID
	waitFor(t, "update found", func() bool { a, _ := s.store.App(id); return a.UpdateAvailable() })
	a, _ := s.store.App(id)
	if a.Location != "/srv/tool" || a.Options.OpenURL != "http://192.0.2.1:9000" || a.Status != "installed" {
		t.Fatalf("tracked app: %+v", a)
	}
	// It can't be updated or uninstalled from here, and Update all skips it.
	if code := b.call("POST", fmt.Sprintf("/api/apps/%d/update", id), map[string]any{}, nil); code != 409 {
		t.Fatalf("update of a tracked app: %d", code)
	}
	if code := b.call("POST", fmt.Sprintf("/api/apps/%d/uninstall", id), map[string]any{}, nil); code != 409 {
		t.Fatalf("uninstall of a tracked app: %d", code)
	}
	var many struct {
		Started int
		Failed  []string
	}
	b.call("POST", "/api/apps/update", map[string]any{"all": true}, &many)
	if many.Started != 0 || len(many.Failed) != 0 {
		t.Fatalf("update all touched a tracked app: %+v", many)
	}
	// Marked updated by hand: no update showing any more.
	b.call("POST", fmt.Sprintf("/api/apps/%d/mark", id), map[string]any{}, nil)
	if a, _ := s.store.App(id); a.InstalledRef != "v1.1.0" || a.UpdateAvailable() {
		t.Fatalf("after mark: %+v", a)
	}
	jobs, _ := s.store.Jobs(0, 10)
	if len(jobs) != 0 {
		t.Fatalf("a tracked app made jobs: %+v", jobs)
	}
	if code := b.call("POST", fmt.Sprintf("/api/apps/%d/unregister", id), nil, nil); code != 200 {
		t.Fatalf("unregister: %d", code)
	}
}
