package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AC3Labs/RepoDepot/internal/spec"
)

// run carries out one job and returns where the app lives, for display.
func (a *Agent) run(ctx context.Context, j *spec.Job, l *jobLog) (string, error) {
	l.Printf("%s %s (%s) on %s", titleCase(j.Action), j.Repo, j.Method, hostname())
	if j.Ref != "" && j.Action != spec.ActionUninstall {
		l.Printf("Version: %s", j.Ref)
	}
	l.Printf("Folder: %s", j.Dir)
	if j.Action == spec.ActionUninstall {
		return j.Dir, a.uninstall(ctx, j, l)
	}
	if j.Method == spec.MethodCommand {
		if fi, err := os.Stat(j.Dir); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("the folder %s doesn't exist on this machine", j.Dir)
		}
		l.Printf("Running the update command")
		return j.Dir, runShell(ctx, l, j.Dir, jobEnv(j), j.Command)
	}
	if err := os.MkdirAll(j.Dir, 0o755); err != nil {
		return "", err
	}
	switch j.Method {
	case spec.MethodCompose:
		return a.installCompose(ctx, j, l)
	case spec.MethodRelease:
		return a.installRelease(ctx, j, l)
	default:
		return a.installSource(ctx, j, l)
	}
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// ---- manifest ----

func manifestPath(dir string) string { return filepath.Join(dir, ".repodepot.json") }

func readManifest(dir string) *spec.Manifest {
	raw, err := os.ReadFile(manifestPath(dir))
	if err != nil {
		return nil
	}
	m := &spec.Manifest{}
	if json.Unmarshal(raw, m) != nil {
		return nil
	}
	return m
}

func writeManifest(dir string, m *spec.Manifest) error {
	m.Installed = time.Now().UTC().Format(time.RFC3339)
	raw, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(manifestPath(dir), raw, 0o644)
}

// ---- commands ----

// runCmd runs a program, streaming its output into the job log.
func runCmd(ctx context.Context, l *jobLog, dir string, env []string, name string, args ...string) error {
	l.Printf("$ %s %s", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = l, l
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

// runShell runs a user-supplied command (post-install) with the system shell.
func runShell(ctx context.Context, l *jobLog, dir string, env []string, command string) error {
	if runtime.GOOS == "windows" {
		return runCmd(ctx, l, dir, env, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	}
	return runCmd(ctx, l, dir, env, "sh", "-c", command)
}

func jobEnv(j *spec.Job) []string {
	return []string{"REPODEPOT_APP=" + j.App, "REPODEPOT_DIR=" + j.Dir, "REPODEPOT_REF=" + j.Ref, "REPODEPOT_REPO=" + j.Repo}
}

func (a *Agent) postInstall(ctx context.Context, j *spec.Job, l *jobLog, dir string) error {
	if strings.TrimSpace(j.PostInstall) == "" {
		return nil
	}
	l.Printf("Running the post-install command")
	return runShell(ctx, l, dir, jobEnv(j), j.PostInstall)
}

// ---- compose ----

func composeProject(app string) string {
	return "rd-" + strings.ToLower(strings.NewReplacer(".", "-", "_", "-").Replace(app))
}

func composeArgs(m *spec.Manifest, dir string) []string {
	args := []string{"compose", "-p", m.Project, "-f", m.Compose}
	if _, err := os.Stat(filepath.Join(dir, filepath.Dir(m.Compose), "repodepot.override.yml")); err == nil {
		args = append(args, "-f", filepath.Join(filepath.Dir(m.Compose), "repodepot.override.yml"))
	}
	return args
}

func (a *Agent) installCompose(ctx context.Context, j *spec.Job, l *jobLog) (string, error) {
	if !dockerUsable(ctx) {
		return "", errors.New("Docker with the compose plugin isn't available on this machine")
	}
	if err := a.fetchSource(ctx, j, l, j.Dir); err != nil {
		return "", err
	}
	compose := j.ComposeFile
	if j.GeneratedCompose != "" {
		if compose == "" {
			compose = "repodepot.compose.yml"
		}
		if err := os.WriteFile(filepath.Join(j.Dir, compose), []byte(j.GeneratedCompose), 0o644); err != nil {
			return "", err
		}
		l.Printf("Wrote %s", compose)
	}
	if compose == "" {
		compose = findCompose(j.Dir)
	}
	if compose == "" {
		return "", errors.New("no docker-compose.yml or compose.yaml found in the repo")
	}
	composeDir := filepath.Join(j.Dir, filepath.Dir(compose))
	if err := writeOptional(filepath.Join(composeDir, "repodepot.override.yml"), j.ComposeOverride, l, "compose override"); err != nil {
		return "", err
	}
	if j.Env != "" {
		if err := os.WriteFile(filepath.Join(composeDir, ".env"), []byte(j.Env), 0o600); err != nil {
			return "", err
		}
		l.Printf("Wrote .env")
	}
	m := &spec.Manifest{App: j.App, Repo: j.Repo, Method: j.Method, Ref: j.Ref, Compose: filepath.ToSlash(compose), Project: composeProject(j.App)}
	args := composeArgs(m, j.Dir)
	l.Printf("Pulling images")
	if err := runCmd(ctx, l, j.Dir, nil, "docker", append(args, "pull", "--ignore-buildable", "--quiet")...); err != nil {
		l.Printf("  (pull failed; continuing with what's available)")
	}
	l.Printf("Starting containers")
	if err := runCmd(ctx, l, j.Dir, nil, "docker", append(args, "up", "-d", "--build", "--remove-orphans")...); err != nil {
		return "", err
	}
	if err := a.postInstall(ctx, j, l, composeDir); err != nil {
		return "", err
	}
	runCmd(ctx, l, j.Dir, nil, "docker", append(args, "ps", "--format", "table {{.Name}}\t{{.Status}}\t{{.Ports}}")...)
	return j.Dir, writeManifest(j.Dir, m)
}

// findCompose looks for the usual compose file names at the top of the
// repo, then one level down (docker/, deploy/).
func findCompose(dir string) string {
	names := []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return n
		}
	}
	for _, sub := range []string{"docker", "deploy", ".docker"} {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(dir, sub, n)); err == nil {
				return sub + "/" + n
			}
		}
	}
	return ""
}

func writeOptional(path, content string, l *jobLog, what string) error {
	if strings.TrimSpace(content) == "" {
		if _, err := os.Stat(path); err == nil {
			l.Printf("Removed the %s", what)
		}
		os.Remove(path)
		return nil
	}
	l.Printf("Wrote the %s", what)
	return os.WriteFile(path, []byte(content), 0o644)
}

// fetchSource downloads the repo at the job's ref and unpacks it over
// dest. Files the app created (data, .env) are left alone.
func (a *Agent) fetchSource(ctx context.Context, j *spec.Job, l *jobLog, dest string) error {
	l.Printf("Downloading the source")
	tmp := filepath.Join(j.Dir, ".download-source.tar.gz")
	defer os.Remove(tmp)
	if err := download(ctx, j.SourceURL, j.Token, tmp, l); err != nil {
		return err
	}
	l.Printf("Unpacking")
	return extract(tmp, "tgz", dest, "always")
}

// ---- release files ----

func (a *Agent) installRelease(ctx context.Context, j *spec.Job, l *jobLog) (string, error) {
	name := filepath.Base(j.AssetName)
	if name == "" || name == "." {
		name = "download"
	}
	l.Printf("Downloading %s", name)
	tmp := filepath.Join(j.Dir, ".download-"+name)
	defer os.Remove(tmp)
	if err := download(ctx, j.AssetURL, j.Token, tmp, l); err != nil {
		return "", err
	}
	if j.AssetSHA256 != "" {
		if err := verifySHA256(tmp, j.AssetSHA256); err != nil {
			return "", err
		}
		l.Printf("✓ Checksum matches (SHA-256)")
	}
	old := readManifest(j.Dir)
	m := &spec.Manifest{App: j.App, Repo: j.Repo, Method: j.Method, Ref: j.Ref}
	if old != nil {
		m.Service, m.Links = old.Service, old.Links
	}
	lower := strings.ToLower(name)
	appDir := filepath.Join(j.Dir, "app")
	dataDir := filepath.Join(j.Dir, "data")
	os.MkdirAll(dataDir, 0o755)

	switch {
	case strings.HasSuffix(lower, ".deb"):
		pkg, err := exec.CommandContext(ctx, "dpkg-deb", "-f", tmp, "Package").Output()
		if err != nil {
			return "", fmt.Errorf("couldn't read the .deb package name: %w", err)
		}
		m.Package = strings.TrimSpace(string(pkg))
		if err := runCmd(ctx, l, j.Dir, []string{"DEBIAN_FRONTEND=noninteractive"}, "apt-get", "install", "-y", "--allow-downgrades", tmp); err != nil {
			return "", err
		}
		if err := a.postInstall(ctx, j, l, j.Dir); err != nil {
			return "", err
		}
		return "package " + m.Package, writeManifest(j.Dir, m)
	case strings.HasSuffix(lower, ".rpm"):
		pkg, err := exec.CommandContext(ctx, "rpm", "-qp", "--qf", "%{NAME}", tmp).Output()
		if err != nil {
			return "", fmt.Errorf("couldn't read the .rpm package name: %w", err)
		}
		m.Package = strings.TrimSpace(string(pkg))
		if err := runCmd(ctx, l, j.Dir, nil, "rpm", "-Uvh", "--replacepkgs", "--oldpackage", tmp); err != nil {
			return "", err
		}
		if err := a.postInstall(ctx, j, l, j.Dir); err != nil {
			return "", err
		}
		return "package " + m.Package, writeManifest(j.Dir, m)
	case strings.HasSuffix(lower, ".msi"):
		os.MkdirAll(appDir, 0o755)
		keep := filepath.Join(appDir, name)
		os.Remove(keep)
		if err := os.Rename(tmp, keep); err != nil {
			return "", err
		}
		m.Package = keep
		if err := runCmd(ctx, l, j.Dir, nil, "msiexec", "/i", keep, "/qn", "/norestart"); err != nil {
			return "", err
		}
		if err := a.postInstall(ctx, j, l, j.Dir); err != nil {
			return "", err
		}
		return j.Dir, writeManifest(j.Dir, m)
	}

	// Archives and single programs go into app/, swapped in as a whole so
	// a running service never sees half an update.
	staged := filepath.Join(j.Dir, "app.new")
	os.RemoveAll(staged)
	if kind := archiveKind(name); kind != "" {
		l.Printf("Unpacking")
		if err := extract(tmp, kind, staged, "auto"); err != nil {
			return "", err
		}
	} else {
		os.MkdirAll(staged, 0o755)
		target := filepath.Join(staged, name)
		if err := os.Rename(tmp, target); err != nil {
			return "", err
		}
		os.Chmod(target, 0o755)
	}

	if m.Service != "" {
		stopService(ctx, l, m.Service)
	}
	backup := filepath.Join(j.Dir, "app.old")
	os.RemoveAll(backup)
	if _, err := os.Stat(appDir); err == nil {
		if err := os.Rename(appDir, backup); err != nil {
			return "", err
		}
	}
	if err := os.Rename(staged, appDir); err != nil {
		os.Rename(backup, appDir) // put the old version back
		return "", err
	}
	os.RemoveAll(backup)
	l.Printf("Installed into %s", appDir)

	if err := a.linkAndService(ctx, j, l, m, appDir, dataDir); err != nil {
		return "", err
	}
	if err := a.postInstall(ctx, j, l, appDir); err != nil {
		return "", err
	}
	return appDir, writeManifest(j.Dir, m)
}

// linkAndService puts the program on the PATH and/or runs it as a service
// when the job asks for it.
func (a *Agent) linkAndService(ctx context.Context, j *spec.Job, l *jobLog, m *spec.Manifest, base, workDir string) error {
	if j.LinkBinary != "" && runtime.GOOS != "windows" {
		exe, err := pickProgram(base, j.LinkBinary, j.Repo)
		if err != nil {
			return err
		}
		link := filepath.Join("/usr/local/bin", filepath.Base(exe))
		if j.LinkBinary != "auto" && !strings.ContainsAny(j.LinkBinary, `/\`) {
			link = filepath.Join("/usr/local/bin", j.LinkBinary)
		}
		if cur, err := os.Readlink(link); err == nil || !os.IsNotExist(err) {
			if cur != exe && !contains(m.Links, link) {
				return fmt.Errorf("%s already exists and wasn't made by The Repo Depot", link)
			}
		}
		os.Remove(link)
		if err := os.Symlink(exe, link); err != nil {
			return fmt.Errorf("couldn't link %s: %w", link, err)
		}
		if !contains(m.Links, link) {
			m.Links = append(m.Links, link)
		}
		l.Printf("Linked %s -> %s", link, exe)
	}
	if j.Service != nil && j.Service.Exec != "" {
		exe, err := pickProgram(base, j.Service.Exec, j.Repo)
		if err != nil {
			return err
		}
		name := "repodepot-" + composeProject(j.App)[3:]
		if err := installService(ctx, l, name, exe, j.Service.Args, workDir); err != nil {
			return err
		}
		m.Service = name
	} else if m.Service != "" {
		// The service was turned off in the app's settings.
		removeService(ctx, l, m.Service)
		m.Service = ""
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// pickProgram resolves which file to run: a path given by the user
// (relative to base), or with "auto" the executable named like the repo,
// or the only executable there is.
func pickProgram(base, want, repo string) (string, error) {
	if want != "auto" {
		p := want
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, want)
		}
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("program %s not found in %s", want, base)
		}
		return p, nil
	}
	repoName := strings.ToLower(repo[strings.LastIndex(repo, "/")+1:])
	var found []string
	filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if rel, _ := filepath.Rel(base, p); strings.Count(rel, string(filepath.Separator)) >= 2 {
				return filepath.SkipDir
			}
			return nil
		}
		if isProgram(p, d) {
			found = append(found, p)
		}
		return nil
	})
	for _, p := range found {
		n := strings.ToLower(strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
		if n == repoName || strings.TrimSuffix(n, "-linux") == repoName {
			return p, nil
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) == 0 {
		return "", fmt.Errorf("no program found in %s; set which file to run in the app's settings", base)
	}
	return "", fmt.Errorf("several programs in %s (%s); set which one to run in the app's settings", base, strings.Join(relAll(base, found), ", "))
}

func relAll(base string, ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i], _ = filepath.Rel(base, p)
	}
	return out
}

func isProgram(p string, d fs.DirEntry) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Ext(p), ".exe")
	}
	info, err := d.Info()
	if err != nil || info.Mode()&0o111 == 0 {
		return false
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".sh", ".so", ".dylib", ".txt", ".md":
		return false
	}
	return true
}

// ---- source ----

func (a *Agent) installSource(ctx context.Context, j *spec.Job, l *jobLog) (string, error) {
	src := filepath.Join(j.Dir, "src")
	if err := a.fetchSource(ctx, j, l, src); err != nil {
		return "", err
	}
	old := readManifest(j.Dir)
	m := &spec.Manifest{App: j.App, Repo: j.Repo, Method: j.Method, Ref: j.Ref}
	if old != nil {
		m.Service, m.Links = old.Service, old.Links
	}
	if m.Service != "" {
		stopService(ctx, l, m.Service)
	}
	if err := a.postInstall(ctx, j, l, src); err != nil {
		return "", err
	}
	data := filepath.Join(j.Dir, "data")
	os.MkdirAll(data, 0o755)
	if err := a.linkAndService(ctx, j, l, m, src, data); err != nil {
		return "", err
	}
	return src, writeManifest(j.Dir, m)
}

// ---- uninstall ----

func (a *Agent) uninstall(ctx context.Context, j *spec.Job, l *jobLog) error {
	m := readManifest(j.Dir)
	if m == nil {
		if _, err := os.Stat(j.Dir); os.IsNotExist(err) {
			l.Printf("Nothing installed at %s; nothing to remove", j.Dir)
			return nil
		}
		return fmt.Errorf("%s has no .repodepot.json, so it wasn't installed by The Repo Depot; not touching it", j.Dir)
	}
	switch {
	case m.Removed:
		// Already uninstalled earlier with its data kept; only the folder is left.
	case m.Method == spec.MethodCompose:
		if dockerUsable(ctx) {
			args := append(composeArgs(m, j.Dir), "down", "--remove-orphans")
			if j.RemoveData {
				args = append(args, "--volumes", "--rmi", "local")
			}
			if err := runCmd(ctx, l, j.Dir, nil, "docker", args...); err != nil {
				return err
			}
		}
	default:
		if m.Service != "" {
			removeService(ctx, l, m.Service)
		}
		for _, link := range m.Links {
			if os.Remove(link) == nil {
				l.Printf("Removed %s", link)
			}
		}
		if m.Package != "" {
			switch {
			case strings.HasSuffix(strings.ToLower(m.Package), ".msi"):
				if err := runCmd(ctx, l, j.Dir, nil, "msiexec", "/x", m.Package, "/qn", "/norestart"); err != nil {
					return err
				}
			case hasCmd("apt-get") && !strings.ContainsAny(m.Package, `/\`):
				if err := runCmd(ctx, l, j.Dir, []string{"DEBIAN_FRONTEND=noninteractive"}, "apt-get", "remove", "-y", m.Package); err != nil {
					return err
				}
			case hasCmd("rpm"):
				if err := runCmd(ctx, l, j.Dir, nil, "rpm", "-e", m.Package); err != nil {
					return err
				}
			}
		}
	}
	if j.RemoveData {
		if err := os.RemoveAll(j.Dir); err != nil {
			return err
		}
		l.Printf("Deleted %s", j.Dir)
		return nil
	}
	if m.Method != spec.MethodCompose { // a compose project's folder is the repo itself
		for _, sub := range []string{"app", "src"} {
			os.RemoveAll(filepath.Join(j.Dir, sub))
		}
	}
	m.Removed = true
	writeManifest(j.Dir, m)
	l.Printf("Kept your data in %s", j.Dir)
	return nil
}

func hasCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
