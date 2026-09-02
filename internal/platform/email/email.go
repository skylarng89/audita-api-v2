package email

// Package email wraps SMTP dispatch used by identity (invites, password
// reset) and notification (email fan-out) modules.

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
)

// Sender abstracts email dispatch so tests can stub it and a future
// transactional-email provider can be swapped in (ADR-1 split-readiness).
type Sender interface {
	Send(ctx context.Context, to, subject, htmlBody string) error
}

// Config carries SMTP settings from the app config.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	FromName string
	UseAuth  bool
	UseTLS   bool
}

// SMTPSender sends mail over SMTP (compatible with MailHog in dev).
type SMTPSender struct {
	cfg    Config
	logger *slog.Logger
}

// NewSMTPSender builds an SMTPSender.
func NewSMTPSender(cfg Config, logger *slog.Logger) *SMTPSender {
	return &SMTPSender{cfg: cfg, logger: logger}
}

// Send delivers an HTML email. Failures are logged and returned; callers
// decide retry policy (typically none for invites — the token remains valid).
func (s *SMTPSender) Send(ctx context.Context, to, subject, htmlBody string) error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	from := s.cfg.From
	if s.cfg.FromName != "" {
		from = fmt.Sprintf("%s <%s>", s.cfg.FromName, s.cfg.From)
	}
	msg := strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=\"utf-8\"",
		"",
		htmlBody,
	}, "\r\n") + "\r\n"

	var auth smtp.Auth
	if s.cfg.UseAuth && s.cfg.Username != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	}
	var err error
	if s.cfg.UseTLS {
		err = s.sendTLS(addr, auth, s.cfg.From, to, []byte(msg))
	} else {
		err = smtp.SendMail(addr, auth, s.cfg.From, []string{to}, []byte(msg))
	}
	if err != nil {
		s.logger.Error("email send failed", slog.String("to", to), slog.String("subject", subject), slog.String("error", err.Error()))
		return fmt.Errorf("email: send to %s: %w", to, err)
	}
	return nil
}

func (s *SMTPSender) sendTLS(addr string, auth smtp.Auth, from, to string, body []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: s.cfg.Host})
	if err != nil {
		return fmt.Errorf("email: tls dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("email: smtp client: %w", err)
	}
	defer func() { _ = c.Quit() }()
	if err = c.Hello(s.cfg.Host); err != nil {
		return err
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err = c.Auth(auth); err != nil {
				return fmt.Errorf("email: auth: %w", err)
			}
		}
	}
	if err = c.Mail(from); err != nil {
		return err
	}
	if err = c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write(body); err != nil {
		return err
	}
	return w.Close()
}

// NopSender is a no-op sender for tests and storage-only deployments.
type NopSender struct{}

// Send implements Sender without side effects.
func (NopSender) Send(context.Context, string, string, string) error { return nil }
