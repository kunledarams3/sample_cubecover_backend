package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxImageSize = 10 << 20 // 10 MB
	maxVoiceSize = 10 << 20
)

type mediaFile struct {
	id        string
	sessionID string
	uploader  string
	kind      string // image | voice
	mime      string
	size      int64
	path      string
	used      bool // attached to a chat message
}

var mediaDir = func() string {
	d := os.Getenv("MEDIA_DIR")
	if d == "" {
		d = filepath.Join(os.TempDir(), "telehealth-media")
	}
	_ = os.MkdirAll(d, 0o700)
	return d
}()

var imageMimes = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
}

// Voice notes: browsers record webm/ogg, iOS/Android record mp4/m4a/aac/3gp.
// http.DetectContentType reports mp4/webm containers as video/*, so accept those too.
var voiceMimes = map[string]string{
	"audio/ogg": ".ogg", "application/ogg": ".ogg", "audio/mpeg": ".mp3", "audio/wave": ".wav",
	"video/webm": ".webm", "video/mp4": ".m4a",
}

func setCORS(w http.ResponseWriter, r *http.Request, allowed map[string]bool) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client (native app)
	}
	if !allowed[origin] {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	return true
}

// POST /upload?clientId=<id>&kind=image|voice  (multipart field "file")
// clientId is the id from the websocket "welcome" message. The uploader must be
// in a consultation. Returns {"mediaId","url","kind","mime","size"}; then send a
// websocket {"type":"chat","payload":{"kind":...,"mediaId":...}}.
func handleUpload(hub *Hub, allowed map[string]bool, w http.ResponseWriter, r *http.Request) {
	if !setCORS(w, r, allowed) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "image" && kind != "voice" {
		http.Error(w, "kind must be 'image' or 'voice'", http.StatusBadRequest)
		return
	}
	limit := int64(maxImageSize)
	if kind == "voice" {
		limit = maxVoiceSize
	}
	sessionID, ok := hub.UploadSession(r.URL.Query().Get("clientId"))
	if !ok {
		http.Error(w, "not in a consultation", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "multipart field 'file' required (or file too large)", http.StatusBadRequest)
		return
	}
	defer file.Close()

	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	mime := http.DetectContentType(head[:n])
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = mime[:i]
	}
	allowedMimes := imageMimes
	if kind == "voice" {
		allowedMimes = voiceMimes
	}
	ext, ok := allowedMimes[mime]
	if !ok {
		http.Error(w, "unsupported file type: "+mime, http.StatusUnsupportedMediaType)
		return
	}

	id := newID()
	path := filepath.Join(mediaDir, id+ext)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		log.Printf("upload create: %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	_, _ = out.Write(head[:n])
	rest, err := io.Copy(out, io.LimitReader(file, limit+1))
	out.Close()
	size := int64(n) + rest
	if err != nil || size > limit {
		os.Remove(path)
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}
	f := &mediaFile{id: id, sessionID: sessionID, uploader: r.URL.Query().Get("clientId"),
		kind: kind, mime: mime, size: size, path: path}
	if !hub.AddMedia(f) {
		os.Remove(path)
		http.Error(w, "not in a consultation", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"mediaId":"` + id + `","url":"/media/` + id + `","kind":"` + kind +
		`","mime":"` + mime + `","size":` + itoa(size) + `}`))
}

// GET /media/<id>: only available while the consultation is open; ids are unguessable.
func handleMedia(hub *Hub, allowed map[string]bool, w http.ResponseWriter, r *http.Request) {
	if !setCORS(w, r, allowed) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	f, ok := hub.GetMedia(strings.TrimPrefix(r.URL.Path, "/media/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", f.mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, f.path) // supports Range requests for audio seeking
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ---- hub helpers (these take the lock) ----

func (h *Hub) UploadSession(clientID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.clients[clientID]
	if !ok || c.sessionID == "" {
		return "", false
	}
	return c.sessionID, true
}

func (h *Hub) AddMedia(f *mediaFile) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.sessions[f.sessionID]; !ok {
		return false
	}
	h.media[f.id] = f
	return true
}

func (h *Hub) GetMedia(id string) (*mediaFile, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.media[id]
	if !ok || !f.used {
		return nil, false
	}
	return f, true
}

// deleteSessionMedia (lock held) removes files when a consultation ends.
func (h *Hub) deleteSessionMedia(sessionID string) {
	for id, f := range h.media {
		if f.sessionID == sessionID {
			os.Remove(f.path)
			delete(h.media, id)
		}
	}
}
