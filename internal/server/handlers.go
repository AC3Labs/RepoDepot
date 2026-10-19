package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"repodepot/internal/spec"
)

// ---- setup & login ----

func (s *Server) showSetup(w http.ResponseWriter, r *http.Request) {
	if s.userCount() > 0 {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.render(w, r, 200, "setup", "Welcome", "", map[string]string{})
}

func (s *Server) doSetup(w http.ResponseWriter, r *http.Request) {
	if s.userCount() > 0 {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	name, email, pw := strings.TrimSpace(r.FormValue("name")), strings.TrimSpace(r.FormValue("email")), r.FormValue("password")
	errMsg := ""
	if _, err := mail.ParseAddress(email); err != nil || name == "" {
		errMsg = "Enter your name and a valid email address."
	} else if len(pw) < 8 || len(pw) > 72 {
		errMsg = "Use a password of 8 to 72 characters."
	} else if pw != r.FormValue("password_confirm") {
		errMsg = "The passwords don't match."
	}
	if errMsg != "" {
		s.render(w, r, 422, "setup", "Welcome", "", map[string]string{"Error": errMsg, "Name": name, "Email": email})
		return
	}
	id, err := s.store.exec(`INSERT INTO users (name, email, password, created_at) VALUES (?, ?, ?, ?)`, name, email, hashPassword(pw), nowDB())
	if err != nil {
		s.render(w, r, 500, "setup", "Welcome", "", map[string]string{"Error": err.Error()})
		return
	}
	if s.settings.Get("public_url") == "" {
		if b, ok := s.seenBase.Load().(string); ok {
			s.settings.Set("public_url", b)
		}
	}
	s.newSession(w, r, id)
	http.Redirect(w, r, "/machines?welcome=1", http.StatusSeeOther)
}

func (s *Server) showLogin(w http.ResponseWriter, r *http.Request) {
	if s.userCount() == 0 {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if userFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.render(w, r, 200, "login", "Sign in", "", map[string]string{"Next": r.URL.Query().Get("next")})
}

func safeNext(n string) string {
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.HasPrefix(n, "/\\") || strings.ContainsAny(n, "\r\n") {
		return "/"
	}
	return n
}

func (s *Server) doLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	email, next := r.FormValue("email"), safeNext(r.FormValue("next"))
	if s.limiter.blocked(ip, s.settings.Int("login_attempts")) {
		s.render(w, r, 429, "login", "Sign in", "", map[string]string{"Error": "Too many wrong passwords. Try again in 15 minutes.", "Email": email, "Next": next})
		return
	}
	id, ok := s.checkLogin(email, r.FormValue("password"))
	if !ok {
		s.limiter.failed(ip)
		s.render(w, r, 422, "login", "Sign in", "", map[string]string{"Error": "That email and password don't match.", "Email": email, "Next": next})
		return
	}
	s.limiter.reset(ip)
	s.newSession(w, r, id)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) doLogout(w http.ResponseWriter, r *http.Request) {
	s.store.db.Exec(`DELETE FROM sessions WHERE id = ?`, sessFrom(r).ID)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- view models ----

type appView struct {
	*App
	Name            string `json:"name"`
	Owner           string `json:"owner"`
	LogoURL         string `json:"logo_url"`
	UpdateAvailable bool   `json:"update_available"`
	CheckedAgo      string `json:"checked_ago"`
	InstalledAgo    string `json:"installed_ago"`
}

func (s *Server) viewApp(a *App) appView {
	v := appView{App: a, Name: a.Name(), Owner: a.Owner(), UpdateAvailable: a.UpdateAvailable(), CheckedAgo: ago(a.CheckedAt), InstalledAgo: ago(a.InstalledAt)}
	if a.Logo != "" {
		v.LogoURL = "/logos/" + a.Logo
	} else {
		v.LogoURL = "https://github.com/" + a.Owner() + ".png?size=160"
	}
	return v
}

func (s *Server) appViews() []appView {
	apps, _ := s.store.Apps()
	out := make([]appView, 0, len(apps))
	for _, a := range apps {
		out = append(out, s.viewApp(a))
	}
	return out
}

func (s *Server) machineViews() []*Machine {
	ms, _ := s.store.Machines()
	if ms == nil {
		ms = []*Machine{}
	}
	return ms
}

// ---- pages ----

func (s *Server) pageDepot(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, 200, "depot", "My Depot", "depot", map[string]any{
		"Apps": s.appViews(), "Machines": s.machineViews(), "View": s.settings.Get("depot_view"),
		"LastCheck": s.checker.lastDone.Load(),
	})
}

func (s *Server) pageDiscover(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, 200, "discover", "Discover", "discover", map[string]any{"Query": r.URL.Query().Get("q")})
}

func (s *Server) pageRepo(w http.ResponseWriter, r *http.Request) {
	full := r.PathValue("owner") + "/" + r.PathValue("name")
	if _, _, ok := splitRepo(full); !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, 200, "repo", full, "discover", map[string]any{
		"Repo": full, "Machines": s.machineViews(), "Defaults": s.installDefaults(),
	})
}

func (s *Server) installDefaults() map[string]any {
	return map[string]any{
		"link_binaries": s.settings.Bool("link_binaries"), "auto_update": s.settings.Bool("auto_update_default"),
		"notify": true, "track": s.settings.Get("default_track"),
	}
}

func (s *Server) pageApp(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.store.App(id)
	if err != nil {
		s.render(w, r, 404, "app", "Not found", "depot", map[string]any{"Missing": true})
		return
	}
	jobs, _ := s.store.Jobs(id, 30)
	s.render(w, r, 200, "app", a.Name(), "depot", map[string]any{"App": s.viewApp(a), "Jobs": jobs})
}

func (s *Server) pageMachines(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, 200, "machines", "Machines", "machines", map[string]any{
		"Machines": s.machineViews(), "Welcome": r.URL.Query().Get("welcome") == "1",
		"Port": s.settings.Int("agent_default_port"), "Base": s.publicURL(),
	})
}

func (s *Server) pageActivity(w http.ResponseWriter, r *http.Request) {
	jobs, _ := s.store.Jobs(0, 200)
	if jobs == nil {
		jobs = []Job{}
	}
	s.render(w, r, 200, "activity", "Activity", "activity", map[string]any{"Jobs": jobs})
}

type settingView struct {
	Setting
	Value string
	Set   bool // secret has a value
}

func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request) {
	bySection := map[string][]settingView{}
	for _, d := range settingDefs {
		v := settingView{Setting: d, Value: s.settings.Get(d.Key)}
		if d.Type == "secret" {
			v.Set, v.Value = v.Value != "", ""
		}
		bySection[d.Section] = append(bySection[d.Section], v)
	}
	type section struct {
		Key, Label string
		Items      []settingView
	}
	var secs []section
	for _, sc := range settingSections {
		secs = append(secs, section{sc[0], sc[1], bySection[sc[0]]})
	}
	left, limit, reset := s.gh.Rate()
	s.render(w, r, 200, "settings", "Settings", "settings", map[string]any{
		"Sections": secs, "Tab": r.URL.Query().Get("tab"), "RateLeft": left, "RateLimit": limit,
		"RateReset": reset.Local().Format("15:04"), "Base": s.publicURL(),
	})
}

// ---- API: status ----

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	left, limit, _ := s.gh.Rate()
	writeJSON(w, 200, map[string]any{"updates": s.updateCount(), "checking": s.checker.running.Load(),
		"last_check": s.checker.lastDone.Load(), "rate_left": left, "rate_limit": limit})
}

// ---- API: discover ----

var repoInput = regexp.MustCompile(`^(?:https?://)?(?:www\.)?(?:github\.com/)?([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?(?:[/#?].*)?$`)

type searchItem struct {
	FullName    string   `json:"full_name"`
	Owner       string   `json:"owner"`
	Name        string   `json:"name"`
	Avatar      string   `json:"avatar"`
	Description string   `json:"description"`
	Stars       int      `json:"stars"`
	StarsLabel  string   `json:"stars_label"`
	Language    string   `json:"language"`
	Topics      []string `json:"topics"`
	Updated     string   `json:"updated"`
	Archived    bool     `json:"archived"`
	Installed   int      `json:"installed"`
}

func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	installed := map[string]int{}
	rows, _ := s.store.db.Query(`SELECT lower(repo), count(*) FROM apps GROUP BY lower(repo)`)
	if rows != nil {
		for rows.Next() {
			var k string
			var n int
			rows.Scan(&k, &n)
			installed[k] = n
		}
		rows.Close()
	}
	conv := func(rp Repo) searchItem {
		topics := rp.Topics
		if len(topics) > 4 {
			topics = topics[:4]
		}
		return searchItem{FullName: rp.FullName, Owner: rp.Owner.Login, Name: rp.Name, Avatar: rp.Owner.AvatarURL + "&s=96",
			Description: rp.Description, Stars: rp.Stars, StarsLabel: humanNum(rp.Stars), Language: rp.Language,
			Topics: topics, Updated: ago(rp.PushedAt), Archived: rp.Archived, Installed: installed[strings.ToLower(rp.FullName)]}
	}
	// A pasted link or owner/name goes straight to that repo.
	if m := repoInput.FindStringSubmatch(q); m != nil && (strings.Contains(q, "github.com") || strings.Count(q, "/") == 1 && !strings.Contains(q, " ")) {
		if rp, err := s.gh.Repo(r.Context(), m[1]+"/"+m[2], s.gh.minutes("cache_repo_minutes")); err == nil {
			writeJSON(w, 200, map[string]any{"total": 1, "items": []searchItem{conv(*rp)}, "exact": rp.FullName})
			return
		}
	}
	query := q
	if query == "" {
		query = "topic:self-hosted stars:>2000"
	}
	if !strings.Contains(query, "archived:") {
		query += " archived:false"
	}
	res, err := s.gh.Search(r.Context(), query, r.URL.Query().Get("sort"), page)
	if err != nil {
		jsonError(w, 502, err.Error())
		return
	}
	items := make([]searchItem, 0, len(res.Items))
	for _, rp := range res.Items {
		items = append(items, conv(rp))
	}
	writeJSON(w, 200, map[string]any{"total": res.Total, "items": items})
}

type machineFit struct {
	ID      int64             `json:"id"`
	Name    string            `json:"name"`
	OS      string            `json:"os"`
	Arch    string            `json:"arch"`
	Status  string            `json:"status"`
	Docker  bool              `json:"docker"`
	Admin   bool              `json:"admin"`
	Asset   string            `json:"asset"`
	AssetSz int64             `json:"asset_size"`
	Assets  []string          `json:"assets"`
	Dir     string            `json:"dir"`
	Reason  map[string]string `json:"reason"` // method -> why it can't be used here
}

func (s *Server) apiInspect(w http.ResponseWriter, r *http.Request) {
	full := r.PathValue("owner") + "/" + r.PathValue("name")
	if _, _, ok := splitRepo(full); !ok {
		jsonError(w, 400, "Not a repo name.")
		return
	}
	in, err := s.inspect(r.Context(), full)
	if err != nil {
		status := 502
		var ge *GitHubError
		if errors.As(err, &ge) && ge.Status == 404 {
			status = 404
		}
		jsonError(w, status, err.Error())
		return
	}
	go s.ensureLogo(context.Background(), in.Repo)

	machines := s.machineViews()
	fits := make([]machineFit, 0, len(machines))
	slugBase := strings.ToLower(in.Repo.Name)
	for _, m := range machines {
		f := machineFit{ID: m.ID, Name: m.Name, OS: m.OS, Arch: m.Arch, Status: m.Status, Docker: m.Docker, Admin: m.Admin, Reason: map[string]string{}}
		f.Dir = s.installDir(m, s.store.slugFor(m.ID, slugBase), "")
		if !m.Docker {
			f.Reason[spec.MethodCompose] = "No Docker on this machine"
		}
		if in.Latest != nil {
			best, all := pickAsset(in.Latest.Assets, m.OS, m.Arch, m.OS == "linux", "")
			if best != nil {
				f.Asset, f.AssetSz = best.Name, best.Size
			} else {
				f.Reason[spec.MethodRelease] = "No release file for " + m.OSLabel() + " " + m.Arch
			}
			for _, a := range all {
				f.Assets = append(f.Assets, a.Name)
			}
		}
		if m.OS == "" {
			f.Reason["all"] = "Not checked yet"
		}
		fits = append(fits, f)
	}
	installed, _ := s.store.AppsForRepo(full)
	var inst []appView
	for _, a := range installed {
		inst = append(inst, s.viewApp(a))
	}
	var allAssets []Asset
	if in.Latest != nil {
		allAssets = usableAssets(in.Latest.Assets)
	}
	lic := ""
	if in.Repo.License != nil && in.Repo.License.SPDX != "NOASSERTION" {
		lic = in.Repo.License.SPDX
	}
	writeJSON(w, 200, map[string]any{
		"inspect": in, "logo": s.LogoURL(full, in.Repo.Owner.AvatarURL), "machines": fits, "installed": inst,
		"license": lic, "stars": humanNum(in.Repo.Stars), "forks": humanNum(in.Repo.Forks), "pushed": ago(in.Repo.PushedAt),
		"assets": allAssets, "generated_compose": generatedCompose(slugBase),
	})
}

func (s *Server) apiReadme(w http.ResponseWriter, r *http.Request) {
	full := r.PathValue("owner") + "/" + r.PathValue("name")
	if _, _, ok := splitRepo(full); !ok {
		jsonError(w, 400, "Not a repo name.")
		return
	}
	raw, err := s.gh.Readme(r.Context(), full)
	if err != nil {
		jsonError(w, 502, err.Error())
		return
	}
	branch := r.URL.Query().Get("branch")
	writeJSON(w, 200, map[string]string{"html": sanitizeReadme(raw, full, branch, s.settings.Bool("readme_images"))})
}

func (s *Server) apiRepoFile(w http.ResponseWriter, r *http.Request) {
	full := r.PathValue("owner") + "/" + r.PathValue("name")
	p := r.URL.Query().Get("path")
	if _, _, ok := splitRepo(full); !ok || p == "" || strings.Contains(p, "..") {
		jsonError(w, 400, "Bad request.")
		return
	}
	text, err := s.gh.FileText(r.Context(), full, r.URL.Query().Get("ref"), p)
	if err != nil {
		jsonError(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"text": text})
}

// ---- API: install ----

type installReq struct {
	Repo       string            `json:"repo"`
	Machines   []int64           `json:"machines"`
	Method     string            `json:"method"`
	Track      string            `json:"track"`
	Branch     string            `json:"branch"`
	Version    string            `json:"version"`
	Dirs       map[string]string `json:"dirs"`
	Assets     map[string]string `json:"assets"` // machine id -> chosen file name
	Options    AppOptions        `json:"options"`
	AutoUpdate bool              `json:"auto_update"`
	Notify     bool              `json:"notify"`
}

type installResult struct {
	Machine string `json:"machine"`
	AppID   int64  `json:"app_id,omitempty"`
	JobID   int64  `json:"job_id,omitempty"`
	Error   string `json:"error,omitempty"`
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 2<<20)).Decode(v)
}

func (s *Server) apiInstall(w http.ResponseWriter, r *http.Request) {
	var req installReq
	if err := decodeJSON(r, &req); err != nil {
		jsonError(w, 400, "Invalid request.")
		return
	}
	if _, _, ok := splitRepo(req.Repo); !ok {
		jsonError(w, 400, "Not a repo name.")
		return
	}
	if len(req.Machines) == 0 {
		jsonError(w, 422, "Pick at least one machine.")
		return
	}
	switch req.Method {
	case spec.MethodCompose, spec.MethodRelease, spec.MethodSource:
	default:
		jsonError(w, 422, "Pick an install method.")
		return
	}
	switch req.Track {
	case "release", "tag", "commit":
	default:
		jsonError(w, 422, "Pick what counts as an update.")
		return
	}
	if req.Method == spec.MethodRelease {
		req.Track = "release"
	}
	repo, err := s.gh.Repo(r.Context(), req.Repo, s.gh.minutes("cache_repo_minutes"))
	if err != nil {
		jsonError(w, 502, err.Error())
		return
	}
	req.Options.Private = repo.Private
	go s.ensureLogo(context.Background(), repo)

	var results []installResult
	for _, mid := range req.Machines {
		m, err := s.store.Machine(mid)
		if err != nil {
			results = append(results, installResult{Machine: fmt.Sprint(mid), Error: "No such machine."})
			continue
		}
		s.refreshMachine(r.Context(), m)
		res := installResult{Machine: m.Name}
		if m.Status != "online" {
			res.Error = m.Name + " is offline."
			results = append(results, res)
			continue
		}
		if req.Method == spec.MethodCompose && !m.Docker {
			res.Error = m.Name + " doesn't have Docker with the compose plugin."
			results = append(results, res)
			continue
		}
		opts := req.Options
		if name := req.Assets[fmt.Sprint(mid)]; name != "" && req.Method == spec.MethodRelease {
			opts.AssetPattern = assetPattern(name)
		}
		slug := s.store.slugFor(m.ID, repo.Name)
		dir := s.installDir(m, slug, req.Dirs[fmt.Sprint(mid)])
		raw, _ := json.Marshal(opts)
		id, err := s.store.exec(`INSERT INTO apps (repo, description, html_url, slug, machine_id, method, track, branch, dir, status, auto_update, notify, options, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'installing', ?, ?, ?, ?, ?)`,
			repo.FullName, repo.Description, repo.HTMLURL, slug, m.ID, req.Method, req.Track, req.Branch, dir, req.AutoUpdate, req.Notify, string(raw), nowDB(), nowDB())
		if err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		app, _ := s.store.App(id)
		jobID, err := s.startJob(r.Context(), app, spec.ActionInstall, req.Version, false)
		if err != nil {
			s.store.db.Exec(`DELETE FROM apps WHERE id = ?`, id)
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		res.AppID, res.JobID = id, jobID
		results = append(results, res)
	}
	writeJSON(w, 200, map[string]any{"results": results})
}

// ---- API: installed apps ----

func (s *Server) apiApps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"apps": s.appViews(), "checking": s.checker.running.Load(), "last_check": s.checker.lastDone.Load()})
}

func (s *Server) appFromPath(w http.ResponseWriter, r *http.Request) *App {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.store.App(id)
	if err != nil {
		jsonError(w, 404, "That app isn't registered any more.")
		return nil
	}
	return a
}

func (s *Server) apiUpdate(w http.ResponseWriter, r *http.Request) {
	a := s.appFromPath(w, r)
	if a == nil {
		return
	}
	var body struct {
		Ref string `json:"ref"`
	}
	decodeJSON(r, &body)
	jobID, err := s.startJob(r.Context(), a, spec.ActionUpdate, body.Ref, false)
	if err != nil {
		jsonError(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int64{"job_id": jobID})
}

func (s *Server) apiReinstall(w http.ResponseWriter, r *http.Request) {
	a := s.appFromPath(w, r)
	if a == nil {
		return
	}
	ref := a.InstalledRef
	action := spec.ActionUpdate
	if ref == "" {
		action = spec.ActionInstall
	}
	jobID, err := s.startJob(r.Context(), a, action, ref, false)
	if err != nil {
		jsonError(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int64{"job_id": jobID})
}

func (s *Server) apiUpdateMany(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
		All bool    `json:"all"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonError(w, 400, "Invalid request.")
		return
	}
	apps, _ := s.store.Apps()
	want := map[int64]bool{}
	for _, id := range body.IDs {
		want[id] = true
	}
	started, failed := 0, []string{}
	for _, a := range apps {
		if !(body.All && a.UpdateAvailable()) && !want[a.ID] {
			continue
		}
		if _, err := s.startJob(r.Context(), a, spec.ActionUpdate, "", false); err != nil {
			failed = append(failed, a.Name()+" on "+a.MachineName+": "+err.Error())
			continue
		}
		started++
	}
	writeJSON(w, 200, map[string]any{"started": started, "failed": failed})
}

func (s *Server) apiUninstall(w http.ResponseWriter, r *http.Request) {
	a := s.appFromPath(w, r)
	if a == nil {
		return
	}
	var body struct {
		RemoveData bool `json:"remove_data"`
	}
	decodeJSON(r, &body)
	jobID, err := s.startJob(r.Context(), a, spec.ActionUninstall, "", body.RemoveData)
	if err != nil {
		jsonError(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int64{"job_id": jobID})
}

func (s *Server) apiUnregister(w http.ResponseWriter, r *http.Request) {
	a := s.appFromPath(w, r)
	if a == nil {
		return
	}
	if a.ActiveJob != 0 {
		jsonError(w, 409, "Wait for the running job to finish first.")
		return
	}
	s.store.db.Exec(`DELETE FROM apps WHERE id = ?`, a.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) apiAppSettings(w http.ResponseWriter, r *http.Request) {
	a := s.appFromPath(w, r)
	if a == nil {
		return
	}
	var body struct {
		Track      string     `json:"track"`
		Branch     string     `json:"branch"`
		AutoUpdate bool       `json:"auto_update"`
		Notify     bool       `json:"notify"`
		Options    AppOptions `json:"options"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonError(w, 400, "Invalid request.")
		return
	}
	switch body.Track {
	case "release", "tag", "commit":
	default:
		jsonError(w, 422, "Pick what counts as an update.")
		return
	}
	if a.Method == spec.MethodRelease {
		body.Track = "release"
	}
	body.Options.Private = a.Options.Private
	raw, _ := json.Marshal(body.Options)
	trackChanged := body.Track != a.Track || strings.TrimSpace(body.Branch) != a.Branch
	s.store.db.Exec(`UPDATE apps SET track = ?, branch = ?, auto_update = ?, notify = ?, options = ?, updated_at = ? WHERE id = ?`,
		body.Track, strings.TrimSpace(body.Branch), body.AutoUpdate, body.Notify, string(raw), nowDB(), a.ID)
	if trackChanged {
		s.store.db.Exec(`UPDATE apps SET latest_ref = '', latest_label = '', notified_ref = '' WHERE id = ?`, a.ID)
		go s.checkUpdates(context.Background())
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) apiCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	found := s.checkUpdates(ctx)
	writeJSON(w, 200, map[string]any{"updates": s.updateCount(), "found": found})
}

func (s *Server) apiJobs(w http.ResponseWriter, r *http.Request) {
	app, _ := strconv.ParseInt(r.URL.Query().Get("app"), 10, 64)
	jobs, _ := s.store.Jobs(app, 100)
	if jobs == nil {
		jobs = []Job{}
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
}

func (s *Server) apiJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	j, err := s.store.Job(id)
	if err != nil {
		jsonError(w, 404, "No such job.")
		return
	}
	writeJSON(w, 200, j)
}

// ---- API: machines ----

func (s *Server) apiMachines(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"machines": s.machineViews()})
}

func (s *Server) installCommands(token string, port int) map[string]string {
	base := s.publicURL()
	return map[string]string{
		"linux":   fmt.Sprintf("curl -fsSL %s/agent/install.sh | sudo sh -s -- --token %s --port %d", base, token, port),
		"windows": fmt.Sprintf("& ([scriptblock]::Create((irm '%s/agent/install.ps1'))) -Token '%s' -Port %d", base, token, port),
	}
}

func (s *Server) apiSaveMachine(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Host   string `json:"host"`
		Port   int    `json:"port"`
		UseTLS bool   `json:"use_tls"`
		Token  string `json:"token"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonError(w, 400, "Invalid request.")
		return
	}
	body.Name, body.Host = strings.TrimSpace(body.Name), strings.TrimSpace(body.Host)
	if body.Name == "" || body.Host == "" || strings.ContainsAny(body.Host, "/ :") && !strings.HasPrefix(body.Host, "[") {
		jsonError(w, 422, "Enter a name and a host (an IP address or hostname, without http://).")
		return
	}
	if body.Port <= 0 || body.Port > 65535 {
		body.Port = s.settings.Int("agent_default_port")
	}
	body.Token = strings.TrimSpace(body.Token)
	idStr := r.PathValue("id")
	if idStr == "" {
		if body.Token == "" {
			body.Token = randomHex(24)
		}
		if len(body.Token) < 16 {
			jsonError(w, 422, "The token must be at least 16 characters.")
			return
		}
		id, err := s.store.exec(`INSERT INTO machines (name, host, port, token, use_tls, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			body.Name, body.Host, body.Port, body.Token, body.UseTLS, nowDB())
		if err != nil {
			jsonError(w, 500, err.Error())
			return
		}
		m, _ := s.store.Machine(id)
		writeJSON(w, 200, map[string]any{"machine": m, "token": body.Token, "commands": s.installCommands(body.Token, body.Port)})
		return
	}
	id, _ := strconv.ParseInt(idStr, 10, 64)
	m, err := s.store.Machine(id)
	if err != nil {
		jsonError(w, 404, "No such machine.")
		return
	}
	if body.Token != "" && len(body.Token) < 16 {
		jsonError(w, 422, "The token must be at least 16 characters.")
		return
	}
	if body.Token == "" {
		body.Token = m.Token
	}
	s.store.db.Exec(`UPDATE machines SET name = ?, host = ?, port = ?, use_tls = ?, token = ? WHERE id = ?`, body.Name, body.Host, body.Port, body.UseTLS, body.Token, id)
	m, _ = s.store.Machine(id)
	s.refreshMachine(r.Context(), m)
	writeJSON(w, 200, map[string]any{"machine": m})
}

func (s *Server) apiNewToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	m, err := s.store.Machine(id)
	if err != nil {
		jsonError(w, 404, "No such machine.")
		return
	}
	tok := randomHex(24)
	s.store.db.Exec(`UPDATE machines SET token = ? WHERE id = ?`, tok, id)
	writeJSON(w, 200, map[string]any{"token": tok, "commands": s.installCommands(tok, m.Port)})
}

func (s *Server) apiDeleteMachine(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var n int
	s.store.db.QueryRow(`SELECT count(*) FROM apps WHERE machine_id = ?`, id).Scan(&n)
	if n > 0 {
		jsonError(w, 409, fmt.Sprintf("%d app(s) are registered on this machine. Uninstall or unregister them first.", n))
		return
	}
	s.store.db.Exec(`DELETE FROM machines WHERE id = ?`, id)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) apiMachineStatus(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	m, err := s.store.Machine(id)
	if err != nil {
		jsonError(w, 404, "No such machine.")
		return
	}
	s.refreshMachine(r.Context(), m)
	var apps int
	s.store.db.QueryRow(`SELECT count(*) FROM apps WHERE machine_id = ?`, id).Scan(&apps)
	writeJSON(w, 200, map[string]any{"machine": m, "apps": apps, "last_seen": ago(m.LastSeen)})
}

// ---- API: settings & account ----

func (s *Server) apiSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := decodeJSON(r, &body); err != nil {
		jsonError(w, 400, "Invalid request.")
		return
	}
	clean := map[string]string{}
	for k, v := range body {
		d := settingIndex[k]
		if d == nil {
			continue
		}
		if d.Type == "secret" {
			switch v {
			case "":
				continue // unchanged
			case "__clear__":
				clean[k] = ""
				continue
			}
		}
		val, msg := d.validate(v)
		if msg != "" {
			jsonError(w, 422, msg)
			return
		}
		clean[k] = val
	}
	for k, v := range clean {
		s.settings.Set(k, v)
	}
	writeJSON(w, 200, map[string]any{"saved": len(clean)})
}

func (s *Server) apiTestNtfy(w http.ResponseWriter, r *http.Request) {
	if s.settings.Get("ntfy_topic") == "" {
		jsonError(w, 422, "Set a topic first.")
		return
	}
	err := s.sendNtfy(s.settings.Get("site_name")+" test", "Notifications are working. Updates will show up here.", "tada", "/", nil)
	if err != nil {
		jsonError(w, 502, "Couldn't send: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) apiAccount(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Name, Email, Current, New string
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonError(w, 400, "Invalid request.")
		return
	}
	var hash string
	s.store.db.QueryRow(`SELECT password FROM users WHERE id = ?`, u.ID).Scan(&hash)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Current)) != nil {
		jsonError(w, 422, "Your current password isn't right.")
		return
	}
	if _, err := mail.ParseAddress(body.Email); err != nil || strings.TrimSpace(body.Name) == "" {
		jsonError(w, 422, "Enter a name and a valid email address.")
		return
	}
	if body.New != "" && (len(body.New) < 8 || len(body.New) > 72) {
		jsonError(w, 422, "Use a new password of 8 to 72 characters.")
		return
	}
	s.store.db.Exec(`UPDATE users SET name = ?, email = ? WHERE id = ?`, strings.TrimSpace(body.Name), strings.TrimSpace(body.Email), u.ID)
	if body.New != "" {
		s.store.db.Exec(`UPDATE users SET password = ? WHERE id = ?`, hashPassword(body.New), u.ID)
		s.store.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND id != ?`, u.ID, sessFrom(r).ID)
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- one-tap update from a notification ----

var hookMu sync.Mutex

func (s *Server) hookUpdate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, _ := strconv.ParseInt(q.Get("app"), 10, 64)
	exp, _ := strconv.ParseInt(q.Get("exp"), 10, 64)
	ref := q.Get("ref")
	if !s.settings.Bool("one_tap_updates") || time.Now().Unix() > exp ||
		subtle.ConstantTimeCompare([]byte(q.Get("sig")), []byte(s.hookSig(id, ref, exp))) != 1 {
		http.Error(w, "This update link has expired or isn't valid.", http.StatusForbidden)
		return
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	a, err := s.store.App(id)
	if err != nil {
		http.Error(w, "That app isn't registered any more.", http.StatusNotFound)
		return
	}
	if a.InstalledRef == ref {
		w.Write([]byte(a.Name() + " is already on this version."))
		return
	}
	if a.LatestRef != ref {
		http.Error(w, "A newer version came out since; open The Repo Depot to update.", http.StatusConflict)
		return
	}
	if _, err := s.startJob(r.Context(), a, spec.ActionUpdate, ref, false); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Write([]byte("Updating " + a.Name() + " on " + a.MachineName + "."))
}

// ---- agent installers ----

func (s *Server) agentScript(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := os.ReadFile(filepath.Join(s.cfg.AgentsDir, name))
		if err != nil {
			raw, err = webFS.ReadFile("web/agent/" + name)
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		body := strings.ReplaceAll(string(raw), "__SERVER__", scheme+"://"+r.Host)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(body))
	}
}

var platforms = map[string]string{"linux-amd64": "repodepot-agent-linux-amd64", "linux-arm64": "repodepot-agent-linux-arm64", "windows-amd64": "repodepot-agent-windows-amd64.exe"}

func (s *Server) agentDownload(w http.ResponseWriter, r *http.Request) {
	file, ok := platforms[r.PathValue("platform")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(s.cfg.AgentsDir, file)
	if _, err := os.Stat(p); err != nil {
		http.Error(w, "This server has no agent for that platform.", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	http.ServeFile(w, r, p)
}
