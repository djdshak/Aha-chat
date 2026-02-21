package main

import (
	"log"
	"net/http"
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
		token := r.URL.Query().Get("token")
		username, ok := usernameFromToken(token)
		if !ok {
			http.Error(w, "unauthorized (missing/invalid token)", http.StatusUnauthorized)
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
