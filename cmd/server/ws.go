package main

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// dev：接受任何 origin（你之后上域名/https 再收紧）
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// token：先用 query 参数，最省事
		token := strings.TrimSpace(r.URL.Query().Get("token"))
		if token == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}

		username, err := UsernameByToken(r.Context(), token)
		if err != nil {
			http.Error(w, "invalid token "+err.Error(), http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("ws upgrade:", err)
			return
		}

		c := &Client{
			username: username,
			conn:     conn,
			send:     make(chan []byte, 64),
			hub:      hub,
		}

		hub.register <- c

		// 给自己发一条欢迎信息（可删）
		_ = c.enqueueJSON(WSOut{
			Type: "info",
			Text: "connected as " + username,
			Ts:   time.Now().Unix(),
		})

		go c.writePump()
		c.readPump() // 阻塞直到断开

		// readPump 退出后
		hub.unregister <- c
		_ = conn.Close()
	}
}
