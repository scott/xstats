package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const otherSitesImport = "import /etc/caddy/*.caddy"

func readCaddyTemplate(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "Caddyfile.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// set-auth.sh rewrites /etc/caddy/Caddyfile from the template on every
// deploy, so other sites on the server (xpost's /etc/caddy/xpost.caddy) stay
// served only if the template itself imports them: once, at top level.
func TestCaddyfileImportsOtherSites(t *testing.T) {
	src := readCaddyTemplate(t)
	depth, found := 0, 0
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "import ") {
			if line != otherSitesImport {
				t.Errorf("unexpected import %q", line)
			}
			if depth != 0 {
				t.Errorf("%q is inside a block (depth %d); it must be top level", line, depth)
			}
			found++
		}
		if !strings.HasPrefix(line, "#") {
			depth += strings.Count(line, "{") - strings.Count(line, "}")
		}
	}
	if found != 1 {
		t.Fatalf("want exactly one %q, found %d", otherSitesImport, found)
	}
	glob := strings.TrimPrefix(otherSitesImport, "import ")
	for name, want := range map[string]bool{
		"xpost.caddy":                true,
		"Caddyfile":                  false,
		"Caddyfile.new":              false, // set-auth.sh's staging file
		"Caddyfile.bak-202610220242": false, // xstats deploy backups
		"Caddyfile.bak-xpost-20261102T004318Z-123": false, // xpost provision backups
		"xpost.caddy.bak-xpost-20261102T004318Z-1": false,
		"xpost.caddy.new":                          false,
	} {
		got, err := filepath.Match(glob, "/etc/caddy/"+name)
		if err != nil || got != want {
			t.Errorf("%s matches %s: %v (want %v, err %v)", glob, name, got, want, err)
		}
	}
}

// With a caddy binary on PATH (the box has one; CI usually does not), render
// the template the way set-auth.sh does and validate it with no other sites,
// with one, and check adapt routes both hosts.
func TestCaddyfileTemplateValidates(t *testing.T) {
	caddy, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("caddy not on PATH")
	}
	hash, err := exec.Command(caddy, "hash-password", "--plaintext", "example-test-password").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := strings.NewReplacer("__HASH__", strings.TrimSpace(string(hash)), "__DOMAIN__", "stats.example.com", "__USER__", "example", "/etc/caddy/", dir+"/")
	cf := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(cf, []byte(r.Replace(readCaddyTemplate(t))), 0o644); err != nil {
		t.Fatal(err)
	}
	validate := func(what string) {
		t.Helper()
		if out, err := exec.Command(caddy, "validate", "--config", cf, "--adapter", "caddyfile").CombinedOutput(); err != nil {
			t.Fatalf("%s: caddy validate: %v\n%s", what, err, out)
		}
	}
	validate("no other sites")
	// Backups and the staging file must not be pulled in (they would duplicate the stats site).
	b, _ := os.ReadFile(cf)
	for _, n := range []string{"Caddyfile.new", "Caddyfile.bak-202610220242", "Caddyfile.bak-xpost-20261102T004318Z-1"} {
		os.WriteFile(filepath.Join(dir, n), b, 0o644)
	}
	site := "post.example.com {\n\treverse_proxy 127.0.0.1:8091\n}\n"
	os.WriteFile(filepath.Join(dir, "xpost.caddy"), []byte(site), 0o644)
	validate("with xpost.caddy and backup decoys")
	out, err := exec.Command(caddy, "adapt", "--config", cf, "--adapter", "caddyfile").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"stats.example.com"`, `"post.example.com"`, `"127.0.0.1:8080"`, `"127.0.0.1:8091"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("adapt output lacks %s", want)
		}
	}
}
