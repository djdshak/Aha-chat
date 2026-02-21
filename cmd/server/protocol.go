package main

import "strings"

type WSIn struct {
	Type  string `json:"type"` // "msg"
	MsgID string `json:"msg_id,omitempty"`
	To    string `json:"to,omitempty"`
	Text  string `json:"text,omitempty"`
}

type WSOut struct {
	Type  string `json:"type"` // "msg", "info", "error", "ack"
	MsgID string `json:"msg_id,omitempty"`
	Ack   string `json:"ack,omitempty"`

	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Text string `json:"text,omitempty"`
	Ts   int64  `json:"ts,omitempty"`
}

func usernameFromToken(token string) (string, bool) {
	const prefix = "dev-token-"
	if !strings.HasPrefix(token, prefix) {
		return "", false
	}
	u := strings.TrimSpace(token[len(prefix):])
	if u == "" {
		return "", false
	}
	return u, true
}
