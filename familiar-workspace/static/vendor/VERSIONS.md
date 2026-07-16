# Vendored third-party JavaScript

These libraries are checked in (minified) rather than installed via a package
manager, so the workspace serves them directly from disk with no build step.
All are MIT-licensed. Update by replacing the file with the official release
build and bumping the `?v=` cache-buster in `index.html` / `mobile.html` (and
the service-worker `CACHE` name in `sw.js` if it precaches the asset).

| Path | Library | Version | Upstream | License |
|------|---------|---------|----------|---------|
| `toastui/toastui-editor-all.min.js`, `toastui-editor*.css` | TOAST UI Editor | 3.2.x | https://github.com/nhn/tui.editor | MIT |
| `mermaid/mermaid.min.js` | Mermaid | 11.12 | https://github.com/mermaid-js/mermaid | MIT |
| `marked/marked.min.js` | Marked | 13.0.3 | https://github.com/markedjs/marked | MIT |
| `dompurify/purify.min.js` | DOMPurify | 3.1.7 | https://github.com/cure53/DOMPurify | Apache-2.0 / MPL-2.0 |
| `highlight/core.min.js`, `highlight/atom-one-dark.min.css` | highlight.js (core) | 11.10.0 | https://github.com/highlightjs/highlight.js | BSD-3-Clause |
| `cytoscape/cytoscape.min.js` | Cytoscape.js | 3.30.4 | https://github.com/cytoscape/cytoscape.js | MIT |

The TOAST UI bundle also embeds DOMPurify. The chat/notes markdown renderer
(`marked` + standalone `DOMPurify` + `highlight.js`) and the memory-graph view
(`cytoscape`) used to load from a public CDN; they were vendored so the app
runs fully offline/airgapped and ships under a strict CSP (`script-src 'self'`,
no third-party origins). Update by replacing the file with the pinned upstream
release and bumping the referrer's `?v=` cache-buster.

> Versions reflect what was vendored; confirm against the upstream release tag
> before publishing a security advisory or upgrade.
