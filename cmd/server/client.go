package main

import (
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

			// 1) 基本清洗
			to := strings.TrimSpace(in.To)
			text := strings.TrimSpace(in.Text)
			msgID := strings.TrimSpace(in.MsgID)

			// 2) 最小 ACK 版本：要求客户端带 msg_id
			if msgID == "" {
				_ = c.enqueueJSON(WSOut{
					Type: "error",
					Text: "missing msg_id",
					Ts:   now,
				})
				continue
			}

			if text == "" {
				_ = c.enqueueJSON(WSOut{
					Type:  "error",
					MsgID: msgID,
					Text:  "empty text",
					Ts:    now,
				})
				continue
			}

			// 3) 先回 server_received ACK（表示服务器已收到并解析）
			_ = c.enqueueJSON(WSOut{
				Type:  "ack", // <- 修正为 ack
				MsgID: msgID,
				Ack:   "server_received",
				To:    to,
				Ts:    now,
			})

			// 4) 组装真正消息（发给接收方，也给发送者回显）
			out := WSOut{
				Type:  "msg",
				MsgID: msgID,
				From:  c.username,
				To:    to,
				Text:  text,
				Ts:    now,
			}
			b, _ := json.Marshal(out)

			// 5) 发给发送者自己回显（UI 可立即显示）
			select {
			case c.send <- b:
			default:
			}

			// 6) 如果没填 to，仍走广播
			if out.To == "" {
				c.hub.broadcast <- b
				continue
			}

			deliveredAck, _ := json.Marshal(WSOut{
				Type:  "ack",
				MsgID: msgID,
				Ack:   "delivered",
				To:    to,
				Ts:    time.Now().Unix(),
			})

			offlineErr, _ := json.Marshal(WSOut{
				Type:  "error",
				MsgID: msgID,
				Text:  "user offline: " + to,
				Ts:    time.Now().Unix(),
			})

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
