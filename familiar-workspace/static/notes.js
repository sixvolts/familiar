// Notes surface (FAMILIAR-WORKSPACE-SPEC Phase 2b).
//
// Two-pane shell rendered into a workspace tab:
//
//   ┌────────────────────────┬───────────────────────────────────┐
//   │ Search box             │ Title (editable)                   │
//   │ + New note             │ Folder · Updated 3m ago            │
//   ├────────────────────────┼───────────────────────────────────┤
//   │ ▾ Inbox                │                                    │
//   │   • Cannonball notes   │ ┌───────────┬─────────────────────┐│
//   │   • Phase planning     │ │  Editor   │   Live preview      ││
//   │ ▾ Reference            │ │  (text-   │   (markdown render) ││
//   │   • API endpoints      │ │  area)    │                     ││
//   │ ▾ Unfiled              │ │           │                     ││
//   │   • scratch             │ └───────────┴─────────────────────┘│
//   └────────────────────────┴───────────────────────────────────┘
//
// Editor decision: the spec called for inline rendering (Typora/
// Obsidian model). Doing that without a build step requires
// ProseMirror/Milkdown via importmaps or rolling cursor-aware live
// rendering by hand — both several days. v1 ships textarea on the
// left + live markdown preview on the right, sharing the chat
// surface's marked + DOMPurify + highlight.js pipeline. Toggle
// button collapses to editor-only or preview-only. A true inline
// editor is a polish-pass.
//
// Auto-save: 500ms debounce on title or content changes. Visible
// "saved" indicator confirms; the workspace tab also shows a dirty
// dot while a write is pending.

(function () {
    "use strict";

    const helpers = window.familiarAppHelpers;
    if (!helpers) {
        console.error("notes: app helpers not loaded; notes surface disabled");
        return;
    }

    // Surface an action failure through the toast UI instead of a
    // blocking native alert (EXTERNAL-READINESS-REVIEW.md P2). Falls
    // back to alert only if the toast helper is somehow unavailable.
    function notifyErr(msg) {
        if (helpers && helpers.toast) helpers.toast(msg, "error");
        else window.alert(msg);
    }
    const { apiJSON, toast } = helpers;

    // ── CDN deps ──────────────────────────────────────────────
    //
    // Reuses the chat surface's markdown pipeline. If chat.js
    // already loaded the deps, ensureMarkdownDeps is a no-op
    // promise; otherwise notes.js bootstraps them itself. Code
    // is duplicated rather than shared via a sibling module to
    // keep each surface self-contained — surface modules don't
    // depend on each other, only on the workspace runtime.

    const CDN = {
        marked:    "/vendor/marked/marked.min.js",
        dompurify: "/vendor/dompurify/purify.min.js",
        hljsJS:    "/vendor/highlight/core.min.js",
        hljsCSS:   "/vendor/highlight/atom-one-dark.min.css",
    };

    let depsPromise = null;
    function ensureMarkdownDeps() {
        if (depsPromise) return depsPromise;
        // If chat.js already loaded marked, reuse it.
        if (window.marked && window.DOMPurify) {
            depsPromise = Promise.resolve(renderMarkdownReal);
            return depsPromise;
        }
        depsPromise = (async () => {
            try {
                if (!document.querySelector('link[href="' + CDN.hljsCSS + '"]')) {
                    const css = document.createElement("link");
                    css.rel = "stylesheet";
                    css.href = CDN.hljsCSS;
                    document.head.appendChild(css);
                }
                if (!window.marked) await loadScript(CDN.marked);
                if (!window.DOMPurify) await loadScript(CDN.dompurify);
                if (!window.hljs) await loadScript(CDN.hljsJS);
                return renderMarkdownReal;
            } catch (e) {
                console.warn("notes: markdown deps failed, falling back", e);
                return renderMarkdownFallback;
            }
        })();
        return depsPromise;
    }

    function loadScript(src) {
        return new Promise((resolve, reject) => {
            const s = document.createElement("script");
            s.src = src;
            s.async = true;
            s.onload = () => resolve();
            s.onerror = () => reject(new Error("script load failed: " + src));
            document.head.appendChild(s);
        });
    }

    function renderMarkdownReal(md) {
        if (!window.marked || !window.DOMPurify) return renderMarkdownFallback(md);
        const html = window.marked.parse(md || "");
        return window.DOMPurify.sanitize(html, { ADD_ATTR: ["class"] });
    }

    function renderMarkdownFallback(md) {
        return '<pre class="chat-md-fallback">' + escapeHTML(md || "") + '</pre>';
    }

    function escapeHTML(s) {
        return String(s)
            .replace(/&/g, "&amp;").replace(/</g, "&lt;")
            .replace(/>/g, "&gt;").replace(/"/g, "&quot;")
            .replace(/'/g, "&#39;");
    }
    function relTime(iso) {
        if (!iso) return "";
        const t = new Date(iso).getTime();
        if (isNaN(t)) return "";
        const d = (Date.now() - t) / 1000;
        if (d < 60) return "now";
        if (d < 3600) return Math.floor(d / 60) + "m";
        if (d < 86400) return Math.floor(d / 3600) + "h";
        if (d < 86400 * 7) return Math.floor(d / 86400) + "d";
        const dt = new Date(t);
        const mon = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
        return mon[dt.getMonth()] + " " + String(dt.getDate()).padStart(2, "0");
    }
    function pageGlyphSVG() {
        return (
            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" width="32" height="32">' +
                '<path d="M6 3 h8 l5 5 v12 a1.5 1.5 0 0 1 -1.5 1.5 H6 a1.5 1.5 0 0 1 -1.5 -1.5 V4.5 A1.5 1.5 0 0 1 6 3 Z"/>' +
                '<path d="M14 3 v3.5 A1.5 1.5 0 0 0 15.5 8 H19"/>' +
            '</svg>'
        );
    }

    // ── Surface renderer ──────────────────────────────────────

    const shells = new Map(); // tab.id -> { root, model }

    // updateStatusBar pushes the notes-tab's context-sensitive
    // status text (DESIGN.md: "N words · N backlinks").
    // Backlinks are a future-add — the wiki surface will own
    // that graph; for now ship 0.
    function updateStatusBar(content) {
        const sb = window.familiarStatusBar;
        if (!sb || !sb.setContext) return;
        const text = (content || "").trim();
        const words = text === "" ? 0 : text.split(/\s+/).length;
        sb.setContext(words + " word" + (words === 1 ? "" : "s") + " · 0 backlinks");
    }

    function render(host, tab) {
        // Push initial status; the per-tab model below pushes
        // fresh counts on each keystroke via input handler.
        updateStatusBar("");
        const cached = shells.get(tab.id);
        if (cached) {
            host.innerHTML = "";
            host.appendChild(cached.root);
            // Detach+reattach during workspace re-render leaves
            // CodeMirror's viewport stale — it doesn't paint until
            // a real resize. Force a refresh once layout settles.
            if (cached.model.refreshEditor) cached.model.refreshEditor();
            cached.model.refreshList();
            return;
        }
        const model = newNotesModel(tab);
        host.innerHTML = "";
        host.appendChild(model.root);
        shells.set(tab.id, { root: model.root, model });
        model.init();
    }

    function newNotesModel(tab) {
        const persisted = (tab.state && tab.state.noteId) || null;
        const localState = {
            noteId: persisted,
            note: null,           // currently-loaded full note
            list: [],             // NoteSummary list
            folders: [],          // distinct folders
            search: "",
            saving: false,
            saveTimer: null,
            mode: (tab.state && tab.state.mode) || "split", // edit|preview|split
            // Per-tab back stack of note IDs visited via [[link]].
            // Pushed in notesWikiNavigate before loadNote, popped by
            // the back chevron, cleared by direct list jumps.
            history: [],
        };

        // ── Shell DOM ─────────────────────────────────────────
        const root = document.createElement("div");
        root.className = "notes-shell";

        // Left rail.
        const left = document.createElement("aside");
        left.className = "notes-left";
        const leftHead = document.createElement("div");
        leftHead.className = "notes-left-head";
        const searchWrap = document.createElement("form");
        searchWrap.className = "notes-search-wrap";
        const searchInput = document.createElement("input");
        searchInput.type = "search";
        searchInput.className = "notes-search";
        searchInput.placeholder = "Search notes…";
        searchWrap.appendChild(searchInput);
        const newBtn = document.createElement("button");
        newBtn.type = "button";
        newBtn.className = "chat-new-btn";
        newBtn.textContent = "+ New";
        newBtn.title = "New note";
        leftHead.append(searchWrap, newBtn);
        left.appendChild(leftHead);
        const tree = document.createElement("div");
        tree.className = "notes-tree";
        left.appendChild(tree);
        root.appendChild(left);

        // Right rail.
        const right = document.createElement("section");
        right.className = "notes-right";

        const header = document.createElement("header");
        header.className = "notes-header";

        // Back chevron — visible only when localState.history has
        // entries (the user got here by following a wikilink).
        const backBtn = document.createElement("button");
        backBtn.type = "button";
        backBtn.className = "notes-back";
        backBtn.title = "Back";
        backBtn.setAttribute("aria-label", "Back");
        backBtn.hidden = true;
        backBtn.innerHTML = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 6 L9 12 L15 18"/></svg>';
        function updateBackBtn() {
            backBtn.hidden = localState.history.length === 0;
        }
        backBtn.addEventListener("click", () => {
            const prev = localState.history.pop();
            updateBackBtn();
            if (prev) loadNote(prev);
        });

        const titleInput = document.createElement("input");
        titleInput.className = "notes-title";
        titleInput.placeholder = "Untitled";
        titleInput.spellcheck = true;
        const meta = document.createElement("div");
        meta.className = "notes-meta";
        const savedDot = document.createElement("span");
        savedDot.className = "notes-saved";
        savedDot.textContent = "";
        meta.appendChild(savedDot);

        // Overflow menu — "⋯" button with a dropdown for actions.
        const overflow = document.createElement("div");
        overflow.className = "notes-overflow";
        const overflowBtn = document.createElement("button");
        overflowBtn.type = "button";
        overflowBtn.className = "notes-overflow-btn";
        overflowBtn.textContent = "⋯";
        overflowBtn.title = "More actions";
        overflowBtn.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.toggle("is-open");
        });
        const overflowMenu = document.createElement("div");
        overflowMenu.className = "notes-overflow-menu";
        // Content insertion (MEDIA-DIAGRAMS): discoverable entry
        // points for the paste-an-image and mermaid-fence features.
        const imagePicker = document.createElement("input");
        imagePicker.type = "file";
        imagePicker.accept = "image/png,image/jpeg,image/gif,image/webp";
        imagePicker.hidden = true;
        imagePicker.addEventListener("change", () => {
            const file = imagePicker.files && imagePicker.files[0];
            imagePicker.value = "";
            if (!file || !localState.noteId || !tuiEditor) return;
            window.familiarWikiLink.uploadImage(
                { bookSlug: "personal", pageId: localState.noteId }, file,
            ).then((d) => {
                tuiEditor.exec("addImage", { imageUrl: d.url, altText: d.alt_text || file.name });
            }).catch((e) => {
                if (window.familiarAppHelpers && window.familiarAppHelpers.toast) {
                    window.familiarAppHelpers.toast("Image upload failed: " + (e.message || e), "error");
                }
            });
        });
        const addImageItem = document.createElement("button");
        addImageItem.type = "button";
        addImageItem.className = "notes-overflow-item";
        addImageItem.textContent = "Add image…";
        addImageItem.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            imagePicker.click();
        });
        overflowMenu.appendChild(addImageItem);
        overflowMenu.appendChild(imagePicker);
        const addDiagramItem = document.createElement("button");
        addDiagramItem.type = "button";
        addDiagramItem.className = "notes-overflow-item";
        addDiagramItem.textContent = "Add diagram";
        addDiagramItem.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            if (!tuiEditor || !localState.noteId) return;
            // Append a starter fence (renders inline immediately) and
            // open its diagram tab for editing.
            const md = tuiEditor.getMarkdown();
            const fenceIndex = (md.match(/```mermaid/g) || []).length;
            const starter = "graph TD;\n  A[Start] --> B[Next];";
            tuiEditor.setMarkdown(
                md.replace(/\n*$/, "") + "\n\n```mermaid\n" + starter + "\n```\n",
            );
            const ws = window.FamiliarWorkspace;
            if (ws && ws.openDoc) {
                const title = ((localState.note && localState.note.title) || "Note") + " · diagram";
                ws.openDoc("diagram", "personal/" + localState.noteId + "#" + fenceIndex, title, {
                    book_slug: "personal",
                    page_id: localState.noteId,
                    fence_index: fenceIndex,
                    source: starter,
                    page_title: (localState.note && localState.note.title) || "Note",
                });
            }
        });
        overflowMenu.appendChild(addDiagramItem);
        const pinItem = document.createElement("button");
        pinItem.type = "button";
        pinItem.className = "notes-overflow-item";
        pinItem.textContent = "Pin note";
        pinItem.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            togglePinNote();
        });
        overflowMenu.appendChild(pinItem);
        // Public sharing — toggle + copy-link. The copy-link row only
        // makes sense when a share exists, so it's hidden until then.
        const shareItem = document.createElement("button");
        shareItem.type = "button";
        shareItem.className = "notes-overflow-item";
        shareItem.textContent = "Share publicly";
        shareItem.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            toggleShareNote();
        });
        overflowMenu.appendChild(shareItem);
        const copyLinkItem = document.createElement("button");
        copyLinkItem.type = "button";
        copyLinkItem.className = "notes-overflow-item";
        copyLinkItem.textContent = "Copy public link";
        copyLinkItem.hidden = true;
        copyLinkItem.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            copyShareLink();
        });
        overflowMenu.appendChild(copyLinkItem);
        const deleteItem = document.createElement("button");
        deleteItem.type = "button";
        deleteItem.className = "notes-overflow-item danger";
        deleteItem.textContent = "Delete note";
        deleteItem.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            deleteNote();
        });
        overflowMenu.appendChild(deleteItem);
        overflow.append(overflowBtn, overflowMenu);
        // Refresh per-state labels every time the menu is opened —
        // Pin/Unpin, Share/Stop sharing, and copy-link visibility.
        overflowBtn.addEventListener("click", () => {
            const pinned = !!(localState.note && localState.note.pinned);
            pinItem.textContent = pinned ? "Unpin note" : "Pin note";
            const shared = !!(localState.note && localState.note.share);
            shareItem.textContent = shared ? "Stop sharing publicly" : "Share publicly";
            copyLinkItem.hidden = !shared;
        });

        // Globe indicator next to the title — visible only when the
        // note is publicly shared. Tooltip is enough; no menu opens.
        const shareIndicator = document.createElement("span");
        shareIndicator.className = "notes-share-indicator";
        shareIndicator.title = "Page shared publicly";
        shareIndicator.setAttribute("aria-label", "Page shared publicly");
        shareIndicator.hidden = true;
        shareIndicator.innerHTML =
            '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor"' +
            ' stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">' +
            '<circle cx="12" cy="12" r="9"/><path d="M3 12h18"/>' +
            '<path d="M12 3a14 14 0 0 1 4 9 14 14 0 0 1 -4 9 14 14 0 0 1 -4 -9 14 14 0 0 1 4 -9 Z"/>' +
            '</svg>';
        meta.appendChild(shareIndicator);
        meta.appendChild(overflow);

        // Sync the globe indicator's visibility to the current note's
        // share state. Called whenever localState.note.share changes.
        function updateShareIndicator() {
            const shared = !!(localState.note && localState.note.share);
            shareIndicator.hidden = !shared;
        }

        // Close overflow menu when clicking elsewhere.
        document.addEventListener("click", () => {
            overflow.classList.remove("is-open");
        });

        header.append(backBtn, titleInput, meta);
        right.appendChild(header);

        const editor = document.createElement("div");
        editor.className = "notes-editor";
        // Toast UI Editor renders into a div (not a textarea) and
        // builds its own chrome inside. Lazy-init so the container
        // has real dimensions when ProseMirror measures. The
        // notes-editor-host class is what CSS sizes; the
        // notes-page-links footer below sits as a sibling and
        // doesn't get the same flex-fill rule.
        const editorContainer = document.createElement("div");
        editorContainer.id = "tui-editor-" + tab.id;
        editorContainer.className = "notes-editor-host";
        editor.appendChild(editorContainer);
        // Page-links footer for the personal book. Notes are
        // pages; the same backlinks UI from the wiki surface
        // applies here.
        const linksHost = document.createElement("div");
        linksHost.className = "notes-page-links";
        linksHost.hidden = true;
        editor.appendChild(linksHost);
        right.appendChild(editor);

        // Toast UI Editor instance — initialized after the
        // container is in the DOM (deferred to init()). Stores
        // native markdown via getMarkdown() / setMarkdown().
        let tuiEditor = null;
        let currentEditorMode = "wysiwyg";

        // Our own mode toggle — replaces Toast UI's built-in switch.
        const modeBar = document.createElement("div");
        modeBar.className = "wiki-mode-bar";
        modeBar.innerHTML =
            '<button type="button" class="wiki-mode-btn" data-mode="markdown">Markdown</button>' +
            '<button type="button" class="wiki-mode-btn active" data-mode="wysiwyg">Rich Text</button>';
        editor.appendChild(modeBar);
        modeBar.addEventListener("click", (e) => {
            const btn = e.target.closest(".wiki-mode-btn");
            if (!btn || btn.dataset.mode === currentEditorMode) return;
            switchEditorMode(btn.dataset.mode);
        });

        function updateModeBar() {
            modeBar.querySelectorAll(".wiki-mode-btn").forEach((b) => {
                b.classList.toggle("active", b.dataset.mode === currentEditorMode);
            });
        }
        // Suppress flag prevents setMarkdown() in refreshCurrentNote
        // from triggering a save cycle (Toast UI fires "change" when
        // content is set programmatically).
        let suppressSave = false;

        const empty = document.createElement("div");
        empty.className = "notes-no-selection";
        empty.textContent = "Pick a note from the list, or click + New.";
        right.appendChild(empty);

        // Splash host — full-width landing page rendered when the
        // user clicks "Notes" in the sidebar nav, or when a fresh
        // tab opens with no note selected. Mirrors the wiki splash:
        // big "+ New note" tile on the left, pinned notes on the
        // right. Reuses .wiki-splash-* classes for visual parity.
        const splashHost = document.createElement("div");
        splashHost.className = "notes-splash-host wiki-splash-host";
        right.appendChild(splashHost);

        root.appendChild(right);

        function layoutSymbol(mode) {
            return mode === "edit" ? "✎" : mode === "preview" ? "○" : "◐";
        }

        // ── Behavior ──────────────────────────────────────────

        function setMode(mode) {
            localState.mode = mode;
            tab.state = { ...(tab.state || {}), mode };
            editor.className = "notes-editor notes-editor-" + mode;
            modeBtn.textContent = layoutSymbol(mode);
        }

        async function refreshList() {
            const url = localState.search
                ? "/console/api/books/personal/search?q=" + encodeURIComponent(localState.search)
                : "/console/api/books/personal/pages";
            try {
                const resp = await apiJSON(url);
                localState.list = (resp && resp.items) || [];
                if (resp && resp.folders) {
                    localState.folders = resp.folders;
                }
                renderTree();
            } catch (e) {
                tree.innerHTML = '<div class="chat-conv-error">' + escapeHTML(e.message || String(e)) + '</div>';
            }
        }

        function renderTree() {
            tree.innerHTML = "";
            if (localState.list.length === 0) {
                const stub = document.createElement("div");
                stub.className = "chat-conv-empty";
                stub.textContent = localState.search
                    ? 'No notes match "' + localState.search + '".'
                    : "No notes yet — click + New.";
                tree.appendChild(stub);
                return;
            }
            // Group by folder. Empty/null folder rows go under "Unfiled".
            const groups = new Map();
            for (const n of localState.list) {
                const key = n.folder || "Unfiled";
                if (!groups.has(key)) groups.set(key, []);
                groups.get(key).push(n);
            }
            // Stable folder order: alpha, with Unfiled last.
            const folderNames = [...groups.keys()].sort((a, b) => {
                if (a === "Unfiled") return 1;
                if (b === "Unfiled") return -1;
                return a.localeCompare(b);
            });
            for (const folderName of folderNames) {
                const folderEl = document.createElement("div");
                folderEl.className = "notes-folder-group";
                const head = document.createElement("div");
                head.className = "notes-folder-name";
                head.textContent = folderName;
                folderEl.appendChild(head);
                for (const n of groups.get(folderName)) {
                    folderEl.appendChild(renderRow(n));
                }
                tree.appendChild(folderEl);
            }
        }

        function renderRow(n) {
            const row = document.createElement("button");
            row.type = "button";
            row.className = "notes-row";
            if (n.id === localState.noteId) row.classList.add("is-active");
            const title = document.createElement("div");
            title.className = "notes-row-title";
            if (n.pinned) title.classList.add("is-pinned");
            title.textContent = n.title || "Untitled";
            row.appendChild(title);
            if (n.snippet) {
                const snip = document.createElement("div");
                snip.className = "notes-row-snippet";
                snip.textContent = n.snippet;
                row.appendChild(snip);
            }
            row.addEventListener("click", () => {
                const ws = window.FamiliarWorkspace;
                if (ws && ws.focusExistingDoc("notes", n.id)) return;
                // Direct jump from the notes list — drop any
                // wikilink trail.
                localState.history.length = 0;
                loadNote(n.id);
            });
            return row;
        }

        // Shared wiki-link navigate function — used by both
        // widgetRules (WYSIWYG clicks) and wireClickHandler
        // (Markdown preview clicks).
        async function notesWikiNavigate(parsed) {
            const ws = window.FamiliarWorkspace;
            const isPersonal = !parsed.bookSlug ||
                parsed.bookSlug.startsWith("personal:");
            if (isPersonal) {
                try {
                    const p = await apiJSON(
                        "/console/api/books/personal/pages/" +
                        encodeURIComponent(parsed.pageSlug));
                    if (p && p.id) {
                        // Link follows always load in this tab —
                        // hopping to a duplicate in an adjacent
                        // panel surprises the user. Push the
                        // current note onto the back stack so the
                        // new note's chevron can return here.
                        if (localState.noteId) {
                            localState.history.push(localState.noteId);
                        }
                        loadNote(p.id);
                    }
                } catch (e) {
                    console.warn("notes: wiki-link target not found", parsed, e);
                }
                return;
            }
            // Cross-book link — different surface entirely (Notes ⇒
            // Wiki). This branch is the one case we DO want to focus
            // an existing tab if the target's already open: the
            // user is leaving the Notes scope, so it's a navigation,
            // not an in-tab follow.
            const compositeId = parsed.bookSlug + "/" + parsed.pageSlug;
            // openDoc dedups against the composite id itself, so the
            // already-open case focuses the existing tab; otherwise
            // the wiki tab opens per the workspace's targeting rules.
            if (ws && ws.openDoc) {
                ws.openDoc("wiki", compositeId, null);
                return;
            }
            if (ws && ws.focusExistingDoc("wiki", compositeId)) return;
            if (window.appSwitchPanel) window.appSwitchPanel("workspace");
            if (ws && ws.focusSurface) ws.focusSurface("wiki");
            window.dispatchEvent(new CustomEvent("familiar:openDoc", {
                detail: { surface: "wiki",
                          id: compositeId },
            }));
        }

        // Lazy Toast UI Editor initialization — called once from
        // loadNote() when the first note is opened. WYSIWYG by
        // default; markdown source mode is one click away via the
        // mode-switch button in the editor's bottom-right corner.
        function initEditor(mode) {
            mode = mode || currentEditorMode;
            if (tuiEditor) return;
            var opts = window.familiarWikiLink
                ? window.familiarWikiLink.editorOptions(mode, notesWikiNavigate, function () {
                    return { slug: "personal", name: "Your Notes" };
                })
                : { height: "100%", initialEditType: mode, theme: "dark", usageStatistics: false, hideModeSwitch: true, toolbarItems: [] };
            opts.el = editorContainer;
            tuiEditor = new toastui.Editor(opts);

            if (window.familiarWikiLink) {
                window.familiarWikiLink.wireClickHandler(editorContainer, notesWikiNavigate);
            }
            if (window.familiarMermaid) {
                window.familiarMermaid.observe(editorContainer);
            }
            if (window.familiarWikiLink && window.familiarWikiLink.wireImageUpload) {
                window.familiarWikiLink.wireImageUpload(tuiEditor, function () {
                    return localState.noteId
                        ? { bookSlug: "personal", pageId: localState.noteId }
                        : null;
                });
            }
            // A rendered mermaid block was clicked — open it in a
            // diagram tab with this note's identity attached.
            editorContainer.addEventListener("familiar:openDiagram", (ev) => {
                const d = ev.detail || {};
                const ws = window.FamiliarWorkspace;
                if (!ws || !ws.openDoc || !localState.noteId) return;
                const title = ((localState.note && localState.note.title) || "Note") + " · diagram";
                ws.openDoc("diagram", "personal/" + localState.noteId + "#" + (d.fenceIndex || 0), title, {
                    book_slug: "personal",
                    page_id: localState.noteId,
                    fence_index: d.fenceIndex || 0,
                    source: d.source || "",
                    page_title: (localState.note && localState.note.title) || "Note",
                });
            });
            tuiEditor.on("change", () => {
                if (suppressSave) return;
                updateStatusBar(tuiEditor.getMarkdown());
                scheduleSave();
            });
            editor.addEventListener("keydown", (e) => {
                if ((e.metaKey || e.ctrlKey) && e.key === "s") {
                    e.preventDefault();
                    if (localState.saveTimer) {
                        clearTimeout(localState.saveTimer);
                        localState.saveTimer = null;
                    }
                    flushSave(true);
                }
            });
        }

        function switchEditorMode(newMode) {
            if (newMode === currentEditorMode) return;
            var md = tuiEditor ? tuiEditor.getMarkdown() : "";
            if (tuiEditor) {
                tuiEditor.destroy();
                tuiEditor = null;
            }
            editorContainer.innerHTML = "";
            currentEditorMode = newMode;
            updateModeBar();
            initEditor(newMode);
            if (tuiEditor && md) {
                suppressSave = true;
                tuiEditor.setMarkdown(md);
                suppressSave = false;
            }
        }

        // ── Splash view ────────────────────────────────────────
        // Renders into splashHost; toggled by enterSplash / exit
        // routines. Pulls pinned notes from /console/api/home/pins
        // (same endpoint the home surface uses) and filters to the
        // notes kind. The button mirrors the wiki "+ New" tile.
        async function renderSplash() {
            splashHost.innerHTML = "";

            const head = document.createElement("div");
            head.className = "wiki-splash-head";
            head.innerHTML = '<h1 class="wiki-splash-title">Your notes</h1>';
            splashHost.appendChild(head);

            const grid = document.createElement("div");
            grid.className = "wiki-splash-grid";

            // Left column: compact "+ New note" button + pinned notes
            // below (splash rework 2026-06-12). Recents on the right.
            const tileCol = document.createElement("div");
            tileCol.className = "wiki-splash-tile-col";
            const tile = document.createElement("button");
            tile.type = "button";
            tile.className = "wiki-empty-tile is-iris is-compact";
            tile.innerHTML =
                '<div class="wiki-empty-tile-glyph">' + pageGlyphSVG() + '</div>' +
                '<div class="wiki-empty-tile-title">New note</div>';
            tile.addEventListener("click", () => { newNote(); });
            const pinLabel = document.createElement("div");
            pinLabel.className = "wiki-splash-list-label";
            pinLabel.textContent = "Pinned";
            const pinList = document.createElement("div");
            pinList.className = "wiki-splash-list";
            pinList.innerHTML = '<div class="wiki-splash-empty">Loading…</div>';
            tileCol.append(tile, pinLabel, pinList);
            grid.appendChild(tileCol);

            const listCol = document.createElement("div");
            listCol.className = "wiki-splash-list-col";
            const recLabel = document.createElement("div");
            recLabel.className = "wiki-splash-list-label";
            recLabel.textContent = "Recent";
            const list = document.createElement("div");
            list.className = "wiki-splash-list is-dense";
            list.innerHTML = '<div class="wiki-splash-empty">Loading recent notes…</div>';
            listCol.append(recLabel, list);
            grid.appendChild(listCol);

            splashHost.appendChild(grid);

            const noteRowGlyph =
                '<div class="wiki-splash-row-glyph">' +
                    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round" width="18" height="18">' +
                        '<path d="M6 3 h8 l5 5 v12 a1.5 1.5 0 0 1 -1.5 1.5 H6 a1.5 1.5 0 0 1 -1.5 -1.5 V4.5 A1.5 1.5 0 0 1 6 3 Z"/>' +
                        '<path d="M14 3 v3.5 A1.5 1.5 0 0 0 15.5 8 H19"/>' +
                    '</svg>' +
                '</div>';

            apiJSON("/console/api/home/pins").then((resp) => {
                const items = ((resp && resp.items) || []).filter((it) => it.kind === "note");
                pinList.innerHTML = "";
                if (items.length === 0) {
                    pinList.innerHTML = '<div class="wiki-splash-empty">Nothing pinned — pin a note from its ⋯ menu.</div>';
                    return;
                }
                items.forEach((it) => {
                    const row = document.createElement("button");
                    row.type = "button";
                    row.className = "wiki-splash-row";
                    row.innerHTML =
                        noteRowGlyph +
                        '<div class="wiki-splash-row-body">' +
                            '<div class="wiki-splash-row-title">' + escapeHTML(it.title || "Untitled") + '</div>' +
                        '</div>';
                    row.addEventListener("click", () => loadNote(it.id));
                    pinList.appendChild(row);
                });
            }).catch(() => {
                pinList.innerHTML = '<div class="wiki-splash-empty">Couldn’t load pins.</div>';
            });

            // Recents: the personal book's pages, newest-edited first.
            apiJSON("/console/api/books/personal/pages").then((resp) => {
                const items = ((resp && resp.items) || [])
                    .slice()
                    .sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at))
                    .slice(0, 30);
                list.innerHTML = "";
                if (items.length === 0) {
                    list.innerHTML = '<div class="wiki-splash-empty">No notes yet — click New note.</div>';
                    return;
                }
                items.forEach((it) => {
                    const row = document.createElement("button");
                    row.type = "button";
                    row.className = "wiki-splash-row";
                    row.innerHTML =
                        noteRowGlyph +
                        '<div class="wiki-splash-row-body">' +
                            '<div class="wiki-splash-row-title">' + escapeHTML(it.title || "Untitled") + '</div>' +
                        '</div>' +
                        '<div class="wiki-splash-row-meta">' + escapeHTML(relTime(it.updated_at)) + '</div>';
                    row.addEventListener("click", () => loadNote(it.id));
                    list.appendChild(row);
                });
            }).catch((e) => {
                list.innerHTML = '<div class="wiki-splash-empty">' + escapeHTML(e.message || String(e)) + '</div>';
            });
        }
        function enterSplash() {
            // Clear note state so the workspace's isTabEmpty() sees
            // this tab as "available" — the next sidebar nav click
            // will reuse it instead of stacking another splash tab.
            localState.noteId = null;
            localState.note = null;
            tab.state = { ...(tab.state || {}), noteId: null };
            if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                window.FamiliarWorkspace.updateTabTitle(tab.id, "Notes");
            }
            root.classList.add("is-splash");
            empty.hidden = true;
            header.style.display = "none";
            editor.style.display = "none";
            renderSplash();
        }
        function exitSplash() {
            root.classList.remove("is-splash");
        }

        async function loadNote(id) {
            // Flush any pending save before switching.
            if (localState.saveTimer) {
                clearTimeout(localState.saveTimer);
                localState.saveTimer = null;
                await flushSave(true);
            }
            localState.noteId = id;
            tab.state = { ...(tab.state || {}), noteId: id };
            // Reflect the (possibly just-mutated) history in the
            // chrome. Callers manage the stack themselves.
            updateBackBtn();
            try {
                const n = await apiJSON("/console/api/books/personal/page-by-id/" + encodeURIComponent(id));
                localState.note = n;
                // Loading the latest server state clears any
                // outstanding conflict from the previous note.
                localState.saveBlocked = false;
                updateShareIndicator();
                exitSplash();
                empty.hidden = true;
                editor.style.display = "";
                header.style.display = "";
                // Initialize Toast UI lazily — the container is now
                // visible so ProseMirror measures correct dimensions.
                initEditor();
                titleInput.value = n.title || "";
                if (tuiEditor) {
                    suppressSave = true;
                    tuiEditor.setMarkdown(n.content || "");
                    suppressSave = false;
                }
                renderTree();
                // Page-links footer disabled until graph-based UI is designed.
                // renderPageLinks(n.id);
                savedDot.textContent = "";
                // Update the workspace tab label to show the note title.
                if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                    window.FamiliarWorkspace.updateTabTitle(tab.id, n.title || "Untitled");
                }
            } catch (e) {
                // Deleted elsewhere / stale tab restore → splash,
                // not a dead-end error pane.
                if (/not found|HTTP 404/i.test(e.message || String(e))) {
                    refreshList();
                    enterSplash();
                    return;
                }
                empty.textContent = "Couldn't load note: " + (e.message || String(e));
                empty.hidden = false;
                editor.style.display = "none";
                header.style.display = "none";
            }
        }

        // renderPageLinks paints the outbound + backlinks footer
        // for the personal-book page. Same shape as the wiki
        // surface; the only difference is navigation: same-book
        // (personal book) targets stay in the notes panel via
        // loadNote; cross-book targets dispatch openDoc so the
        // wiki tab takes over.
        async function renderPageLinks(pageID) {
            linksHost.innerHTML = "";
            linksHost.hidden = true;
            if (!pageID) return;
            const base = "/console/api/books/personal/page-by-id/" +
                encodeURIComponent(pageID);
            let outbound = [], inbound = [];
            try {
                const [linksResp, backResp] = await Promise.all([
                    apiJSON(base + "/links"),
                    apiJSON(base + "/backlinks"),
                ]);
                outbound = (linksResp && linksResp.items) || [];
                inbound = (backResp && backResp.items) || [];
            } catch (e) {
                console.warn("notes: renderPageLinks failed", e);
                return;
            }
            if (outbound.length === 0 && inbound.length === 0) return;
            if (outbound.length > 0) {
                linksHost.appendChild(buildLinksSection(
                    "Links from this note", outbound, makeOutboundClick));
            }
            if (inbound.length > 0) {
                linksHost.appendChild(buildLinksSection(
                    "Linked from", inbound, makeInboundClick));
            }
            linksHost.hidden = false;
        }

        function buildLinksSection(label, items, makeClick) {
            const section = document.createElement("div");
            section.className = "notes-page-links-section";
            const eyebrow = document.createElement("div");
            eyebrow.className = "notes-page-links-eyebrow";
            eyebrow.textContent = label + " (" + items.length + ")";
            section.appendChild(eyebrow);
            for (const it of items) {
                section.appendChild(buildLinkRow(it, makeClick));
            }
            return section;
        }

        function buildLinkRow(it, makeClick) {
            const row = document.createElement("div");
            row.className = "notes-page-link-row";
            const isOutbound = it.target_page_slug !== undefined;
            const isBroken = isOutbound && !it.target_page_id;
            if (isBroken) row.classList.add("is-broken");
            const title = document.createElement("span");
            title.className = "notes-page-link-title";
            if (isOutbound) {
                title.textContent = it.display_text || it.target_page_title ||
                    it.target_page_slug;
            } else {
                title.textContent = it.source_page_title || it.source_page_slug;
            }
            row.appendChild(title);
            // Cross-book: stamp the book slug so the source is
            // legible. Notes-side anything not "personal:..." is
            // a wiki book.
            const bookSlug = isOutbound ? it.target_book_slug : it.source_book_slug;
            const isCrossBook = bookSlug && !bookSlug.startsWith("personal:");
            if (isCrossBook) {
                const book = document.createElement("span");
                book.className = "notes-page-link-book";
                book.textContent = bookSlug;
                row.appendChild(book);
            }
            if (!isBroken) {
                row.addEventListener("click", makeClick(it));
            }
            return row;
        }

        function makeOutboundClick(it) {
            return () => {
                const bookSlug = it.target_book_slug || "";
                if (!bookSlug || bookSlug.startsWith("personal:")) {
                    // Personal-book target — stay in the notes panel.
                    if (it.target_page_id) loadNote(it.target_page_id);
                    return;
                }
                // Cross-book target — hand off to the wiki tab.
                navigateToWikiPage(bookSlug, it.target_page_slug, it.target_page_id);
            };
        }
        function makeInboundClick(it) {
            return () => {
                const bookSlug = it.source_book_slug || "";
                if (!bookSlug || bookSlug.startsWith("personal:")) {
                    if (it.source_page_id) loadNote(it.source_page_id);
                    return;
                }
                navigateToWikiPage(bookSlug, it.source_page_slug, it.source_page_id);
            };
        }
        function navigateToWikiPage(bookSlug, pageSlug, _pageID) {
            // Open a wiki tab focused on the target page through the
            // workspace's doc-open contract ("bookSlug/pageSlug" is
            // the wiki deep-link id form).
            const ws = window.FamiliarWorkspace;
            if (ws && ws.openDoc) {
                ws.openDoc("wiki", bookSlug + "/" + pageSlug, null);
                return;
            }
            if (window.appSwitchPanel) window.appSwitchPanel("workspace");
            if (ws && ws.focusSurface) ws.focusSurface("wiki");
            window.dispatchEvent(new CustomEvent("familiar:openDoc", {
                detail: { surface: "wiki", id: bookSlug + "/" + pageSlug },
            }));
        }

        async function newNote() {
            try {
                const n = await apiJSON("/console/api/books/personal/pages", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ title: "Untitled", content: "" }),
                });
                localState.list.unshift({
                    id: n.id, title: n.title, folder: n.folder || "",
                    pinned: false, snippet: "", updated_at: n.updated_at,
                });
                renderTree();
                localState.history.length = 0;
                loadNote(n.id);
            } catch (e) {
                notifyErr("Couldn't create note: " + (e.message || String(e)));
            }
        }

        // One-shot guard so a save-failure streak toasts once, not on
        // every debounced retry. Reset on the next successful save.
        let saveFailedNotified = false;
        async function flushSave(immediate) {
            if (!localState.note) return;
            if (localState.saving) return;
            // A previous save hit a conflict; pause until the user
            // reloads the note. The block clears in loadNote().
            if (localState.saveBlocked) return;
            localState.saving = true;
            savedDot.textContent = "Saving…";
            const patch = {
                title: titleInput.value || "Untitled",
                content: tuiEditor ? tuiEditor.getMarkdown() : "",
            };
            // If-Match carries the updated_at we last saw from the
            // server. UpdatePage refuses with 409 when the row has
            // moved since — that's the optimistic-concurrency guard
            // that stops a stale local copy from clobbering an
            // upstream edit (made by another device, by the AI via a
            // tool call, etc.).
            const headers = { "Content-Type": "application/json" };
            if (localState.note.updated_at) {
                headers["If-Match"] = localState.note.updated_at;
            }
            try {
                const resp = await fetch(
                    "/console/api/books/personal/page-by-id/" +
                        encodeURIComponent(localState.note.id),
                    {
                        method: "PATCH",
                        credentials: "include",
                        headers,
                        body: JSON.stringify(patch),
                    },
                );
                const text = await resp.text();
                let respBody = null;
                try { respBody = text ? JSON.parse(text) : null; } catch (e) { /* fall through */ }
                if (resp.status === 409 && respBody && respBody.error === "stale") {
                    handleSaveConflict(patch, respBody.current);
                    return;
                }
                if (!resp.ok) {
                    throw new Error((respBody && respBody.error) || ("HTTP " + resp.status));
                }
                const n = respBody;
                // The PATCH response doesn't join share state (only
                // GET does) — carry the known share forward so the
                // diagram-render trigger below keeps working.
                if (!n.share && localState.note.share) n.share = localState.note.share;
                localState.note = n;
                savedDot.textContent = "Saved";
                // Publicly shared pages keep their diagram PNGs in
                // step with the content (the share page is script-
                // free, so diagrams ship as pre-rendered bitmaps).
                if (n.share && window.familiarMermaid) {
                    window.familiarMermaid.syncShareRenders(
                        { bookSlug: "personal", pageId: n.id }, patch.content);
                }
                // Bump in list view.
                const idx = localState.list.findIndex((x) => x.id === n.id);
                if (idx >= 0) {
                    localState.list[idx] = {
                        id: n.id, title: n.title, folder: n.folder || "",
                        pinned: n.pinned, snippet: localState.list[idx].snippet,
                        updated_at: n.updated_at,
                    };
                    renderTree();
                }
                saveFailedNotified = false;
                setTimeout(() => {
                    if (savedDot.textContent === "Saved") savedDot.textContent = "";
                }, 1500);
            } catch (e) {
                // Network / 5xx save failure (NOT a 409 conflict, which
                // returns above). Autosave keeps retrying on the next
                // keystroke — saveBlocked stays false — so this isn't
                // permanent loss, but the status dot alone is easy to
                // miss on flaky wifi. Toast ONCE per failure streak so
                // the user knows their edits aren't landing, without
                // spamming a toast on every debounced retry.
                savedDot.textContent = "Save failed";
                console.warn("notes: save failed", e);
                if (!saveFailedNotified && toast) {
                    toast("Couldn't save — check your connection. Your edits are still here and will retry.", "error");
                    saveFailedNotified = true;
                }
            } finally {
                localState.saving = false;
            }
        }

        // handleSaveConflict reconciles a 409-stale response from the
        // server. Two cases:
        //
        //   1. Our local content already matches the server's — the
        //      conflict was purely a stale updated_at (e.g. a pin
        //      toggle bumped the timestamp). Adopt the server's row
        //      and continue saving silently.
        //   2. Real divergence — another writer landed content we
        //      don't have. Pause autosave to avoid clobbering the
        //      upstream edit, surface the conflict to the user, and
        //      wait for them to reload (clicking the note in the
        //      sidebar will trigger loadNote, which clears the
        //      block and pulls the latest server state). Local
        //      in-flight keystrokes since the last successful save
        //      are preserved in the editor so the user can copy them
        //      out if they want to merge manually.
        function handleSaveConflict(patch, serverPage) {
            if (!serverPage) {
                localState.saveBlocked = true;
                savedDot.textContent = "Conflict — reload to continue";
                if (toast) toast("This page was updated elsewhere. Reload to continue editing.", "warn");
                return;
            }
            if ((serverPage.content || "") === (patch.content || "") &&
                (serverPage.title || "") === (patch.title || "")) {
                localState.note = serverPage;
                savedDot.textContent = "Saved";
                setTimeout(() => {
                    if (savedDot.textContent === "Saved") savedDot.textContent = "";
                }, 1500);
                return;
            }
            localState.saveBlocked = true;
            savedDot.textContent = "Conflict — reload to continue";
            if (toast) {
                toast(
                    "This page was updated elsewhere. Your unsaved local edits weren't saved — copy them out if you need them, then click the note in the sidebar to reload.",
                    "warn",
                );
            }
        }

        function scheduleSave() {
            if (!localState.note) return;
            savedDot.textContent = "•"; // dirty
            if (localState.saveTimer) clearTimeout(localState.saveTimer);
            localState.saveTimer = setTimeout(() => {
                localState.saveTimer = null;
                flushSave();
            }, 500);
        }

        // Toast UI handles markdown rendering inline — no separate
        // preview pane or renderMarkdown dependency needed.

        async function togglePinNote() {
            if (!localState.note) return;
            const nextPinned = !localState.note.pinned;
            try {
                const n = await apiJSON("/console/api/books/personal/page-by-id/" + encodeURIComponent(localState.note.id), {
                    method: "PATCH",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ pinned: nextPinned }),
                });
                localState.note = n;
                const idx = localState.list.findIndex((x) => x.id === n.id);
                if (idx >= 0) {
                    localState.list[idx] = {
                        id: n.id, title: n.title, folder: n.folder || "",
                        pinned: n.pinned, snippet: localState.list[idx].snippet,
                        updated_at: n.updated_at,
                    };
                    renderTree();
                }
                window.dispatchEvent(new CustomEvent("familiar:pinsChanged"));
                updateShareIndicator();
            } catch (e) {
                notifyErr("Couldn't pin: " + (e.message || String(e)));
            }
        }

        // Toggle public-link sharing for the current note. POSTs to
        // the share endpoint, then patches localState.note.share with
        // the new state (or null when disabled) and refreshes the
        // globe indicator. The endpoint is idempotent server-side so
        // re-enabling returns the same share key.
        async function toggleShareNote() {
            if (!localState.note) return;
            const nextEnabled = !localState.note.share;
            try {
                const resp = await apiJSON(
                    "/console/api/books/personal/page-by-id/" +
                        encodeURIComponent(localState.note.id) + "/share",
                    {
                        method: "POST",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({ enabled: nextEnabled }),
                    },
                );
                if (resp && resp.enabled) {
                    // Render the page's diagrams for the share NOW —
                    // the public page can't run mermaid itself.
                    if (window.familiarMermaid && tuiEditor) {
                        window.familiarMermaid.syncShareRenders(
                            { bookSlug: "personal", pageId: localState.note.id },
                            tuiEditor.getMarkdown());
                    }
                    localState.note.share = {
                        share_key: resp.share_key,
                        public_url: resp.public_url,
                        visibility: resp.visibility,
                    };
                    toast("Page shared publicly", "success");
                } else {
                    localState.note.share = null;
                    toast("Page is no longer shared", "success");
                }
                updateShareIndicator();
            } catch (e) {
                notifyErr("Couldn't update share: " + (e.message || String(e)));
            }
        }

        // Copy the current note's public share URL to the clipboard.
        // No-op when nothing's shared (the menu item is hidden in
        // that state, but guard anyway).
        async function copyShareLink() {
            const s = localState.note && localState.note.share;
            if (!s || !s.public_url) return;
            try {
                await navigator.clipboard.writeText(s.public_url);
                toast("Public link copied", "success");
            } catch (e) {
                notifyErr("Couldn't copy: " + (e.message || String(e)));
            }
        }

        async function deleteNote() {
            if (!localState.note) return;
            if (!confirm('Delete "' + (localState.note.title || "Untitled") + '"?')) return;
            try {
                await apiJSON("/console/api/books/personal/page-by-id/" + encodeURIComponent(localState.note.id), { method: "DELETE" });
                const idx = localState.list.findIndex((x) => x.id === localState.note.id);
                if (idx >= 0) localState.list.splice(idx, 1);
                localState.note = null;
                localState.noteId = null;
                tab.state = { ...(tab.state || {}), noteId: null };
                empty.hidden = false;
                empty.textContent = "Pick a note from the list, or click + New.";
                editor.style.display = "none";
                header.style.display = "none";
                renderTree();
                // A deleted note that was pinned leaves an echo on the
                // Home pins grid + the sidebar rail until those re-fetch.
                // Pin toggles fire pinsChanged; a delete must too — plus
                // notesChanged so the rail drops the row.
                window.dispatchEvent(new CustomEvent("familiar:pinsChanged"));
                window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
            } catch (e) {
                notifyErr("Couldn't delete: " + (e.message || String(e)));
            }
        }

        // Re-fetch the current note from the server and update the
        // editor without triggering a save. Used when external changes
        // happen (e.g. the AI appends to a note via a tool call).
        async function refreshCurrentNote() {
            if (!localState.note || !localState.noteId) return;
            try {
                const n = await apiJSON("/console/api/books/personal/page-by-id/" + encodeURIComponent(localState.noteId));
                // Refresh share state regardless of content changes —
                // a remote share toggle should reflect immediately.
                localState.note.share = n.share || null;
                updateShareIndicator();
                // Always adopt the server's updated_at and title so
                // the next save's If-Match precondition isn't stale.
                // Without this, a no-content-change refresh would
                // leave updated_at behind and the very next save
                // would 409.
                localState.note.updated_at = n.updated_at;
                localState.note.title = n.title;
                // Only update content if it actually changed
                // (avoids resetting the editor's cursor position).
                const currentContent = tuiEditor ? tuiEditor.getMarkdown() : "";
                if (n.content !== currentContent) {
                    localState.note = n;
                    titleInput.value = n.title || "";
                    suppressSave = true;
                    if (tuiEditor) {
                        tuiEditor.setMarkdown(n.content || "");
                    }
                    suppressSave = false;
                    // Content changed → links may have too. Re-render
                    // the footer so [[]] additions/removals reflect.
                    renderPageLinks(n.id);
                }
                // Always refresh the sidebar list (title/order may
                // have changed even if content didn't).
                refreshList();
            } catch (e) {
                console.warn("notes: refreshCurrentNote failed", e);
            }
        }

        function init() {
            // No note loaded → splash. Once a note loads, exitSplash()
            // restores the editor view.
            if (!localState.noteId) {
                enterSplash();
            }

            newBtn.addEventListener("click", newNote);
            // Delete is now in the overflow menu — wired above.
            // Mode toggle removed — Toast UI's built-in WYSIWYG ↔
            // markdown switch lives in the editor's own corner.
            // The mode button stays in the DOM but is hidden via CSS
            // (or can be repurposed for source/preview toggle later).

            titleInput.addEventListener("input", () => {
                scheduleSave();
                // Update tab label live as user types.
                if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                    window.FamiliarWorkspace.updateTabTitle(tab.id, titleInput.value || "Untitled");
                }
            });
            // Refresh sidebar list on blur so renamed titles appear
            // without a page refresh.
            titleInput.addEventListener("blur", () => {
                if (localState.note) {
                    refreshList();
                    window.dispatchEvent(new Event("familiar:sidebarRefresh"));
                }
            });

            // Toast UI is initialized lazily in initEditor(), called
            // from loadNote() the first time a note is opened. This
            // ensures the container is visible when CodeMirror
            // measures its dimensions (avoids the zero-height bug).

            // Search debounce.
            let searchTimer = null;
            searchInput.addEventListener("input", () => {
                if (searchTimer) clearTimeout(searchTimer);
                searchTimer = setTimeout(() => {
                    localState.search = searchInput.value.trim();
                    refreshList();
                }, 200);
            });
            searchWrap.addEventListener("submit", (e) => {
                e.preventDefault();
                localState.search = searchInput.value.trim();
                refreshList();
            });

            refreshList().then(() => {
                if (localState.noteId) loadNote(localState.noteId);
            });

            // Flush on page hide.
            window.addEventListener("beforeunload", () => {
                if (localState.saveTimer) flushSave(true);
            });

            // Live-refresh when the chat surface modifies a note
            // via tool calls (append_to_note, update_note, etc.).
            window.addEventListener("familiar:notesChanged", () => {
                refreshCurrentNote();
            });

            // Server-pushed page-saved / page-deleted events. Lets a
            // device idle on a note pick up an edit made on another
            // device without polling. We only act on the page
            // currently open AND only when the local editor is clean
            // (no pending autosave, no in-flight save) — if the user
            // is mid-typing the next save will hit 409 and surface
            // the conflict via the existing path.
            window.addEventListener("familiar:pageEvent", (ev) => {
                const d = ev.detail || {};
                if (!localState.note || d.page_id !== localState.note.id) return;
                if (d.kind === "page-deleted") {
                    // Page was removed elsewhere. Refreshing the
                    // list drops the row; closing the editor would
                    // be more aggressive — leave that to the user.
                    refreshList();
                    return;
                }
                if (d.kind !== "page-saved") return;
                const payload = (d.payload) || {};
                // Skip the echo of our own write.
                if (payload.updated_at && localState.note.updated_at === payload.updated_at) return;
                // Refuse to clobber a dirty editor — flushSave on
                // the next tick will trigger the 409 path instead.
                if (localState.saveTimer || localState.saving) return;
                refreshCurrentNote().then(() => {
                    // Surface "Synced — <actor>" inline in the
                    // title bar (same slot as "Saved"). For shard
                    // writes the actor is the shard's name, not the
                    // owner's, so the viewer knows an automated
                    // agent moved the page.
                    if (payload.updated_by) {
                        savedDot.textContent = "Synced — " + payload.updated_by;
                        setTimeout(() => {
                            if (savedDot.textContent.indexOf("Synced") === 0) savedDot.textContent = "";
                        }, 4000);
                    }
                });
            });
        }

        // Expose loadNote + newNote so sidebar child-clicks
        // (Phase 3e openDoc) can drive the shell from outside.
        // Toast UI's WYSIWYG mode is contenteditable-based, so
        // workspace re-renders don't break its layout the way
        // CodeMirror's stale-viewport bug did. Keep refreshEditor
        // exposed as a no-op for callers that still invoke it.
        function refreshEditor() { /* no-op */ }
        // External callers (openDoc handler, focusExistingDoc) get a
        // wrapper that clears the back stack — those are direct
        // jumps, not link follows. Internal call sites continue to
        // call the bare loadNote and manage history themselves.
        function externalLoadNote(id) {
            localState.history.length = 0;
            return loadNote(id);
        }
        return { root, init, refreshList, loadNote: externalLoadNote, newNote, refreshEditor, enterSplash };
    }

    // ── Register ──────────────────────────────────────────────

    function register() {
        if (window.FamiliarWorkspace && window.FamiliarWorkspace.registerSurfaceRenderer) {
            window.FamiliarWorkspace.registerSurfaceRenderer("notes", render);
        } else {
            setTimeout(register, 0);
        }
    }
    register();

    // Sidebar children → openDoc consumer (Phase 3e). When a Notes
    // child is clicked, find the live notes shell and drive its
    // loadNote / newNote.
    window.addEventListener("familiar:openDoc", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "notes") return;
        const drive = (entry) => {
            if (d.id) entry.model.loadNote(d.id);
            else entry.model.newNote();
        };
        // Prefer the exact tab the workspace prepared (see chat.js's
        // listener for why the mounted-shell scan alone misroutes
        // with two same-surface panels).
        if (d.tabId) {
            const entry = shells.get(d.tabId);
            if (entry && document.body.contains(entry.root)) {
                drive(entry);
                return;
            }
        }
        for (const [, entry] of shells) {
            if (document.body.contains(entry.root)) {
                drive(entry);
                return;
            }
        }
    });

    // Tab closed (or its splash morphed to another surface) — drop
    // the shell entry so the map doesn't accumulate dead roots.
    // Mirrors the wiki surface's listener.
    window.addEventListener("familiar:tabClosed", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "notes") return;
        shells.delete(d.tabId);
    });

    // Sidebar primary nav click → fall back to splash. Mirrors the
    // wiki surface's behavior: clicking "Notes" in the rail returns
    // the user to the landing page regardless of what was open.
    window.addEventListener("familiar:surfaceNavRoot", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "notes") return;
        // Prefer the exact tab the workspace just focused. Falls
        // back to the first live shell only if the tabId wasn't
        // supplied (older callers).
        if (d.tabId) {
            const entry = shells.get(d.tabId);
            if (entry && entry.model.enterSplash) entry.model.enterSplash();
            return;
        }
        for (const [tabId, entry] of shells) {
            if (document.body.contains(entry.root) && entry.model.enterSplash) {
                entry.model.enterSplash();
                return;
            }
        }
    });
})();
