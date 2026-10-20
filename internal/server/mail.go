package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Email goes through the mail server set in Settings → Email.

var errNoMail = errors.New("no mail server is set up yet (Settings → Email)")

func (s *Server) mailConfigured() bool { return s.settings.Get("smtp_host") != "" }

// sendMail sends a message with a plain-text and an HTML version.
func (s *Server) sendMail(to, subject, text, htmlBody string) error {
	return s.smtpSend(to, subject, text, htmlBody)
}

func (s *Server) smtpSend(to, subject, text, htmlBody string) error {
	host := s.settings.Get("smtp_host")
	if host == "" {
		return errNoMail
	}
	port := strconv.Itoa(s.settings.Int("smtp_port"))
	user, pass := s.settings.Get("smtp_username"), s.settings.Get("smtp_password")
	from := s.settings.Get("smtp_from")
	if from == "" {
		from = user
	}
	if from == "" {
		return errors.New("set a From address in Settings → Email")
	}
	fromName := s.settings.Get("smtp_from_name")
	if fromName == "" {
		fromName = s.settings.Get("site_name")
	}
	security := s.settings.Get("smtp_security")

	addr := net.JoinHostPort(host, port)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if security == "ssl" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("can't reach the mail server: %w", err)
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("the mail server didn't answer properly: %w", err)
	}
	defer c.Close()
	if security == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("STARTTLS failed: %w", err)
		}
	}
	if user != "" {
		if err := c.Auth(smtp.PlainAuth("", user, pass, host)); err != nil {
			return fmt.Errorf("the mail server refused the login: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("the mail server refused the sender %s: %w", from, err)
	}
	for _, rcpt := range strings.Split(to, ",") {
		if rcpt = strings.TrimSpace(rcpt); rcpt != "" {
			if err := c.Rcpt(rcpt); err != nil {
				return fmt.Errorf("the mail server refused %s: %w", rcpt, err)
			}
		}
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	msg := buildMessage(mime.QEncoding.Encode("utf-8", fromName)+" <"+from+">", to, subject, text, htmlBody)
	if _, err := wc.Write([]byte(msg)); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// buildMessage writes a multipart/alternative email. Bodies are
// quoted-printable so long lines stay inside SMTP's limit.
func buildMessage(from, to, subject, text, htmlBody string) string {
	head := []string{
		"From: " + from,
		"To: " + to,
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Message-ID: <" + randomHex(16) + "@repodepot>",
		"MIME-Version: 1.0",
	}
	part := func(ctype, body string) []string {
		return []string{"Content-Type: " + ctype + "; charset=utf-8", "Content-Transfer-Encoding: quoted-printable", "", quotedPrintable(body)}
	}
	if htmlBody == "" {
		return strings.Join(append(head, part("text/plain", text)...), "\r\n")
	}
	boundary := "repodepot-" + randomHex(12)
	lines := append(head, `Content-Type: multipart/alternative; boundary="`+boundary+`"`, "", "--"+boundary)
	lines = append(lines, part("text/plain", text)...)
	lines = append(lines, "--"+boundary)
	lines = append(lines, part("text/html", htmlBody)...)
	lines = append(lines, "--"+boundary+"--", "")
	return strings.Join(lines, "\r\n")
}

func quotedPrintable(s string) string {
	var b strings.Builder
	w := quotedprintable.NewWriter(&b)
	w.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")))
	w.Close()
	return b.String()
}

// mailHTML wraps a short message in a simple, mail-client-safe layout
// with an optional button. Everything passed in is plain text.
func (s *Server) mailHTML(heading string, paras []string, button, link string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><body style="margin:0;padding:24px;background:#f4f5f7;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#1f2328">`)
	b.WriteString(`<div style="max-width:520px;margin:0 auto;background:#fff;border-radius:12px;padding:28px;border:1px solid #e3e6ea">`)
	b.WriteString(`<div style="font-size:13px;color:#6b7280;margin-bottom:14px">` + html.EscapeString(s.settings.Get("site_name")) + `</div>`)
	b.WriteString(`<h1 style="font-size:20px;margin:0 0 14px">` + html.EscapeString(heading) + `</h1>`)
	for _, p := range paras {
		b.WriteString(`<p style="font-size:15px;line-height:1.55;margin:0 0 14px">` + html.EscapeString(p) + `</p>`)
	}
	if link != "" {
		b.WriteString(`<p style="margin:22px 0"><a href="` + html.EscapeString(link) + `" style="display:inline-block;background:#c99243;color:#1b1306;text-decoration:none;font-weight:600;padding:11px 20px;border-radius:8px">` + html.EscapeString(button) + `</a></p>`)
		b.WriteString(`<p style="font-size:12px;color:#6b7280;word-break:break-all">Or open this link: ` + html.EscapeString(link) + `</p>`)
	}
	b.WriteString(`</div></body></html>`)
	return b.String()
}
