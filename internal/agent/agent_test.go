package agent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
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
