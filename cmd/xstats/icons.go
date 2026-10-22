package main

import (
	"io/fs"
	"net/http"
)

// iconCacheControl is a day. The files are embedded in the binary and change
// only when a new build is deployed, and the names are not content-hashed.
const iconCacheControl = "public, max-age=86400"

// iconFiles are static branding. They carry no account data.
// Basic auth, when the public site uses it, is enforced by Caddy. The
// Caddyfile leaves these paths open because browsers and iOS request a tab
// icon and apple-touch-icon without the dashboard credentials. This process
// only checks a bearer token on /api/ingest and /api/health.
//
// apple-touch-icon-precomposed.png is the legacy iOS name for the same PNG.
var iconFiles = map[string]struct {
	file  string
	ctype string
}{
	"/favicon.svg":                      {"favicon.svg", "image/svg+xml"},
	"/favicon.ico":                      {"favicon.ico", "image/vnd.microsoft.icon"},
	"/favicon-16.png":                   {"favicon-16.png", "image/png"},
	"/favicon-32.png":                   {"favicon-32.png", "image/png"},
	"/favicon-192.png":                  {"favicon-192.png", "image/png"},
	"/favicon-512.png":                  {"favicon-512.png", "image/png"},
	"/apple-touch-icon.png":             {"apple-touch-icon.png", "image/png"},
	"/apple-touch-icon-precomposed.png": {"apple-touch-icon.png", "image/png"},
	"/site.webmanifest":                 {"site.webmanifest", "application/manifest+json"},
}

func registerIcons(mux *http.ServeMux, root fs.FS) {
	for path, spec := range iconFiles {
		path, spec := path, spec
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			b, err := fs.ReadFile(root, spec.file)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", spec.ctype)
			w.Header().Set("Cache-Control", iconCacheControl)
			w.Write(b)
		})
	}
}
