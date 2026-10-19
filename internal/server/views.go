package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"
)

type Page struct {
	Title   string
	Active  string
	User    *User
	CSRF    string
	Site    string
	Theme   string
	Density string
	Version string
	Updates int
	Data    any
}

var funcs = template.FuncMap{
	"ago":      ago,
	"json":     func(v any) template.JS { b, _ := json.Marshal(v); return template.JS(b) },
	"short":    shortSHA,
	"lower":    strings.ToLower,
	"num":      humanNum,
	"eq2":      func(a, b, c string) bool { return a == b || a == c },
	"title":    titleWord,
	"contains": strings.Contains,
}

func ago(v string) string {
	if v == "" {
		return "never"
	}
	t, err := time.Parse(dbTime, v)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, v); err != nil {
			return v
		}
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month") + " ago"
	}
	return plural(int(d.Hours()/24/365), "year") + " ago"
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func humanNum(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

var pages = []string{"depot", "discover", "repo", "app", "machines", "activity", "settings", "login", "setup"}

func (s *Server) loadTemplates() error {
	s.tmpl = map[string]*template.Template{}
	for _, p := range pages {
		t, err := template.New("").Funcs(funcs).ParseFS(webFS, "web/templates/layout.html", "web/templates/partials.html", "web/templates/"+p+".html")
		if err != nil {
			return fmt.Errorf("template %s: %w", p, err)
		}
		s.tmpl[p] = t
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, title, active string, data any) {
	p := Page{Title: title, Active: active, User: userFrom(r), Site: s.settings.Get("site_name"),
		Theme: s.settings.Get("theme"), Density: s.settings.Get("density"), Version: Version, Data: data}
	if se := sessFrom(r); se != nil {
		p.CSRF = se.CSRF
	}
	if p.User != nil {
		p.Updates = s.updateCount()
	}
	layout := "layout"
	if page == "login" || page == "setup" {
		layout = "guest"
	}
	var buf bytes.Buffer
	if err := s.tmpl[page].ExecuteTemplate(&buf, layout, p); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "Something went wrong showing this page.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) updateCount() int {
	var n int
	s.store.db.QueryRow(`SELECT count(*) FROM apps WHERE latest_ref != '' AND installed_ref != '' AND latest_ref != installed_ref AND status IN ('installed', 'error')`).Scan(&n)
	return n
}

// staticFS lists the embedded assets (used by tests).
func staticFS() fs.FS {
	sub, _ := fs.Sub(webFS, "web/static")
	return sub
}
