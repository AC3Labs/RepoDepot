package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// ---- audit log ----

// event records who changed what (users, machines, settings) for the
// Activity page. Jobs carry their own "started by".
func (s *Server) event(r *http.Request, action, detail string) {
	who := "System"
	if u := userFrom(r); u != nil {
		who = u.Name
	}
	s.store.db.Exec(`INSERT INTO events (at, user_label, action, detail) VALUES (?, ?, ?, ?)`, nowDB(), who, action, detail)
}

type Event struct {
	ID     int64  `json:"id"`
	At     string `json:"at"`
	User   string `json:"user"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

func (s *Server) events(limit int) []Event {
	out := []Event{}
	rows, err := s.store.db.Query(`SELECT id, at, user_label, action, detail FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		rows.Scan(&e.ID, &e.At, &e.User, &e.Action, &e.Detail)
		out = append(out, e)
	}
	return out
}

// ---- users ----

type userRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Pending   bool   `json:"pending"`
	Expired   bool   `json:"expired"`
	InvitedBy string `json:"invited_by"`
	LastLogin string `json:"last_login"`
	Created   string `json:"created"`
}

func (s *Server) userRows() []userRow {
	out := []userRow{}
	rows, err := s.store.db.Query(`SELECT id, name, email, role, invite_hash != '', invite_expires, invited_by, last_login, created_at FROM users ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var u userRow
		var exp string
		rows.Scan(&u.ID, &u.Name, &u.Email, &u.Role, &u.Pending, &exp, &u.InvitedBy, &u.LastLogin, &u.Created)
		u.Expired = u.Pending && time.Now().After(parseDB(exp))
		if u.LastLogin != "" {
			u.LastLogin = ago(u.LastLogin)
		}
		u.Created = ago(u.Created)
		out = append(out, u)
	}
	return out
}

func validRole(r string) bool { return r == RoleAdmin || r == RoleMember || r == RoleViewer }

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"users": s.userRows(), "mail": s.mailConfigured()})
}

// apiInvite creates a pending account and emails a link to finish it.
// The link is also returned, for sharing another way when there's no
// mail server (or the email fails).
func (s *Server) apiInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonError(w, 400, "Invalid request.")
		return
	}
	body.Email, body.Name = strings.ToLower(strings.TrimSpace(body.Email)), strings.TrimSpace(body.Name)
	if a, err := mail.ParseAddress(body.Email); err != nil || a.Address != body.Email || len(body.Email) > 254 {
		jsonError(w, 422, "Enter a valid email address.")
		return
	}
	if !validRole(body.Role) {
		jsonError(w, 422, "Pick a role.")
		return
	}
	var n int
	s.store.db.QueryRow(`SELECT count(*) FROM users WHERE email = ?`, body.Email).Scan(&n)
	if n > 0 {
		jsonError(w, 422, "Someone with that email already has an account or an invite.")
		return
	}
	name := body.Name
	if name == "" {
		name, _, _ = strings.Cut(body.Email, "@")
	}
	if len(name) > 100 {
		name = name[:100]
	}
	id, err := s.store.exec(`INSERT INTO users (name, email, password, role, invited_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		name, body.Email, hashPassword(randomHex(24)), body.Role, userFrom(r).Name, nowDB())
	if err != nil {
		jsonError(w, 500, "Couldn't create the invite: "+err.Error())
		return
	}
	link, emailErr := s.sendInvite(id, body.Email, userFrom(r).Name)
	s.event(r, "Invited a user", body.Email+" as "+body.Role)
	writeJSON(w, 200, s.inviteResult(link, emailErr))
}

func (s *Server) inviteResult(link string, emailErr error) map[string]any {
	res := map[string]any{"link": link, "emailed": emailErr == nil}
	if emailErr != nil {
		res["email_error"] = emailErr.Error()
	}
	return res
}

// sendInvite makes a fresh invite link (replacing any earlier one) and
// emails it. The link is returned either way.
func (s *Server) sendInvite(userID int64, email, from string) (string, error) {
	tok := randomHex(32)
	days := s.settings.Int("invite_days")
	s.store.db.Exec(`UPDATE users SET invite_hash = ?, invite_expires = ? WHERE id = ?`,
		hashToken(tok), time.Now().UTC().Add(time.Duration(days)*24*time.Hour).Format(dbTime), userID)
	link := s.publicURL() + "/invite/" + tok
	if !s.mailConfigured() {
		return link, errNoMail
	}
	site := s.settings.Get("site_name")
	subject := fmt.Sprintf("%s invited you to %s", from, site)
	text := fmt.Sprintf("%s invited you to %s, where you can find, install and update software from GitHub on your machines.\n\nChoose a password to get started (the link works for %d days):\n\n%s\n", from, site, days, link)
	htmlBody := s.mailHTML("You're invited", []string{
		from + " invited you to " + site + ", where you can find, install and update software from GitHub on your machines.",
		fmt.Sprintf("Choose a password to get started. The link works for %d days.", days),
	}, "Accept the invite", link)
	err := s.sendMail(email, subject, text, htmlBody)
	if err != nil {
		log.Printf("invite email to %s failed: %v", email, err)
	}
	return link, err
}

func (s *Server) userFromPath(w http.ResponseWriter, r *http.Request) *userRow {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	for _, u := range s.userRows() {
		if u.ID == id {
			return &u
		}
	}
	jsonError(w, 404, "No such user.")
	return nil
}

func (s *Server) apiResendInvite(w http.ResponseWriter, r *http.Request) {
	u := s.userFromPath(w, r)
	if u == nil {
		return
	}
	if !u.Pending {
		jsonError(w, 409, u.Name+" has already accepted their invite.")
		return
	}
	link, err := s.sendInvite(u.ID, u.Email, userFrom(r).Name)
	s.event(r, "Re-sent an invite", u.Email)
	writeJSON(w, 200, s.inviteResult(link, err))
}

func (s *Server) adminCount() int {
	var n int
	s.store.db.QueryRow(`SELECT count(*) FROM users WHERE role = 'admin' AND invite_hash = ''`).Scan(&n)
	return n
}

func (s *Server) apiUserRole(w http.ResponseWriter, r *http.Request) {
	u := s.userFromPath(w, r)
	if u == nil {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &body); err != nil || !validRole(body.Role) {
		jsonError(w, 422, "Pick a role.")
		return
	}
	if u.ID == userFrom(r).ID {
		jsonError(w, 409, "You can't change your own role. Ask another admin.")
		return
	}
	if u.Role == RoleAdmin && body.Role != RoleAdmin && !u.Pending && s.adminCount() <= 1 {
		jsonError(w, 409, "There must always be at least one admin.")
		return
	}
	s.store.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, body.Role, u.ID)
	s.event(r, "Changed a role", u.Email+": "+u.Role+" → "+body.Role)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	u := s.userFromPath(w, r)
	if u == nil {
		return
	}
	if u.ID == userFrom(r).ID {
		jsonError(w, 409, "You can't remove your own account.")
		return
	}
	if u.Role == RoleAdmin && !u.Pending && s.adminCount() <= 1 {
		jsonError(w, 409, "There must always be at least one admin.")
		return
	}
	s.store.db.Exec(`DELETE FROM users WHERE id = ?`, u.ID) // sessions go with it
	what := "Removed a user"
	if u.Pending {
		what = "Cancelled an invite"
	}
	s.event(r, what, u.Email)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) apiTestEmail(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	site := s.settings.Get("site_name")
	err := s.sendMail(u.Email, site+" test email", "Email from "+site+" is working.\n",
		s.mailHTML("Email is working", []string{"This test came from " + site + ". Invites, password resets and notifications will arrive like this."}, "", ""))
	if err != nil {
		jsonError(w, 502, "Couldn't send: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"to": u.Email})
}

// ---- accepting an invite ----

// tokenUser finds the user a still-valid invite or reset link is for.
func (s *Server) tokenUser(col, expCol, tok string) (id int64, name, email string, ok bool) {
	if len(tok) != 64 {
		return 0, "", "", false
	}
	var exp string
	err := s.store.db.QueryRow(`SELECT id, name, email, `+expCol+` FROM users WHERE `+col+` = ?`, hashToken(tok)).Scan(&id, &name, &email, &exp)
	if err != nil || time.Now().After(parseDB(exp)) {
		return 0, "", "", false
	}
	return id, name, email, true
}

func (s *Server) showInvite(w http.ResponseWriter, r *http.Request) {
	_, name, email, ok := s.tokenUser("invite_hash", "invite_expires", r.PathValue("token"))
	if !ok {
		s.render(w, r, 410, "message", "Invite", "", map[string]string{"Title": "This invite has expired", "Text": "Invite links work for a limited time and only once. Ask whoever invited you to send a new one."})
		return
	}
	s.render(w, r, 200, "password", "Accept invite", "", map[string]string{"Mode": "invite", "Name": name, "Email": email, "Token": r.PathValue("token")})
}

func (s *Server) doInvite(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	id, _, email, ok := s.tokenUser("invite_hash", "invite_expires", tok)
	if !ok {
		s.render(w, r, 410, "message", "Invite", "", map[string]string{"Title": "This invite has expired", "Text": "Ask whoever invited you to send a new one."})
		return
	}
	name, pw := strings.TrimSpace(r.FormValue("name")), r.FormValue("password")
	if msg := passwordProblem(pw, r.FormValue("password_confirm")); msg != "" || name == "" {
		if msg == "" {
			msg = "Enter your name."
		}
		s.render(w, r, 422, "password", "Accept invite", "", map[string]string{"Mode": "invite", "Name": name, "Email": email, "Token": tok, "Error": msg})
		return
	}
	if len(name) > 100 {
		name = name[:100]
	}
	s.store.db.Exec(`UPDATE users SET name = ?, password = ?, invite_hash = '', invite_expires = '', last_login = ? WHERE id = ?`, name, hashPassword(pw), nowDB(), id)
	s.store.db.Exec(`INSERT INTO events (at, user_label, action, detail) VALUES (?, ?, 'Accepted an invite', ?)`, nowDB(), name, email)
	s.newSession(w, r, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func passwordProblem(pw, confirm string) string {
	if len(pw) < 8 || len(pw) > 72 {
		return "Use a password of 8 to 72 characters."
	}
	if pw != confirm {
		return "The passwords don't match."
	}
	return ""
}

// ---- forgotten passwords ----

func (s *Server) resetEnabled() bool { return s.mailConfigured() && s.settings.Bool("password_reset") }

func (s *Server) showForgot(w http.ResponseWriter, r *http.Request) {
	if !s.resetEnabled() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.render(w, r, 200, "forgot", "Reset password", "", map[string]string{})
}

// doForgot always gives the same answer, so it can't be used to find out
// who has an account. Each address can get one email a minute.
func (s *Server) doForgot(w http.ResponseWriter, r *http.Request) {
	if !s.resetEnabled() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	ip := clientIP(r)
	if s.limiter.blocked("reset:"+ip, s.settings.Int("login_attempts")) {
		s.render(w, r, 429, "forgot", "Reset password", "", map[string]string{"Error": "Too many requests. Try again in 15 minutes."})
		return
	}
	s.limiter.failed("reset:" + ip)
	var id int64
	var name, pending, resetExp string
	if s.store.db.QueryRow(`SELECT id, name, invite_hash, reset_expires FROM users WHERE email = ?`, email).Scan(&id, &name, &pending, &resetExp) == nil && pending == "" {
		// A link sent in the last minute is still on its way.
		if resetExp == "" || time.Until(parseDB(resetExp)) < 59*time.Minute {
			tok := randomHex(32)
			s.store.db.Exec(`UPDATE users SET reset_hash = ?, reset_expires = ? WHERE id = ?`, hashToken(tok), time.Now().UTC().Add(time.Hour).Format(dbTime), id)
			link := s.publicURL() + "/reset/" + tok
			site := s.settings.Get("site_name")
			go func() {
				err := s.sendMail(email, "Reset your "+site+" password",
					"Someone (hopefully you) asked to reset your password.\n\nChoose a new one here (the link works for one hour):\n\n"+link+"\n\nIf it wasn't you, ignore this email; your password hasn't changed.\n",
					s.mailHTML("Reset your password", []string{"Hi " + name + ", someone (hopefully you) asked to reset your password. The link works for one hour.", "If it wasn't you, ignore this email; your password hasn't changed."}, "Choose a new password", link))
				if err != nil {
					log.Printf("password reset email to %s failed: %v", email, err)
				}
			}()
		}
	}
	s.render(w, r, 200, "message", "Reset password", "", map[string]string{"Title": "Check your email", "Text": "If that address has an account, a link to reset its password is on its way. It works for one hour.", "Back": "1"})
}

func (s *Server) showReset(w http.ResponseWriter, r *http.Request) {
	_, name, email, ok := s.tokenUser("reset_hash", "reset_expires", r.PathValue("token"))
	if !ok {
		s.render(w, r, 410, "message", "Reset password", "", map[string]string{"Title": "This link has expired", "Text": "Reset links work for one hour and only once. Ask for a new one from the sign-in page.", "Back": "1"})
		return
	}
	s.render(w, r, 200, "password", "Reset password", "", map[string]string{"Mode": "reset", "Name": name, "Email": email, "Token": r.PathValue("token")})
}

func (s *Server) doReset(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	id, name, email, ok := s.tokenUser("reset_hash", "reset_expires", tok)
	if !ok {
		s.render(w, r, 410, "message", "Reset password", "", map[string]string{"Title": "This link has expired", "Text": "Ask for a new one from the sign-in page.", "Back": "1"})
		return
	}
	pw := r.FormValue("password")
	if msg := passwordProblem(pw, r.FormValue("password_confirm")); msg != "" {
		s.render(w, r, 422, "password", "Reset password", "", map[string]string{"Mode": "reset", "Name": name, "Email": email, "Token": tok, "Error": msg})
		return
	}
	s.store.db.Exec(`UPDATE users SET password = ?, reset_hash = '', reset_expires = '', last_login = ? WHERE id = ?`, hashPassword(pw), nowDB(), id)
	s.store.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id) // sign out everywhere else
	s.store.db.Exec(`INSERT INTO events (at, user_label, action, detail) VALUES (?, ?, 'Reset their password', ?)`, nowDB(), name, email)
	s.newSession(w, r, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
