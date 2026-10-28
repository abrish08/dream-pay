// Package sms sends text messages (verification codes, security alerts).
//
//	SendEt  send.et, Ethiopian SMS gateway: POST https://api.send.et/v1/messages
//	        {to, sender, body} with Authorization: Bearer <API key>. Built from
//	        send.et's public API preview; confirm fields with send.et when you
//	        get your account (their docs say paths may still change).
//	Log     development: writes the SMS to the log.
//	Memory  tests.
package sms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Message struct {
	To   string // E.164, e.g. +251911000000
	Body string
}

type Sender interface {
	Send(ctx context.Context, m Message) error
}

// ---- send.et ----

type SendEt struct {
	BaseURL string // default https://api.send.et/v1
	APIKey  string
	Sender  string // approved sender name
	HTTP    *http.Client
}

func (s SendEt) Send(ctx context.Context, m Message) error {
	base := strings.TrimRight(s.BaseURL, "/")
	if base == "" {
		base = "https://api.send.et/v1"
	}
	body, _ := json.Marshal(map[string]string{"to": m.To, "sender": s.Sender, "body": m.Body})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("send.et: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return fmt.Errorf("send.et: HTTP %d %s %s", resp.StatusCode, e.Error.Code, e.Error.Message)
}

// ---- Log (development) ----

type Log struct{ Log *slog.Logger }

func (l Log) Send(_ context.Context, m Message) error {
	l.Log.Warn("SMS NOT SENT (no SMS provider configured; development only)", "to", m.To, "body", m.Body)
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

func (m *Memory) Last(to string) (Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		if m.sent[i].To == to {
			return m.sent[i], true
		}
	}
	return Message{}, false
}
