package main

import (
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	hub := NewHub()

	allowed := map[string]bool{}
	origins := os.Getenv("ALLOWED_ORIGINS")
	if origins == "" {
		origins = "http://localhost:5173,http://localhost:5174"
	}
	for _, o := range strings.Split(origins, ",") {
		allowed[strings.TrimSpace(o)] = true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWS(hub, allowed, w, r)
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		handleUpload(hub, allowed, w, r)
	})
	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		handleMedia(hub, allowed, w, r)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	log.Printf("telehealth backend listening on %s (allowed origins: %s)", addr, origins)
	log.Fatal(http.ListenAndServe(addr, mux))
}
