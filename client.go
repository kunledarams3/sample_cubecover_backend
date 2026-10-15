package main

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type Role string

const (
	RolePatient Role = "patient"
	RoleDoctor  Role = "doctor"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 8 * 1024
	sendBuffer     = 64
)

type Client struct {
	ID   string
	Name string
	Role Role

	hub  *Hub
	conn *websocket.Conn
	send chan []byte

	// Fields below are owned by the hub and only touched while holding hub.mu.
	sessionID string
	closed    bool
}

func (c *Client) person() Person {
	return Person{ID: c.ID, Name: c.Name, Role: c.Role}
}

func serveWS(hub *Hub, allowed map[string]bool, w http.ResponseWriter, r *http.Request) {
	role := Role(r.URL.Query().Get("role"))
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if role != RolePatient && role != RoleDoctor {
		http.Error(w, "role must be 'patient' or 'doctor'", http.StatusBadRequest)
		return
	}
	if name == "" || len(name) > 60 {
		http.Error(w, "name is required (max 60 chars)", http.StatusBadRequest)
		return
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return allowed[r.Header.Get("Origin")]
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade failed: %v", err)
		return
	}

	c := &Client{
		ID:   newID(),
		Name: name,
		Role: role,
		hub:  hub,
		conn: conn,
		send: make(chan []byte, sendBuffer),
	}
	hub.Register(c)

	go c.writePump()
	go c.readPump()
}

func (c *Client) readPump() {
	defer func() {
		c.hub.Unregister(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("read error (%s): %v", c.Name, err)
			}
			return
		}
		c.hub.HandleMessage(c, data)
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
