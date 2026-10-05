// Package mailer sends transactional emails over SMTP, or logs them when no SMTP server is configured.
package mailer

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"net/smtp"
	"strings"
	"time"
)

type Message struct {
	To      string
	Subject string
	Text    string
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
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(m.Text, "\n", "\r\n"))
	return smtp.SendMail(s.cfg.Host+":"+s.cfg.Port, auth, s.cfg.From, []string{m.To}, []byte(b.String()))
}
