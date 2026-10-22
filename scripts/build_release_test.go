package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

func TestBuildReleaseAssets(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(file))
	out := t.TempDir()

	cmd := exec.Command(filepath.Join(root, "scripts", "build-release.sh"), "v0.0.0-test", "abc123", out)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build-release: %v\n%s", err, b)
	}

	required := []string{"xstats.service", "Caddyfile.tmpl", "set-auth.sh"}
	sources := []string{"deploy/xstats.service", "deploy/Caddyfile.tmpl", "deploy/set-auth.sh"}
	for i, name := range required {
		got, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		src, err := os.ReadFile(filepath.Join(root, sources[i]))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, src) {
			t.Fatalf("%s does not match %s", name, sources[i])
		}
	}

	names := []string{
		"xstats-linux-amd64",
		"xstats-linux-arm64",
		"xstats.service",
		"Caddyfile.tmpl",
		"set-auth.sh",
	}
	bootstrap := filepath.Join(root, "deploy", "bootstrap.sh")
	if _, err := os.Stat(bootstrap); err == nil {
		names = append(names, "bootstrap.sh")
		got, err := os.ReadFile(filepath.Join(out, "bootstrap.sh"))
		if err != nil {
			t.Fatal(err)
		}
		src, err := os.ReadFile(bootstrap)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, src) {
			t.Fatal("bootstrap.sh does not match deploy/bootstrap.sh")
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	} else if _, err := os.Stat(filepath.Join(out, "bootstrap.sh")); !os.IsNotExist(err) {
		t.Fatal("bootstrap.sh staged but deploy/bootstrap.sh is absent")
	}

	sums, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		re := regexp.MustCompile(`(?m)^[0-9a-f]{64}  ` + regexp.QuoteMeta(name) + `$`)
		if !re.Match(sums) {
			t.Errorf("SHA256SUMS has no entry for %s\n%s", name, sums)
		}
	}
	lines := bytes.Split(bytes.TrimSuffix(sums, []byte("\n")), []byte("\n"))
	if len(lines) != len(names) {
		t.Fatalf("SHA256SUMS has %d lines, want %d:\n%s", len(lines), len(names), sums)
	}

	check := exec.Command("sha256sum", "-c", "SHA256SUMS")
	check.Dir = out
	if b, err := check.CombinedOutput(); err != nil {
		t.Fatalf("sha256sum -c: %v\n%s", err, b)
	}
}
