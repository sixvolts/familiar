// mermaid-blocks.js — MEDIA-DIAGRAMS Phase 0: ```mermaid fences
// render as diagrams wherever markdown is previewed (notes, wiki,
// mobile). The diagram source IS the markdown — no storage, fully
// git-diffable, and the assistant can author/edit diagrams through
// the existing wiki tools with no new tooling.
//
// Pipeline: Toast UI's customHTMLRenderer (wikilink.js merges our
// codeBlock hook) emits the fence body as escaped text inside a
// .mermaid-block div; observe() watches the preview container and
// renderAll() swaps pending blocks for inline SVG. Render failures
// degrade to the visible source with an error note — never a blank
// hole, and the editor copy of the text is untouched either way.
//
// The public share page intentionally ships no script, so mermaid
// fences degrade to plain code blocks there (Phase 0 decision).
(function () {
    "use strict";

    // strictConfig is the interactive-rendering posture: diagram
    // text is user (and model) supplied, so strict escaping always.
    // (Mermaid forces HTML labels under strict — that's fine for
    // DOM rendering; rasterization swaps config temporarily below.)
    function strictConfig() {
        return {
            startOnLoad: false,
            securityLevel: "strict",
            theme: "dark",
        };
    }

    var initialized = false;
    function ensureInit() {
        if (!window.mermaid) return false;
        if (!initialized) {
            window.mermaid.initialize(strictConfig());
            initialized = true;
        }
        return true;
    }

    // codeBlock is merged into Toast UI's customHTMLRenderer by
    // wikilink.js. Non-mermaid fences fall through to the default.
    function codeBlock(node, context) {
        var lang = (node.info || "").trim().toLowerCase();
        if (lang !== "mermaid") return context.origin();
        return [
            {
                type: "openTag",
                tagName: "div",
                classNames: ["mermaid-block"],
                attributes: { "data-mermaid-pending": "1" },
            },
            { type: "text", content: node.literal || "" },
            { type: "closeTag", tagName: "div" },
        ];
    }

    var seq = 0;
    async function renderAll(rootEl) {
        if (!rootEl || !ensureInit()) return;
        var blocks = rootEl.querySelectorAll(".mermaid-block[data-mermaid-pending]");
        for (var i = 0; i < blocks.length; i++) {
            var el = blocks[i];
            var src = el.textContent || "";
            el.removeAttribute("data-mermaid-pending");
            await renderBlock(el, src);
        }
    }

    // observe re-renders as the preview updates (Toast UI rebuilds
    // preview DOM on every keystroke). The pending-attribute handoff
    // makes the observer self-quiescing: our own SVG swap triggers
    // one callback that finds nothing pending.
    function observe(rootEl) {
        if (!rootEl || rootEl.__familiarMermaid) return;
        rootEl.__familiarMermaid = true;
        var timer = null;
        var kick = function () {
            if (timer) clearTimeout(timer);
            timer = setTimeout(function () {
                timer = null;
                renderAll(rootEl);
            }, 150);
        };
        new MutationObserver(kick).observe(rootEl, { childList: true, subtree: true });
        kick();
    }

    // renderBlock renders one source string into one element —
    // shared by renderAll (md preview) and the WYSIWYG node view.
    async function renderBlock(el, src) {
        if (!ensureInit()) {
            el.textContent = src;
            return;
        }
        var id = "familiar-mmd-" + (++seq);
        try {
            var out = await exclusive(function () { return window.mermaid.render(id, src); });
            el.innerHTML = out.svg;
            el.classList.add("is-rendered");
            el.classList.remove("is-error");
        } catch (e) {
            el.textContent = src;
            el.classList.remove("is-rendered");
            el.classList.add("is-error");
            el.title = String((e && e.message) || e);
            // This render's own leftovers: its SVG and mermaid's wrapper
            // (d<id>), which a parse error leaves in the body. It removed
            // "familiar-mmd-" + seq, the latest id, which by then was
            // another block's render in progress: that diagram failed too.
            removeStray(id);
        }
    }

    // ── WYSIWYG node view ───────────────────────────────────────
    // Rich Text renders mermaid code blocks as the diagram itself
    // (read-only inline). Editing happens in a dedicated diagram
    // tab — clicking the block dispatches familiar:openDiagram with
    // the source and the block's fence index; the owning surface
    // (notes/wiki) adds page identity and opens the tab. ProseMirror
    // owns the document model; the node view only owns this DOM, so
    // the markdown round-trip is untouched.
    function MermaidWWView(node, view, getPos) {
        var self = this;
        this.dom = document.createElement("div");
        this.dom.className = "mermaid-block is-ww";
        this.dom.contentEditable = "false";
        this.dom.title = "Click to open in a diagram tab";
        this._src = node.textContent || "";
        renderBlock(this.dom, this._src);
        this.dom.addEventListener("click", function (e) {
            e.preventDefault();
            e.stopPropagation();
            var idx = 0;
            try {
                var pos = typeof getPos === "function" ? getPos() : 0;
                view.state.doc.nodesBetween(0, pos, function (n) {
                    if (n.type && n.type.name === "codeBlock" &&
                        String((n.attrs && n.attrs.language) || "").toLowerCase() === "mermaid") {
                        idx++;
                    }
                });
            } catch (_) { /* index 0 fallback */ }
            self.dom.dispatchEvent(new CustomEvent("familiar:openDiagram", {
                bubbles: true,
                detail: { source: self._src, fenceIndex: idx },
            }));
        });
    }
    MermaidWWView.prototype.update = function (node) {
        if (!node.type || node.type.name !== "codeBlock") return false;
        if (String((node.attrs && node.attrs.language) || "").toLowerCase() !== "mermaid") return false;
        var src = node.textContent || "";
        if (src !== this._src) {
            this._src = src;
            renderBlock(this.dom, src);
        }
        return true;
    };
    // We mutate our DOM asynchronously (SVG swap) — ProseMirror must
    // not interpret that as a document edit.
    MermaidWWView.prototype.ignoreMutation = function () { return true; };
    MermaidWWView.prototype.stopEvent = function () { return true; };

    // editorPlugin is passed in Toast UI's `plugins` option. Only
    // mermaid code blocks get the custom view; returning null falls
    // back to the default editable code block for every other
    // language.
    function editorPlugin() {
        return {
            wysiwygNodeViews: {
                codeBlock: function (node, view, getPos) {
                    var lang = String((node.attrs && node.attrs.language) || "").toLowerCase();
                    if (lang !== "mermaid") return null;
                    return new MermaidWWView(node, view, getPos);
                },
                // Image resize handles live in image-resize.js; the
                // view registers HERE because multi-plugin nodeViews
                // merge semantics are undocumented in tui — one
                // plugin owns the whole map.
                image: function (node, view, getPos) {
                    var ir = window.familiarImageResize;
                    return ir ? ir.nodeView(node, view, getPos) : null;
                },
            },
        };
    }

    // ── Render queue ────────────────────────────────────────────
    // Every mermaid.render goes through here, one at a time, so a render
    // that changes mermaid's global config (rasterize) can't have
    // another render start under that config.
    var renderQueue = Promise.resolve();
    function exclusive(fn) {
        var run = renderQueue.then(fn, fn);
        renderQueue = run.catch(function () {});
        return run;
    }

    // removeStray drops what a failed render leaves in the document: its
    // SVG and mermaid's wrapper div (d<id>).
    function removeStray(id) {
        ["d" + id, id].forEach(function (x) {
            var n = document.getElementById(x);
            if (n) n.remove();
        });
    }

    // ── Fences ──────────────────────────────────────────────────
    // mermaidFences lists the mermaid code blocks in markdown, in order,
    // as the editor's parser sees them: ``` or ~~~ fences of any length,
    // the language matched without case, and nothing inside another code
    // block counted. Each is {start, end, bodyStart, bodyEnd, body}. The
    // diagram tab and the share renders counted regex matches of
    // "```mermaid" instead, so on a page showing mermaid syntax in a
    // ````markdown example, Save overwrote the example.
    function mermaidFences(md) {
        md = String(md || "");
        var out = [];
        var lines = md.split("\n");
        var pos = 0;
        var open = null;
        for (var i = 0; i < lines.length; i++) {
            var line = lines[i];
            var lineEnd = pos + line.length;
            var m = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(line);
            if (!open) {
                // A backtick fence's info string may not contain a backtick.
                if (m && !(m[1].charAt(0) === "`" && m[2].indexOf("`") >= 0)) {
                    var lang = (m[2].trim().split(/\s+/)[0] || "").toLowerCase();
                    open = { ch: m[1].charAt(0), len: m[1].length, mermaid: lang === "mermaid",
                        start: pos, bodyStart: Math.min(lineEnd + 1, md.length) };
                }
            } else if (m && m[1].charAt(0) === open.ch && m[1].length >= open.len && m[2].trim() === "") {
                if (open.mermaid) {
                    out.push({ start: open.start, end: lineEnd, bodyStart: open.bodyStart,
                        bodyEnd: pos, body: md.slice(open.bodyStart, pos) });
                }
                open = null;
            }
            pos = lineEnd + 1;
        }
        // An unclosed fence runs to the end of the document.
        if (open && open.mermaid) {
            out.push({ start: open.start, end: md.length, bodyStart: open.bodyStart,
                bodyEnd: md.length, body: md.slice(open.bodyStart) });
        }
        return out;
    }

    // ── Share pre-renders (MEDIA-DIAGRAMS) ──────────────────────
    // The public share page ships NO script, so diagrams must arrive
    // as bitmaps. syncShareRenders rasterizes each fence to a PNG
    // named mermaid-<hash>.png (hash convention shared with the
    // gateway's substituteMermaidRenders: sha256 of the trimmed
    // fence body, first 12 hex) and reconciles the page's stored
    // set: upload missing, prune stale. Called after saves and on
    // share-toggle for publicly shared pages.
    async function hashFence(srcText) {
        var data = new TextEncoder().encode(srcText.trim());
        var digest = await crypto.subtle.digest("SHA-256", data);
        return Array.prototype.map.call(new Uint8Array(digest), function (b) {
            return b.toString(16).padStart(2, "0");
        }).join("").slice(0, 12);
    }

    async function rasterize(srcText) {
        if (!ensureInit()) return null;
        // Canvas export requires foreignObject-FREE SVG (foreignObject
        // taints canvases), and mermaid only honors htmlLabels:false
        // at securityLevel "loose". Loose is set, used and put back
        // inside one turn of the render queue: set around an await
        // outside it, the next queued interactive render (a preview
        // re-rendering while the page saved) ran under loose, which
        // skips mermaid's sanitizing of the SVG and its click hrefs.
        var id = "familiar-share-" + (++seq);
        var out;
        try {
            out = await exclusive(async function () {
                window.mermaid.initialize(Object.assign(strictConfig(), {
                    securityLevel: "loose",
                    htmlLabels: false,
                    flowchart: { htmlLabels: false },
                }));
                try {
                    return await window.mermaid.render(id, srcText);
                } finally {
                    window.mermaid.initialize(strictConfig());
                }
            });
        } catch (e) {
            removeStray(id);
            throw e;
        }
        // Parsed into an inert document (DOMParser), never the page's:
        // innerHTML on a page-owned element is live. Give the SVG
        // explicit pixel dimensions from its viewBox so the Image
        // decodes at natural size (mermaid emits width attributes in
        // percent).
        var svgEl = new DOMParser().parseFromString(out.svg, "image/svg+xml").documentElement;
        if (!svgEl || svgEl.nodeName.toLowerCase() !== "svg") return null;
        var vb = svgEl.viewBox && svgEl.viewBox.baseVal;
        var w = (vb && vb.width) || 800;
        var hgt = (vb && vb.height) || 600;
        svgEl.setAttribute("width", String(w));
        svgEl.setAttribute("height", String(hgt));
        var blobURL = URL.createObjectURL(new Blob(
            [new XMLSerializer().serializeToString(svgEl)],
            { type: "image/svg+xml" }));
        try {
            var img = new Image();
            await new Promise(function (res, rej) {
                img.onload = res;
                img.onerror = rej;
                img.src = blobURL;
            });
            var scale = 2; // 2x for crisp text on retina-ish screens
            var canvas = document.createElement("canvas");
            canvas.width = Math.max(1, Math.round(w * scale));
            canvas.height = Math.max(1, Math.round(hgt * scale));
            canvas.getContext("2d").drawImage(img, 0, 0, canvas.width, canvas.height);
            return await new Promise(function (res) { canvas.toBlob(res, "image/png"); });
        } finally {
            URL.revokeObjectURL(blobURL);
        }
    }

    async function syncShareRenders(pageCtx, content) {
        if (!window.crypto || !crypto.subtle || !pageCtx || !pageCtx.pageId) return;
        var fences = mermaidFences(content).map(function (f) { return f.body; });
        var base = "/console/api/books/" + encodeURIComponent(pageCtx.bookSlug) +
            "/page-by-id/" + encodeURIComponent(pageCtx.pageId) + "/media";

        var wanted = {};
        for (var i = 0; i < fences.length; i++) {
            wanted["mermaid-" + (await hashFence(fences[i])) + ".png"] = fences[i];
        }
        var existing = [];
        try {
            var r = await fetch(base, { credentials: "include" });
            if (!r.ok) return;
            existing = (await r.json()).items || [];
        } catch (_) { return; }
        var have = {};
        existing.forEach(function (it) {
            if (/^mermaid-[0-9a-f]{12}\.png$/.test(it.filename)) have[it.filename] = it;
        });

        for (var name in wanted) {
            if (have[name]) continue;
            try {
                var blob = await rasterize(wanted[name]);
                if (!blob) continue; // bad fence: share shows the source
                var form = new FormData();
                form.append("file", new File([blob], name, { type: "image/png" }));
                await fetch(base, { method: "POST", credentials: "include", body: form });
            } catch (e) {
                // Render failure degrades to the fence on the share
                // page — log it so the owner can find out why.
                console.warn("[mermaid] share render failed:", e);
            }
        }
        for (var fname in have) {
            if (wanted[fname]) continue;
            try {
                await fetch("/console/api/media/" + encodeURIComponent(have[fname].id), {
                    method: "DELETE",
                    credentials: "include",
                });
            } catch (_) { /* stale render lingers until next sync */ }
        }
    }

    window.familiarMermaid = {
        codeBlock: codeBlock,
        renderAll: renderAll,
        renderBlock: renderBlock,
        observe: observe,
        editorPlugin: editorPlugin,
        syncShareRenders: syncShareRenders,
        fences: mermaidFences,
    };
})();
