package server

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Logos: GitHub has no "logo" field, so the best image is found in this
// order: a logo/icon file in the repo (top level, then common asset
// folders), the first non-badge image in the README, the owner's avatar.
// The result is downloaded once and served from disk.

var logoName = regexp.MustCompile(`(?i)^(logo|icon|app[-_]?icon|favicon|brand|mark)([-_.].*)?\.(png|svg|jpe?g|webp)$`)
var logoDirs = map[string]bool{"assets": true, ".github": true, "docs": true, "images": true, "img": true, "media": true,
	"static": true, "public": true, "resources": true, "branding": true, "art": true, "logo": true, "icons": true, ".assets": true}

var logoMu sync.Map // repo -> *sync.Mutex, so one repo's logo is found once

type logoFinder struct {
	s   *Server
	dir string
}

func (s *Server) logoDir() string { return filepath.Join(s.cfg.DataDir, "logos") }

// LogoURL is where the browser gets a repo's logo: our cached copy when
// there is one, otherwise the owner's avatar straight from GitHub.
func (s *Server) LogoURL(full, avatar string) string {
	var logo string
	s.store.db.QueryRow(`SELECT logo FROM repo_meta WHERE repo = ?`, full).Scan(&logo)
	if logo != "" {
		return "/logos/" + logo
	}
	if avatar != "" {
		return avatar + "&s=160"
	}
	owner, _, _ := strings.Cut(full, "/")
	return "https://github.com/" + owner + ".png?size=160"
}

// ensureLogo finds and stores a repo's logo if it hasn't been looked for
// in the last week. Safe to call often.
func (s *Server) ensureLogo(ctx context.Context, repo *Repo) string {
	full := repo.FullName
	m, _ := logoMu.LoadOrStore(full, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	var logo, checked string
	s.store.db.QueryRow(`SELECT logo, logo_checked FROM repo_meta WHERE repo = ?`, full).Scan(&logo, &checked)
	if checked != "" && time.Since(parseDB(checked)) < 7*24*time.Hour {
		return logo
	}
	src := s.findLogo(ctx, repo)
	file := ""
	if src != "" {
		file = s.downloadLogo(ctx, full, src)
	}
	if file == "" {
		file = s.downloadLogo(ctx, full, repo.Owner.AvatarURL+"&s=256")
	}
	if file != "" && file != logo && logo != "" {
		os.Remove(filepath.Join(s.logoDir(), logo))
	}
	s.store.db.Exec(`INSERT INTO repo_meta (repo, logo, logo_checked) VALUES (?, ?, ?)
		ON CONFLICT(repo) DO UPDATE SET logo = excluded.logo, logo_checked = excluded.logo_checked`, full, file, nowDB())
	return file
}

func (s *Server) findLogo(ctx context.Context, repo *Repo) string {
	full := repo.FullName
	root, err := s.gh.Contents(ctx, full, "")
	if err != nil {
		return ""
	}
	pick := func(list []ContentEntry) string {
		best := ""
		for _, e := range list {
			if e.Type == "file" && e.Size < 2<<20 && logoName.MatchString(e.Name) {
				// Prefer "logo" over "icon"/"favicon", and png/svg over jpg.
				if best == "" || strings.HasPrefix(strings.ToLower(e.Name), "logo") && !strings.HasPrefix(strings.ToLower(filepath.Base(best)), "logo") {
					best = e.DownloadURL
				}
			}
		}
		return best
	}
	if u := pick(root); u != "" {
		return u
	}
	checked := 0
	for _, e := range root {
		if e.Type == "dir" && logoDirs[strings.ToLower(e.Name)] && checked < 4 {
			checked++
			list, err := s.gh.Contents(ctx, full, e.Path)
			if err != nil {
				continue
			}
			if u := pick(list); u != "" {
				return u
			}
			for _, sub := range list { // one more level: assets/images/logo.png
				if sub.Type == "dir" && logoDirs[strings.ToLower(sub.Name)] {
					if l2, err := s.gh.Contents(ctx, full, sub.Path); err == nil {
						if u := pick(l2); u != "" {
							return u
						}
					}
				}
			}
		}
	}
	if html, err := s.gh.Readme(ctx, full); err == nil {
		if u := firstReadmeImage(html, full, repo.DefaultBranch); u != "" {
			return u
		}
	}
	return ""
}

var imgSrc = regexp.MustCompile(`(?i)<img[^>]+src="([^"]+)"`)
var badgeHint = regexp.MustCompile(`(?i)shields\.io|badge|travis-ci|codecov|coveralls|circleci|workflows/|/actions/|sponsor|buymeacoffee|ko-fi|paypal|discord|gitter|star-history|contrib\.rocks|stats\.|readme-stats|visitor|hits\.|\.gif$|github\.com/.+/raw/.+/screenshots?`)

var imgTag = regexp.MustCompile(`(?i)<img[^>]*>`)
var imgAlt = regexp.MustCompile(`(?i)alt="([^"]*)"`)
var logoHint = regexp.MustCompile(`(?i)logo|icon|brand|mascot`)

// firstReadmeImage takes a README image only when it looks like a logo:
// named or labelled as one, or named after the repo (fzf.png). The first
// image is often a screenshot, which makes a poor logo; the owner's avatar
// is better then.
func firstReadmeImage(html, full, branch string) string {
	_, repoName, _ := strings.Cut(strings.ToLower(full), "/")
	for i, tag := range imgTag.FindAllString(html, 10) {
		if i > 5 {
			break // a logo is near the top
		}
		m := imgSrc.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		src := strings.ReplaceAll(m[1], "&amp;", "&")
		if badgeHint.MatchString(src) {
			continue
		}
		alt := ""
		if a := imgAlt.FindStringSubmatch(tag); a != nil {
			alt = a[1]
		}
		file := strings.ToLower(path.Base(strings.Split(src, "?")[0]))
		base := strings.TrimSuffix(file, path.Ext(file))
		if logoHint.MatchString(file) || logoHint.MatchString(alt) || base == repoName || strings.EqualFold(strings.TrimSpace(alt), repoName) {
			return absoluteRepoURL(src, full, branch, true)
		}
	}
	return ""
}

// absoluteRepoURL makes a README link absolute: images to raw files,
// other links to the GitHub page.
func absoluteRepoURL(src, full, branch string, raw bool) string {
	if src == "" || strings.HasPrefix(src, "#") || strings.HasPrefix(src, "mailto:") {
		return src
	}
	if u, err := url.Parse(src); err == nil && u.IsAbs() {
		return src
	}
	if strings.HasPrefix(src, "//") {
		return "https:" + src
	}
	if branch == "" {
		branch = "HEAD"
	}
	p := strings.TrimPrefix(src, "./")
	if strings.HasPrefix(p, "/") {
		// "/owner/repo/blob/..." style links GitHub already resolved
		return "https://github.com" + p
	}
	if raw {
		return "https://raw.githubusercontent.com/" + full + "/" + branch + "/" + p
	}
	return "https://github.com/" + full + "/blob/" + branch + "/" + p
}

var imageTypes = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/svg+xml": ".svg", "image/gif": ".gif", "image/x-icon": ".ico", "image/vnd.microsoft.icon": ".ico"}

func (s *Server) downloadLogo(ctx context.Context, full, src string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "TheRepoDepot/"+Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	if err != nil || len(body) == 0 {
		return ""
	}
	ct := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	ext := imageTypes[ct]
	if ext == "" { // raw.githubusercontent serves files as text/plain
		ext = imageTypes[http.DetectContentType(body)]
		if ext == "" && bytes.Contains(body[:min(len(body), 512)], []byte("<svg")) {
			ext = ".svg"
		}
		if ext == "" {
			switch strings.ToLower(filepath.Ext(strings.Split(src, "?")[0])) {
			case ".svg":
				ext = ".svg"
			case ".png", ".jpg", ".jpeg", ".webp":
				ext = imageTypes[http.DetectContentType(body)]
			}
		}
	}
	if ext == "" {
		return ""
	}
	h := sha1.Sum([]byte(full))
	name := strings.ReplaceAll(full, "/", "__") + "-" + hex.EncodeToString(h[:4]) + ext
	os.MkdirAll(s.logoDir(), 0o755)
	if os.WriteFile(filepath.Join(s.logoDir(), name), body, 0o644) != nil {
		return ""
	}
	return name
}
