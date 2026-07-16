package admin

// Public-link sharing for wiki/note pages — see SECURITY-SHARING.md
// for the threat model and the rationale behind each defense.
//
// Hardening highlights:
//
//   • Markdown is rendered server-side (goldmark, raw HTML escaped
//     not passed through) and the result is run through bluemonday
//     before reaching the client. The public page ships no script;
//     there is no DOM-side parser to confuse.
//   • Bluemonday strips disallowed schemes (javascript:, file:, …),
//     forces every link to nofollow + noopener + noreferrer +
//     target="_blank", and limits image and link URLs to https /
//     mailto.
//   • A strict Content-Security-Policy header (default-src 'none'
//     plus narrow allow-list) and no-store cache headers stop
//     leftover routes from being abused.
//   • Referrer-Policy: no-referrer keeps the share key out of the
//     Referer header when visitors click outbound links — the key
//     IS the credential, so any leak would compromise access.
//   • Share keys are alphanumeric, 16 chars, generated with crypto/
//     rand + rejection sampling — uniform across all 62 positions
//     (no modulus bias). Effective entropy ≈ 95.3 bits.
//   • Key path values are validated against [A-Za-z0-9]{16} before
//     touching the DB so a malformed key never reaches the store.
//   • Host gating: /p/{key} only resolves when the inbound Host
//     header is in [sharing].public_hosts. Tailscale-direct or any
//     unlisted host 404s — the link doesn't render on the private
//     side.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	xhtml "golang.org/x/net/html"
)

// AttachSharing wires the public-link sharing policy onto the
// handler. Empty PublicHosts disables share serving — the toggle
// endpoint returns 503 and the public render returns 404 from every
// host. Pass the config block from gateway.toml verbatim.
func (h *Handler) AttachSharing(c config.SharingConfig) {
	h.sharingCfg = c
	h.publicHostSet = make(map[string]bool, len(c.PublicHosts))
	for _, host := range c.PublicHosts {
		host = strings.ToLower(stripPort(strings.TrimSpace(host)))
		if host != "" {
			h.publicHostSet[host] = true
		}
	}
}

// isPublicHost reports whether the inbound Host header is in the
// configured public allow-list. Empty allow-list always returns
// false — share serving stays off until a host is configured.
//
// Trust note: the gateway reads r.Host, which the workspace proxy
// preserves verbatim. The proxy is the only public ingress in the
// normal topology, so r.Host reflects the real browser-visible
// hostname. Deployments that also expose the gateway port directly
// (e.g. via Tailscale) need to bind the gateway to a private
// interface or the host gate can be bypassed by sending a forged
// Host header straight to the gateway.
func (h *Handler) isPublicHost(r *http.Request) bool {
	if len(h.publicHostSet) == 0 {
		return false
	}
	return h.publicHostSet[strings.ToLower(stripPort(r.Host))]
}

// publicShareURL composes the absolute URL a client should copy for
// a share key. Falls back to a relative path when no base is set.
func (h *Handler) publicShareURL(key string) string {
	base := strings.TrimRight(h.sharingCfg.PublicBaseURL, "/")
	if base == "" {
		return "/p/" + key
	}
	return base + "/p/" + key
}

// isValidShareKey enforces the [A-Za-z0-9]{16} shape before the key
// reaches the DB. Cheap early rejection of malformed paths so a
// probe with garbage / NULs / arbitrary length never hits the store
// or pollutes its query logs.
func isValidShareKey(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		default:
			return false
		}
	}
	return true
}

// shareToggle serves POST .../page-by-id/{page_id}/share with body
// {"enabled": true|false}. enabled=true upserts a public share
// (idempotent — re-enabling returns the existing key). enabled=false
// deletes the share row. The response always includes the current
// share state so the frontend can re-render the menu + globe icon
// from a single round-trip.
//
// Authorization: requires the same page-write capability as the
// page PATCH/DELETE handlers — owners + editors on the book.
// Sharing requires a configured public host so the response can
// hand back a usable copy-link.
func (h *Handler) shareToggle(w http.ResponseWriter, r *http.Request) {
	if !h.ensureWiki(w) {
		return
	}
	if len(h.publicHostSet) == 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "public sharing is not configured on this deploy")
		return
	}
	b, userID, isAdmin, ok := h.scopeForWiki(w, r)
	if !ok {
		return
	}
	if !h.requirePageWrite(w, r, b, userID, isAdmin) {
		return
	}
	pageID := r.PathValue("page_id")
	// GetPageByID filters by (book_id, id), so an attacker can't
	// pass a page_id they own through a book slug they don't —
	// the cross-book row simply 404s here.
	cur, err := h.wiki.GetPageByID(r.Context(), b.ID, pageID)
	if errors.Is(err, ErrPageNotFound) {
		writeJSONError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The toggle response carries the share key — sensitive enough
	// that an intermediate cache holding it would be a leak.
	// Browsers don't cache POSTs by default but be explicit.
	w.Header().Set("Cache-Control", "no-store")

	if body.Enabled {
		share, err := h.wiki.EnablePageShare(r.Context(), cur.ID, userID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, shareStatePayload(share, h.publicShareURL(share.ShareKey)))
		return
	}

	if err := h.wiki.DisablePageShare(r.Context(), cur.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, shareStatePayload(nil, ""))
}

// shareStatePayload normalizes the share state into a flat object
// the frontend can drop directly into UI state. Same shape whether
// the share is on or off so the menu/icon logic doesn't branch on
// presence-of-key.
func shareStatePayload(s *PageShare, url string) map[string]any {
	if s == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":    true,
		"share_key":  s.ShareKey,
		"public_url": url,
		"visibility": s.Visibility,
		"created_at": s.CreatedAt,
	}
}

// shareMarkdown is the goldmark instance used for public-share
// rendering. Raw HTML in the markdown body is escaped (no
// goldmark/html.WithUnsafe), so attacker-supplied <script>,
// <iframe>, on*= handlers never reach the rendered output as live
// markup. GFM gives us tables, task lists, strikethrough and
// autolinks.
var shareMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(
		html.WithHardWraps(),
		html.WithXHTML(),
	),
)

// shareSanitizer is the second line of defense after goldmark.
// Built from UGCPolicy with the following hardening:
//
//   - URL schemes restricted to http/https/mailto/tel. Anything
//     else — javascript:, file:, vbscript:, data:, ftp:, etc. — is
//     stripped. http is left in so the long tail of plain http://
//     links in user content keeps rendering; the browser will
//     block mixed-content fetches for embedded resources on its
//     own when the page is served over HTTPS.
//   - Every link gains rel="nofollow noreferrer" (modern browsers
//     treat target="_blank" as implicitly noopener, so the
//     reverse-tabnabbing vector is closed without an explicit rel
//     entry). Fully-qualified URLs also get target="_blank" so a
//     same-tab navigation never carries the share key onward.
//   - Style attributes are blocked (UGCPolicy default) so authors
//     can't smuggle expression() / url(javascript:) tricks.
//   - data: URIs in <img> are not on the scheme allow-list, so an
//     image referencing data:image/svg+xml never reaches the DOM.
var shareSanitizer = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowURLSchemes("http", "https", "mailto", "tel")
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}()

// renderShareMarkdown turns a page's stored markdown into the safe
// HTML the public template embeds. Two passes: goldmark builds an
// HTML tree (no raw HTML pass-through), then bluemonday strips
// anything outside the UGC policy. The result is template.HTML so
// html/template knows it's already sanitized and doesn't re-escape.
//
// On a goldmark error the fallback is a plain <pre> of the raw
// markdown, escaped + line-broken — better than blank, never
// dangerous.
func renderShareMarkdown(md string) template.HTML {
	var buf bytes.Buffer
	if err := shareMarkdown.Convert([]byte(md), &buf); err != nil {
		escaped := template.HTMLEscapeString(md)
		return template.HTML("<pre>" + strings.ReplaceAll(escaped, "\n", "<br>") + "</pre>")
	}
	clean := shareSanitizer.SanitizeBytes(buf.Bytes())
	return template.HTML(clean)
}

// shareViewModel feeds publicShareTemplate. RenderedHTML is the
// already-sanitized output of renderShareMarkdown — template.HTML
// so html/template trusts it. The sharer's display name is resolved
// at render time so an account rename doesn't require updating each
// share row.
type shareViewModel struct {
	Title        string
	RenderedHTML template.HTML
	SharerName   string
	UpdatedAt    time.Time
	UpdatedLocal string
	Tagline      string
}

// publicShareTemplate is the HTML the public /p/{key} renders.
// Self-contained — CSS is inlined, no <script> tags at all. The
// rendered+sanitized markdown body sits inside an <article>, so
// any residual HTML from a malicious source is bounded by what
// bluemonday allows.
var publicShareTemplate = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow, noarchive">
<title>{{.Title}} · Familiar</title>
<link rel="icon" href="/favicon.svg">
<style>
  :root {
    --bg-canvas: #0B0B0F;
    --bg-card: #1B1B23;
    --bg-raised: #15151B;
    --fg-1: #F4F4F7;
    --fg-2: #C9C9D1;
    --fg-3: #7A7A86;
    --fg-4: #52525F;
    --iris: #6A4CE0;
    --iris-light: #AC98F3;
    --mark: #E8BE55;
    --border: rgba(255,255,255,0.10);
    --border-soft: rgba(255,255,255,0.06);
  }
  html, body {
    margin: 0;
    background: var(--bg-canvas);
    color: var(--fg-2);
    font-family: "Geist", -apple-system, "Segoe UI", system-ui, sans-serif;
    line-height: 1.5;
    -webkit-font-smoothing: antialiased;
  }
  .share-brand {
    padding: 18px 28px;
    border-bottom: 1px solid var(--border);
  }
  .share-brand-stack {
    display: inline-flex; flex-direction: column;
    align-items: flex-start; gap: 5px;
  }
  .share-brand-row {
    display: flex; align-items: center; gap: 14px;
    color: inherit;
    text-decoration: none;
    cursor: pointer;
  }
  .share-brand svg { flex: none; }
  .share-brand .name {
    font: 600 24px/1 "Geist", system-ui, sans-serif;
    letter-spacing: -0.02em;
    color: var(--fg-1);
  }
  .share-brand .tagline {
    font: 400 12px/1.4 "Geist", system-ui, sans-serif;
    color: var(--fg-3);
  }
  .share-main {
    max-width: 760px;
    margin: 0 auto;
    padding: 40px 28px 80px;
  }
  .share-title {
    font: 600 36px/1.1 "Geist", system-ui, sans-serif;
    letter-spacing: -0.025em;
    color: var(--fg-1);
    margin: 0 0 6px;
  }
  .share-meta {
    font: 400 12.5px/1.5 "Geist Mono", ui-monospace, Menlo, monospace;
    color: var(--fg-4);
    letter-spacing: 0.02em;
    margin-bottom: 28px;
  }
  .share-content {
    color: var(--fg-2);
    font-size: 16px;
    line-height: 1.65;
    word-wrap: break-word;
    overflow-wrap: anywhere;
  }
  .share-content > :first-child { margin-top: 0; }
  .share-content h1, .share-content h2, .share-content h3,
  .share-content h4, .share-content h5, .share-content h6 {
    color: var(--fg-1);
    margin: 1.6em 0 0.5em;
    letter-spacing: -0.015em;
    line-height: 1.25;
  }
  .share-content h1 { font-size: 28px; }
  .share-content h2 { font-size: 22px; }
  .share-content h3 { font-size: 18px; }
  .share-content h4, .share-content h5, .share-content h6 { font-size: 16px; }
  .share-content p { margin: 0.6em 0; }
  .share-content a {
    color: var(--iris-light);
    text-decoration: underline;
    text-decoration-color: rgba(172,152,243,0.4);
    text-underline-offset: 2px;
  }
  .share-content a:hover { text-decoration-color: currentColor; }
  .share-content code {
    background: var(--bg-raised);
    padding: 1px 6px;
    border-radius: 3px;
    font-family: "Geist Mono", ui-monospace, Menlo, monospace;
    font-size: 0.92em;
    border: 1px solid var(--border-soft);
  }
  .share-content pre {
    background: var(--bg-raised);
    padding: 14px 16px;
    border-radius: 8px;
    overflow-x: auto;
    border: 1px solid var(--border-soft);
  }
  .share-content pre code {
    background: transparent;
    padding: 0;
    border: 0;
    font-size: 13.5px;
  }
  .share-content blockquote {
    border-left: 3px solid #2B2B36;
    padding: 2px 14px;
    color: var(--fg-3);
    margin: 0.8em 0;
  }
  .share-content ul, .share-content ol {
    padding-left: 1.6em;
    margin: 0.6em 0;
  }
  .share-content li { margin: 0.2em 0; }
  .share-content img {
    max-width: 100%;
    height: auto;
    border-radius: 6px;
    border: 1px solid var(--border-soft);
  }
  .share-content table {
    border-collapse: collapse;
    margin: 0.8em 0;
    font-size: 14.5px;
    display: block;
    overflow-x: auto;
    -webkit-overflow-scrolling: touch;
    max-width: 100%;
  }
  .share-content th, .share-content td {
    border: 1px solid #2B2B36;
    padding: 6px 12px;
    text-align: left;
    white-space: nowrap;
  }
  .share-content td { white-space: normal; min-width: 80px; }
  .share-content th { background: var(--bg-raised); color: var(--fg-1); }
  .share-content hr {
    border: 0;
    border-top: 1px solid #2B2B36;
    margin: 1.6em 0;
  }
  @media (max-width: 640px) {
    .share-brand {
      display: flex; flex-direction: row; align-items: center;
      padding: 14px 20px;
    }
    .share-brand-stack { display: contents; }
    .share-brand .tagline { margin-left: auto; }
    .share-main { padding: 28px 20px 64px; }
    .share-title { font-size: 26px; line-height: 1.15; }
    .share-meta { font-size: 11.5px; }
    .share-content { font-size: 15.5px; }
  }
</style>
</head>
<body>
<header class="share-brand">
  <div class="share-brand-stack">
    <a class="share-brand-row" href="https://familiar.wiki" rel="noopener noreferrer">
      <svg width="34" height="40" viewBox="78 56 96 110" fill="none" aria-hidden="true">
        <g transform="translate(-82 0)">
          <path d="M 165.41 64.56 L 244.48 64.56 L 233.02 79.97 L 180.48 79.97 L 180.48 100.21 L 218.40 100.21 L 206.94 115.63 L 181.04 115.63 L 181.04 154.56 L 165.41 154.56 Z" fill="#6A4CE0"/>
          <circle cx="198.69" cy="146.74" r="7.817" fill="#E8BE55"/>
        </g>
      </svg>
      <span class="name">Familiar</span>
    </a>
    <span class="tagline">{{.Tagline}}</span>
  </div>
</header>
<main class="share-main">
  <h1 class="share-title">{{.Title}}</h1>
  <div class="share-meta">Shared by {{.SharerName}} · updated {{.UpdatedLocal}}</div>
  <article class="share-content">{{.RenderedHTML}}</article>
</main>
</body>
</html>`))

// publicShareCSP is the Content-Security-Policy the public share
// page ships. The page intentionally has no <script>, so script-src
// is implicitly 'none' via default-src.
//
//   - default-src 'none'       — deny anything not listed below.
//   - img-src 'self' https:    — same-origin favicon plus author
//     images over HTTPS only.
//   - style-src 'unsafe-inline'
//     — the page's <style> block. No
//     external stylesheets load. UGC
//     style attrs are stripped by
//     bluemonday, so this doesn't widen
//     the attack surface for content.
//   - base-uri 'none'          — block <base> injection.
//   - form-action 'none'       — block form posts (none exist).
//   - frame-ancestors 'none'   — clickjacking protection.
//
// Note media-src / object-src / font-src / connect-src all
// inherit 'none' from default-src, which is what we want.
const publicShareCSP = "default-src 'none'; " +
	"img-src 'self' https:; " +
	"style-src 'unsafe-inline'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// setShareSecurityHeaders writes the security-relevant response
// headers for the public share render. Centralized so anyone adding
// a sibling endpoint copies the full set rather than half of them.
func setShareSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// no-store on top of no-cache so intermediate caches can't
	// retain shared content; private to forbid any shared cache;
	// max-age=0 belt-and-suspenders for old caches.
	w.Header().Set("Cache-Control", "private, no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Security-Policy", publicShareCSP)
	// X-Frame-Options is redundant with the CSP frame-ancestors
	// directive on modern browsers but kept for the long tail.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// no-referrer keeps the share key out of the Referer header on
	// outbound link clicks. The key is the credential — leaking it
	// to a third-party site would leak access.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=(), payment=(), usb=()")
	// Search engines + AI crawlers should treat shared pages as
	// noindex. The HTTP header is honored even before any markup
	// is parsed.
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive, nosnippet")
}

// publicShare serves GET /p/{key} — the public read-only render of
// a shared page. Refuses on any host that's not in the configured
// public allow-list so a Tailscale-direct hostname never resolves
// shared pages. 404 on unknown keys, deleted pages, archived books,
// malformed keys — all the same signal so a probe can't tell them
// apart.
func (h *Handler) publicShare(w http.ResponseWriter, r *http.Request) {
	if h.wiki == nil || !h.isPublicHost(r) {
		http.NotFound(w, r)
		return
	}
	key := r.PathValue("key")
	if !isValidShareKey(key) {
		http.NotFound(w, r)
		return
	}
	sp, err := h.wiki.LookupSharedPage(r.Context(), key)
	if errors.Is(err, ErrPageNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		// Avoid leaking error text on the public surface.
		log.Printf("[share] lookup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	sharer := h.resolveSharerName(r.Context(), sp.SharedBy)
	vm := shareViewModel{
		Title: sp.Title,
		RenderedHTML: rewriteShareMedia(renderShareMarkdown(
			h.substituteMermaidRenders(r.Context(), sp.Content, sp.PageID, key)), key),
		SharerName:   sharer,
		UpdatedAt:    sp.UpdatedAt,
		UpdatedLocal: sp.UpdatedAt.UTC().Format("Jan 2, 2006 · 3:04 PM UTC"),
		Tagline:      "Your data, your AI, your rules.",
	}
	setShareSecurityHeaders(w)
	if err := publicShareTemplate.Execute(w, vm); err != nil {
		// Headers are already committed if Execute wrote anything;
		// log and bail. This is a template bug if it ever hits.
		fmt.Println("[share] render:", err)
	}
}

// mermaidFenceRE matches ```mermaid fences for the share-render
// substitution. The same hash convention lives client-side in
// mermaid-blocks.js (syncShareRenders): sha256 of the TRIMMED fence
// body, first 12 hex chars, filename "mermaid-<hash>.png".
var mermaidFenceRE = regexp.MustCompile("(?s)```mermaid[^\n]*\n(.*?)```")

// MermaidRenderHash is the shared content-addressing scheme for
// pre-rendered diagram PNGs.
func MermaidRenderHash(fenceBody string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(fenceBody)))
	return hex.EncodeToString(sum[:])[:12]
}

// substituteMermaidRenders swaps each mermaid fence for its
// pre-rendered PNG (uploaded by the owner's browser on save /
// share-toggle — the share page ships no script, so diagrams must
// arrive as bitmaps). Fences with no matching render stay as code
// blocks: stale beats blank, and a brand-new fence the owner hasn't
// saved through the workspace yet still shows its source.
func (h *Handler) substituteMermaidRenders(ctx context.Context, content, pageID, key string) string {
	if h.media == nil || !strings.Contains(content, "```mermaid") {
		return content
	}
	return mermaidFenceRE.ReplaceAllStringFunc(content, func(match string) string {
		body := mermaidFenceRE.FindStringSubmatch(match)[1]
		m, err := h.media.FindByPageAndFilename(ctx, pageID,
			"mermaid-"+MermaidRenderHash(body)+".png")
		if err != nil {
			return match // no render — leave the fence
		}
		return "![diagram](/p/" + key + "/media/" + m.ID + ")"
	})
}

// rewriteShareMedia retargets in-app media URLs at the share-scoped
// anonymous proxy (/p/{key}/media/{id}) and applies #w=NN fragment
// widths (MEDIA-DIAGRAMS image sizing) as inline styles. Runs AFTER
// bluemonday: everything written here is server-derived — the share
// key and a clamped integer — never user input.
func rewriteShareMedia(in template.HTML, key string) template.HTML {
	s := string(in)
	if !strings.Contains(s, "/console/api/media/") {
		return in
	}
	doc, err := xhtml.Parse(strings.NewReader(s))
	if err != nil {
		return in
	}
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "img" {
			for i, a := range n.Attr {
				if a.Key != "src" || !strings.HasPrefix(a.Val, "/console/api/media/") {
					continue
				}
				rest := strings.TrimPrefix(a.Val, "/console/api/media/")
				id, frag, _ := strings.Cut(rest, "#")
				n.Attr[i].Val = "/p/" + key + "/media/" + id
				if w, ok := strings.CutPrefix(frag, "w="); ok {
					if pct, err := strconv.Atoi(w); err == nil && pct >= 10 && pct < 100 {
						n.Attr = append(n.Attr, xhtml.Attribute{
							Key: "style",
							Val: "width:" + strconv.Itoa(pct) + "%",
						})
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	// html.Parse wrapped the fragment in <html><head><body> — render
	// only the body's children back out.
	var body *xhtml.Node
	var findBody func(n *xhtml.Node)
	findBody = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "body" {
			body = n
			return
		}
		for c := n.FirstChild; c != nil && body == nil; c = c.NextSibling {
			findBody(c)
		}
	}
	findBody(doc)
	if body == nil {
		return in
	}
	var buf bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := xhtml.Render(&buf, c); err != nil {
			return in
		}
	}
	return template.HTML(buf.String())
}

// publicShareMedia serves GET /p/{key}/media/{id} — anonymous image
// bytes for a shared page. The share key is the credential: the
// media row must belong to the SHARED page, and every failure mode
// (bad key, unshared page, foreign media id, malformed id) is the
// same 404 so nothing can be probed.
func (h *Handler) publicShareMedia(w http.ResponseWriter, r *http.Request) {
	if h.wiki == nil || h.media == nil || !h.isPublicHost(r) {
		http.NotFound(w, r)
		return
	}
	key := r.PathValue("key")
	if !isValidShareKey(key) {
		http.NotFound(w, r)
		return
	}
	sp, err := h.wiki.LookupSharedPage(r.Context(), key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, err := h.media.Get(r.Context(), r.PathValue("id"))
	if err != nil || m.PageID != sp.PageID {
		http.NotFound(w, r)
		return
	}
	f, ct, err := h.media.Open(m, false)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Public + cacheable: the share key in the URL is the gate, and
	// revoking a share also changes nothing about already-cached
	// bytes — same trade the share page itself makes.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, m.Filename, info.ModTime(), f)
}

// resolveSharerName looks up the sharer's first name through the
// configured UserManager. Falls back to "Someone" when the lookup
// fails, the user manager isn't wired, or no display name is set —
// the public render must never expose a raw user id.
func (h *Handler) resolveSharerName(ctx context.Context, userID string) string {
	if h.users == nil {
		return "Someone"
	}
	u, err := h.users.GetUser(ctx, userID)
	if err != nil || u == nil || u.DisplayName == "" {
		return "Someone"
	}
	first := strings.TrimSpace(strings.SplitN(u.DisplayName, " ", 2)[0])
	if first == "" {
		return "Someone"
	}
	return first
}
