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
		return true // Allow all origins for the API, or configure via CORS policies
	},
}

type AuthFunc func(token string) (userID string, err error)

type Handler struct {
	hub      *Hub
	authFunc AuthFunc
}

func NewHandler(hub *Hub, authFunc AuthFunc) *Handler {
	return &Handler{hub: hub, authFunc: authFunc}
}

func (h *Handler) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	client := newClient(h.hub, conn)
	h.hub.Register(client)

	go client.writePump()
	client.readPump(h)
}

func (c *Client) readPump(h *Handler) {
	defer func() {
		h.hub.Unregister(c)
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
		h.handleControlMessage(c, message)
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
	Token  string   `json:"token"`
}

func (h *Handler) handleControlMessage(c *Client, message []byte) {
	var msg controlMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		log.Printf("ws: invalid control message: %v", err)
		return
	}

	action := strings.ToLower(strings.TrimSpace(msg.Action))
	
	if action == "auth" {
		if h.authFunc == nil {
			return
		}
		userID, err := h.authFunc(msg.Token)
		if err != nil {
			log.Printf("ws auth failed: %v", err)
			return
		}
		c.setUserID(userID)
		return
	}

	topics := msg.Topics
	if strings.TrimSpace(msg.Topic) != "" {
		topics = append(topics, msg.Topic)
	}

	switch action {
	case "subscribe":
		for _, topic := range topics {
			topic = strings.TrimSpace(topic)
			if topic != "" {
				if strings.HasPrefix(topic, "user:") {
					parts := strings.Split(topic, ":")
					if len(parts) >= 2 && parts[1] != c.getUserID() {
						continue // Unauthorized
					}
				}
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
