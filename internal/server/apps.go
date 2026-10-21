package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"repodepot/internal/spec"
)

type AppOptions struct {
	AssetPattern     string `json:"asset_pattern,omitempty"`
	ComposeFile      string `json:"compose_file,omitempty"`
	ComposeOverride  string `json:"compose_override,omitempty"`
	GeneratedCompose string `json:"generated_compose,omitempty"`
	Env              string `json:"env,omitempty"`
	PostInstall      string `json:"post_install,omitempty"`
	Service          bool   `json:"service,omitempty"`
	ServiceExec      string `json:"service_exec,omitempty"`
	ServiceArgs      string `json:"service_args,omitempty"`
	LinkBinary       bool   `json:"link_binary,omitempty"`
	Private          bool   `json:"private,omitempty"`
	OpenURL          string `json:"open_url,omitempty"`        // the app's own web address, for an "Open" button
	Prerelease       string `json:"prerelease,omitempty"`      // "", "yes" or "no": offer pre-releases ("" = Settings → Updates)
	IgnoreVersions   string `json:"ignore_versions,omitempty"` // versions matching this pattern are never offered
	UpdateCommand    string `json:"update_command,omitempty"`  // tracked apps: shell command, run in its folder, that updates it
}

type App struct {
	ID             int64      `json:"id"`
	Repo           string     `json:"repo"`
	Description    string     `json:"description"`
	HTMLURL        string     `json:"html_url"`
	Slug           string     `json:"slug"`
	MachineID      int64      `json:"machine_id"`
	MachineName    string     `json:"machine_name"`
	MachineOS      string     `json:"machine_os"`
	MachineStatus  string     `json:"machine_status"`
	Method         string     `json:"method"`
	Track          string     `json:"track"`
	Branch         string     `json:"branch"`
	Dir            string     `json:"dir"`
	Location       string     `json:"location"`
	InstalledRef   string     `json:"installed_ref"`
	InstalledLabel string     `json:"installed_label"`
	InstalledAt    string     `json:"installed_at"`
	LatestRef      string     `json:"latest_ref"`
	LatestLabel    string     `json:"latest_label"`
	LatestURL      string     `json:"latest_url"`
	LatestDate     string     `json:"latest_date"`
	CheckedAt      string     `json:"checked_at"`
	CheckError     string     `json:"check_error"`
	Status         string     `json:"status"`
	NotifiedRef    string     `json:"-"`
	AutoUpdate     bool       `json:"auto_update"`
	Notify         bool       `json:"notify"`
	Options        AppOptions `json:"options"`
	CreatedAt      string     `json:"created_at"`
	Logo           string     `json:"logo"`
	ActiveJob      int64      `json:"active_job"`
	SkippedRef     string     `json:"skipped_ref"`
	Held           bool       `json:"held"`
	PreviousRef    string     `json:"previous_ref"`
	PreviousLabel  string     `json:"previous_label"`
	AutoTriedRef   string     `json:"-"`
}

// NewerKnown: GitHub has a different version from the one installed.
func (a *App) NewerKnown() bool {
	return a.LatestRef != "" && a.InstalledRef != "" && a.LatestRef != a.InstalledRef
}

// UpdateAvailable: a newer version is known, nothing is running, and the
// user hasn't held the app or skipped that version.
func (a *App) UpdateAvailable() bool {
	return a.NewerKnown() && !a.Held && a.LatestRef != a.SkippedRef && (a.Status == "installed" || a.Status == "error")
}

// CanUpdate: The Repo Depot can update it itself. True for everything it
// installed, and for tracked apps that have an update command.
func (a *App) CanUpdate() bool {
	return a.Method != MethodTrack || strings.TrimSpace(a.Options.UpdateCommand) != ""
}

// updateAvailableSQL is UpdateAvailable as a query condition.
const updateAvailableSQL = `latest_ref != '' AND installed_ref != '' AND latest_ref != installed_ref AND held = 0 AND latest_ref != skipped_ref AND status IN ('installed', 'error')`

func (a *App) Name() string {
	_, n, _ := strings.Cut(a.Repo, "/")
	return n
}

func (a *App) Owner() string {
	o, _, _ := strings.Cut(a.Repo, "/")
	return o
}

const appCols = `a.id, a.repo, a.description, a.html_url, a.slug, a.machine_id, m.name, m.os, m.status, a.method, a.track, a.branch, a.dir, a.location,
	a.installed_ref, a.installed_label, a.installed_at, a.latest_ref, a.latest_label, a.latest_url, a.latest_date, a.checked_at, a.check_error,
	a.status, a.notified_ref, a.auto_update, a.notify, a.options, a.created_at, coalesce(rm.logo, ''),
	coalesce((SELECT max(j.id) FROM jobs j WHERE j.app_id = a.id AND j.status = 'running'), 0),
	a.skipped_ref, a.held, a.previous_ref, a.previous_label, a.auto_tried_ref`

const appFrom = ` FROM apps a JOIN machines m ON m.id = a.machine_id LEFT JOIN repo_meta rm ON rm.repo = a.repo`

func scanApp(sc interface{ Scan(...any) error }) (*App, error) {
	a := &App{}
	var opts string
	err := sc.Scan(&a.ID, &a.Repo, &a.Description, &a.HTMLURL, &a.Slug, &a.MachineID, &a.MachineName, &a.MachineOS, &a.MachineStatus, &a.Method, &a.Track, &a.Branch, &a.Dir, &a.Location,
		&a.InstalledRef, &a.InstalledLabel, &a.InstalledAt, &a.LatestRef, &a.LatestLabel, &a.LatestURL, &a.LatestDate, &a.CheckedAt, &a.CheckError,
		&a.Status, &a.NotifiedRef, &a.AutoUpdate, &a.Notify, &opts, &a.CreatedAt, &a.Logo, &a.ActiveJob,
		&a.SkippedRef, &a.Held, &a.PreviousRef, &a.PreviousLabel, &a.AutoTriedRef)
	json.Unmarshal([]byte(opts), &a.Options)
	return a, err
}

func (s *Store) Apps() ([]*App, error) {
	rows, err := s.db.Query(`SELECT ` + appCols + appFrom + ` ORDER BY a.repo COLLATE NOCASE, m.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Store) App(id int64) (*App, error) {
	return scanApp(s.db.QueryRow(`SELECT `+appCols+appFrom+` WHERE a.id = ?`, id))
}

func (s *Store) AppsForRepo(full string) ([]*App, error) {
	rows, err := s.db.Query(`SELECT `+appCols+appFrom+` WHERE a.repo = ? COLLATE NOCASE`, full)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Store) saveOptions(id int64, o AppOptions) {
	raw, _ := json.Marshal(o)
	s.db.Exec(`UPDATE apps SET options = ?, updated_at = ? WHERE id = ?`, string(raw), nowDB(), id)
}

// slugFor picks a folder/service name for a repo on a machine, unique on
// that machine.
func (s *Store) slugFor(machineID int64, repoName string) string {
	base := strings.ToLower(repoName)
	var b strings.Builder
	for _, c := range base {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	base = strings.Trim(b.String(), "-.")
	if base == "" {
		base = "app"
	}
	if len(base) > 50 {
		base = base[:50]
	}
	slug := base
	for i := 2; ; i++ {
		var n int
		s.db.QueryRow(`SELECT count(*) FROM apps WHERE machine_id = ? AND slug = ?`, machineID, slug).Scan(&n)
		if n == 0 {
			return slug
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
}

// ---- jobs ----

type Job struct {
	ID           int64  `json:"id"`
	AppID        int64  `json:"app_id"`
	AppLabel     string `json:"app_label"`
	MachineLabel string `json:"machine_label"`
	Action       string `json:"action"`
	Ref          string `json:"ref"`
	Status       string `json:"status"`
	Log          string `json:"log"`
	Error        string `json:"error"`
	StartedBy    string `json:"started_by"`
	CreatedAt    string `json:"created_at"`
	FinishedAt   string `json:"finished_at"`
}

func (s *Store) Jobs(appID int64, limit int) ([]Job, error) {
	q := `SELECT id, coalesce(app_id, 0), app_label, machine_label, action, ref, status, log, error, started_by, created_at, finished_at FROM jobs`
	var args []any
	if appID > 0 {
		q += ` WHERE app_id = ?`
		args = append(args, appID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		rows.Scan(&j.ID, &j.AppID, &j.AppLabel, &j.MachineLabel, &j.Action, &j.Ref, &j.Status, &j.Log, &j.Error, &j.StartedBy, &j.CreatedAt, &j.FinishedAt)
		out = append(out, j)
	}
	return out, nil
}

func (s *Store) Job(id int64) (*Job, error) {
	j := &Job{}
	err := s.db.QueryRow(`SELECT id, coalesce(app_id, 0), app_label, machine_label, action, ref, status, log, error, started_by, created_at, finished_at FROM jobs WHERE id = ?`, id).
		Scan(&j.ID, &j.AppID, &j.AppLabel, &j.MachineLabel, &j.Action, &j.Ref, &j.Status, &j.Log, &j.Error, &j.StartedBy, &j.CreatedAt, &j.FinishedAt)
	return j, err
}

// Runner limits how many jobs run at once (Settings → Updates).
type Runner struct {
	mu      sync.Mutex
	cond    *sync.Cond
	running int
}

func newRunner() *Runner {
	r := &Runner{}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *Runner) acquire(limit func() int) {
	r.mu.Lock()
	for r.running >= limit() {
		r.cond.Wait()
	}
	r.running++
	r.mu.Unlock()
}

func (r *Runner) release() {
	r.mu.Lock()
	r.running--
	r.mu.Unlock()
	r.cond.Broadcast()
}

var errBusy = errors.New("this app already has a job running")

var errTracked = errors.New("this app is tracked only: give it an update command in its Settings, or update it the way it was installed and mark it updated here")

// startJob prepares and queues an install, update or uninstall for an app.
// ref "" means the latest version for the app's tracking. by is who asked
// (a user's name, "Automatic" or "Notification").
func (s *Server) startJob(ctx context.Context, app *App, action, ref string, removeData bool, by string) (int64, error) {
	if app.ActiveJob != 0 {
		return 0, errBusy
	}
	tracked := app.Method == MethodTrack
	if tracked && (action != spec.ActionUpdate || !app.CanUpdate()) {
		return 0, errTracked
	}
	if tracked && ref != "" && ref != app.LatestRef {
		return 0, errors.New("a tracked app's update command can only update it to the latest version")
	}
	if tracked && strings.TrimSpace(app.Dir) == "" {
		return 0, errors.New("set where it is (its folder) in the app's Settings first")
	}
	m, err := s.store.Machine(app.MachineID)
	if err != nil {
		return 0, errors.New("the machine for this app no longer exists")
	}
	j := &spec.Job{Action: action, Method: app.Method, App: app.Slug, Repo: app.Repo, Dir: app.Dir, RemoveData: removeData}
	if tracked {
		j.Method, j.Command = spec.MethodCommand, app.Options.UpdateCommand
	}
	label := ""
	if action != spec.ActionUninstall {
		repo, err := s.gh.Repo(ctx, app.Repo, s.gh.minutes("cache_repo_minutes"))
		if err != nil {
			return 0, err
		}
		target, err := s.targetFor(ctx, repo, app, ref)
		if err != nil {
			return 0, err
		}
		j.Ref, label = target.Ref, target.Label
		if !tracked {
			if err := s.fillJob(ctx, j, app, repo, m, target); err != nil {
				return 0, err
			}
		}
	}
	status := map[string]string{spec.ActionInstall: "installing", spec.ActionUpdate: "updating", spec.ActionUninstall: "uninstalling"}[action]
	res, err := s.store.db.Exec(`INSERT INTO jobs (app_id, app_label, machine_id, machine_label, action, ref, target, status, started_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'running', ?, ?)`,
		app.ID, app.Repo, m.ID, m.Name, action, label, j.Ref, by, nowDB())
	if err != nil {
		return 0, err
	}
	jobID, _ := res.LastInsertId()
	s.store.db.Exec(`UPDATE apps SET status = ?, updated_at = ? WHERE id = ?`, status, nowDB(), app.ID)
	go s.runJob(jobID, app.ID, m, j, label)
	return jobID, nil
}

// targetFor resolves which version to install: a given ref, or the latest.
func (s *Server) targetFor(ctx context.Context, repo *Repo, app *App, ref string) (*Target, error) {
	if ref == "" {
		return s.latestTarget(ctx, repo, s.trackFor(app), 0)
	}
	if app.Track == "release" || app.Method == spec.MethodRelease {
		rel, err := s.gh.ReleaseByTag(ctx, repo.FullName, ref)
		if err == nil {
			return &Target{Ref: rel.TagName, Label: rel.TagName, URL: rel.HTMLURL, Date: rel.PublishedAt, Rel: rel}, nil
		}
	}
	label := ref
	if app.Track == "commit" {
		label = shortSHA(ref)
	}
	return &Target{Ref: ref, Label: label}, nil
}

// fillJob adds the method-specific parts of a job: what to download and
// how to set it up on this machine.
func (s *Server) fillJob(ctx context.Context, j *spec.Job, app *App, repo *Repo, m *Machine, t *Target) error {
	o := app.Options
	j.Token = s.settings.Get("github_token")
	j.PostInstall = o.PostInstall
	switch app.Method {
	case spec.MethodRelease:
		rel := t.Rel
		if rel == nil {
			var err error
			if rel, err = s.gh.ReleaseByTag(ctx, repo.FullName, t.Ref); err != nil {
				return fmt.Errorf("release %s: %w", t.Ref, err)
			}
		}
		asset, _ := pickAsset(rel.Assets, m.OS, m.Arch, m.OS == "linux", o.AssetPattern)
		if asset == nil {
			if o.AssetPattern != "" {
				return fmt.Errorf("release %s has no file matching %q for %s; change it in the app's settings", rel.TagName, o.AssetPattern, m.Name)
			}
			return fmt.Errorf("release %s has no file for %s (%s/%s)", rel.TagName, m.Name, m.OS, m.Arch)
		}
		j.AssetURL, j.AssetName = asset.DownloadURL, asset.Name
		j.AssetSHA256 = s.assetChecksum(ctx, rel, asset, o.Private)
		if o.AssetPattern == "" {
			// Remember the kind of file chosen (say musl vs glibc), so
			// updates pick the same one from each new release.
			o.AssetPattern = assetPattern(asset.Name)
			app.Options.AssetPattern = o.AssetPattern
			s.store.saveOptions(app.ID, app.Options)
		}
		if o.Private {
			j.AssetURL = asset.APIURL
		}
		if o.LinkBinary && m.OS == "linux" {
			j.LinkBinary = "auto"
		}
	default:
		j.SourceURL = s.gh.SourceURL(repo.FullName, t.Ref, repo.Private)
		j.ComposeFile = o.ComposeFile
		j.ComposeOverride = o.ComposeOverride
		j.GeneratedCompose = o.GeneratedCompose
		j.Env = o.Env
	}
	if o.Service && app.Method != spec.MethodCompose {
		exec := o.ServiceExec
		if exec == "" {
			exec = "auto"
		}
		j.Service = &spec.Service{Exec: exec, Args: o.ServiceArgs}
	}
	return nil
}

// assetChecksum finds the published SHA-256 of a release file: GitHub's
// own digest, or the release's checksum file (checksums.txt, SHA256SUMS,
// <file>.sha256). "" when the release publishes none.
func (s *Server) assetChecksum(ctx context.Context, rel *Release, asset *Asset, private bool) string {
	if d, ok := strings.CutPrefix(asset.Digest, "sha256:"); ok && len(d) == 64 {
		return strings.ToLower(d)
	}
	if private {
		return ""
	}
	for _, a := range rel.Assets {
		n := strings.ToLower(a.Name)
		own := n == strings.ToLower(asset.Name)+".sha256" || n == strings.ToLower(asset.Name)+".sha256sum"
		list := strings.Contains(n, "checksum") || strings.Contains(n, "sha256sum") || n == "sha256sums" || strings.HasSuffix(n, "sha256sums.txt")
		if (!own && !list) || a.Size > 1<<20 || strings.HasSuffix(n, ".sig") || strings.HasSuffix(n, ".asc") || strings.HasSuffix(n, ".pem") {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 15*time.Second)
		body, err := s.gh.Download(c, a.DownloadURL, 1<<20)
		cancel()
		if err != nil {
			continue
		}
		if sum := checksumFor(string(body), asset.Name, own); sum != "" {
			return sum
		}
	}
	return ""
}

var hexSum = regexp.MustCompile(`(?i)\b[0-9a-f]{64}\b`)

// checksumFor reads "<sha256>  <name>" lines (sha256sum's format, with or
// without the binary-mode *). A <file>.sha256 may hold just the hash.
func checksumFor(text, name string, own bool) string {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && hexSum.MatchString(f[0]) && len(f[0]) == 64 {
			file := strings.TrimPrefix(strings.TrimPrefix(f[len(f)-1], "*"), "./")
			if file == name || path.Base(file) == name {
				return strings.ToLower(f[0])
			}
		}
	}
	if own {
		if m := hexSum.FindString(text); m != "" {
			return strings.ToLower(m)
		}
	}
	return ""
}

func (s *Server) runJob(jobID, appID int64, m *Machine, j *spec.Job, label string) {
	s.runner.acquire(func() int { return s.settings.Int("max_parallel_jobs") })
	defer s.runner.release()
	ctx := context.Background()
	st, err := s.agent(m).StartJob(ctx, j)
	if err != nil {
		s.finishJob(jobID, appID, j, label, "", fmt.Sprintf("Couldn't start on %s: %v", m.Name, err), "")
		return
	}
	s.store.db.Exec(`UPDATE jobs SET agent_job = ? WHERE id = ?`, st.ID, jobID)
	s.followJob(jobID, appID, m, j, label, st.ID)
}

// followJob polls the agent until the job ends, copying its log as it goes.
func (s *Server) followJob(jobID, appID int64, m *Machine, j *spec.Job, label, agentJob string) {
	ctx := context.Background()
	failures := 0
	last := 0
	for {
		time.Sleep(700 * time.Millisecond)
		st, err := s.agent(m).Job(ctx, agentJob)
		if err != nil {
			failures++
			if failures > 120 { // ~2 minutes without an answer
				s.finishJob(jobID, appID, j, label, "", "Lost contact with "+m.Name+" during the job: "+err.Error(), "")
				return
			}
			continue
		}
		failures = 0
		if len(st.Log) != last {
			last = len(st.Log)
			s.store.db.Exec(`UPDATE jobs SET log = ? WHERE id = ?`, strings.Join(st.Log, "\n"), jobID)
		}
		if st.Status != "running" {
			s.finishJob(jobID, appID, j, label, strings.Join(st.Log, "\n"), st.Error, st.Location)
			return
		}
	}
}

func (s *Server) finishJob(jobID, appID int64, j *spec.Job, label, logText, errMsg, location string) {
	status := "done"
	if errMsg != "" {
		status = "error"
	}
	if logText != "" {
		s.store.db.Exec(`UPDATE jobs SET status = ?, error = ?, log = ?, finished_at = ? WHERE id = ?`, status, errMsg, logText, nowDB(), jobID)
	} else {
		s.store.db.Exec(`UPDATE jobs SET status = ?, error = ?, finished_at = ? WHERE id = ?`, status, errMsg, nowDB(), jobID)
	}
	app, err := s.store.App(appID)
	if err != nil {
		return // unregistered meanwhile
	}
	name := app.Name() + " on " + app.MachineName
	if errMsg != "" {
		log.Printf("job #%d (%s %s) failed: %s", jobID, j.Action, name, errMsg)
		next := "error"
		if j.Action == spec.ActionUpdate || j.Action == spec.ActionUninstall {
			next = "installed" // the old version is still there
			if app.InstalledRef == "" {
				next = "error"
			}
		}
		s.store.db.Exec(`UPDATE apps SET status = ?, updated_at = ? WHERE id = ?`, next, nowDB(), appID)
		if s.settings.Bool("notify_failures") {
			s.notify(titleWord(j.Action)+" failed: "+name, errMsg, "x", fmt.Sprintf("/apps/%d", appID), nil)
		}
		return
	}
	switch j.Action {
	case spec.ActionUninstall:
		s.store.db.Exec(`DELETE FROM apps WHERE id = ?`, appID)
		if s.settings.Bool("notify_done") {
			s.notify("Removed "+name, "Uninstalled from "+app.MachineName+".", "wastebasket", "/", nil)
		}
	default:
		if app.InstalledRef != "" && app.InstalledRef != j.Ref {
			// Remember what it was on, for "Roll back".
			s.store.db.Exec(`UPDATE apps SET previous_ref = ?, previous_label = ? WHERE id = ?`, app.InstalledRef, app.InstalledLabel, appID)
		}
		s.store.db.Exec(`UPDATE apps SET status = 'installed', installed_ref = ?, installed_label = ?, installed_at = ?, location = ?, notified_ref = '', updated_at = ? WHERE id = ?`,
			j.Ref, label, nowDB(), location, nowDB(), appID)
		if app.LatestRef == "" {
			s.store.db.Exec(`UPDATE apps SET latest_ref = ?, latest_label = ? WHERE id = ?`, j.Ref, label, appID)
		}
		if s.settings.Bool("notify_done") {
			verb := "Installed"
			if j.Action == spec.ActionUpdate {
				verb = "Updated"
			}
			s.notify(verb+" "+name, label, "white_check_mark", fmt.Sprintf("/apps/%d", appID), nil)
		}
	}
}

func titleWord(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// resumeJobs picks up jobs that were running when the server stopped. The
// agent still has them unless it restarted too.
func (s *Server) resumeJobs() {
	rows, err := s.store.db.Query(`SELECT j.id, coalesce(j.app_id, 0), coalesce(j.machine_id, 0), j.action, j.ref, j.target, j.agent_job FROM jobs j WHERE j.status = 'running'`)
	if err != nil {
		return
	}
	type pending struct {
		id, app, machine                int64
		action, label, target, agentJob string
	}
	var list []pending
	for rows.Next() {
		var p pending
		rows.Scan(&p.id, &p.app, &p.machine, &p.action, &p.label, &p.target, &p.agentJob)
		list = append(list, p)
	}
	rows.Close()
	for _, p := range list {
		app, err1 := s.store.App(p.app)
		m, err2 := s.store.Machine(p.machine)
		if p.agentJob == "" || err1 != nil || err2 != nil {
			s.store.db.Exec(`UPDATE jobs SET status = 'error', error = 'The Repo Depot restarted before this job started. Try again.', finished_at = ? WHERE id = ?`, nowDB(), p.id)
			if err1 == nil {
				back := "installed"
				if app.InstalledRef == "" {
					back = "error"
				}
				s.store.db.Exec(`UPDATE apps SET status = ? WHERE id = ?`, back, app.ID)
			}
			continue
		}
		j := &spec.Job{Action: p.action, Method: app.Method, App: app.Slug, Repo: app.Repo, Dir: app.Dir, Ref: p.target}
		go s.followJob(p.id, p.app, m, j, p.label, p.agentJob)
	}
}

// installDir decides where an app goes on a machine.
func (s *Server) installDir(m *Machine, slug, custom string) string {
	if strings.TrimSpace(custom) != "" {
		return strings.TrimSpace(custom)
	}
	root := ""
	if m.OS == "windows" {
		root = s.settings.Get("default_dir_windows")
	} else {
		root = s.settings.Get("default_dir_linux")
	}
	if root == "" {
		root = m.AppsDir
	}
	if root == "" {
		if m.OS == "windows" {
			root = `C:\RepoDepot\apps`
		} else {
			root = "/opt/repodepot/apps"
		}
	}
	if m.OS == "windows" {
		return strings.TrimRight(root, `\/`) + `\` + slug
	}
	return path.Join(root, slug)
}
