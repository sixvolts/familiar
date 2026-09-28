package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The stamper rewrites each versioned asset ref in a shell to the
// content hash of the file it names: unchanged file, unchanged URL (warm
// caches); changed file, new URL (the service worker serves stamped URLs
// cache-first, so without this a deploy never reached installed apps).
// It had no tests.
func TestAssetStamper(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app.js", "console.log(1)")
	write("app.css", "body{}")
	write("mobile.html", `<link href="/app.css?v=hand1"><script src="/app.js?v=20260101a"></script>`+
		`<script src="/vendor/lib.min.js"></script><script src="/gone.js?v=7"></script><script src="/../etc/passwd.js?v=1"></script>`)
	s := newAssetStamper(dir)
	render := func() string {
		t.Helper()
		out, _, err := s.render(filepath.Join(dir, "mobile.html"))
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	versionOf := func(html, asset string) string {
		m := regexp.MustCompile(regexp.QuoteMeta(asset) + `\?v=([^"]*)`).FindStringSubmatch(html)
		if m == nil {
			t.Fatalf("no %s ref in %s", asset, html)
		}
		return m[1]
	}

	first := render()
	js1 := versionOf(first, "/app.js")
	if js1 == "20260101a" || js1 == "" {
		t.Errorf("/app.js kept its hand-written version %q", js1)
	}
	if versionOf(first, "/app.css") == "hand1" {
		t.Error("/app.css kept its hand-written version")
	}
	for _, untouched := range []string{`src="/vendor/lib.min.js"`, `/gone.js?v=7`, `/../etc/passwd.js?v=1`} {
		if !strings.Contains(first, untouched) {
			t.Errorf("%s was rewritten; unversioned, missing and outside refs stay as written", untouched)
		}
	}

	if again := versionOf(render(), "/app.js"); again != js1 {
		t.Errorf("unchanged file, new version: %q then %q", js1, again)
	}

	write("app.js", "console.log(2)")
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "app.js"), later, later); err != nil {
		t.Fatal(err)
	}
	if js2 := versionOf(render(), "/app.js"); js2 == js1 {
		t.Errorf("changed file kept version %q", js1)
	}
}
