package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// uiFiles is the review UI: a static single-page app with no build step.
//
//go:embed ui/*
var uiFiles embed.FS

// mountUI serves the review UI at / (index.html) and /ui/* (assets).
func (s *Server) mountUI() {
	static, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err) // embedded at build time
	}
	files := http.StripPrefix("/ui/", http.FileServer(http.FS(static)))
	s.router.Get("/ui/*", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	s.router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		index, err := fs.ReadFile(static, "index.html")
		if err != nil {
			http.Error(w, "UI not built", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		_, _ = w.Write(index)
	})
}
