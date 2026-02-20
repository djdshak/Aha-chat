package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	addr := getenv("LISTEN_ADDR", "127.0.0.1:8080")

	mux := http.NewServeMux()

	// 建一个 hub，专门管理在线连接与广播
	hub := newHub()
	go hub.run()

	// Home（建议严格一点，避免 “拼错路径也 200”）
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("aha-chat server\n"))
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// HTTP login -> 返回 dev token
	mux.HandleFunc("/v1/login", func(w http.ResponseWriter, r *http.Request) {
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

		// 先别做真实鉴权，dev 阶段够用
		writeJSON(w, http.StatusOK, map[string]any{
			"token": "dev-token-" + req.Username,
		})
	})

	// WebSocket：实时聊天通道（先做广播）
	mux.HandleFunc("/v1/ws", wsHandler(hub))

	srv := &http.Server{
		Addr:              addr,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Println("listening on", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("ListenAndServe:", err)
		}
	}()

	waitForSignal()
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = srv.Shutdown(ctx)

	log.Println("bye")
}

/* -------------------- WebSocket + Hub -------------------- */

type WSIn struct {
	Type string `json:"type"` // "msg"
	Text string `json:"text,omitempty"`
}

type WSOut struct {
	Type string `json:"type"` // "msg", "info", "error"
	From string `json:"from,omitempty"`
	Text string `json:"text,omitempty"`
	Ts   int64  `json:"ts,omitempty"`
}

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

type Hub struct {
	clients    map[*Client]bool
	register   chan *Client
	unregister chan *Client
	broadcast  chan []byte
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan []byte, 256),
	}
}

func (h *Hub) run() {
	for {
		select {
		case c := <-h.register:
			h.clients[c] = true
			log.Printf("ws: user %s connected (online=%d)", c.username, len(h.clients))

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
				log.Printf("ws: user %s disconnected (online=%d)", c.username, len(h.clients))
			}

		case msg := <-h.broadcast:
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					// 发送队列满了：踢掉这个慢客户端
					delete(h.clients, c)
					close(c.send)
				}
			}
		}
	}
}

type Client struct {
	username string
	conn     *websocket.Conn
	send     chan []byte
	hub      *Hub
}

func (c *Client) readPump() {
	// 可选：设置读超时
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}

		var in WSIn
		if err := json.Unmarshal(data, &in); err != nil {
			_ = c.enqueueJSON(WSOut{Type: "error", Text: "bad json", Ts: time.Now().Unix()})
			continue
		}

		switch in.Type {
		case "msg":
			out := WSOut{
				Type: "msg",
				From: c.username,
				Text: in.Text,
				Ts:   time.Now().Unix(),
			}
			b, _ := json.Marshal(out)
			c.hub.broadcast <- b

		default:
			_ = c.enqueueJSON(WSOut{Type: "error", Text: "unknown type", Ts: time.Now().Unix()})
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second) // ping 保活
	defer ticker.Stop()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}

		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) enqueueJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	select {
	case c.send <- b:
		return nil
	default:
		return nil
	}
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

/* -------------------- util funcs -------------------- */

func getenv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
