package main

import "net/http"

// registerRoutes 负责把所有 HTTP/WS 路由挂到 mux 上
func registerRoutes(mux *http.ServeMux, hub *Hub) {
	mux.HandleFunc("/", homeHandler)
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/v1/login", loginHandler)

	// WebSocket
	mux.HandleFunc("/v1/ws", wsHandler(hub))

	// DB read APIs
	mux.HandleFunc("/v1/history", historyHandler)
	mux.HandleFunc("/v1/sync", syncHandler)
}
