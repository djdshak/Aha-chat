package main

import (
	"encoding/json"
	"log"
	"time"
)

type sendReq struct {
	to        string
	msg       []byte
	from      *Client
	offline   []byte
	delivered []byte
}

type Hub struct {
	clients    map[*Client]bool
	byUser     map[string]*Client
	register   chan *Client
	unregister chan *Client
	broadcast  chan []byte

	sendTo chan sendReq
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		byUser:     make(map[string]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan []byte, 256),
		sendTo:     make(chan sendReq, 256),
	}
}

func (h *Hub) run() {
	for {
		select {
		case c := <-h.register:
			// 同用户名新连接上线：踢掉旧连接
			if old, ok := h.byUser[c.username]; ok && old != c {
				if _, ok2 := h.clients[old]; ok2 {
					delete(h.clients, old)
					close(old.send)
				}
			}
			h.clients[c] = true
			h.byUser[c.username] = c
			log.Printf("ws: user %s connected (online=%d)", c.username, len(h.clients))

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				if cur, ok2 := h.byUser[c.username]; ok2 && cur == c {
					delete(h.byUser, c.username)
				}
				close(c.send)
				log.Printf("ws: user %s disconnected (online=%d)", c.username, len(h.clients))
			}

		case req := <-h.sendTo:
			dst, ok := h.byUser[req.to]
			if !ok {
				// 目标离线：给发送者回离线错误
				if req.from != nil && req.offline != nil {
					select {
					case req.from.send <- req.offline:
					default:
					}
				}
				continue
			}

			select {
			case dst.send <- req.msg:
				// 投递成功：给发送者回 delivered ACK
				if req.from != nil && req.delivered != nil {
					select {
					case req.from.send <- req.delivered:
					default:
					}
				}

			default:
				// 接收者慢/挂了：踢掉
				delete(h.clients, dst)
				if cur, ok2 := h.byUser[dst.username]; ok2 && cur == dst {
					delete(h.byUser, dst.username)
				}
				close(dst.send)

				// 给发送者回一个错误（可选）
				if req.from != nil {
					fail, _ := json.Marshal(WSOut{
						Type: "error",
						Text: "delivery failed: receiver connection is slow/closed",
						Ts:   time.Now().Unix(),
					})
					select {
					case req.from.send <- fail:
					default:
					}
				}
			}

		case msg := <-h.broadcast:
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					// 广播时某个客户端太慢：踢掉
					delete(h.clients, c)
					if cur, ok2 := h.byUser[c.username]; ok2 && cur == c {
						delete(h.byUser, c.username)
					}
					close(c.send)
				}
			}
		}
	}
}
