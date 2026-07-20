package main

// stamp.go — content-hash cache busting for the HTML shells.
//
// The shells pin their assets with ?v= query params (see sw.js: assets
// are cache-first, so a changed file is invisible to clients until its
// URL changes). Hand-maintained date strings drift: chat.js changes,
// index.html keeps the old ?v=, and the deploy silently never reaches
// anyone who does not hard-refresh.
//
// Rather than maintain the version by hand, derive it: on the way out,
// rewrite every versioned asset ref to a short content hash of the file
// it points at. Change the file and the URL changes on its own; leave
// it alone and the URL is byte-identical, so caches stay warm.
//
// Hashes are computed lazily and cached against (mtime, size), and the
// rewrite happens per request rather than once at startup — the deploy
// script restarts the gateway but NOT the workspace, so a startup-only
// scheme would go stale on the very next deploy. Shell requests are
// page loads, so the cost is a handful of stats.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// assetRefRe matches a same-origin asset reference that already carries
// a cache-busting param, e.g. /chat.js?v=20260720a or /app.css?v=3.
// Refs without a ?v= (vendored bundles) are deliberately left alone.
var assetRefRe = regexp.MustCompile(`(/[A-Za-z0-9_\-./]+\.(?:js|css))\?v=[A-Za-z0-9_.\-]*`)

type hashEntry struct {
	modTime time.Time
	size    int64
	hash    string
}

// assetStamper rewrites ?v= params in an HTML shell into content hashes
// of the assets they reference. Safe for concurrent use.
type assetStamper struct {
	staticDir string
	mu        sync.RWMutex
	cache     map[string]hashEntry
}

func newAssetStamper(staticDir string) *assetStamper {
	return &assetStamper{staticDir: staticDir, cache: make(map[string]hashEntry)}
}

// hashFor returns a short content hash for a URL path, plus the asset's
// mtime. ok is false when the ref does not resolve to a readable file
// inside staticDir — callers then leave the original ref untouched, so a
// typo or an external URL can never break page rendering.
func (s *assetStamper) hashFor(urlPath string) (hash string, modTime time.Time, ok bool) {
	full := filepath.Join(s.staticDir, filepath.Clean("/"+strings.TrimPrefix(urlPath, "/")))
	if !strings.HasPrefix(full, s.staticDir) {
		return "", time.Time{}, false
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return "", time.Time{}, false
	}

	s.mu.RLock()
	entry, cached := s.cache[full]
	s.mu.RUnlock()
	if cached && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
		return entry.hash, entry.modTime, true
	}

	data, err := os.ReadFile(full)
	if err != nil {
		return "", time.Time{}, false
	}
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])[:10]

	s.mu.Lock()
	s.cache[full] = hashEntry{modTime: info.ModTime(), size: info.Size(), hash: h}
	s.mu.Unlock()
	return h, info.ModTime(), true
}

// render returns the shell with every versioned asset ref rewritten to a
// content hash, and the newest mtime across the shell and the assets it
// stamped — so conditional requests revalidate when any asset changes,
// not just when the shell itself is edited.
func (s *assetStamper) render(shellPath string) ([]byte, time.Time, error) {
	info, err := os.Stat(shellPath)
	if err != nil {
		return nil, time.Time{}, err
	}
	body, err := os.ReadFile(shellPath)
	if err != nil {
		return nil, time.Time{}, err
	}

	newest := info.ModTime()
	out := assetRefRe.ReplaceAllFunc(body, func(match []byte) []byte {
		sub := assetRefRe.FindSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		ref := string(sub[1])
		h, mod, ok := s.hashFor(ref)
		if !ok {
			return match
		}
		if mod.After(newest) {
			newest = mod
		}
		return []byte(ref + "?v=" + h)
	})
	return out, newest, nil
}

// serveShell writes an HTML shell with stamped asset refs. On any error
// it falls back to serving the file untouched: stale caching is a much
// smaller problem than a blank page.
func (s *assetStamper) serveShell(w http.ResponseWriter, r *http.Request, shellPath string) {
	body, modTime, err := s.render(shellPath)
	if err != nil {
		http.ServeFile(w, r, shellPath)
		return
	}
	http.ServeContent(w, r, filepath.Base(shellPath), modTime, bytes.NewReader(body))
}
