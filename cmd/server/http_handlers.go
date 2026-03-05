package main

import (
	"context"
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
			"error":   "bad_json",
			"details": err.Error(),
		})
		return
	}

	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "missing_username_or_password",
		})
		return
	}

	// dev 阶段：先不做真实鉴权
	writeJSON(w, http.StatusOK, map[string]any{
		"token": "dev-token-" + req.Username,
	})
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
	me := strings.TrimSpace(q.Get("me"))
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
	user := strings.TrimSpace(q.Get("user"))
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
