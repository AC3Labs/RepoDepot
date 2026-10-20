package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHub is a small GitHub API client built for speed: every answer is
// kept (memory + database), reused without a request while fresh, and
// re-validated with ETags afterwards (a 304 doesn't count against the rate
// limit). Identical requests in flight at once share one call.
type GitHub struct {
	settings *Settings
	store    *Store
	client   *http.Client

	mu       sync.Mutex
	mem      map[string]*cached
	inflight map[string]*call

	rateMu    sync.Mutex
	rateLimit int
	rateLeft  int
	rateReset time.Time
}

type cached struct {
	etag    string
	status  int
	body    []byte
	fetched time.Time
}

type call struct {
	done chan struct{}
	c    *cached
	err  error
}

func newGitHub(s *Settings, st *Store) *GitHub {
	return &GitHub{settings: s, store: st, client: &http.Client{Timeout: 30 * time.Second},
		mem: map[string]*cached{}, inflight: map[string]*call{}, rateLeft: -1}
}

// GitHubError is a non-2xx answer from GitHub, with its message.
type GitHubError struct {
	Status  int
	Message string
}

func (e *GitHubError) Error() string { return e.Message }

func (g *GitHub) base() string { return strings.TrimRight(g.settings.Get("github_api"), "/") }

// get fetches an API path (or full URL), using the cache. ttl 0 always
// revalidates (still free when nothing changed).
func (g *GitHub) get(ctx context.Context, path string, ttl time.Duration, accept string) ([]byte, error) {
	u := path
	if !strings.HasPrefix(u, "http") {
		u = g.base() + path
	}
	key := accept + " " + u
	c := g.lookup(key)
	if c != nil && time.Since(c.fetched) < ttl {
		return c.body, statusErr(c)
	}

	g.mu.Lock()
	if in, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		<-in.done
		if in.err != nil {
			return nil, in.err
		}
		return in.c.body, statusErr(in.c)
	}
	in := &call{done: make(chan struct{})}
	g.inflight[key] = in
	g.mu.Unlock()

	in.c, in.err = g.fetch(ctx, u, key, accept, c)
	g.mu.Lock()
	delete(g.inflight, key)
	g.mu.Unlock()
	close(in.done)
	if in.err != nil {
		return nil, in.err
	}
	return in.c.body, statusErr(in.c)
}

func statusErr(c *cached) error {
	if c.status >= 200 && c.status < 300 {
		return nil
	}
	var body struct {
		Message string `json:"message"`
	}
	json.Unmarshal(c.body, &body)
	msg := body.Message
	if msg == "" {
		msg = fmt.Sprintf("GitHub answered %d", c.status)
	}
	if c.status == 404 {
		msg = "Not found on GitHub."
	}
	return &GitHubError{Status: c.status, Message: msg}
}

func (g *GitHub) lookup(key string) *cached {
	g.mu.Lock()
	c := g.mem[key]
	g.mu.Unlock()
	if c != nil {
		return c
	}
	var etag, fetched string
	var status int
	var body []byte
	if g.store.db.QueryRow(`SELECT etag, status, body, fetched_at FROM http_cache WHERE url = ?`, key).Scan(&etag, &status, &body, &fetched) != nil {
		return nil
	}
	c = &cached{etag: etag, status: status, body: body, fetched: parseDB(fetched)}
	g.mu.Lock()
	g.mem[key] = c
	g.mu.Unlock()
	return c
}

func (g *GitHub) save(key string, c *cached) {
	g.mu.Lock()
	g.mem[key] = c
	if len(g.mem) > 5000 { // keep memory bounded; the database still has everything
		for k := range g.mem {
			delete(g.mem, k)
			if len(g.mem) < 4000 {
				break
			}
		}
	}
	g.mu.Unlock()
	g.store.db.Exec(`INSERT INTO http_cache (url, etag, status, body, fetched_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET etag = excluded.etag, status = excluded.status, body = excluded.body, fetched_at = excluded.fetched_at`,
		key, c.etag, c.status, c.body, c.fetched.UTC().Format(dbTime))
}

func (g *GitHub) fetch(ctx context.Context, u, key, accept string, old *cached) (*cached, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "TheRepoDepot/"+Version)
	if t := g.settings.Get("github_token"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	if old != nil && old.etag != "" {
		req.Header.Set("If-None-Match", old.etag)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		if old != nil {
			return old, nil // offline: the last answer is better than none
		}
		return nil, fmt.Errorf("couldn't reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	g.noteRate(resp)
	if resp.StatusCode == http.StatusNotModified && old != nil {
		c := &cached{etag: old.etag, status: old.status, body: old.body, fetched: time.Now()}
		g.save(key, c)
		return c, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	limited := resp.StatusCode == 429 || resp.StatusCode == 403 && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" || strings.Contains(strings.ToLower(string(body)), "rate limit"))
	if resp.StatusCode == 403 && !limited {
		// Forbidden for another reason (a private repo the token can't
		// see, a blocked repo): GitHub's own message says why.
		c := &cached{status: resp.StatusCode, body: body, fetched: time.Now()}
		return c, nil
	}
	if limited {
		if old != nil {
			return old, nil
		}
		msg := "GitHub's rate limit was reached."
		if g.settings.Get("github_token") == "" {
			msg += " Add a GitHub token in Settings → GitHub to raise it from 60 to 5,000 requests an hour."
		}
		if reset := g.RateReset(); !reset.IsZero() {
			msg += " It resets at " + reset.Local().Format("15:04") + "."
		}
		return nil, &GitHubError{Status: resp.StatusCode, Message: msg}
	}
	if resp.StatusCode >= 500 {
		if old != nil {
			return old, nil
		}
		return nil, &GitHubError{Status: resp.StatusCode, Message: "GitHub is having trouble right now (" + resp.Status + ")."}
	}
	c := &cached{etag: resp.Header.Get("ETag"), status: resp.StatusCode, body: body, fetched: time.Now()}
	g.save(key, c)
	return c, nil
}

func (g *GitHub) noteRate(resp *http.Response) {
	left, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	if err != nil || resp.Header.Get("X-RateLimit-Resource") == "search" {
		return
	}
	limit, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
	reset, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	g.rateMu.Lock()
	g.rateLeft, g.rateLimit, g.rateReset = left, limit, time.Unix(reset, 0)
	g.rateMu.Unlock()
}

// Rate is the last seen core API allowance (left -1 = not known yet).
func (g *GitHub) Rate() (left, limit int, reset time.Time) {
	g.rateMu.Lock()
	defer g.rateMu.Unlock()
	return g.rateLeft, g.rateLimit, g.rateReset
}

func (g *GitHub) RateReset() time.Time {
	_, _, r := g.Rate()
	return r
}

func (g *GitHub) getJSON(ctx context.Context, path string, ttl time.Duration, out any) error {
	body, err := g.get(ctx, path, ttl, "")
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func (g *GitHub) minutes(key string) time.Duration {
	return time.Duration(g.settings.Int(key)) * time.Minute
}

// ---- types ----

type Repo struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Owner    struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	} `json:"owner"`
	Description string   `json:"description"`
	HTMLURL     string   `json:"html_url"`
	Homepage    string   `json:"homepage"`
	Stars       int      `json:"stargazers_count"`
	Forks       int      `json:"forks_count"`
	OpenIssues  int      `json:"open_issues_count"`
	Language    string   `json:"language"`
	Topics      []string `json:"topics"`
	License     *struct {
		SPDX string `json:"spdx_id"`
		Name string `json:"name"`
	} `json:"license"`
	PushedAt      string `json:"pushed_at"`
	UpdatedAt     string `json:"updated_at"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
}

type SearchResult struct {
	Total int    `json:"total_count"`
	Items []Repo `json:"items"`
}

type Asset struct {
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	DownloadURL   string `json:"browser_download_url"`
	APIURL        string `json:"url"`
	DownloadCount int    `json:"download_count"`
	Digest        string `json:"digest"` // "sha256:…" on releases published since mid-2025
}

type Release struct {
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	HTMLURL     string  `json:"html_url"`
	Draft       bool    `json:"draft"`
	Prerelease  bool    `json:"prerelease"`
	PublishedAt string  `json:"published_at"`
	Body        string  `json:"body"`
	BodyHTML    string  `json:"body_html"`
	Assets      []Asset `json:"assets"`
}

type Tag struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type Commit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
			Date string `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}

type ContentEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"download_url"`
}

// ---- calls ----

func (g *GitHub) Search(ctx context.Context, q, sort string, page int) (*SearchResult, error) {
	v := url.Values{"q": {q}, "per_page": {strconv.Itoa(g.settings.Int("search_per_page"))}, "page": {strconv.Itoa(max(page, 1))}}
	if sort == "stars" || sort == "updated" {
		v.Set("sort", sort)
	}
	out := &SearchResult{}
	return out, g.getJSON(ctx, "/search/repositories?"+v.Encode(), g.minutes("cache_search_minutes"), out)
}

func splitRepo(full string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(full, "/")
	return owner, name, ok && validSlugPart(owner) && validSlugPart(name)
}

func validSlugPart(s string) bool {
	if s == "" || len(s) > 100 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (g *GitHub) Repo(ctx context.Context, full string, ttl time.Duration) (*Repo, error) {
	r := &Repo{}
	return r, g.getJSON(ctx, "/repos/"+full, ttl, r)
}

// Readme returns GitHub's own rendering of the README (already sanitised
// by GitHub; sanitised again before display).
func (g *GitHub) Readme(ctx context.Context, full string) (string, error) {
	body, err := g.get(ctx, "/repos/"+full+"/readme", g.minutes("cache_repo_minutes"), "application/vnd.github.html+json")
	if err != nil {
		var ge *GitHubError
		if errors.As(err, &ge) && ge.Status == 404 {
			return "", nil
		}
		return "", err
	}
	return string(body), nil
}

func (g *GitHub) Releases(ctx context.Context, full string, ttl time.Duration) ([]Release, error) {
	var out []Release
	return out, g.getJSON(ctx, "/repos/"+full+"/releases?per_page=30", ttl, &out)
}

// LatestRelease picks the newest published release (optionally including
// pre-releases). nil when the repo has none.
func (g *GitHub) LatestRelease(ctx context.Context, full string, prerelease bool, ttl time.Duration) (*Release, error) {
	rels, err := g.Releases(ctx, full, ttl)
	if err != nil {
		return nil, err
	}
	for i := range rels {
		if rels[i].Draft || (rels[i].Prerelease && !prerelease) {
			continue
		}
		return &rels[i], nil
	}
	return nil, nil
}

func (g *GitHub) ReleaseByTag(ctx context.Context, full, tag string) (*Release, error) {
	r := &Release{}
	return r, g.getJSON(ctx, "/repos/"+full+"/releases/tags/"+url.PathEscape(tag), time.Hour, r)
}

func (g *GitHub) Tags(ctx context.Context, full string, ttl time.Duration) ([]Tag, error) {
	var out []Tag
	return out, g.getJSON(ctx, "/repos/"+full+"/tags?per_page=100", ttl, &out)
}

func (g *GitHub) Head(ctx context.Context, full, branch string, ttl time.Duration) (*Commit, error) {
	c := &Commit{}
	return c, g.getJSON(ctx, "/repos/"+full+"/commits/"+url.PathEscape(branch), ttl, c)
}

func (g *GitHub) Contents(ctx context.Context, full, path string) ([]ContentEntry, error) {
	var out []ContentEntry
	err := g.getJSON(ctx, "/repos/"+full+"/contents/"+strings.TrimLeft(path, "/"), g.minutes("cache_repo_minutes"), &out)
	return out, err
}

// Tree lists every file path in the repo at a branch (one request; GitHub
// truncates huge repos, which is fine for finding a logo).
func (g *GitHub) Tree(ctx context.Context, full, branch string) ([]string, error) {
	var out struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	if err := g.getJSON(ctx, "/repos/"+full+"/git/trees/"+url.PathEscape(branch)+"?recursive=1", g.minutes("cache_repo_minutes"), &out); err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range out.Tree {
		if e.Type == "blob" && e.Size < 2<<20 {
			paths = append(paths, e.Path)
		}
	}
	return paths, nil
}

// File is one file's details, including where to download it.
func (g *GitHub) File(ctx context.Context, full, path string) (*ContentEntry, error) {
	e := &ContentEntry{}
	return e, g.getJSON(ctx, "/repos/"+full+"/contents/"+strings.TrimLeft(path, "/"), g.minutes("cache_repo_minutes"), e)
}

// FileText fetches a small text file (a compose file to show the user).
func (g *GitHub) FileText(ctx context.Context, full, ref, path string) (string, error) {
	u := "/repos/" + full + "/contents/" + strings.TrimLeft(path, "/")
	if ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	body, err := g.get(ctx, u, g.minutes("cache_repo_minutes"), "application/vnd.github.raw+json")
	return string(body), err
}

// Download fetches a small public file (a release's checksum list) without
// using the API allowance.
func (g *GitHub) Download(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "TheRepoDepot/"+Version)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// SourceURL is a tarball of the repo at ref, for agents to download.
// Public repos use codeload (no API quota); private ones need the API.
func (g *GitHub) SourceURL(full, ref string, private bool) string {
	if private || g.base() != "https://api.github.com" {
		return g.base() + "/repos/" + full + "/tarball/" + url.PathEscape(ref)
	}
	return "https://codeload.github.com/" + full + "/tar.gz/" + url.PathEscape(ref)
}
