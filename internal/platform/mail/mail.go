// Package mail sends transactional email (password reset, notices).
//
//	SMTP    production: any provider (Amazon SES, Mailgun, SendGrid, Gmail
//	        Workspace, a local relay) through SMTP_HOST/PORT/USERNAME/PASSWORD.
//	Log     development only: writes the email to the log instead of sending.
//	Memory  tests: keeps the emails so a test can read the reset link.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

type Message struct {
	To      string
	Subject string
	Text    string
}

type Sender interface {
	Send(ctx context.Context, m Message) error
}

// clean blocks header injection through addresses or subjects.
func clean(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n") {
		return "", errors.New("mail: line breaks are not allowed in headers")
	}
	return strings.TrimSpace(s), nil
}

// ---- SMTP ----

type SMTP struct {
	Host, Port, Username, Password, From string
}

func (s SMTP) Send(ctx context.Context, m Message) error {
	to, err := clean(m.To)
	if err != nil {
		return err
	}
	subject, err := clean(m.Subject)
	if err != nil {
		return err
	}
	from, err := clean(s.From)
	if err != nil {
		return err
	}
	body := strings.Join([]string{
		"From: " + from, "To: " + to, "Subject: " + subject,
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"MIME-Version: 1.0", "Content-Type: text/plain; charset=UTF-8", "Content-Transfer-Encoding: 8bit",
		"", strings.ReplaceAll(m.Text, "\n", "\r\n"),
	}, "\r\n")

	addr := net.JoinHostPort(s.Host, s.Port)
	var auth smtp.Auth
	if s.Username != "" {
		auth = smtp.PlainAuth("", s.Username, s.Password, s.Host) // PlainAuth refuses to send over plain text
	}
	done := make(chan error, 1)
	go func() {
		if s.Port == "465" { // implicit TLS
			done <- sendTLS(addr, s.Host, auth, from, to, []byte(body))
			return
		}
		done <- smtp.SendMail(addr, auth, from, []string{to}, []byte(body)) // upgrades with STARTTLS
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sendTLS(addr, host string, auth smtp.Auth, from, to string, msg []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// ---- Log (development) ----

type Log struct{ Log *slog.Logger }

func (l Log) Send(_ context.Context, m Message) error {
	l.Log.Warn("EMAIL NOT SENT (no SMTP configured; development only)", "to", m.To, "subject", m.Subject, "body", m.Text)
	return nil
}

// ---- Memory (tests) ----

type Memory struct {
	mu   sync.Mutex
	sent []Message
}

func (m *Memory) Send(_ context.Context, msg Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

// Last returns the newest email sent to the address.
func (m *Memory) Last(to string) (Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		if strings.EqualFold(m.sent[i].To, to) {
			return m.sent[i], true
		}
	}
	return Message{}, false
}

func (m *Memory) Count(to string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sent {
		if strings.EqualFold(s.To, to) {
			n++
		}
	}
	return n
}
