package server

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"repodepot/internal/spec"
)

// Inspection is what The Repo Depot can tell about installing a repo.
type Inspection struct {
	Repo          *Repo      `json:"repo"`
	Latest        *Release   `json:"latest"`   // newest usable release, nil if none
	Releases      []Release  `json:"releases"` // recent releases (for the Releases tab)
	ComposeFiles  []string   `json:"compose_files"`
	HasDockerfile bool       `json:"has_dockerfile"`
	Methods       []MethodOK `json:"methods"`
	Recommended   string     `json:"recommended"`
	Head          *Commit    `json:"head"`
	DefaultTrack  string     `json:"default_track"`
}

type MethodOK struct {
	Method string `json:"method"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
	Needs  string `json:"needs"` // what the target machine must have: docker, linux, windows, ""
}

var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

func (s *Server) inspect(ctx context.Context, full string) (*Inspection, error) {
	ttl := s.gh.minutes("cache_repo_minutes")
	repo, err := s.gh.Repo(ctx, full, ttl)
	if err != nil {
		return nil, err
	}
	in := &Inspection{Repo: repo}

	// The rest is independent: fetch it all at once.
	var wg sync.WaitGroup
	var root []ContentEntry
	var subs = map[string][]ContentEntry{}
	var mu sync.Mutex
	wg.Add(3)
	go func() {
		defer wg.Done()
		in.Releases, _ = s.gh.Releases(ctx, full, ttl)
	}()
	go func() {
		defer wg.Done()
		in.Head, _ = s.gh.Head(ctx, full, repo.DefaultBranch, ttl)
	}()
	go func() {
		defer wg.Done()
		root, _ = s.gh.Contents(ctx, full, "")
		var w2 sync.WaitGroup
		for _, e := range root {
			if e.Type == "dir" && (e.Name == "docker" || e.Name == "deploy") {
				w2.Add(1)
				go func(name string) {
					defer w2.Done()
					list, _ := s.gh.Contents(ctx, full, name)
					mu.Lock()
					subs[name] = list
					mu.Unlock()
				}(e.Name)
			}
		}
		w2.Wait()
	}()
	wg.Wait()

	pre := s.settings.Bool("include_prereleases")
	for i := range in.Releases {
		r := &in.Releases[i]
		if r.Draft || (r.Prerelease && !pre) {
			continue
		}
		in.Latest = r
		break
	}
	for _, e := range root {
		if e.Type != "file" {
			continue
		}
		for _, n := range composeNames {
			if strings.EqualFold(e.Name, n) {
				in.ComposeFiles = append(in.ComposeFiles, e.Name)
			}
		}
		if e.Name == "Dockerfile" {
			in.HasDockerfile = true
		}
	}
	for _, dir := range []string{"docker", "deploy"} {
		for _, e := range subs[dir] {
			for _, n := range composeNames {
				if e.Type == "file" && strings.EqualFold(e.Name, n) {
					in.ComposeFiles = append(in.ComposeFiles, dir+"/"+e.Name)
				}
			}
		}
	}

	if len(in.ComposeFiles) > 0 {
		in.Methods = append(in.Methods, MethodOK{spec.MethodCompose, "Docker Compose", "Runs the project's own " + in.ComposeFiles[0] + " on the machine's Docker.", "docker"})
	} else if in.HasDockerfile {
		in.Methods = append(in.Methods, MethodOK{spec.MethodCompose, "Docker (built from the Dockerfile)", "Builds the image from the repo's Dockerfile and runs it with a compose file The Repo Depot writes; add ports and volumes before installing.", "docker"})
	}
	if in.Latest != nil && len(usableAssets(in.Latest.Assets)) > 0 {
		in.Methods = append(in.Methods, MethodOK{spec.MethodRelease, "Release download", "Installs a file from release " + in.Latest.TagName + ", picked to suit each machine.", ""})
	}
	in.Methods = append(in.Methods, MethodOK{spec.MethodSource, "Source code", "Downloads the code and runs an optional build command you give it.", ""})

	in.Recommended = in.Methods[0].Method
	if p := s.settings.Get("prefer_method"); p != "auto" {
		for _, m := range in.Methods {
			if m.Method == p {
				in.Recommended = p
			}
		}
	}
	in.DefaultTrack = s.defaultTrack(in.Latest != nil)
	return in, nil
}

func (s *Server) defaultTrack(hasReleases bool) string {
	t := s.settings.Get("default_track")
	if t == "auto" {
		if hasReleases {
			return "release"
		}
		return "commit"
	}
	return t
}

// ---- picking a release file for a machine ----

var skipAsset = regexp.MustCompile(`(?i)(\.(sha\d*|sha256sum|md5|sig|asc|pem|crt|sbom|spdx|json|txt|yml|yaml|blockmap|intoto\.jsonl|pub)$|checksums?|sha256sums|\.sig$|source code)`)

// usableAssets drops checksums, signatures and metadata.
func usableAssets(as []Asset) []Asset {
	var out []Asset
	for _, a := range as {
		if !skipAsset.MatchString(a.Name) {
			out = append(out, a)
		}
	}
	return out
}

var osWords = map[string][]string{
	"linux":   {"linux"},
	"windows": {"windows", "win64", "win32", "win-x64", "win_x64", "-win.", "_win.", "-win-", "_win_", ".exe", ".msi"},
	"darwin":  {"darwin", "macos", "mac-", "osx", "apple", ".dmg", ".pkg"},
	"freebsd": {"freebsd"},
	"android": {"android", ".apk"},
}

var archWords = map[string][]string{
	"amd64": {"amd64", "x86_64", "x86-64", "x64", "64bit", "64-bit", "win64", "linux64"},
	"arm64": {"arm64", "aarch64", "armv8"},
	"386":   {"i386", "i686", "386", "x86", "32bit", "32-bit", "win32"},
	"arm":   {"armv7", "armv6", "armhf", "armel", "arm32"},
}

func hasAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// scoreAsset rates how well a release file suits a machine; < 0 means no.
func scoreAsset(name, goos, arch string, apt bool) int {
	n := strings.ToLower(name)
	score := 0
	for o, words := range osWords {
		if hasAny(n, words) {
			if o == goos {
				score += 100
			} else {
				return -1
			}
		}
	}
	matchedArch := false
	for a, words := range archWords {
		if a == "386" && hasAny(n, archWords["amd64"]) {
			continue // "x86_64" also contains "x86"
		}
		if a == "arm" && hasAny(n, archWords["arm64"]) {
			continue
		}
		if hasAny(n, words) {
			if a == arch {
				score += 50
				matchedArch = true
			} else {
				return -1
			}
		}
	}
	if !matchedArch && arch == "amd64" {
		score += 5 // no arch in the name usually means x86-64
	}
	switch {
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"), strings.HasSuffix(n, ".tar.xz"), strings.HasSuffix(n, ".tar.bz2"):
		if goos == "windows" {
			score += 5
		} else {
			score += 30
		}
	case strings.HasSuffix(n, ".zip"):
		if goos == "windows" {
			score += 30
		} else {
			score += 15
		}
	case strings.HasSuffix(n, ".deb"):
		if goos != "linux" {
			return -1
		}
		if apt {
			score += 25
		} else {
			score += 2
		}
	case strings.HasSuffix(n, ".rpm"):
		if goos != "linux" {
			return -1
		}
		score += 3
	case strings.HasSuffix(n, ".appimage"):
		if goos != "linux" {
			return -1
		}
		score += 20
	case strings.HasSuffix(n, ".msi"):
		score += 25
	case strings.HasSuffix(n, ".exe"):
		score += 20
	case strings.HasSuffix(n, ".dmg"), strings.HasSuffix(n, ".pkg"), strings.HasSuffix(n, ".apk"), strings.HasSuffix(n, ".snap"), strings.HasSuffix(n, ".flatpak"):
		return -1
	default:
		if !strings.Contains(n, ".") || score >= 100 {
			score += 18 // a bare program
		}
	}
	if score < 100 && hasAnyOS(n) {
		return -1
	}
	if score < 20 {
		return -1 // nothing says it suits this machine
	}
	if strings.Contains(n, "musl") {
		score -= 2 // glibc builds first, musl still works
	}
	if strings.Contains(n, "debug") || strings.Contains(n, "symbols") || strings.Contains(n, "-dev") {
		score -= 40
	}
	return score
}

func hasAnyOS(n string) bool {
	for _, words := range osWords {
		if hasAny(n, words) {
			return true
		}
	}
	return false
}

// pickAsset chooses the best release file for a machine. pattern, when
// set (from the app's settings), must appear in the file name; version
// numbers in it are ignored so it keeps working across releases.
func pickAsset(assets []Asset, goos, arch string, apt bool, pattern string) (*Asset, []Asset) {
	type scored struct {
		a Asset
		s int
	}
	var list []scored
	for _, a := range usableAssets(assets) {
		if pattern != "" && !assetMatches(a.Name, pattern) {
			continue
		}
		sc := scoreAsset(a.Name, goos, arch, apt)
		if pattern != "" && sc < 0 {
			sc = 1 // the user chose it; trust them
		}
		if sc >= 0 {
			list = append(list, scored{a, sc})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].s > list[j].s })
	var out []Asset
	for _, s := range list {
		out = append(out, s.a)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], out
}

var versionLike = regexp.MustCompile(`v?\d+(\.\d+)+([-.](rc|beta|alpha)\.?\d*)?`)

// assetPattern turns a chosen file name into a pattern that survives new
// versions: "tool-1.4.2-linux-amd64.tar.gz" -> "tool-*-linux-amd64.tar.gz".
func assetPattern(name string) string {
	return versionLike.ReplaceAllString(name, "*")
}

func assetMatches(name, pattern string) bool {
	parts := strings.Split(strings.ToLower(pattern), "*")
	n := strings.ToLower(name)
	if !strings.HasPrefix(n, parts[0]) {
		return false
	}
	n = n[len(parts[0]):]
	for i, p := range parts[1:] {
		last := i == len(parts)-2
		if last {
			return strings.HasSuffix(n, p)
		}
		k := strings.Index(n, p)
		if k < 0 {
			return false
		}
		n = n[k+len(p):]
	}
	return n == ""
}

// generatedCompose is the compose file for a repo that only has a
// Dockerfile. Ports and volumes are the user's to add.
func generatedCompose(slug string) string {
	return "services:\n  " + slug + ":\n    build: .\n    restart: unless-stopped\n    # ports:\n    #   - \"8080:8080\"\n    # volumes:\n    #   - ./data:/data\n"
}

// ---- update targets ----

// Target is a version an app can be installed at.
type Target struct {
	Ref   string
	Label string
	URL   string
	Date  string
	Rel   *Release
}

// latestTarget finds what an app tracking track should be on now.
func (s *Server) latestTarget(ctx context.Context, repo *Repo, track, branch string, ttl time.Duration) (*Target, error) {
	full := repo.FullName
	switch track {
	case "release":
		r, err := s.gh.LatestRelease(ctx, full, s.settings.Bool("include_prereleases"), ttl)
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, &GitHubError{Status: 404, Message: "This repo has no releases; track commits instead."}
		}
		label := r.TagName
		return &Target{Ref: r.TagName, Label: label, URL: r.HTMLURL, Date: r.PublishedAt, Rel: r}, nil
	case "tag":
		tags, err := s.gh.Tags(ctx, full, ttl)
		if err != nil {
			return nil, err
		}
		if len(tags) == 0 {
			return nil, &GitHubError{Status: 404, Message: "This repo has no tags; track commits instead."}
		}
		t := tags[0]
		return &Target{Ref: t.Name, Label: t.Name, URL: repo.HTMLURL + "/releases/tag/" + t.Name}, nil
	default:
		if branch == "" {
			branch = repo.DefaultBranch
		}
		c, err := s.gh.Head(ctx, full, branch, ttl)
		if err != nil {
			return nil, err
		}
		msg, _, _ := strings.Cut(c.Commit.Message, "\n")
		return &Target{Ref: c.SHA, Label: shortSHA(c.SHA) + " · " + truncate(msg, 60), URL: c.HTMLURL, Date: c.Commit.Author.Date}, nil
	}
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
