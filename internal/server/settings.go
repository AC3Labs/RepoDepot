package server

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Setting describes one entry on the Settings page. The page is built from
// this list, so every setting the server reads is editable there.
type Setting struct {
	Key     string
	Section string
	Label   string
	Help    string
	Type    string // text, url, secret, bool, int, select, textarea
	Default string
	Options [][2]string // select: value, label
	Min     int
	Max     int
}

var settingSections = [][2]string{
	{"general", "General"},
	{"github", "GitHub"},
	{"updates", "Updates"},
	{"installs", "Install defaults"},
	{"notifications", "Notifications"},
	{"email", "Email"},
	{"security", "Security"},
	{"backups", "Backups"},
	{"advanced", "Advanced"},
}

var settingDefs = []Setting{
	{Key: "site_name", Section: "general", Label: "Name", Help: "Shown in the sidebar, page titles and notifications.", Type: "text", Default: "The Repo Depot"},
	{Key: "public_url", Section: "general", Label: "Public address", Help: "How you reach this app, e.g. http://192.168.0.70:8470 or https://depot.example.com. Used in notification links. Empty: the address you're using now.", Type: "url"},
	{Key: "timezone", Section: "general", Label: "Time zone", Help: "For auto-update hours and backup names, e.g. America/New_York or Europe/London. Empty: the server's own (often UTC in Docker).", Type: "timezone"},
	{Key: "theme", Section: "general", Label: "Theme", Type: "select", Default: "dark", Options: [][2]string{{"dark", "Dark"}, {"light", "Light"}, {"system", "Match my device"}}},
	{Key: "density", Section: "general", Label: "Layout density", Type: "select", Default: "comfortable", Options: [][2]string{{"comfortable", "Comfortable"}, {"compact", "Compact"}}},
	{Key: "depot_view", Section: "general", Label: "My Depot view", Type: "select", Default: "grid", Options: [][2]string{{"grid", "Cards"}, {"list", "List"}}},

	{Key: "github_token", Section: "github", Label: "GitHub token", Help: "Optional but recommended: raises GitHub's limit from 60 to 5,000 requests an hour and allows private repos. A fine-grained token with read-only access to public repositories is enough.", Type: "secret"},
	{Key: "github_api", Section: "github", Label: "API address", Help: "Change only for GitHub Enterprise.", Type: "url", Default: "https://api.github.com"},
	{Key: "search_per_page", Section: "github", Label: "Search results per page", Type: "int", Default: "30", Min: 10, Max: 100},
	{Key: "cache_search_minutes", Section: "github", Label: "Keep search results for (minutes)", Help: "Repeated searches are instant and cost no API requests.", Type: "int", Default: "10", Min: 0, Max: 1440},
	{Key: "cache_repo_minutes", Section: "github", Label: "Keep repo details for (minutes)", Type: "int", Default: "30", Min: 0, Max: 1440},
	{Key: "readme_images", Section: "github", Label: "Show images in READMEs", Type: "bool", Default: "true"},

	{Key: "check_enabled", Section: "updates", Label: "Check for updates automatically", Type: "bool", Default: "true"},
	{Key: "check_interval_minutes", Section: "updates", Label: "Check every (minutes)", Help: "Unchanged repos cost nothing against GitHub's limit (cached answers), so short intervals are fine.", Type: "int", Default: "60", Min: 5, Max: 10080},
	{Key: "include_prereleases", Section: "updates", Label: "Offer pre-releases", Help: "When tracking releases, also offer betas and release candidates.", Type: "bool", Default: "false"},
	{Key: "auto_update_default", Section: "updates", Label: "Auto-update new installs", Help: "Install updates as soon as they're found. Can be changed per app.", Type: "bool", Default: "false"},
	{Key: "auto_update_window", Section: "updates", Label: "Only auto-update at set hours", Help: "Automatic updates wait for the hours below (in the time zone from General). Updates you start yourself run straight away.", Type: "bool", Default: "false"},
	{Key: "auto_update_start", Section: "updates", Label: "Auto-update from (hour, 0–23)", Type: "int", Default: "2", Min: 0, Max: 23},
	{Key: "auto_update_end", Section: "updates", Label: "Auto-update until (hour, 0–23)", Help: "The window can pass midnight, e.g. 22 to 5.", Type: "int", Default: "6", Min: 0, Max: 23},
	{Key: "max_parallel_jobs", Section: "updates", Label: "Jobs at once", Help: "How many installs or updates run at the same time.", Type: "int", Default: "3", Min: 1, Max: 20},

	{Key: "default_track", Section: "installs", Label: "Track", Help: "What counts as an update for new installs. Releases are official versions; commits are every change pushed to the branch.", Type: "select", Default: "auto", Options: [][2]string{{"auto", "Releases when the repo has them, otherwise commits"}, {"release", "Releases"}, {"tag", "Tags"}, {"commit", "Commits"}}},
	{Key: "default_dir_linux", Section: "installs", Label: "Install folder (Linux)", Help: "Apps go in a subfolder here. Empty: the agent's default (/opt/repodepot/apps).", Type: "text"},
	{Key: "default_dir_windows", Section: "installs", Label: "Install folder (Windows)", Help: "Empty: the agent's default (C:\\RepoDepot\\apps).", Type: "text"},
	{Key: "link_binaries", Section: "installs", Label: "Put command-line programs on the PATH", Help: "Linux: a link in /usr/local/bin for single-program releases.", Type: "bool", Default: "true"},
	{Key: "prefer_method", Section: "installs", Label: "Preferred method", Help: "When a repo can be installed more than one way.", Type: "select", Default: "auto", Options: [][2]string{{"auto", "Best match"}, {"compose", "Docker Compose"}, {"release", "Release download"}, {"source", "Source"}}},

	{Key: "ntfy_enabled", Section: "notifications", Label: "Send ntfy notifications", Type: "bool", Default: "false"},
	{Key: "ntfy_server", Section: "notifications", Label: "ntfy server", Type: "url", Default: "https://ntfy.sh"},
	{Key: "ntfy_topic", Section: "notifications", Label: "Topic", Help: "Subscribe to this topic in the ntfy app. Anyone who knows the name can read it on ntfy.sh, so keep it hard to guess.", Type: "text"},
	{Key: "ntfy_token", Section: "notifications", Label: "Access token", Help: "For a protected topic or your own ntfy server.", Type: "secret"},
	{Key: "ntfy_priority", Section: "notifications", Label: "Priority", Type: "select", Default: "3", Options: [][2]string{{"1", "Min"}, {"2", "Low"}, {"3", "Default"}, {"4", "High"}, {"5", "Urgent"}}},
	{Key: "notify_updates", Section: "notifications", Label: "When an update is available", Type: "bool", Default: "true"},
	{Key: "notify_done", Section: "notifications", Label: "When an install or update finishes", Type: "bool", Default: "false"},
	{Key: "notify_failures", Section: "notifications", Label: "When something fails", Type: "bool", Default: "true"},
	{Key: "notify_machines", Section: "notifications", Label: "When a machine goes offline or comes back", Type: "bool", Default: "false"},
	{Key: "email_notify", Section: "notifications", Label: "Also send notifications by email", Help: "Uses the mail server in Settings → Email.", Type: "bool", Default: "false"},
	{Key: "email_to", Section: "notifications", Label: "Email them to", Help: "Addresses separated by commas. Empty: every admin.", Type: "text"},
	{Key: "webhook_url", Section: "notifications", Label: "Webhook", Help: "Also post notifications to this address: Discord, Slack, or your own service.", Type: "url"},
	{Key: "webhook_format", Section: "notifications", Label: "Webhook format", Type: "select", Default: "json", Options: [][2]string{{"json", "JSON (title, message, link, event)"}, {"discord", "Discord"}, {"slack", "Slack"}}},
	{Key: "one_tap_updates", Section: "notifications", Label: "\"Update now\" button in notifications", Help: "Lets the notification start the update without signing in. The button only works for that one update and expires after 7 days. Your phone must be able to reach the public address.", Type: "bool", Default: "true"},

	{Key: "smtp_host", Section: "email", Label: "Mail server", Help: "Used for invites, password resets and (if turned on) notifications. Empty: no email.", Type: "text"},
	{Key: "smtp_port", Section: "email", Label: "Port", Type: "int", Default: "587", Min: 1, Max: 65535},
	{Key: "smtp_security", Section: "email", Label: "Encryption", Type: "select", Default: "starttls", Options: [][2]string{{"starttls", "STARTTLS (usually port 587)"}, {"ssl", "SSL/TLS (usually port 465)"}, {"none", "None (port 25, trusted networks only)"}}},
	{Key: "smtp_username", Section: "email", Label: "Username", Type: "text"},
	{Key: "smtp_password", Section: "email", Label: "Password", Type: "secret"},
	{Key: "smtp_from", Section: "email", Label: "From address", Help: "Empty: the username.", Type: "text"},
	{Key: "smtp_from_name", Section: "email", Label: "From name", Help: "Empty: the app's name.", Type: "text"},

	{Key: "session_days", Section: "security", Label: "Stay signed in for (days)", Type: "int", Default: "30", Min: 1, Max: 365},
	{Key: "login_attempts", Section: "security", Label: "Wrong passwords before a 15-minute lockout", Type: "int", Default: "5", Min: 3, Max: 50},
	{Key: "invite_days", Section: "security", Label: "Invite links work for (days)", Type: "int", Default: "7", Min: 1, Max: 60},
	{Key: "password_reset", Section: "security", Label: "\"Forgot password\" by email", Help: "Shows a reset link on the sign-in page when a mail server is set up. The emailed link works for one hour.", Type: "bool", Default: "true"},

	{Key: "backup_enabled", Section: "backups", Label: "Back up the database every day", Help: "Copies go in the data folder's backups/ folder. They include your settings and tokens, so keep the data folder private.", Type: "bool", Default: "true"},
	{Key: "backup_keep", Section: "backups", Label: "Daily copies to keep", Type: "int", Default: "7", Min: 1, Max: 365},

	{Key: "agent_timeout_seconds", Section: "advanced", Label: "Agent connect timeout (seconds)", Type: "int", Default: "4", Min: 1, Max: 60},
	{Key: "agent_default_port", Section: "advanced", Label: "Agent port for new machines", Type: "int", Default: "8471", Min: 1, Max: 65535},
	{Key: "machine_check_minutes", Section: "advanced", Label: "Check machines every (minutes)", Type: "int", Default: "2", Min: 1, Max: 1440},
	{Key: "job_log_days", Section: "advanced", Label: "Keep job logs for (days)", Type: "int", Default: "60", Min: 1, Max: 3650},
}

var settingIndex = func() map[string]*Setting {
	m := map[string]*Setting{}
	for i := range settingDefs {
		m[settingDefs[i].Key] = &settingDefs[i]
	}
	return m
}()

// Settings caches the settings table; reads are on every request, writes
// are rare.
type Settings struct {
	mu    sync.RWMutex
	store *Store
	vals  map[string]string
}

func loadSettings(st *Store) (*Settings, error) {
	s := &Settings{store: st, vals: map[string]string{}}
	rows, err := st.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		s.vals[k] = v
	}
	// Secrets the app generates for itself on first start.
	for _, k := range []string{"hook_secret"} {
		if s.vals[k] == "" {
			s.Set(k, randomHex(32))
		}
	}
	if _, ok := s.vals["ntfy_topic"]; !ok {
		s.Set("ntfy_topic", "repodepot-"+randomHex(6))
	}
	return s, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Settings) Get(key string) string {
	s.mu.RLock()
	v, ok := s.vals[key]
	s.mu.RUnlock()
	if ok {
		return v
	}
	if d := settingIndex[key]; d != nil {
		return d.Default
	}
	return ""
}

func (s *Settings) Bool(key string) bool { return s.Get(key) == "true" }

// Location is the time zone from Settings → General, else the server's.
func (s *Settings) Location() *time.Location {
	if tz := s.Get("timezone"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

func (s *Settings) Int(key string) int {
	n, err := strconv.Atoi(s.Get(key))
	if err != nil {
		n, _ = strconv.Atoi(settingIndex[key].Default)
	}
	return n
}

func (s *Settings) Set(key, value string) error {
	if _, err := s.store.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return err
	}
	s.mu.Lock()
	s.vals[key] = value
	s.mu.Unlock()
	return nil
}

// validate cleans a submitted value; ok is false when it can't be used.
func (d *Setting) validate(v string) (string, string) {
	v = strings.TrimSpace(v)
	switch d.Type {
	case "bool":
		if v == "true" || v == "on" || v == "1" {
			return "true", ""
		}
		return "false", ""
	case "int":
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", d.Label + " must be a whole number."
		}
		if (d.Min != 0 || d.Max != 0) && (n < d.Min || n > d.Max) {
			return "", d.Label + " must be between " + strconv.Itoa(d.Min) + " and " + strconv.Itoa(d.Max) + "."
		}
		return strconv.Itoa(n), ""
	case "select":
		for _, o := range d.Options {
			if o[0] == v {
				return v, ""
			}
		}
		return "", d.Label + ": pick one of the options."
	case "timezone":
		if v != "" {
			if _, err := time.LoadLocation(v); err != nil {
				return "", d.Label + ": \"" + v + "\" isn't a time zone name. Examples: America/New_York, Europe/London, UTC."
			}
		}
		return v, ""
	case "url":
		if v != "" && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return "", d.Label + " must start with http:// or https://."
		}
		return strings.TrimRight(v, "/"), ""
	}
	return v, ""
}
