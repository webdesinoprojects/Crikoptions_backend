package realtime

import (
	"sync"

	"github.com/gorilla/websocket"
)

const sendBufferSize = 64

type Client struct {
	hub   *Hub
	conn  *websocket.Conn
	send  chan []byte
	topics map[string]struct{}
	userID string
	mu     sync.RWMutex
}

func newClient(hub *Hub, conn *websocket.Conn) *Client {
	return &Client{
		hub:    hub,
		conn:   conn,
		send:   make(chan []byte, sendBufferSize),
		topics: make(map[string]struct{}),
	}
}

func (c *Client) subscribe(topic string) {
	c.mu.Lock()
	c.topics[topic] = struct{}{}
	c.mu.Unlock()
}

func (c *Client) unsubscribe(topic string) {
	c.mu.Lock()
	delete(c.topics, topic)
	c.mu.Unlock()
}

func (c *Client) isSubscribed(topic string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.topics[topic]
	return ok
}

func (c *Client) setUserID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userID = id
}

func (c *Client) getUserID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.userID
}

func (c *Client) trySend(topic string, payload []byte) {
	if !c.isSubscribed(topic) {
		return
	}
	select {
	case c.send <- payload:
	default:
	}
}
