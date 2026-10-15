package main

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"
)

const maxChatLen = 2000

type Session struct {
	ID        string
	Doctor    *Client
	Patient   *Client
	StartedAt time.Time
	// msgs tracks each chat message's sender (for routing receipts) and the
	// peer's progress; ordered holds IDs in send order for "read up to".
	msgs    map[string]*msgState
	ordered []string
}

type msgState struct {
	sender    *Client
	delivered bool
	read      bool
}

func (s *Session) peerOf(c *Client) *Client {
	if c == s.Doctor {
		return s.Patient
	}
	return s.Doctor
}

// Hub owns all shared state. Every exported method takes the lock;
// lowercase helpers assume the lock is already held.
type Hub struct {
	mu       sync.Mutex
	clients  map[string]*Client
	queue    []QueueEntry
	sessions map[string]*Session
	media    map[string]*mediaFile
}

func NewHub() *Hub {
	return &Hub{
		clients:  map[string]*Client{},
		sessions: map[string]*Session{},
		media:    map[string]*mediaFile{},
	}
}

func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.clients[c.ID] = c
	log.Printf("+ %s %q connected (%s)", c.Role, c.Name, c.ID)

	h.sendTo(c, "welcome", c.person())
	if c.Role == RoleDoctor {
		h.sendTo(c, "queue", h.queue)
	}
	h.broadcastPresence()
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.clients[c.ID]; !ok {
		return
	}
	delete(h.clients, c.ID)
	log.Printf("- %s %q disconnected", c.Role, c.Name)

	if c.Role == RolePatient && h.removeFromQueue(c.ID) {
		h.broadcastQueue()
	}
	if s, ok := h.sessions[c.sessionID]; ok {
		h.endSession(s, string(c.Role)+"_disconnected")
	}

	c.closed = true
	close(c.send)
	h.broadcastPresence()
}

func (h *Hub) HandleMessage(c *Client, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var msg Incoming
	if err := json.Unmarshal(data, &msg); err != nil {
		h.sendError(c, "Message must be JSON: {\"type\": ..., \"payload\": ...}")
		return
	}

	switch msg.Type {
	case "request_consult":
		h.handleRequestConsult(c, msg.Payload)
	case "cancel_request":
		h.handleCancelRequest(c)
	case "accept_patient":
		h.handleAcceptPatient(c, msg.Payload)
	case "chat":
		h.handleChat(c, msg.Payload)
	case "typing":
		h.handleTyping(c, msg.Payload)
	case "delivered":
		h.handleReceipt(c, msg.Payload, "delivered")
	case "read":
		h.handleReceipt(c, msg.Payload, "read")
	case "end_session":
		if s, ok := h.sessions[c.sessionID]; ok {
			h.endSession(s, "ended_by_"+string(c.Role))
		}
	default:
		h.sendError(c, "Unknown message type: "+msg.Type)
	}
}

// ---------------- handlers ----------------

func (h *Hub) handleRequestConsult(c *Client, raw json.RawMessage) {
	if c.Role != RolePatient {
		h.sendError(c, "Only patients can request a consultation.")
		return
	}
	if c.sessionID != "" || h.queueIndex(c.ID) >= 0 {
		h.sendError(c, "You already have an open request or consultation.")
		return
	}
	var p RequestConsultPayload
	_ = json.Unmarshal(raw, &p)
	symptoms := strings.TrimSpace(p.Symptoms)
	if symptoms == "" {
		h.sendError(c, "Describe your symptoms so the doctor can prepare.")
		return
	}
	if len(symptoms) > 1000 {
		symptoms = symptoms[:1000]
	}

	h.queue = append(h.queue, QueueEntry{
		PatientID:   c.ID,
		Name:        c.Name,
		Symptoms:    symptoms,
		RequestedAt: time.Now().UTC(),
	})
	h.broadcastQueue()
	h.broadcastPresence()
}

func (h *Hub) handleCancelRequest(c *Client) {
	if h.removeFromQueue(c.ID) {
		h.sendTo(c, "request_cancelled", nil)
		h.broadcastQueue()
		h.broadcastPresence()
	}
}

func (h *Hub) handleAcceptPatient(c *Client, raw json.RawMessage) {
	if c.Role != RoleDoctor {
		h.sendError(c, "Only doctors can accept patients.")
		return
	}
	if c.sessionID != "" {
		h.sendError(c, "Finish your current consultation before accepting another patient.")
		return
	}
	var p AcceptPatientPayload
	_ = json.Unmarshal(raw, &p)

	idx := h.queueIndex(p.PatientID)
	patient, online := h.clients[p.PatientID]
	if idx < 0 || !online {
		h.sendError(c, "That patient is no longer waiting.")
		h.sendTo(c, "queue", h.queue)
		return
	}
	entry := h.queue[idx]
	h.removeFromQueue(p.PatientID)

	s := &Session{
		ID:        newID(),
		Doctor:    c,
		Patient:   patient,
		StartedAt: time.Now().UTC(),
		msgs:      map[string]*msgState{},
	}
	h.sessions[s.ID] = s
	c.sessionID = s.ID
	patient.sessionID = s.ID
	log.Printf("session %s: Dr %q <-> %q", s.ID, c.Name, patient.Name)

	h.sendTo(c, "session_started", SessionStarted{
		SessionID: s.ID, Peer: patient.person(), Symptoms: entry.Symptoms, StartedAt: s.StartedAt,
	})
	h.sendTo(patient, "session_started", SessionStarted{
		SessionID: s.ID, Peer: c.person(), Symptoms: entry.Symptoms, StartedAt: s.StartedAt,
	})
	h.broadcastQueue()
	h.broadcastPresence()
}

func (h *Hub) handleChat(c *Client, raw json.RawMessage) {
	s, ok := h.sessions[c.sessionID]
	if !ok {
		h.sendError(c, "You're not in a consultation.")
		return
	}
	var p ChatPayload
	_ = json.Unmarshal(raw, &p)
	text := strings.TrimSpace(p.Text)
	if len(text) > maxChatLen {
		text = text[:maxChatLen]
	}
	kind := p.Kind
	if kind == "" {
		kind = "text"
	}

	m := ChatMessage{
		ID:        newID(),
		SessionID: s.ID,
		From:      c.person(),
		Kind:      kind,
		Text:      text,
		SentAt:    time.Now().UTC(),
	}
	switch kind {
	case "text":
		if text == "" {
			return
		}
	case "image", "voice":
		f, ok := h.media[p.MediaID]
		if !ok || f.sessionID != s.ID || f.uploader != c.ID || f.kind != kind || f.used {
			h.sendError(c, "Attachment not found. Upload it again.")
			return
		}
		f.used = true
		m.Media = &Media{ID: f.id, URL: "/media/" + f.id, Mime: f.mime, Size: f.size}
		if kind == "voice" && p.DurationMs > 0 && p.DurationMs <= 15*60*1000 {
			m.Media.DurationMs = p.DurationMs
		}
	default:
		h.sendError(c, "Unknown chat kind: "+kind)
		return
	}

	s.msgs[m.ID] = &msgState{sender: c}
	s.ordered = append(s.ordered, m.ID)
	// Echo to the sender too, so both sides render the server's copy.
	h.sendTo(s.Doctor, "chat", m)
	h.sendTo(s.Patient, "chat", m)
}

// handleReceipt processes "delivered"/"read" from the recipient and relays a
// "message_status" to the original sender. Receipts only move forward
// (delivered -> read) and are ignored for your own messages.
func (h *Hub) handleReceipt(c *Client, raw json.RawMessage, status string) {
	s, ok := h.sessions[c.sessionID]
	if !ok {
		return
	}
	var p ReceiptPayload
	_ = json.Unmarshal(raw, &p)
	st, ok := s.msgs[p.MessageID]
	if !ok || st.sender == c {
		return
	}
	ids := []string{p.MessageID}
	if status == "read" && p.UpTo {
		ids = ids[:0]
		for _, id := range s.ordered {
			if s.msgs[id].sender != c {
				ids = append(ids, id)
			}
			if id == p.MessageID {
				break
			}
		}
	}
	now := time.Now().UTC()
	for _, id := range ids {
		m := s.msgs[id]
		if status == "delivered" && (m.delivered || m.read) {
			continue
		}
		if status == "read" && m.read {
			continue
		}
		m.delivered = true
		if status == "read" {
			m.read = true
		}
		h.sendTo(m.sender, "message_status", MessageStatus{
			SessionID: s.ID, MessageID: id, Status: status, At: now,
		})
	}
}

func (h *Hub) handleTyping(c *Client, raw json.RawMessage) {
	s, ok := h.sessions[c.sessionID]
	if !ok {
		return
	}
	var p TypingPayload
	_ = json.Unmarshal(raw, &p)
	h.sendTo(s.peerOf(c), "typing", p)
}

// ---------------- helpers (lock held) ----------------

func (h *Hub) endSession(s *Session, reason string) {
	delete(h.sessions, s.ID)
	h.deleteSessionMedia(s.ID)
	s.Doctor.sessionID = ""
	s.Patient.sessionID = ""
	ended := SessionEnded{SessionID: s.ID, Reason: reason}
	h.sendTo(s.Doctor, "session_ended", ended)
	h.sendTo(s.Patient, "session_ended", ended)
	log.Printf("session %s ended: %s", s.ID, reason)
	h.broadcastPresence()
}

func (h *Hub) queueIndex(patientID string) int {
	for i, e := range h.queue {
		if e.PatientID == patientID {
			return i
		}
	}
	return -1
}

func (h *Hub) removeFromQueue(patientID string) bool {
	i := h.queueIndex(patientID)
	if i < 0 {
		return false
	}
	h.queue = append(h.queue[:i], h.queue[i+1:]...)
	return true
}

// broadcastQueue sends the full queue to doctors and each waiting
// patient their own position.
func (h *Hub) broadcastQueue() {
	for _, c := range h.clients {
		if c.Role == RoleDoctor {
			h.sendTo(c, "queue", h.queue)
		}
	}
	for i, e := range h.queue {
		if p, ok := h.clients[e.PatientID]; ok {
			h.sendTo(p, "queue_position", map[string]int{"position": i + 1})
		}
	}
}

func (h *Hub) broadcastPresence() {
	var p Presence
	for _, c := range h.clients {
		if c.Role == RoleDoctor {
			p.DoctorsOnline++
			if c.sessionID == "" {
				p.DoctorsAvailable++
			}
		}
	}
	p.PatientsWaiting = len(h.queue)
	for _, c := range h.clients {
		h.sendTo(c, "presence", p)
	}
}

func (h *Hub) sendError(c *Client, message string) {
	h.sendTo(c, "error", map[string]string{"message": message})
}

// sendTo never blocks: a client whose buffer is full gets the message dropped.
func (h *Hub) sendTo(c *Client, msgType string, payload any) {
	if c == nil || c.closed {
		return
	}
	b, err := json.Marshal(Outgoing{Type: msgType, Payload: payload})
	if err != nil {
		log.Printf("marshal %s: %v", msgType, err)
		return
	}
	select {
	case c.send <- b:
	default:
		log.Printf("dropping %s for slow client %q", msgType, c.Name)
	}
}
