package agent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"repodepot/internal/spec"
)

func makeTgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "abc"}})
	for name, body := range files {
		mode := int64(0o644)
		if strings.HasSuffix(name, "/tool") {
			mode = 0o755
		}
		tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractStripsTheTopFolderAndKeepsDotFiles(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "a.tar.gz")
	os.WriteFile(arc, makeTgz(t, map[string]string{"tool-1.0/tool": "#!/bin/sh\n", "tool-1.0/.env.example": "X=1", "tool-1.0/docs/readme.md": "hi"}), 0o644)
	dest := filepath.Join(dir, "out")
	if err := extract(arc, "tgz", dest, "auto"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"tool", ".env.example", "docs/readme.md"} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("%s missing: %v", f, err)
		}
	}
}

func TestExtractRefusesPathsOutsideTheFolder(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "evil.zip")
	f, _ := os.Create(arc)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../../escaped.txt")
	w.Write([]byte("no"))
	zw.Close()
	f.Close()
	err := extract(arc, "zip", filepath.Join(dir, "out"), "never")
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escaped.txt")); err == nil {
		t.Fatal("file escaped")
	}
}

func TestSingleTop(t *testing.T) {
	if !singleTop([]string{"a/", "a/b", "a/c/d"}) || singleTop([]string{"a/b", "c"}) || singleTop([]string{"a/b", "x/y"}) || singleTop([]string{"onlyfile"}) {
		t.Fatal("singleTop")
	}
}

func TestValidateRejectsBadJobs(t *testing.T) {
	a := New(&Config{Token: strings.Repeat("x", 20), AppsDir: "/opt/repodepot/apps"})
	cases := []spec.Job{
		{Action: "install", Method: "release", App: "../etc", AssetURL: "http://x"},
		{Action: "install", Method: "release", App: "ok", Dir: "/etc", AssetURL: "http://x"},
		{Action: "install", Method: "release", App: "ok", Dir: "relative/path", AssetURL: "http://x"},
		{Action: "install", Method: "compose", App: "ok", SourceURL: "http://x", ComposeFile: "../../x.yml"},
		{Action: "explode", Method: "release", App: "ok"},
		{Action: "install", Method: "release", App: "ok"}, // no asset
	}
	for i, j := range cases {
		if err := a.validate(&j); err == nil {
			t.Errorf("case %d accepted: %+v", i, j)
		}
	}
	ok := spec.Job{Action: "install", Method: "release", App: "lazydocker", AssetURL: "http://x"}
	if err := a.validate(&ok); err != nil || ok.Dir != filepath.Join("/opt/repodepot/apps", "lazydocker") {
		t.Fatalf("valid job: %v %s", err, ok.Dir)
	}
}

func TestReleaseInstallUpdateAndUninstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	v := "1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(makeTgz(t, map[string]string{"tool-v" + v + "/tool": "#!/bin/sh\necho v" + v + "\n", "tool-v" + v + "/LICENSE": "MIT"}))
	}))
	defer srv.Close()
	root := t.TempDir()
	a := New(&Config{Token: strings.Repeat("x", 20), AppsDir: root})
	dir := filepath.Join(root, "apps", "tool")
	j := &spec.Job{Action: "install", Method: "release", App: "tool", Repo: "me/tool", Ref: "v1", Dir: dir, AssetURL: srv.URL + "/tool-linux-amd64.tar.gz", AssetName: "tool-linux-amd64.tar.gz"}
	l := &jobLog{}
	loc, err := a.run(context.Background(), j, l)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, strings.Join(l.snapshot(), "\n"))
	}
	if loc != filepath.Join(dir, "app") {
		t.Fatalf("location %s", loc)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "app", "tool")); !strings.Contains(string(b), "v1") {
		t.Fatal("v1 not installed")
	}
	os.WriteFile(filepath.Join(dir, "data", "state.db"), []byte("keep"), 0o644)

	v = "2"
	j.Action, j.Ref = "update", "v2"
	if _, err := a.run(context.Background(), j, &jobLog{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "app", "tool")); !strings.Contains(string(b), "v2") {
		t.Fatal("v2 not installed")
	}
	if _, err := os.Stat(filepath.Join(dir, "app.old")); err == nil {
		t.Fatal("old copy left behind")
	}
	if m := readManifest(dir); m == nil || m.Ref != "v2" {
		t.Fatalf("manifest %+v", m)
	}

	j.Action = "uninstall"
	if _, err := a.run(context.Background(), j, &jobLog{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "app")); err == nil {
		t.Fatal("program not removed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "data", "state.db")); string(b) != "keep" {
		t.Fatal("data should be kept unless asked")
	}
	j.RemoveData = true
	if _, err := a.run(context.Background(), j, &jobLog{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("folder not deleted")
	}
}

func TestUninstallLeavesForeignFoldersAlone(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "apps", "notours")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "important.txt"), []byte("x"), 0o644)
	a := New(&Config{Token: strings.Repeat("x", 20), AppsDir: root})
	_, err := a.run(context.Background(), &spec.Job{Action: "uninstall", Method: "release", App: "notours", Dir: dir, RemoveData: true}, &jobLog{})
	if err == nil {
		t.Fatal("removed a folder without a manifest")
	}
	if _, err := os.Stat(filepath.Join(dir, "important.txt")); err != nil {
		t.Fatal("deleted foreign file")
	}
}

func TestSourceInstallRunsTheBuildCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(makeTgz(t, map[string]string{"me-proj-abc123/Makefile": "all:\n", "me-proj-abc123/main.c": "int main(){}"}))
	}))
	defer srv.Close()
	root := t.TempDir()
	a := New(&Config{Token: strings.Repeat("x", 20), AppsDir: root})
	dir := filepath.Join(root, "apps", "proj")
	l := &jobLog{}
	_, err := a.run(context.Background(), &spec.Job{Action: "install", Method: "source", App: "proj", Repo: "me/proj", Ref: "abc123", Dir: dir,
		SourceURL: srv.URL, PostInstall: "echo built $REPODEPOT_REF > built.txt"}, l)
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(l.snapshot(), "\n"))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "src", "built.txt")); strings.TrimSpace(string(b)) != "built abc123" {
		t.Fatalf("build command output %q", b)
	}
}

func TestJobsAPIWithTokenAndOneJobPerApp(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.Write(makeTgz(t, map[string]string{"x/tool": "#!/bin/sh\n"}))
	}))
	defer srv.Close()
	defer close(block)
	root := t.TempDir()
	tok := strings.Repeat("t", 24)
	a := New(&Config{Token: tok, AppsDir: root})
	h := a.Handler()
	body := `{"action":"install","method":"release","app":"tool","asset_url":"` + srv.URL + `/tool.tar.gz","asset_name":"tool.tar.gz"}`
	req := httptest.NewRequest("POST", "/api/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
	for i, want := range []int{202, 409} {
		req = httptest.NewRequest("POST", "/api/jobs", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	_ = time.Second
}

func TestVerifySHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("hello"), 0o644)
	good := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if err := verifySHA256(p, good); err != nil {
		t.Fatalf("good sum refused: %v", err)
	}
	if err := verifySHA256(p, strings.ToUpper(good)); err != nil {
		t.Fatalf("upper-case sum refused: %v", err)
	}
	if err := verifySHA256(p, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad sum accepted: %v", err)
	}
}

// Windows PowerShell 5.1 writes the config with a UTF-8 byte-order mark.
func TestLoadConfigBOM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte("\xef\xbb\xbf{\"token\":\"0123456789abcdef0123\",\"listen\":\":8471\"}"), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8471" {
		t.Fatalf("listen = %q", c.Listen)
	}
}

// fakeProgram stands in for a new agent: it prints a version, like
// `repodepot-agent version`.
func fakeProgram(version string) []byte { return []byte("#!/bin/sh\necho " + version + "\n") }

func TestSelfUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in program is a shell script")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	oldKey := UpdatePublicKey
	UpdatePublicKey = base64.StdEncoding.EncodeToString(pub)
	defer func() { UpdatePublicKey = oldKey }()
	current := Version // "dev" in tests: any numbered version is newer
	dir := t.TempDir()
	exe := filepath.Join(dir, "repodepot-agent")
	os.WriteFile(exe, fakeProgram(current), 0o755)
	agentExecutable = func() (string, error) { return exe, nil }
	restarted := make(chan bool, 1)
	restartAgent = func() { restarted <- true }
	a := New(&Config{Token: strings.Repeat("t", 32), Listen: ":0", AppsDir: t.TempDir()})

	send := func(body []byte, version, sum string, key ed25519.PrivateKey) int {
		if sum == "" {
			s := sha256.Sum256(body)
			sum = hex.EncodeToString(s[:])
		}
		req := httptest.NewRequest(http.MethodPost, "/api/self-update", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
		req.Header.Set("X-RepoDepot-SHA256", sum)
		req.Header.Set("X-RepoDepot-Version", version)
		req.Header.Set("X-RepoDepot-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(key, UpdateMessage(version, runtime.GOOS, runtime.GOARCH, sum))))
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	unchanged := func(why string) {
		t.Helper()
		if got, _ := os.ReadFile(exe); !bytes.Equal(got, fakeProgram(current)) {
			t.Fatalf("%s: the agent was replaced", why)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, ".repodepot-agent-new-*")); len(left) > 0 {
			t.Fatalf("%s: left %v behind", why, left)
		}
	}
	next := "0.6.0.0"
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if code := send(fakeProgram(next), next, "", other); code != 403 {
		t.Fatalf("foreign key: %d", code)
	}
	unchanged("foreign key")
	good := sha256.Sum256(fakeProgram(next))
	if code := send([]byte("something else"), next, hex.EncodeToString(good[:]), priv); code != 400 {
		t.Fatalf("damaged: %d", code)
	}
	unchanged("damaged")
	if code := send(fakeProgram(current), current, "", priv); code != 409 {
		t.Fatalf("same version: %d", code)
	}
	unchanged("same version")
	if code := send([]byte("not a program"), next, "", priv); code != 422 {
		t.Fatalf("won't run: %d", code)
	}
	unchanged("won't run")
	if code := send(fakeProgram("0.5.9.0"), next, "", priv); code != 422 {
		t.Fatalf("wrong version inside: %d", code)
	}
	unchanged("wrong version inside")
	// Not while a job runs.
	release := make(chan bool)
	a.jobs.start("busy-app", func(*jobLog) (string, error) { <-release; return "", nil })
	if code := send(fakeProgram(next), next, "", priv); code != 409 {
		t.Fatalf("during a job: %d", code)
	}
	close(release)
	waitIdle(t, a)
	unchanged("during a job")

	if code := send(fakeProgram(next), next, "", priv); code != 200 {
		t.Fatalf("update: %d", code)
	}
	<-restarted
	if got, _ := os.ReadFile(exe); !bytes.Equal(got, fakeProgram(next)) {
		t.Fatalf("new agent not in place: %q", got)
	}
	if got, _ := os.ReadFile(exe + ".old"); !bytes.Equal(got, fakeProgram(current)) {
		t.Fatalf("old agent not kept: %q", got)
	}
}

func waitIdle(t *testing.T, a *Agent) {
	t.Helper()
	for i := 0; i < 200 && a.jobs.busy(""); i++ {
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"0.5.0.0", "0.4.0.1", 1}, {"0.4.0.1", "0.5.0.0", -1}, {"v0.5.0.0", "0.5.0.0", 0}, {"dev", "0.0.0.1", -1}, {"0.10.0.0", "0.9.0.0", 1}} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d", c.a, c.b, got)
		}
	}
}
