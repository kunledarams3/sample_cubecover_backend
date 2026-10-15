package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Incoming is what clients send: {"type": "...", "payload": {...}}
type Incoming struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Outgoing is what the server sends back.
type Outgoing struct {
	Type    string `json:"type"`
	Payload any    `json:"payload,omitempty"`
}

// ---- client -> server payloads ----

type RequestConsultPayload struct {
	Symptoms string `json:"symptoms"`
}

type AcceptPatientPayload struct {
	PatientID string `json:"patientId"`
}

// ChatPayload: kind is "text" (default), "image" or "voice". For image/voice,
// mediaId comes from POST /upload and text is an optional caption.
type ChatPayload struct {
	Kind       string `json:"kind,omitempty"`
	Text       string `json:"text,omitempty"`
	MediaID    string `json:"mediaId,omitempty"`
	DurationMs int    `json:"durationMs,omitempty"`
}

// ReceiptPayload is sent by the recipient for "delivered" and "read" events.
// "read" with upTo=true marks every message from the peer up to messageId as read.
type ReceiptPayload struct {
	MessageID string `json:"messageId"`
	UpTo      bool   `json:"upTo,omitempty"`
}

type TypingPayload struct {
	IsTyping bool `json:"isTyping"`
}

// MessageStatus is sent to the original sender: status is "delivered" or "read".
type MessageStatus struct {
	SessionID string    `json:"sessionId"`
	MessageID string    `json:"messageId"`
	Status    string    `json:"status"`
	At        time.Time `json:"at"`
}

// Media describes an uploaded attachment on a chat message.
type Media struct {
	ID         string `json:"id"`
	URL        string `json:"url"` // relative: "/media/<id>"; prefix with the backend origin
	Mime       string `json:"mime"`
	Size       int64  `json:"size"`
	DurationMs int    `json:"durationMs,omitempty"`
}

// ---- server -> client payloads ----

type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role Role   `json:"role"`
}

type QueueEntry struct {
	PatientID   string    `json:"patientId"`
	Name        string    `json:"name"`
	Symptoms    string    `json:"symptoms"`
	RequestedAt time.Time `json:"requestedAt"`
}

type ChatMessage struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	From      Person    `json:"from"`
	Kind      string    `json:"kind"` // text | image | voice
	Text      string    `json:"text,omitempty"`
	Media     *Media    `json:"media,omitempty"`
	SentAt    time.Time `json:"sentAt"`
}

type SessionStarted struct {
	SessionID string    `json:"sessionId"`
	Peer      Person    `json:"peer"`
	Symptoms  string    `json:"symptoms"`
	StartedAt time.Time `json:"startedAt"`
}

type SessionEnded struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason"`
}

type Presence struct {
	DoctorsOnline    int `json:"doctorsOnline"`
	DoctorsAvailable int `json:"doctorsAvailable"`
	PatientsWaiting  int `json:"patientsWaiting"`
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
