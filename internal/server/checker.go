package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"repodepot/internal/spec"
)

// ---- update checks ----

type Checker struct {
	running  atomic.Bool
	lastRun  atomic.Int64 // unix seconds
	lastDone atomic.Int64
}

// scheduler runs update checks, machine checks and clean-up on their
// intervals from Settings.
func (s *Server) scheduler(ctx context.Context) {
	time.Sleep(3 * time.Second)
	s.checkMachines(ctx)
	if s.settings.Bool("check_enabled") {
		s.checkUpdates(ctx)
	}
	lastMachines := time.Now()
	lastClean := time.Time{}
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.settings.Bool("check_enabled") {
			every := time.Duration(s.settings.Int("check_interval_minutes")) * time.Minute
			if time.Since(time.Unix(s.checker.lastRun.Load(), 0)) >= every {
				s.checkUpdates(ctx)
			}
		}
		if time.Since(lastMachines) >= time.Duration(s.settings.Int("machine_check_minutes"))*time.Minute {
			lastMachines = time.Now()
			s.checkMachines(ctx)
		}
		if time.Since(lastClean) > 6*time.Hour {
			lastClean = time.Now()
			cutoff := time.Now().UTC().Add(-time.Duration(s.settings.Int("job_log_days")) * 24 * time.Hour).Format(dbTime)
			s.store.db.Exec(`DELETE FROM jobs WHERE status != 'running' AND created_at < ?`, cutoff)
			s.store.db.Exec(`DELETE FROM http_cache WHERE fetched_at < ?`, time.Now().UTC().Add(-30*24*time.Hour).Format(dbTime))
			s.store.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, nowDB())
		}
	}
}

func (s *Server) checkMachines(ctx context.Context) {
	machines, _ := s.store.Machines()
	var wg sync.WaitGroup
	for _, m := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			s.refreshMachine(c, m)
		}()
	}
	wg.Wait()
}

// checkUpdates looks up the latest version of everything installed. One
// lookup per repo+tracking, six at a time; unchanged answers are free.
func (s *Server) checkUpdates(ctx context.Context) int {
	if !s.checker.running.CompareAndSwap(false, true) {
		return 0
	}
	defer s.checker.running.Store(false)
	s.checker.lastRun.Store(time.Now().Unix())
	defer func() { s.checker.lastDone.Store(time.Now().Unix()) }()

	apps, err := s.store.Apps()
	if err != nil {
		return 0
	}
	groups := map[string][]*App{}
	for _, a := range apps {
		if a.InstalledRef == "" {
			continue
		}
		k := a.Repo + "|" + a.Track + "|" + a.Branch
		groups[k] = append(groups[k], a)
	}
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	var found atomic.Int32
	for _, list := range groups {
		wg.Add(1)
		sem <- struct{}{}
		go func(list []*App) {
			defer wg.Done()
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			first := list[0]
			repo, err := s.gh.Repo(c, first.Repo, s.gh.minutes("cache_repo_minutes"))
			var t *Target
			if err == nil {
				t, err = s.latestTarget(c, repo, first.Track, first.Branch, 0)
			}
			for _, a := range list {
				if err != nil {
					s.store.db.Exec(`UPDATE apps SET checked_at = ?, check_error = ? WHERE id = ?`, nowDB(), err.Error(), a.ID)
					continue
				}
				s.store.db.Exec(`UPDATE apps SET latest_ref = ?, latest_label = ?, latest_url = ?, latest_date = ?, checked_at = ?, check_error = '' WHERE id = ?`,
					t.Ref, t.Label, t.URL, t.Date, nowDB(), a.ID)
				a.LatestRef, a.LatestLabel = t.Ref, t.Label
				if !a.UpdateAvailable() {
					continue
				}
				found.Add(1)
				if a.AutoUpdate && a.ActiveJob == 0 && a.MachineStatus != "offline" {
					if _, err := s.startJob(context.Background(), a, spec.ActionUpdate, t.Ref, false); err != nil {
						log.Printf("auto-update of %s on %s: %v", a.Repo, a.MachineName, err)
					}
					continue
				}
				if a.Notify && a.NotifiedRef != t.Ref && s.settings.Bool("notify_updates") {
					s.store.db.Exec(`UPDATE apps SET notified_ref = ? WHERE id = ?`, t.Ref, a.ID)
					s.notifyUpdate(a, t)
				}
			}
		}(list)
	}
	wg.Wait()
	return int(found.Load())
}

// ---- ntfy ----

type ntfyAction struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	URL    string `json:"url"`
	Method string `json:"method,omitempty"`
	Clear  bool   `json:"clear,omitempty"`
}

func (s *Server) publicURL() string {
	if u := s.settings.Get("public_url"); u != "" {
		return u
	}
	if h, ok := s.seenBase.Load().(string); ok && h != "" {
		return h
	}
	return "http://localhost" + s.cfg.Listen
}

// notify sends an ntfy message when notifications are on. path is a page
// in this app the notification opens.
func (s *Server) notify(title, message, tags, path string, actions []ntfyAction) {
	if !s.settings.Bool("ntfy_enabled") || s.settings.Get("ntfy_topic") == "" {
		return
	}
	go func() {
		if err := s.sendNtfy(title, message, tags, path, actions); err != nil {
			log.Printf("ntfy: %v", err)
		}
	}()
}

func (s *Server) sendNtfy(title, message, tags, path string, actions []ntfyAction) error {
	prio, _ := strconv.Atoi(s.settings.Get("ntfy_priority"))
	msg := map[string]any{
		"topic": s.settings.Get("ntfy_topic"), "title": title, "message": message, "priority": prio,
	}
	if tags != "" {
		msg["tags"] = strings.Split(tags, ",")
	}
	if path != "" {
		msg["click"] = s.publicURL() + path
	}
	if len(actions) > 0 {
		msg["actions"] = actions
	}
	raw, _ := json.Marshal(msg)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(s.settings.Get("ntfy_server"), "/"), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t := s.settings.Get("ntfy_token"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy answered %s", resp.Status)
	}
	return nil
}

func (s *Server) notifyUpdate(a *App, t *Target) {
	from := a.InstalledLabel
	if from == "" {
		from = shortSHA(a.InstalledRef)
	}
	title := fmt.Sprintf("Update for %s", a.Name())
	body := fmt.Sprintf("%s → %s on %s", from, t.Label, a.MachineName)
	page := fmt.Sprintf("/apps/%d", a.ID)
	actions := []ntfyAction{{Action: "view", Label: "Open", URL: s.publicURL() + page}}
	if s.settings.Bool("one_tap_updates") {
		actions = append(actions, ntfyAction{Action: "http", Label: "Update now", URL: s.hookURL(a.ID, t.Ref), Method: "POST", Clear: true})
	}
	if t.URL != "" {
		actions = append(actions, ntfyAction{Action: "view", Label: "What's new", URL: t.URL})
	}
	s.notify(title, body, "package", page, actions)
}

// hookURL is a one-update link: it can only start the update of this app
// to this version, and expires after 7 days.
func (s *Server) hookURL(appID int64, ref string) string {
	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	return fmt.Sprintf("%s/hook/update?app=%d&ref=%s&exp=%d&sig=%s", s.publicURL(), appID, urlQuery(ref), exp, s.hookSig(appID, ref, exp))
}

func (s *Server) hookSig(appID int64, ref string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(s.settings.Get("hook_secret")))
	fmt.Fprintf(mac, "%d|%s|%d", appID, ref, exp)
	return hex.EncodeToString(mac.Sum(nil))
}
