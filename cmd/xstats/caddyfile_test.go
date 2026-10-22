package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The repo has no Caddy renderer. This checks the template directly: /api/health
// shares the ingest matcher, and that handle does not enable basic auth.
func TestCaddyfileHealthIsTokenOnly(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "Caddyfile.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	matchers, handles := parseCaddyHandles(string(b))
	health := ""
	for name, paths := range matchers {
		if !containsPath(paths, "/api/health") {
			continue
		}
		if health != "" {
			t.Fatalf("/api/health is listed on @%s and @%s", health, name)
		}
		health = name
		if !containsPath(paths, "/api/ingest") {
			t.Fatalf("@%s lists /api/health without /api/ingest: %v", name, paths)
		}
	}
	if health == "" {
		t.Fatal("no matcher lists /api/health")
	}
	tokenHandle := false
	basicHandle := false
	for _, h := range handles {
		if h.name == health {
			tokenHandle = true
			if strings.Contains(h.body, "basic_auth") {
				t.Fatalf("@%s handle enables basic_auth:\n%s", health, h.body)
			}
		}
		if strings.Contains(h.body, "basic_auth") {
			basicHandle = true
			if strings.Contains(h.body, "/api/health") || strings.Contains(h.body, "/api/ingest") {
				t.Fatalf("basic_auth handle names a token route:\n%s", h.body)
			}
		}
	}
	if !tokenHandle {
		t.Fatalf("no handle for @%s", health)
	}
	if !basicHandle {
		t.Fatal("dashboard basic_auth handle is missing")
	}
}

type caddyHandle struct {
	name string
	body string
}

func parseCaddyHandles(src string) (map[string][]string, []caddyHandle) {
	matchers := map[string][]string{}
	var handles []caddyHandle
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "@") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[1] == "path" {
				name := strings.TrimPrefix(fields[0], "@")
				matchers[name] = append(matchers[name], fields[2:]...)
			}
			continue
		}
		if line != "handle {" && !strings.HasPrefix(line, "handle @") {
			continue
		}
		name := ""
		if strings.HasPrefix(line, "handle @") {
			name = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "handle @"), "{"))
		}
		var body strings.Builder
		depth := strings.Count(line, "{") - strings.Count(line, "}")
		for depth > 0 && i+1 < len(lines) {
			i++
			body.WriteString(lines[i])
			body.WriteByte('\n')
			depth += strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		}
		handles = append(handles, caddyHandle{name: name, body: body.String()})
	}
	return matchers, handles
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}
