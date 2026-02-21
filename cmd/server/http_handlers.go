package main

import "net/http"

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
