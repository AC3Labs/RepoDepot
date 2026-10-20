package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a tiny mail server that keeps every message it's given.
type fakeSMTP struct {
	mu   sync.Mutex
	ln   net.Listener
	msgs []string // raw DATA, one per message
	rcpt [][]string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { fmt.Fprintf(c, "%s\r\n", s) }
	say("220 fake ESMTP")
	var to []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 fake")
		case strings.HasPrefix(cmd, "MAIL"):
			to = nil
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT"):
			to = append(to, strings.TrimSpace(line[strings.Index(line, ":")+1:]))
			say("250 ok")
		case cmd == "DATA":
			say("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.msgs = append(f.msgs, b.String())
			f.rcpt = append(f.rcpt, to)
			f.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

// last returns the newest message with quoted-printable soft breaks undone.
func (f *fakeSMTP) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.msgs) == 0 {
		return ""
	}
	m := f.msgs[len(f.msgs)-1]
	return strings.ReplaceAll(strings.ReplaceAll(m, "=\r\n", ""), "=3D", "=")
}

func (f *fakeSMTP) port() string {
	_, p, _ := net.SplitHostPort(f.ln.Addr().String())
	return p
}

// testServer is a fresh Repo Depot with an admin signed in.
func testServer(t *testing.T) (*Server, *httptest.Server, *browser) {
	t.Helper()
	gh := newFakeGitHub(t)
	s, err := New(Config{DataDir: t.TempDir(), Listen: ":0", AgentsDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.settings.Set("github_api", gh.srv.URL)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	b := newBrowser(t, srv.URL)
	resp, _ := b.c.PostForm(srv.URL+"/setup", url.Values{"name": {"Admin"}, "email": {"admin@example.com"}, "password": {"pass-word-1"}, "password_confirm": {"pass-word-1"}})
	resp.Body.Close()
	b.page("/")
	s.settings.Set("public_url", srv.URL)
	return s, srv, b
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, c: &http.Client{Jar: jar}}
}

// form posts a form like a browser would, returning the final page.
func (b *browser) form(path string, v url.Values) (int, string) {
	b.t.Helper()
	resp, err := b.c.PostForm(b.base+path, v)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if m := csrfMeta.FindSubmatch(body); m != nil {
		b.csrf = string(m[1])
	}
	return resp.StatusCode, string(body)
}

var inviteLink = regexp.MustCompile(`/(invite|reset)/([0-9a-f]{64})`)

func TestInvitesRolesAndPasswordResets(t *testing.T) {
	s, srv, admin := testServer(t)
	mail := newFakeSMTP(t)

	// Without a mail server, an invite gives a link to pass on yourself.
	var inv struct {
		Link       string `json:"link"`
		Emailed    bool   `json:"emailed"`
		EmailError string `json:"email_error"`
	}
	if code := admin.call("POST", "/api/users/invite", map[string]string{"email": "Viewer@Example.com", "role": "viewer"}, &inv); code != 200 {
		t.Fatalf("invite: %d", code)
	}
	if inv.Emailed || !inviteLink.MatchString(inv.Link) || !strings.HasPrefix(inv.Link, srv.URL) {
		t.Fatalf("invite without mail: %+v", inv)
	}
	viewerLink := inv.Link
	// The same address can't be invited twice; junk isn't an address.
	if code := admin.call("POST", "/api/users/invite", map[string]string{"email": "viewer@example.com", "role": "viewer"}, nil); code != 422 {
		t.Fatalf("duplicate invite: %d", code)
	}
	if code := admin.call("POST", "/api/users/invite", map[string]string{"email": "not an email", "role": "viewer"}, nil); code != 422 {
		t.Fatalf("bad address: %d", code)
	}
	if code := admin.call("POST", "/api/users/invite", map[string]string{"email": "x@example.com", "role": "owner"}, nil); code != 422 {
		t.Fatalf("bad role: %d", code)
	}

	// With a mail server, the invite is emailed.
	s.settings.Set("smtp_host", "127.0.0.1")
	s.settings.Set("smtp_port", mail.port())
	s.settings.Set("smtp_security", "none")
	s.settings.Set("smtp_from", "depot@example.com")
	admin.call("POST", "/api/users/invite", map[string]string{"email": "member@example.com", "name": "Mem", "role": "member"}, &inv)
	if !inv.Emailed || mail.count() != 1 {
		t.Fatalf("invite email: %+v (%d sent)", inv, mail.count())
	}
	msg := mail.last()
	m := inviteLink.FindStringSubmatch(msg)
	if m == nil || !strings.Contains(msg, "Admin invited you") || !strings.Contains(msg, "To: member@example.com") {
		t.Fatalf("invite email body:\n%s", msg)
	}
	memberLink := srv.URL + m[0]

	// A pending user can't sign in yet.
	if code, _ := newBrowser(t, srv.URL).form("/login", url.Values{"email": {"member@example.com"}, "password": {""}}); code != 422 {
		t.Fatalf("pending sign-in: %d", code)
	}

	// Accepting: the link shows the form, a bad password is refused, a
	// good one signs them in. The link then stops working.
	member := newBrowser(t, srv.URL)
	if body := member.page(strings.TrimPrefix(memberLink, srv.URL)); !strings.Contains(body, "member@example.com") || !strings.Contains(body, "Welcome aboard") {
		t.Fatalf("invite page:\n%s", body)
	}
	path := strings.TrimPrefix(memberLink, srv.URL)
	if code, _ := member.form(path, url.Values{"name": {"Mem"}, "password": {"short"}, "password_confirm": {"short"}}); code != 422 {
		t.Fatalf("short password accepted: %d", code)
	}
	if code, body := member.form(path, url.Values{"name": {"Member One"}, "password": {"member-pass-1"}, "password_confirm": {"member-pass-1"}}); code != 200 || !strings.Contains(body, "My Depot") {
		t.Fatalf("accept invite: %d", code)
	}
	if code, _ := newBrowser(t, srv.URL).form(path, url.Values{"name": {"X"}, "password": {"another-pass"}, "password_confirm": {"another-pass"}}); code != 410 {
		t.Fatalf("invite link worked twice: %d", code)
	}

	// Members install and update, but can't touch settings, machines or users.
	if code := member.call("POST", "/api/check", nil, nil); code != 200 {
		t.Fatalf("member check: %d", code)
	}
	for _, p := range []string{"/api/settings", "/api/machines", "/api/users/invite"} {
		if code := member.call("POST", p, map[string]string{}, nil); code != 403 {
			t.Fatalf("member POST %s: %d", p, code)
		}
	}
	if code := member.call("GET", "/api/users", nil, nil); code != 403 {
		t.Fatalf("member listed users: %d", code)
	}
	if body := member.page("/settings"); strings.Contains(body, "GitHub token") || !strings.Contains(body, "Member One") {
		t.Fatal("member sees admin settings")
	}

	// The viewer accepts their (un-emailed) link and can only look.
	viewer := newBrowser(t, srv.URL)
	vpath := strings.TrimPrefix(viewerLink, srv.URL)
	viewer.form(vpath, url.Values{"name": {"Vee"}, "password": {"viewer-pass-1"}, "password_confirm": {"viewer-pass-1"}})
	if body := viewer.page("/"); !strings.Contains(body, `data-role="viewer"`) {
		t.Fatal("viewer not signed in as a viewer")
	}
	for _, p := range []string{"/api/check", "/api/install", "/api/apps/update"} {
		if code := viewer.call("POST", p, map[string]any{}, nil); code != 403 {
			t.Fatalf("viewer POST %s: %d", p, code)
		}
	}
	if code := viewer.call("GET", "/api/apps", nil, nil); code != 200 {
		t.Fatalf("viewer can't look: %d", code)
	}
	// Everyone can change their own account.
	if code := viewer.call("POST", "/api/account", map[string]string{"Name": "Vee 2", "Email": "viewer@example.com", "Current": "viewer-pass-1"}, nil); code != 200 {
		t.Fatalf("viewer account: %d", code)
	}
	// ...but not take someone else's email.
	if code := viewer.call("POST", "/api/account", map[string]string{"Name": "Vee", "Email": "admin@example.com", "Current": "viewer-pass-1"}, nil); code != 422 {
		t.Fatalf("email clash accepted: %d", code)
	}

	// Roles change straight away; the last admin can't be demoted or removed.
	var list struct{ Users []userRow }
	admin.call("GET", "/api/users", nil, &list)
	ids := map[string]int64{}
	for _, u := range list.Users {
		ids[u.Email] = u.ID
	}
	if len(list.Users) != 3 {
		t.Fatalf("users: %+v", list.Users)
	}
	admin.call("POST", fmt.Sprintf("/api/users/%d/role", ids["member@example.com"]), map[string]string{"role": "viewer"}, nil)
	if code := member.call("POST", "/api/check", nil, nil); code != 403 {
		t.Fatalf("demoted member still acts: %d", code)
	}
	if code := admin.call("POST", fmt.Sprintf("/api/users/%d/role", ids["admin@example.com"]), map[string]string{"role": "viewer"}, nil); code != 409 {
		t.Fatalf("admin demoted themselves: %d", code)
	}
	if code := admin.call("POST", fmt.Sprintf("/api/users/%d/delete", ids["admin@example.com"]), nil, nil); code != 409 {
		t.Fatalf("admin removed themselves: %d", code)
	}
	// Removing someone signs them out.
	admin.call("POST", fmt.Sprintf("/api/users/%d/delete", ids["viewer@example.com"]), nil, nil)
	if code := viewer.call("GET", "/api/apps", nil, nil); code != 401 {
		t.Fatalf("removed user still signed in: %d", code)
	}

	// Forgotten password: the same answer whether or not the address
	// exists, an email only for a real account, and the old password
	// stops working.
	sent := mail.count()
	anon := newBrowser(t, srv.URL)
	if body := anon.page("/login"); !strings.Contains(body, "Forgot your password?") {
		t.Fatal("no reset link on the sign-in page")
	}
	_, nobody := anon.form("/forgot", url.Values{"email": {"nobody@example.com"}})
	_, somebody := anon.form("/forgot", url.Values{"email": {"member@example.com"}})
	if nobody != somebody {
		t.Fatal("the reset page tells who has an account")
	}
	waitFor(t, "reset email", func() bool { return mail.count() == sent+1 })
	rm := inviteLink.FindStringSubmatch(mail.last())
	if rm == nil || rm[1] != "reset" {
		t.Fatalf("reset email:\n%s", mail.last())
	}
	if code, _ := anon.form("/reset/"+rm[2], url.Values{"password": {"new-member-pass"}, "password_confirm": {"new-member-pass"}}); code != 200 {
		t.Fatalf("reset: %d", code)
	}
	if code, _ := newBrowser(t, srv.URL).form("/login", url.Values{"email": {"member@example.com"}, "password": {"member-pass-1"}}); code != 422 {
		t.Fatal("old password still works")
	}
	if code, body := newBrowser(t, srv.URL).form("/login", url.Values{"email": {"member@example.com"}, "password": {"new-member-pass"}}); code != 200 || !strings.Contains(body, "My Depot") {
		t.Fatalf("new password: %d", code)
	}
	if code := member.call("GET", "/api/apps", nil, nil); code != 401 {
		t.Fatal("a reset didn't sign out other sessions")
	}

	// An expired invite says so.
	admin.call("POST", "/api/users/invite", map[string]string{"email": "late@example.com", "role": "member"}, &inv)
	s.store.db.Exec(`UPDATE users SET invite_expires = '2000-01-01 00:00:00' WHERE email = 'late@example.com'`)
	if code, _ := newBrowser(t, srv.URL).form(strings.TrimPrefix(inv.Link, srv.URL), url.Values{"name": {"L"}, "password": {"late-pass-1"}, "password_confirm": {"late-pass-1"}}); code != 410 {
		t.Fatalf("expired invite: %d", code)
	}

	// The audit log saw it all.
	var ev struct{ Events []Event }
	admin.call("GET", "/api/events", nil, &ev)
	got := ""
	for _, e := range ev.Events {
		got += e.User + ": " + e.Action + " " + e.Detail + "\n"
	}
	for _, want := range []string{"Admin: Invited a user member@example.com as member", "Member One: Accepted an invite", "Admin: Changed a role", "Admin: Removed a user viewer@example.com", "Reset their password"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit log is missing %q:\n%s", want, got)
		}
	}

	// A test email goes to whoever asked.
	var te map[string]string
	if code := admin.call("POST", "/api/settings/test-email", nil, &te); code != 200 || te["to"] != "admin@example.com" {
		t.Fatalf("test email: %d %v", code, te)
	}
}

func TestUpdateControlsSkipHoldAndRules(t *testing.T) {
	s, _, b := testServer(t)
	gh := newFakeGitHub(t)
	s.settings.Set("github_api", gh.srv.URL)
	gh.releases = []string{"v1.1.0", "v1.0.0"}

	var made struct{ Machine Machine }
	b.call("POST", "/api/machines", map[string]any{"name": "Box", "host": "192.0.2.1", "port": 8471}, &made)
	var res struct{ Results []installResult }
	b.call("POST", "/api/install", map[string]any{"repo": "me/tool", "machines": []int64{made.Machine.ID}, "method": "track", "track": "release", "version": "v1.0.0", "notify": true}, &res)
	id := res.Results[0].AppID
	app := func() *App { a, _ := s.store.App(id); return a }
	check := func() { b.call("POST", "/api/check", nil, nil) }
	waitFor(t, "update found", func() bool { return app().UpdateAvailable() })

	// What's new: the notes of each release in between, cleaned.
	var ch struct {
		Releases []change     `json:"releases"`
		Commits  []commitLine `json:"commits"`
	}
	b.call("GET", fmt.Sprintf("/api/apps/%d/changes", id), nil, &ch)
	if len(ch.Releases) != 1 || ch.Releases[0].Title != "v1.1.0" || !strings.Contains(ch.Releases[0].HTML, "Notes for v1.1.0") || strings.Contains(ch.Releases[0].HTML, "script") {
		t.Fatalf("changes: %+v", ch)
	}
	var vs struct{ Versions []version }
	b.call("GET", fmt.Sprintf("/api/apps/%d/versions", id), nil, &vs)
	if len(vs.Versions) != 2 || vs.Versions[0].Note != "latest" || vs.Versions[1].Note != "installed" {
		t.Fatalf("versions: %+v", vs)
	}

	// Skip: gone until a newer version comes out.
	b.call("POST", fmt.Sprintf("/api/apps/%d/skip", id), nil, nil)
	if app().UpdateAvailable() || s.updateCount() != 0 {
		t.Fatal("skipped version still offered")
	}
	gh.mu.Lock()
	gh.releases = []string{"v1.2.0", "v1.1.0", "v1.0.0"}
	gh.mu.Unlock()
	check()
	if a := app(); !a.UpdateAvailable() || a.LatestRef != "v1.2.0" {
		t.Fatalf("the next version wasn't offered: %+v", a)
	}

	// Ignore versions matching a pattern: v1.2.0 is passed over for v1.1.0.
	set := func(extra map[string]any) {
		body := map[string]any{"track": "release", "notify": true, "options": map[string]any{}}
		for k, v := range extra {
			body[k] = v
		}
		if code := b.call("POST", fmt.Sprintf("/api/apps/%d/settings", id), body, nil); code != 200 {
			t.Fatalf("app settings %v: %d", extra, code)
		}
		waitFor(t, "re-check", func() bool { return app().LatestRef != "" && !s.checker.running.Load() })
	}
	set(map[string]any{"options": map[string]any{"ignore_versions": `1\.2`}})
	if a := app(); a.LatestRef != "v1.1.0" {
		t.Fatalf("ignore pattern: latest %s", a.LatestRef)
	}
	if code := b.call("POST", fmt.Sprintf("/api/apps/%d/settings", id), map[string]any{"track": "release", "options": map[string]any{"ignore_versions": "(unclosed"}}, nil); code != 422 {
		t.Fatalf("bad pattern accepted: %d", code)
	}

	// Pre-releases: off by default, on per app.
	gh.mu.Lock()
	gh.releases = []string{"v2.0.0-rc1", "v1.2.0", "v1.1.0", "v1.0.0"}
	gh.pre["v2.0.0-rc1"] = true
	gh.mu.Unlock()
	set(nil)
	if a := app(); a.LatestRef != "v1.2.0" {
		t.Fatalf("pre-release offered by default: %s", a.LatestRef)
	}
	set(map[string]any{"options": map[string]any{"prerelease": "yes"}})
	if a := app(); a.LatestRef != "v2.0.0-rc1" {
		t.Fatalf("pre-release not offered when asked: %s", a.LatestRef)
	}

	// Hold: nothing offered at all.
	set(map[string]any{"held": true})
	if a := app(); a.UpdateAvailable() || s.updateCount() != 0 || !a.Held {
		t.Fatalf("held app still offers updates: %+v", a)
	}
	set(map[string]any{"held": false})
	if !app().UpdateAvailable() {
		t.Fatal("un-held app offers nothing")
	}
}

func TestCommitTrackingShowsCommitsBetween(t *testing.T) {
	s, _, b := testServer(t)
	var made struct{ Machine Machine }
	b.call("POST", "/api/machines", map[string]any{"name": "Box", "host": "192.0.2.1", "port": 8471}, &made)
	var res struct{ Results []installResult }
	b.call("POST", "/api/install", map[string]any{"repo": "me/tool", "machines": []int64{made.Machine.ID}, "method": "track", "track": "commit", "version": "0000000aaaa"}, &res)
	id := res.Results[0].AppID
	waitFor(t, "update found", func() bool { a, _ := s.store.App(id); return a.UpdateAvailable() })
	var ch struct {
		Commits []commitLine `json:"commits"`
	}
	b.call("GET", fmt.Sprintf("/api/apps/%d/changes", id), nil, &ch)
	if len(ch.Commits) != 2 || ch.Commits[0].Message != "Newer change" || ch.Commits[1].Message != "Older change" || ch.Commits[0].Author != "Bob" {
		t.Fatalf("commits: %+v", ch.Commits)
	}
}

func TestChecksumFor(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	cases := []struct {
		text, name string
		own        bool
		want       string
	}{
		{sum + "  tool-linux.tar.gz\n" + other + "  tool-windows.zip\n", "tool-linux.tar.gz", false, sum},
		{other + " *./dist/tool-windows.zip\n", "tool-windows.zip", false, other},
		{strings.ToUpper(sum) + "  tool.deb", "tool.deb", false, sum},
		{sum + "  something-else\n", "tool.deb", false, ""},
		{sum + "\n", "tool.deb", true, sum},
		{"not a checksum file", "tool.deb", true, ""},
	}
	for _, c := range cases {
		if got := checksumFor(c.text, c.name, c.own); got != c.want {
			t.Errorf("checksumFor(%q, %q) = %q, want %q", c.text, c.name, got, c.want)
		}
	}
	// GitHub's own digest wins and needs no download.
	s := &Server{}
	if got := s.assetChecksum(context.Background(), &Release{}, &Asset{Name: "x", Digest: "sha256:" + sum}, false); got != sum {
		t.Errorf("digest: %q", got)
	}
}

func TestAutoUpdateWindow(t *testing.T) {
	s, _, _ := testServer(t)
	at := func(h int) time.Time { return time.Date(2026, 1, 1, h, 30, 0, 0, time.Local) }
	if !s.autoUpdateWindowOpen(at(14)) {
		t.Fatal("no window set, but closed")
	}
	s.settings.Set("auto_update_window", "true")
	s.settings.Set("auto_update_start", "2")
	s.settings.Set("auto_update_end", "6")
	for h, want := range map[int]bool{1: false, 2: true, 5: true, 6: false, 14: false} {
		if got := s.autoUpdateWindowOpen(at(h)); got != want {
			t.Errorf("2–6 at %d:30: %v", h, got)
		}
	}
	// Hours are in the chosen time zone, not the server's.
	s.settings.Set("timezone", "Asia/Tokyo") // UTC+9, no daylight saving
	utc := func(h int) time.Time { return time.Date(2026, 1, 1, h, 30, 0, 0, time.UTC) }
	if !s.autoUpdateWindowOpen(utc(18)) || s.autoUpdateWindowOpen(utc(2)) { // 03:30 and 11:30 in Tokyo
		t.Error("the window ignores the time zone setting")
	}
	if _, msg := settingIndex["timezone"].validate("Mars/Olympus"); msg == "" {
		t.Error("a made-up time zone was accepted")
	}
	if v, msg := settingIndex["timezone"].validate("America/New_York"); msg != "" || v != "America/New_York" {
		t.Errorf("a real time zone was refused: %s", msg)
	}
	s.settings.Set("timezone", "")
	s.settings.Set("auto_update_start", "22")
	s.settings.Set("auto_update_end", "5")
	for h, want := range map[int]bool{21: false, 22: true, 23: true, 0: true, 4: true, 5: false} {
		if got := s.autoUpdateWindowOpen(at(h)); got != want {
			t.Errorf("22–5 at %d:30: %v", h, got)
		}
	}
}

func TestWebhookFormats(t *testing.T) {
	s, _, b := testServer(t)
	var mu sync.Mutex
	var got []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer hook.Close()
	if code := b.call("POST", "/api/settings/test-webhook", nil, nil); code != 422 {
		t.Fatalf("test without a webhook: %d", code)
	}
	b.call("POST", "/api/settings", map[string]string{"webhook_url": hook.URL, "webhook_format": "discord"}, nil)
	if code := b.call("POST", "/api/settings/test-webhook", nil, nil); code != 200 {
		t.Fatalf("test webhook: %d", code)
	}
	s.settings.Set("webhook_format", "json")
	s.notify("Update for tool", "v1 → v2", "package", "/apps/1", nil)
	waitFor(t, "webhooks", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if emb, ok := got[0]["embeds"].([]any); !ok || len(emb) != 1 {
		t.Fatalf("discord payload: %v", got[0])
	}
	if got[1]["title"] != "Update for tool" || !strings.HasSuffix(fmt.Sprint(got[1]["link"]), "/apps/1") || got[1]["event"] != "package" {
		t.Fatalf("json payload: %v", got[1])
	}
}

func TestBackups(t *testing.T) {
	s, srv, b := testServer(t)
	resp, err := b.c.Get(srv.URL + "/api/backup")
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 16)
	io.ReadFull(resp.Body, head)
	resp.Body.Close()
	if string(head[:15]) != "SQLite format 3" || !strings.Contains(resp.Header.Get("Content-Disposition"), "repodepot-") {
		t.Fatalf("backup download: %q %v", head, resp.Header)
	}
	// Daily copies: one per day, the oldest pruned past the limit.
	dir := s.backupDir()
	for _, d := range []string{"2026-01-01", "2026-01-02", "2026-01-03"} {
		os.WriteFile(filepath.Join(dir, "repodepot-"+d+".db"), []byte("old"), 0o600)
	}
	s.settings.Set("backup_keep", "2")
	s.dailyBackup()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	today := "repodepot-" + time.Now().Format("2006-01-02") + ".db"
	if len(names) != 2 || names[1] != today || names[0] != "repodepot-2026-01-03.db" {
		t.Fatalf("daily backups: %v", names)
	}
	// Only real backup names can be downloaded.
	for _, bad := range []string{"../repodepot.db", "repodepot.db", "repodepot-x/../../etc.db"} {
		r, _ := b.c.Get(srv.URL + "/api/backup?file=" + url.QueryEscape(bad))
		r.Body.Close()
		if r.StatusCode != 404 {
			t.Errorf("backup ?file=%s: %d", bad, r.StatusCode)
		}
	}
	if body := b.page("/settings?tab=backups"); !strings.Contains(body, today) {
		t.Error("the backup isn't listed in Settings")
	}
}
