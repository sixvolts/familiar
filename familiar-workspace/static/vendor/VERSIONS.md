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
| `dompurify/purify.min.js` | DOMPurify | 3.4.16 (npm `dompurify@3.4.16`, sha512 verified against the registry's `dist.integrity`) | https://github.com/cure53/DOMPurify | Apache-2.0 / MPL-2.0 |
| `highlight/atom-one-dark.min.css` (theme only; the core script was removed: its CommonJS build never loaded in a browser) | highlight.js | 11.10.0 | https://github.com/highlightjs/highlight.js | BSD-3-Clause |
| `cytoscape/cytoscape.min.js` | Cytoscape.js | 3.30.4 | https://github.com/cytoscape/cytoscape.js | MIT |

The TOAST UI bundle also embeds DOMPurify **2.3.3** (known mXSS bypasses:
CVE-2024-47875, CVE-2024-45801, CVE-2024-48910). It is not used: every
editor is constructed with `customHTMLSanitizer` set to the shared policy
in `static/sanitize.js`, which runs the standalone copy above. Keep it that
way when adding an editor. The chat/notes markdown renderer
(`marked` + standalone `DOMPurify`, with the highlight.js theme for code blocks) and the memory-graph view
(`cytoscape`) used to load from a public CDN; they were vendored so the app
runs fully offline/airgapped and ships under a strict CSP (`script-src 'self'`,
no third-party origins). Update by replacing the file with the pinned upstream
release and bumping the referrer's `?v=` cache-buster.

> Versions reflect what was vendored; confirm against the upstream release tag
> before publishing a security advisory or upgrade.
