package realtime

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// Non-browser clients (e.g. native apps) omit Origin.
			return true
		}
		// For production, also allow your real frontend domain here.
		return strings.HasPrefix(origin, "http://localhost:") ||
			strings.HasPrefix(origin, "https://localhost:")
	},
}

type Handler struct {
	hub *Hub
}

func NewHandler(hub *Hub) *Handler {
	return &Handler{hub: hub}
}

func (h *Handler) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	client := newClient(h.hub, conn)
	h.hub.Register(client)

	go client.writePump()
	client.readPump(h.hub)
}

func (c *Client) readPump(hub *Hub) {
	defer func() {
		hub.Unregister(c)
		_ = c.conn.Close()
	}()

	c.conn.SetReadLimit(4096)
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		c.handleControlMessage(message)
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case payload, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
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

type controlMessage struct {
	Action string   `json:"action"`
	Topic  string   `json:"topic"`
	Topics []string `json:"topics"`
}

func (c *Client) handleControlMessage(message []byte) {
	var msg controlMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		log.Printf("ws: invalid control message: %v", err)
		return
	}

	action := strings.ToLower(strings.TrimSpace(msg.Action))
	topics := msg.Topics
	if strings.TrimSpace(msg.Topic) != "" {
		topics = append(topics, msg.Topic)
	}

	switch action {
	case "subscribe":
		for _, topic := range topics {
			topic = strings.TrimSpace(topic)
			if topic != "" {
				c.subscribe(topic)
			}
		}
	case "unsubscribe":
		for _, topic := range topics {
			topic = strings.TrimSpace(topic)
			if topic != "" {
				c.unsubscribe(topic)
			}
		}
	}
}
