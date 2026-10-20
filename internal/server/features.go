package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---- skip a version ----

// apiSkip hides one version of an app (the latest unless given): it's not
// offered, notified or auto-installed. The next version is offered as
// usual. An empty ref with "undo" brings a skipped version back.
func (s *Server) apiSkip(w http.ResponseWriter, r *http.Request) {
	a := s.appFromPath(w, r)
	if a == nil {
		return
	}
	var body struct {
		Undo bool `json:"undo"`
	}
	decodeJSON(r, &body)
	if body.Undo {
		s.store.db.Exec(`UPDATE apps SET skipped_ref = '', notified_ref = '' WHERE id = ?`, a.ID)
		s.event(r, "Un-skipped a version", a.Repo+" on "+a.MachineName)
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if !a.NewerKnown() {
		jsonError(w, 422, "There's no newer version to skip.")
		return
	}
	s.store.db.Exec(`UPDATE apps SET skipped_ref = ? WHERE id = ?`, a.LatestRef, a.ID)
	s.event(r, "Skipped a version", a.Repo+" on "+a.MachineName+": "+a.LatestLabel)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- versions to pick from ----

type version struct {
	Ref   string `json:"ref"`
	Label string `json:"label"`
	Date  string `json:"date"`
	URL   string `json:"url"`
	Pre   bool   `json:"pre"`
	Note  string `json:"note"` // installed, latest, previous
}

// apiVersions lists versions an app can be put on: recent releases, tags
// or commits, depending on what it tracks.
func (s *Server) apiVersions(w http.ResponseWriter, r *http.Request) {
	a := s.appFromPath(w, r)
	if a == nil {
		return
	}
	repo, err := s.gh.Repo(r.Context(), a.Repo, s.gh.minutes("cache_repo_minutes"))
	if err != nil {
		jsonError(w, 502, err.Error())
		return
	}
	ttl := 5 * time.Minute
	var list []version
	switch a.Track {
	case "release":
		rels, err := s.gh.Releases(r.Context(), a.Repo, ttl)
		if err != nil {
			jsonError(w, 502, err.Error())
			return
		}
		for _, rl := range rels {
			if !rl.Draft {
				list = append(list, version{Ref: rl.TagName, Label: rl.TagName, Date: rl.PublishedAt, URL: rl.HTMLURL, Pre: rl.Prerelease})
			}
		}
	case "tag":
		tags, err := s.gh.Tags(r.Context(), a.Repo, ttl)
		if err != nil {
			jsonError(w, 502, err.Error())
			return
		}
		sort.SliceStable(tags, func(i, j int) bool { return versionLess(tags[j].Name, tags[i].Name) })
		for i, t := range tags {
			if i == 50 {
				break
			}
			list = append(list, version{Ref: t.Name, Label: t.Name, URL: repo.HTMLURL + "/releases/tag/" + t.Name, Pre: isPre(t.Name)})
		}
	default:
		branch := a.Branch
		if branch == "" {
			branch = repo.DefaultBranch
		}
		commits, err := s.gh.Commits(r.Context(), a.Repo, branch, ttl)
		if err != nil {
			jsonError(w, 502, err.Error())
			return
		}
		for _, c := range commits {
			msg, _, _ := strings.Cut(c.Commit.Message, "\n")
			list = append(list, version{Ref: c.SHA, Label: shortSHA(c.SHA) + " · " + truncate(msg, 60), Date: c.Commit.Author.Date, URL: c.HTMLURL})
		}
	}
	for i := range list {
		switch list[i].Ref {
		case a.InstalledRef:
			list[i].Note = "installed"
		case a.LatestRef:
			list[i].Note = "latest"
		case a.PreviousRef:
			list[i].Note = "previous"
		}
	}
	if list == nil {
		list = []version{}
	}
	writeJSON(w, 200, map[string]any{"versions": list})
}

// ---- what's new ----

type change struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Date  string `json:"date"`
	HTML  string `json:"html,omitempty"`
	Pre   bool   `json:"pre,omitempty"`
}

type commitLine struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	URL     string `json:"url"`
	Date    string `json:"date"`
	Author  string `json:"author"`
}

// apiChanges shows what changed between the installed and latest version:
// the release notes of every release in between, or the commit list.
func (s *Server) apiChanges(w http.ResponseWriter, r *http.Request) {
	a := s.appFromPath(w, r)
	if a == nil {
		return
	}
	if !a.NewerKnown() {
		writeJSON(w, 200, map[string]any{"releases": []change{}, "commits": []commitLine{}})
		return
	}
	ctx := r.Context()
	images := s.settings.Bool("readme_images")
	releases := []change{}
	if a.Track == "release" || a.Track == "tag" {
		rels, err := s.gh.ReleasesHTML(ctx, a.Repo, 10*time.Minute)
		if err == nil {
			t := s.trackFor(a)
			started := false
			for _, rl := range rels { // newest first, down to the installed one
				if rl.TagName == a.LatestRef {
					started = true
				}
				if rl.TagName == a.InstalledRef {
					break
				}
				if !started || rl.Draft || (rl.Prerelease && !t.Prerelease) || t.skip(rl.TagName) {
					continue
				}
				title := rl.TagName
				if rl.Name != "" && rl.Name != rl.TagName {
					title = rl.TagName + " · " + rl.Name
				}
				releases = append(releases, change{Title: title, URL: rl.HTMLURL, Date: rl.PublishedAt, Pre: rl.Prerelease,
					HTML: sanitizeReadme(rl.BodyHTML, a.Repo, "", images)})
				if len(releases) == 15 {
					break
				}
			}
		}
	}
	commits := []commitLine{}
	if len(releases) == 0 {
		cmp, err := s.gh.Compare(ctx, a.Repo, a.InstalledRef, a.LatestRef)
		if err != nil {
			jsonError(w, 502, err.Error())
			return
		}
		for i := len(cmp) - 1; i >= 0 && len(commits) < 100; i-- {
			c := cmp[i]
			msg, _, _ := strings.Cut(c.Commit.Message, "\n")
			commits = append(commits, commitLine{SHA: shortSHA(c.SHA), Message: msg, URL: c.HTMLURL, Date: c.Commit.Author.Date, Author: c.Commit.Author.Name})
		}
	}
	writeJSON(w, 200, map[string]any{"releases": releases, "commits": commits})
}

// ---- your GitHub stars ----

func (s *Server) apiStarred(w http.ResponseWriter, r *http.Request) {
	if s.settings.Get("github_token") == "" {
		jsonError(w, 422, "Add a GitHub token in Settings → GitHub to see your starred repos.")
		return
	}
	page := max(1, atoiDefault(r.URL.Query().Get("page"), 1))
	repos, err := s.gh.Starred(r.Context(), page)
	if err != nil {
		jsonError(w, 502, err.Error())
		return
	}
	installed := s.installedCounts()
	items := make([]searchItem, 0, len(repos))
	for _, rp := range repos {
		items = append(items, toSearchItem(rp, installed))
	}
	writeJSON(w, 200, map[string]any{"total": len(items), "items": items, "more": len(repos) == 100})
}

// ---- audit log ----

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"events": s.events(200)})
}

// ---- webhook test ----

func (s *Server) apiTestWebhook(w http.ResponseWriter, r *http.Request) {
	if s.settings.Get("webhook_url") == "" {
		jsonError(w, 422, "Save a webhook address first.")
		return
	}
	if err := s.sendWebhook(s.settings.Get("site_name")+" test", "The webhook is working. Updates will show up here.", "test", "/"); err != nil {
		jsonError(w, 502, "Couldn't send: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- backups ----

func (s *Server) backupDir() string { return filepath.Join(s.cfg.DataDir, "backups") }

// backupTo writes a consistent copy of the database (safe while running).
func (s *Server) backupTo(path string) error {
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.Remove(path)
	if _, err := s.store.db.Exec(`VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("couldn't back up the database: %w", err)
	}
	return os.Chmod(path, 0o600)
}

type backupFile struct {
	Name string `json:"name"`
	Size string `json:"size"`
	Date string `json:"date"`
}

func (s *Server) backupList() []backupFile {
	out := []backupFile{}
	entries, _ := os.ReadDir(s.backupDir())
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !strings.HasPrefix(e.Name(), "repodepot-") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, backupFile{Name: e.Name(), Size: humanSize(info.Size()), Date: info.ModTime().In(s.settings.Location()).Format("2 Jan 2006 15:04")})
	}
	return out
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// apiBackup downloads a backup: a fresh copy, or a saved daily one (?file=).
func (s *Server) apiBackup(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("file")
	var p string
	if name != "" {
		if filepath.Base(name) != name || !strings.HasPrefix(name, "repodepot-") || !strings.HasSuffix(name, ".db") {
			http.NotFound(w, r)
			return
		}
		p = filepath.Join(s.backupDir(), name)
	} else {
		name = "repodepot-" + time.Now().In(s.settings.Location()).Format("2006-01-02-1504") + ".db"
		p = filepath.Join(s.backupDir(), ".download-"+randomHex(6)+".db")
		if err := s.backupTo(p); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer os.Remove(p)
	}
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	s.event(r, "Downloaded a backup", name)
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, time.Now(), f)
}

// dailyBackup keeps one copy per day, deleting the oldest past the limit.
func (s *Server) dailyBackup() {
	if !s.settings.Bool("backup_enabled") {
		return
	}
	p := filepath.Join(s.backupDir(), "repodepot-"+time.Now().In(s.settings.Location()).Format("2006-01-02")+".db")
	if _, err := os.Stat(p); err == nil {
		return
	}
	if err := s.backupTo(p); err != nil {
		s.notifyFailure("Daily backup failed", err.Error())
		return
	}
	var daily []string
	entries, _ := os.ReadDir(s.backupDir())
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, "repodepot-") && strings.HasSuffix(n, ".db") && len(n) == len("repodepot-2006-01-02.db") {
			daily = append(daily, n)
		}
	}
	sort.Strings(daily)
	for len(daily) > s.settings.Int("backup_keep") {
		os.Remove(filepath.Join(s.backupDir(), daily[0]))
		daily = daily[1:]
	}
}

func (s *Server) notifyFailure(title, msg string) {
	if s.settings.Bool("notify_failures") {
		s.notify(title, msg, "x", "/settings?tab=backups", nil)
	}
}

// ---- helpers ----

func atoiDefault(v string, def int) int {
	var n int
	if _, err := fmt.Sscan(v, &n); err != nil {
		return def
	}
	return n
}

func (s *Server) installedCounts() map[string]int {
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
	return installed
}

func toSearchItem(rp Repo, installed map[string]int) searchItem {
	topics := rp.Topics
	if len(topics) > 4 {
		topics = topics[:4]
	}
	avatar := rp.Owner.AvatarURL
	if avatar != "" {
		if strings.Contains(avatar, "?") {
			avatar += "&s=96"
		} else {
			avatar += "?s=96"
		}
	}
	return searchItem{FullName: rp.FullName, Owner: rp.Owner.Login, Name: rp.Name, Avatar: avatar,
		Description: rp.Description, Stars: rp.Stars, StarsLabel: humanNum(rp.Stars), Language: rp.Language,
		Topics: topics, Updated: ago(rp.PushedAt), Archived: rp.Archived, Installed: installed[strings.ToLower(rp.FullName)]}
}

// ---- extra GitHub calls ----

func (g *GitHub) Commits(ctx context.Context, full, branch string, ttl time.Duration) ([]Commit, error) {
	v := url.Values{"sha": {branch}, "per_page": {"30"}}
	var out []Commit
	return out, g.getJSON(ctx, "/repos/"+full+"/commits?"+v.Encode(), ttl, &out)
}

// Compare lists the commits between two refs (oldest first).
func (g *GitHub) Compare(ctx context.Context, full, base, head string) ([]Commit, error) {
	var out struct {
		Commits []Commit `json:"commits"`
	}
	err := g.getJSON(ctx, "/repos/"+full+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head), time.Hour, &out)
	return out.Commits, err
}

// ReleasesHTML is Releases with notes rendered to HTML by GitHub.
func (g *GitHub) ReleasesHTML(ctx context.Context, full string, ttl time.Duration) ([]Release, error) {
	body, err := g.get(ctx, "/repos/"+full+"/releases?per_page=30", ttl, "application/vnd.github.html+json")
	if err != nil {
		return nil, err
	}
	var out []Release
	return out, json.Unmarshal(body, &out)
}

func (g *GitHub) Starred(ctx context.Context, page int) ([]Repo, error) {
	var out []Repo
	return out, g.getJSON(ctx, fmt.Sprintf("/user/starred?per_page=100&sort=updated&page=%d", page), 10*time.Minute, &out)
}
