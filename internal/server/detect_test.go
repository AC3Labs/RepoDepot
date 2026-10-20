package server

import (
	"strings"
	"testing"
)

func names(list ...string) []Asset {
	var out []Asset
	for _, n := range list {
		out = append(out, Asset{Name: n})
	}
	return out
}

func TestPickAssetChoosesTheRightFileForEachMachine(t *testing.T) {
	// Real asset lists from popular projects.
	lazydocker := names("checksums.txt", "lazydocker_0.24.1_Darwin_arm64.tar.gz", "lazydocker_0.24.1_Darwin_x86_64.tar.gz",
		"lazydocker_0.24.1_Linux_arm64.tar.gz", "lazydocker_0.24.1_Linux_armv6.tar.gz", "lazydocker_0.24.1_Linux_x86_64.tar.gz",
		"lazydocker_0.24.1_Windows_arm64.zip", "lazydocker_0.24.1_Windows_x86_64.zip")
	ripgrep := names("ripgrep-14.1.1-aarch64-unknown-linux-gnu.tar.gz", "ripgrep-14.1.1-x86_64-unknown-linux-musl.tar.gz",
		"ripgrep-14.1.1-x86_64-unknown-linux-musl.tar.gz.sha256", "ripgrep-14.1.1-x86_64-pc-windows-msvc.zip",
		"ripgrep-14.1.1-x86_64-apple-darwin.tar.gz", "ripgrep_14.1.1-1_amd64.deb", "ripgrep-14.1.1-i686-pc-windows-msvc.zip")
	syncthing := names("syncthing-linux-amd64-v1.29.2.tar.gz", "syncthing-linux-arm64-v1.29.2.tar.gz", "syncthing-windows-amd64-v1.29.2.zip",
		"syncthing-macos-universal-v1.29.2.zip", "sha256sum.txt.asc")
	cases := []struct {
		assets           []Asset
		goos, arch, want string
	}{
		{lazydocker, "linux", "amd64", "lazydocker_0.24.1_Linux_x86_64.tar.gz"},
		{lazydocker, "linux", "arm64", "lazydocker_0.24.1_Linux_arm64.tar.gz"},
		{lazydocker, "windows", "amd64", "lazydocker_0.24.1_Windows_x86_64.zip"},
		{ripgrep, "linux", "amd64", "ripgrep-14.1.1-x86_64-unknown-linux-musl.tar.gz"}, // self-contained beats a .deb
		{ripgrep, "windows", "amd64", "ripgrep-14.1.1-x86_64-pc-windows-msvc.zip"},
		{ripgrep, "linux", "arm64", "ripgrep-14.1.1-aarch64-unknown-linux-gnu.tar.gz"},
		{syncthing, "linux", "amd64", "syncthing-linux-amd64-v1.29.2.tar.gz"},
		{syncthing, "windows", "amd64", "syncthing-windows-amd64-v1.29.2.zip"},
	}
	for _, c := range cases {
		got, _ := pickAsset(c.assets, c.goos, c.arch, true, "")
		if got == nil || got.Name != c.want {
			n := "<none>"
			if got != nil {
				n = got.Name
			}
			t.Errorf("%s/%s: got %s, want %s", c.goos, c.arch, n, c.want)
		}
	}
	if got, _ := pickAsset(names("tool-darwin-arm64.tar.gz", "tool.dmg"), "linux", "amd64", true, ""); got != nil {
		t.Errorf("picked %s for Linux from a Mac-only release", got.Name)
	}
}

func TestAssetPatternSurvivesNewVersions(t *testing.T) {
	p := assetPattern("lazydocker_0.24.1_Linux_x86_64.tar.gz")
	if p != "lazydocker_*_Linux_x86_64.tar.gz" {
		t.Fatalf("pattern %q", p)
	}
	if !assetMatches("lazydocker_0.25.0_Linux_x86_64.tar.gz", p) || assetMatches("lazydocker_0.25.0_Linux_arm64.tar.gz", p) {
		t.Fatal("matching")
	}
	got, _ := pickAsset(names("ripgrep-15.0.0-x86_64-unknown-linux-gnu.tar.gz", "ripgrep-15.0.0-x86_64-unknown-linux-musl.tar.gz"), "linux", "amd64", true,
		assetPattern("ripgrep-14.1.1-x86_64-unknown-linux-musl.tar.gz"))
	if got == nil || !strings.Contains(got.Name, "musl") {
		t.Fatalf("the musl build should stay musl: %v", got)
	}
}

func TestSanitizeReadme(t *testing.T) {
	in := `<div><h1 id="x">Tool</h1><script>alert(1)</script><p onclick="evil()">Hi <a href="javascript:alert(1)">bad</a> <a href="docs/guide.md">guide</a></p>
<img src="assets/logo.png" onerror="x()"><iframe src="https://evil"></iframe><style>body{}</style></div>`
	out := sanitizeReadme(in, "me/tool", "main", true)
	for _, bad := range []string{"<script", "alert(1)</script", "onclick", "javascript:", "onerror", "<iframe", "<style"} {
		if strings.Contains(out, bad) {
			t.Errorf("kept %q: %s", bad, out)
		}
	}
	for _, good := range []string{`href="https://github.com/me/tool/blob/main/docs/guide.md"`, `src="https://raw.githubusercontent.com/me/tool/main/assets/logo.png"`, `target="_blank"`, "<h1"} {
		if !strings.Contains(out, good) {
			t.Errorf("missing %q: %s", good, out)
		}
	}
	if strings.Contains(sanitizeReadme(`<img src="x.png">`, "me/tool", "main", false), "<img") {
		t.Error("images should be dropped when turned off")
	}
}

func TestRepoInputAcceptsLinksAndNames(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/jesseduffield/lazydocker":            "jesseduffield/lazydocker",
		"github.com/jesseduffield/lazydocker.git":                "jesseduffield/lazydocker",
		"https://github.com/BurntSushi/ripgrep/releases/tag/v14": "BurntSushi/ripgrep",
		"louislam/uptime-kuma":                                   "louislam/uptime-kuma",
	} {
		m := repoInput.FindStringSubmatch(in)
		if m == nil || m[1]+"/"+m[2] != want {
			t.Errorf("%s: %v", in, m)
		}
	}
}

func TestSettingValidation(t *testing.T) {
	if v, msg := settingIndex["check_interval_minutes"].validate("1"); msg == "" {
		t.Errorf("accepted %s below the minimum", v)
	}
	if v, _ := settingIndex["ntfy_enabled"].validate("on"); v != "true" {
		t.Error("bool")
	}
	if _, msg := settingIndex["public_url"].validate("ftp://x"); msg == "" {
		t.Error("url")
	}
	if _, msg := settingIndex["theme"].validate("neon"); msg == "" {
		t.Error("select")
	}
}

func TestReadmeLogoSkipsScreenshots(t *testing.T) {
	shot := `<p><a href="x"><img src="https://user-images.githubusercontent.com/123/demo.png" alt="demo"></a></p>`
	if got := firstReadmeImage(shot, "me/tool", "main"); got != "" {
		t.Errorf("took a screenshot as the logo: %s", got)
	}
	named := `<img src="https://img.shields.io/badge/x"><img src="docs/tool.png">`
	if got := firstReadmeImage(named, "me/tool", "main"); got != "https://raw.githubusercontent.com/me/tool/main/docs/tool.png" {
		t.Errorf("repo-named image: %q", got)
	}
	alt := `<img alt="Tool logo" src="https://example.com/a.svg">`
	if got := firstReadmeImage(alt, "me/tool", "main"); got != "https://example.com/a.svg" {
		t.Errorf("alt logo: %q", got)
	}
}

func TestHighestTagUsesVersionOrderNotStringOrder(t *testing.T) {
	tags := []Tag{{Name: "v0.9.3.0"}, {Name: "v0.13.0.0"}, {Name: "v0.12.0.0"}, {Name: "v0.14.0.0-rc1"}, {Name: "v0.10.0.1"}}
	if got := highestTag(tags, false).Name; got != "v0.13.0.0" {
		t.Fatalf("highest tag %s", got)
	}
	if !versionLess("1.2.9", "1.2.10") || versionLess("2.0.0", "2.0.0-rc.1") || !versionLess("2.0.0-rc.1", "2.0.0") {
		t.Fatal("versionLess")
	}
}

func TestBestLogoPathFindsDeepLogosAndSkipsVendored(t *testing.T) {
	paths := []string{"README.md", "node_modules/pkg/logo.png", "gohub/web/static/logo.png", "gohub/web/static/favicon.ico", "docs/screenshots/logo-old.png", "hub/public/favicon.png"}
	if got := bestLogoPath(paths, "LANHub"); got != "gohub/web/static/logo.png" {
		t.Fatalf("got %q", got)
	}
	if got := bestLogoPath([]string{"assets/icon.png", "logo.svg"}, "x"); got != "logo.svg" {
		t.Fatalf("got %q", got)
	}
}
