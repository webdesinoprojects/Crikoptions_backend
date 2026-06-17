package realtime

import (
	"encoding/json"
	"sync"
)

// Hub fans out topic messages to subscribed WebSocket clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[*Client]struct{}),
	}
}

func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// Publish sends a message to every client subscribed to topic.
func (h *Hub) Publish(topic string, data any) {
	payload, err := json.Marshal(map[string]any{
		"topic": topic,
		"data":  data,
	})
	if err != nil {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for client := range h.clients {
		client.trySend(topic, payload)
	}
}
