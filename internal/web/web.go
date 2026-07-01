// Package web serves the embedded single-file admin UI. It is a zero-build
// vanilla HTML/CSS/JS app that talks to the existing /admin and /v1 REST
// endpoints, so the server stays a single self-contained binary.
package web

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
)

//go:embed assets/*
var assets embed.FS

// Handler returns an http.Handler that serves the embedded UI assets. index.html
// is served at the mount root.
func Handler() http.Handler {
	return assetsHandler(assets, "assets")
}

func assetsHandler(root fs.FS, dir string) http.Handler {
	if st, err := fs.Stat(root, dir); err != nil || !st.IsDir() {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveAssetsUnavailable(w)
		})
	}
	sub, err := fs.Sub(root, dir)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveAssetsUnavailable(w)
		})
	}
	return http.FileServer(http.FS(sub))
}

func serveAssetsUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, "embedded web assets unavailable\n")
}
