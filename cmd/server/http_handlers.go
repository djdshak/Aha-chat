package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func homeHandler(w http.ResponseWriter, r *http.Request) {
	// 建议严格一点，避免“拼错路径也 200”
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("aha-chat server\n"))
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// db
func historyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "method_not_allowed",
		})
		return
	}

	q := r.URL.Query()
	me, err := authUsernameFromRequest(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"details": err.Error(),
		})
		return
	}

	peer := strings.TrimSpace(q.Get("peer"))
	if me == "" || peer == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "missing_me_or_peer",
		})
	}

	limit := parseIntQuery(q.Get("limit"), 20)
	beforeID := parseInt64Query(q.Get("before_id"), 0)

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	items, err := QueryConversation(ctx, me, peer, beforeID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "query_history_failed",
			"details": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
	})
}

func syncHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "method_not_allowed",
		})
		return
	}

	q := r.URL.Query()
	user, err := authUsernameFromRequest(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"details": err.Error(),
		})
		return
	}

	if user == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "missing_user",
		})
		return
	}

	afterID := parseInt64Query(q.Get("after_id"), 0)
	limit := parseIntQuery(q.Get("limit"), 100)

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	items, err := QuerySyncInboxAfterID(ctx, user, afterID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "query_sync_failed",
			"details": err.Error(),
		})
		return
	}

	var nextAfterID int64 = afterID
	if len(items) > 0 {
		nextAfterID = items[len(items)-1].ID
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":          items,
		"next_after_id":  nextAfterID,
		"requested_user": user,
	})
}

// db
func parseIntQuery(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func parseInt64Query(s string, def int64) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "method_not_allowed"})
		return
	}
	type reqBody struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	var req reqBody
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json",
			"details": err.Error(),
		})
		return
	}

	if err := CreateUser(r.Context(), req.Username, req.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "register_failed", "details": err.Error(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "method_not_allowed",
		})
		return
	}

	type reqBody struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	var req reqBody
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "bad_json", "details": err.Error(),
		})
		return
	}

	u, err := AuthenticateUser(r.Context(), req.Username, req.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "login_failed",
			"details": err.Error(),
		})
		return
	}

	log.Printf("loginHandler: appDB=%p", appDB)
	token, expiresAt, err := IssueToken(r.Context(), u.ID, 30*24*time.Hour)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "issue_token_failed",
			"details": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": expiresAt,
		"username":   u.Username,
	})
}

func authUsernameFromRequest(r *http.Request) (string, error) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer") {
		tok := strings.TrimSpace(auth[7:])
		return UsernameByToken(r.Context(), tok)
	}

	tok := strings.TrimSpace(r.URL.Query().Get("token"))
	if tok != "" {
		return UsernameByToken(r.Context(), tok)
	}
	return "", errors.New("missing token")
}
