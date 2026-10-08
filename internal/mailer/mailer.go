// Package mailer sends transactional emails over SMTP, or logs them when no SMTP server is configured.
package mailer

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string // optional; sent as multipart/alternative next to Text
}

type Mailer interface {
	Send(ctx context.Context, m Message) error
}

type Config struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
}

func New(c Config) Mailer {
	if c.Host == "" {
		return Log{}
	}
	return &SMTP{cfg: c}
}

// Log writes emails to the API log; handy in development to copy magic links.
type Log struct{}

func (Log) Send(_ context.Context, m Message) error {
	slog.Info("email (SMTP not configured)", "to", m.To, "subject", m.Subject, "body", m.Text)
	return nil
}

type SMTP struct{ cfg Config }

func (s *SMTP) Send(_ context.Context, m Message) error {
	var auth smtp.Auth
	if s.cfg.Username != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	}
	if strings.ContainsAny(m.To, "\r\n") {
		return fmt.Errorf("mailer: invalid recipient")
	}
	header, envelope, err := sender(s.cfg.From)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", header)
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	crlf := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n") }
	if m.HTML == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
		b.WriteString(crlf(m.Text))
	} else {
		boundary := fmt.Sprintf("ff-%d", time.Now().UnixNano())
		fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n", boundary, crlf(m.Text))
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n", boundary, crlf(m.HTML))
		fmt.Fprintf(&b, "--%s--\r\n", boundary)
	}
	return smtp.SendMail(s.cfg.Host+":"+s.cfg.Port, auth, envelope, []string{m.To}, []byte(b.String()))
}

// sender splits MAIL_FROM ("Formsis <no-reply@x.com>" or a bare address) into the
// From header and the bare address SMTP expects in MAIL FROM.
func sender(from string) (header, envelope string, err error) {
	a, err := mail.ParseAddress(from)
	if err != nil {
		return "", "", fmt.Errorf("mailer: invalid MAIL_FROM %q: %w", from, err)
	}
	return a.String(), a.Address, nil
}
