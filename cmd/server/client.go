package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

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
			_ = c.enqueueJSON(WSOut{
				Type: "error",
				Text: "bad json",
				Ts:   time.Now().Unix(),
			})
			continue
		}

		switch in.Type {
		case "msg":
			now := time.Now().Unix()

			to := strings.TrimSpace(in.To)
			text := strings.TrimSpace(in.Text)
			msgID := strings.TrimSpace(in.MsgID)

			// 1) 校验
			if msgID == "" {
				_ = c.enqueueJSON(WSOut{Type: "error", Text: "missing msg_id", Ts: now})
				continue
			}
			if to == "" {
				_ = c.enqueueJSON(WSOut{Type: "error", MsgID: msgID, Text: "missing to (use @username msg or /chat <peer>)", Ts: now})
				continue
			}
			if text == "" {
				_ = c.enqueueJSON(WSOut{Type: "error", MsgID: msgID, Text: "empty text", Ts: now})
				continue
			}

			// 2) 先落库（幂等）
			// 用当前请求/连接的 ctx（更规范）
			rowID, inserted, err := StoreTextMessage(
				context.Background(), // 如果你这里拿不到 r，就用 context.Background() 或给 Client 带一个 ctx
				msgID,
				c.username,
				to,
				text,
				now,
			)
			if err != nil {
				_ = c.enqueueJSON(WSOut{Type: "error", MsgID: msgID, Text: "db insert failed: " + err.Error(), Ts: now})
				continue
			}
			_ = rowID // 如果暂时用不上，避免 unused（或你直接删掉 rowID）

			// 3) db 成功后再回 server_received
			_ = c.enqueueJSON(WSOut{
				Type:  "ack",
				MsgID: msgID,
				Ack:   "server_received",
				To:    to,
				Ts:    now,
			})

			// 4) 组装消息（回显 + 发给对方）
			out := WSOut{
				Type:  "msg",
				MsgID: msgID,
				From:  c.username,
				To:    to,
				Text:  text,
				Ts:    now,
			}
			b, _ := json.Marshal(out)

			// 5) 发给发送者自己回显（立即显示）
			select {
			case c.send <- b:
			default:
			}

			// 6) 若是重复 msg_id（幂等重发）：不重复投递给对方
			if !inserted {
				// 可以选择给个提示，也可以不提示
				// _ = c.enqueueJSON(WSOut{Type:"info", MsgID: msgID, Text:"duplicate msg_id ignored", Ts: now})
				continue
			}

			deliveredAck, _ := json.Marshal(WSOut{
				Type:  "ack",
				MsgID: msgID,
				Ack:   "delivered",
				To:    to,
				Ts:    now,
			})

			offlineErr, _ := json.Marshal(WSOut{
				Type:  "error",
				MsgID: msgID,
				Text:  "user offline: " + to,
				Ts:    now,
			})

			// 7) 交给 hub 投递；hub 内部决定在线/离线
			c.hub.sendTo <- sendReq{
				to:        to,
				msg:       b,
				from:      c,
				offline:   offlineErr,
				delivered: deliveredAck,
			}

		default:
			_ = c.enqueueJSON(WSOut{
				Type: "error",
				Text: "unknown type",
				Ts:   time.Now().Unix(),
			})
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
