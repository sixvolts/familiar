# Third-party licenses

Familiar bundles or depends on the projects below. Each is distributed under
its own license; this file is a convenience index, not a substitute for the
upstream license texts.

## Vendored JavaScript (familiar-workspace/static/vendor)
- **TOAST UI Editor** — MIT — https://github.com/nhn/tui.editor
- **Mermaid** — MIT — https://github.com/mermaid-js/mermaid
- **Marked** — MIT — https://github.com/markedjs/marked
- **DOMPurify** — Apache-2.0 / MPL-2.0 — https://github.com/cure53/DOMPurify
- **highlight.js** — BSD-3-Clause — https://github.com/highlightjs/highlight.js
- **Cytoscape.js** — MIT — https://github.com/cytoscape/cytoscape.js

See `familiar-workspace/static/vendor/VERSIONS.md` for vendored versions.

## Go modules (see familiar-gateway/go.mod, familiar-workspace/go.mod)
Key direct dependencies (all permissive — MIT / BSD / Apache-2.0):
- github.com/go-webauthn/webauthn — BSD-3-Clause
- github.com/jackc/pgx, github.com/lib/pq — MIT
- github.com/yuin/goldmark — MIT
- github.com/microcosm-cc/bluemonday — BSD-3-Clause
- github.com/SherClockHolmes/webpush-go — MIT
- golang.org/x/* (net, image, crypto, …) — BSD-3-Clause

Run `go-licenses report ./...` in each module for the full, authoritative list
before a release.
