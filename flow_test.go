package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const origin = "http://localhost:5173"

type tc struct {
	ws *websocket.Conn
	me Person
}

func (c *tc) send(t *testing.T, typ string, p any) {
	t.Helper()
	if err := c.ws.WriteJSON(map[string]any{"type": typ, "payload": p}); err != nil {
		t.Fatal(err)
	}
}

// expect reads until a message of the given type arrives.
func (c *tc) expect(t *testing.T, typ string) json.RawMessage {
	t.Helper()
	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		var m struct {
			Type    string
			Payload json.RawMessage
		}
		if err := c.ws.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		if m.Type == typ {
			return m.Payload
		}
	}
}

func TestReceiptsAndMedia(t *testing.T) {
	hub := NewHub()
	allowed := map[string]bool{origin: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) { serveWS(hub, allowed, w, r) })
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) { handleUpload(hub, allowed, w, r) })
	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) { handleMedia(hub, allowed, w, r) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dial := func(role, name string) *tc {
		h := http.Header{"Origin": {origin}}
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?role="+role+"&name="+name, h)
		if err != nil {
			t.Fatal(err)
		}
		c := &tc{ws: ws}
		_ = json.Unmarshal((&tc{ws: ws}).expect(t, "welcome"), &c.me)
		return c
	}
	doc, pat := dial("doctor", "Dr"), dial("patient", "Pat")
	pat.send(t, "request_consult", map[string]string{"symptoms": "cough"})
	doc.expect(t, "queue")
	time.Sleep(100 * time.Millisecond)
	doc.send(t, "accept_patient", map[string]string{"patientId": pat.me.ID})
	doc.expect(t, "session_started")
	pat.expect(t, "session_started")

	upload := func(clientID, kind string, data []byte) (int, map[string]any) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "f")
		fw.Write(data)
		mw.Close()
		req, _ := http.NewRequest("POST", srv.URL+"/upload?clientId="+clientID+"&kind="+kind, &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGD4DwABBAEAX+XDSwAAAABJRU5ErkJggg==")
	if code, _ := upload(pat.me.ID, "image", []byte("not an image")); code != http.StatusUnsupportedMediaType {
		t.Fatalf("text as image: got %d", code)
	}
	if code, _ := upload("nobody", "image", png); code != http.StatusForbidden {
		t.Fatalf("unknown client: got %d", code)
	}
	code, up := upload(pat.me.ID, "image", png)
	if code != 200 {
		t.Fatalf("upload: %d", code)
	}

	// Voice note from the doctor, with a wrong kind rejected by chat.
	pat.send(t, "chat", map[string]any{"kind": "voice", "mediaId": up["mediaId"]})
	pat.expect(t, "error")

	pat.send(t, "chat", map[string]any{"kind": "image", "mediaId": up["mediaId"], "text": "rash"})
	var msg ChatMessage
	_ = json.Unmarshal(doc.expect(t, "chat"), &msg)
	if msg.Kind != "image" || msg.Media == nil || msg.Text != "rash" {
		t.Fatalf("bad chat message: %+v", msg)
	}
	res, _ := http.Get(srv.URL + msg.Media.URL)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, png) {
		t.Fatalf("media fetch: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	doc.send(t, "delivered", map[string]string{"messageId": msg.ID})
	var st MessageStatus
	_ = json.Unmarshal(pat.expect(t, "message_status"), &st)
	if st.Status != "delivered" {
		t.Fatalf("want delivered, got %s", st.Status)
	}
	doc.send(t, "read", map[string]any{"messageId": msg.ID, "upTo": true})
	_ = json.Unmarshal(pat.expect(t, "message_status"), &st)
	if st.Status != "read" {
		t.Fatalf("want read, got %s", st.Status)
	}

	doc.send(t, "end_session", nil)
	pat.expect(t, "session_ended")
	time.Sleep(100 * time.Millisecond)
	if res, _ := http.Get(srv.URL + msg.Media.URL); res.StatusCode != 404 {
		t.Fatalf("media should be gone after session ends, got %d", res.StatusCode)
	}
}
