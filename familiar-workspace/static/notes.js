// Notes surface (FAMILIAR-WORKSPACE-SPEC Phase 2b): the personal
// book's pages, in a workspace tab.
//
//   ┌────────────────────────┬───────────────────────────────────┐
//   │ Search box    + New    │ ‹ Title (editable)    • 🌐 ⋯       │
//   ├────────────────────────┼───────────────────────────────────┤
//   │ Cannonball notes       │                                    │
//   │ Phase planning         │  Toast UI editor (Rich Text or     │
//   │ API endpoints          │  Markdown): [[links]], images,     │
//   │ …                      │  mermaid diagrams                  │
//   └────────────────────────┴───────────────────────────────────┘
//
// The editor is Toast UI (wikilink.js builds its options); it renders
// and sanitizes content itself, with the shared sanitizer
// (familiarSanitize.editor). With no note open the tab shows a splash
// (new note, pinned, recent).
//
// Saving: 500ms after an edit, against the version last synced
// (If-Match); the server merges a concurrent writer's disjoint edit,
// and an overlapping one opens a conflict banner. A pending edit is
// also sent when the page is hidden or unloaded (keepalive).

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

    function render(host, tab) {
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
            list: [],             // page summaries, the list pane
            search: "",
            saving: false,
            saveTimer: null,
            // Sync baseline: the title and (editor-normalized) body of the
            // version localState.note.updated_at names, i.e. the last state
            // known to match the server. isDirty() compares the editor with
            // it; a save sends If-Match: that version.
            baseTitle: "",
            baseContent: "",
            // A save requested while another is in flight. Dropping it
            // (the old early return) lost the keystrokes typed during the
            // round trip; flushSave re-runs once the first save returns.
            pendingResave: false,
            savePromise: null,
            // Set while a share toggle is in flight: a double click sent
            // two enables.
            shareBusy: false,
            // Per-tab back stack of note IDs visited via [[link]].
            // Pushed in notesWikiNavigate before loadNote, popped by
            // the back chevron, cleared by direct list jumps.
            history: [],
        };

        // Every window/document listener this shell adds is tied to
        // this signal; dispose() (tab closed) aborts it. They outlived
        // the tab: each closed notes tab kept refetching on every note
        // event, and kept its editor alive.
        const shellAbort = new AbortController();
        const signal = shellAbort.signal;

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
        searchInput.setAttribute("aria-label", "Search notes");
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
        titleInput.setAttribute("aria-label", "Note title");
        titleInput.spellcheck = true;
        const meta = document.createElement("div");
        meta.className = "notes-meta";
        const savedDot = document.createElement("span");
        savedDot.className = "notes-saved";
        savedDot.setAttribute("aria-live", "polite");
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
        overflowBtn.setAttribute("aria-label", "Note actions");
        overflowBtn.setAttribute("aria-haspopup", "menu");
        overflowBtn.setAttribute("aria-expanded", "false");
        const overflowMenu = document.createElement("div");
        overflowMenu.className = "notes-overflow-menu";
        overflowMenu.setAttribute("role", "menu");
        function setMenuOpen(open) {
            overflow.classList.toggle("is-open", open);
            overflowBtn.setAttribute("aria-expanded", open ? "true" : "false");
        }
        overflowBtn.addEventListener("click", (e) => {
            e.stopPropagation();
            const open = !overflow.classList.contains("is-open");
            setMenuOpen(open);
            if (open) {
                const first = overflowMenu.querySelector(".notes-overflow-item:not([hidden])");
                if (first) first.focus();
            }
        });
        // Escape closes the menu and returns focus to its button; the
        // arrow keys move between its items.
        overflow.addEventListener("keydown", (e) => {
            if (!overflow.classList.contains("is-open")) return;
            if (e.key === "Escape") {
                e.preventDefault();
                setMenuOpen(false);
                overflowBtn.focus();
                return;
            }
            if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
            const items = [...overflowMenu.querySelectorAll(".notes-overflow-item:not([hidden])")];
            if (!items.length) return;
            e.preventDefault();
            const at = items.indexOf(document.activeElement);
            const next = e.key === "ArrowDown" ? (at + 1) % items.length : (at - 1 + items.length) % items.length;
            items[next].focus();
        });
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
            // The image belongs to the note it was added to. Inserted into
            // whichever note was open when the upload finished, it landed
            // in another note, where a share can't show it.
            const forNote = localState.noteId;
            window.familiarWikiLink.uploadImage(
                { bookSlug: "personal", pageId: forNote }, file,
            ).then((d) => {
                if (localState.noteId !== forNote || !tuiEditor) {
                    toast("The image wasn't inserted: the note you added it to is no longer open.", "error");
                    return;
                }
                tuiEditor.exec("addImage", { imageUrl: d.url, altText: d.alt_text || file.name });
            }).catch((e) => {
                if (window.familiarAppHelpers && window.familiarAppHelpers.toast) {
                    window.familiarAppHelpers.toast("Image upload failed: " + (e.message || e), "error");
                }
            });
        });
        const addImageItem = document.createElement("button");
        addImageItem.type = "button";
        addImageItem.setAttribute("role", "menuitem");
        addImageItem.className = "notes-overflow-item";
        addImageItem.textContent = "Add image…";
        addImageItem.addEventListener("click", (e) => {
            e.stopPropagation();
            setMenuOpen(false);
            imagePicker.click();
        });
        overflowMenu.appendChild(addImageItem);
        overflowMenu.appendChild(imagePicker);
        const addDiagramItem = document.createElement("button");
        addDiagramItem.type = "button";
        addDiagramItem.setAttribute("role", "menuitem");
        addDiagramItem.className = "notes-overflow-item";
        addDiagramItem.textContent = "Add diagram";
        addDiagramItem.addEventListener("click", (e) => {
            e.stopPropagation();
            setMenuOpen(false);
            if (!tuiEditor || !localState.noteId) return;
            // Append a starter fence (renders inline immediately) and
            // open its diagram tab for editing.
            const md = tuiEditor.getMarkdown();
            // Counted as the diagram tab counts them (mermaid-blocks.js):
            // a regex also counted examples inside other code blocks.
            const fenceIndex = window.familiarMermaid && window.familiarMermaid.fences
                ? window.familiarMermaid.fences(md).length
                : (md.match(/```mermaid/g) || []).length;
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
        pinItem.setAttribute("role", "menuitem");
        pinItem.className = "notes-overflow-item";
        pinItem.textContent = "Pin note";
        pinItem.addEventListener("click", (e) => {
            e.stopPropagation();
            setMenuOpen(false);
            togglePinNote();
        });
        overflowMenu.appendChild(pinItem);
        // Public sharing — toggle + copy-link. The copy-link row only
        // makes sense when a share exists, so it's hidden until then.
        const shareItem = document.createElement("button");
        shareItem.type = "button";
        shareItem.setAttribute("role", "menuitem");
        shareItem.className = "notes-overflow-item";
        shareItem.textContent = "Share publicly";
        shareItem.addEventListener("click", (e) => {
            e.stopPropagation();
            setMenuOpen(false);
            toggleShareNote();
        });
        overflowMenu.appendChild(shareItem);
        const copyLinkItem = document.createElement("button");
        copyLinkItem.type = "button";
        copyLinkItem.setAttribute("role", "menuitem");
        copyLinkItem.className = "notes-overflow-item";
        copyLinkItem.textContent = "Copy public link";
        copyLinkItem.hidden = true;
        copyLinkItem.addEventListener("click", (e) => {
            e.stopPropagation();
            setMenuOpen(false);
            copyShareLink();
        });
        overflowMenu.appendChild(copyLinkItem);
        const deleteItem = document.createElement("button");
        deleteItem.type = "button";
        deleteItem.setAttribute("role", "menuitem");
        deleteItem.className = "notes-overflow-item danger";
        deleteItem.textContent = "Delete note";
        deleteItem.addEventListener("click", (e) => {
            e.stopPropagation();
            setMenuOpen(false);
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
        shareIndicator.setAttribute("role", "img");
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
            setMenuOpen(false);
        }, { signal });

        header.append(backBtn, titleInput, meta);
        right.appendChild(header);

        const editor = document.createElement("div");
        editor.className = "notes-editor";
        // Toast UI Editor renders into a div (not a textarea) and
        // builds its own chrome inside. Lazy-init so the container
        // has real dimensions when ProseMirror measures. The
        // notes-editor-host class is what CSS sizes.
        const editorContainer = document.createElement("div");
        editorContainer.id = "tui-editor-" + tab.id;
        editorContainer.className = "notes-editor-host";
        editor.appendChild(editorContainer);
        // Conflict banner host (same styles as the wiki's): a save that
        // collided with another writer's overlapping edit shows an explicit
        // choice here instead of silently pausing saves.
        const bannerHost = document.createElement("div");
        bannerHost.className = "wiki-sync-banner-host";
        right.appendChild(bannerHost);
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

        // ── Behavior ──────────────────────────────────────────

        async function refreshList() {
            const url = localState.search
                ? "/console/api/books/personal/search?q=" + encodeURIComponent(localState.search)
                : "/console/api/books/personal/pages";
            try {
                const resp = await apiJSON(url);
                localState.list = (resp && resp.items) || [];
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
            // Pages have no folders (grouping by one put every note
            // under a lone "Unfiled" heading).
            for (const n of localState.list) tree.appendChild(renderRow(n));
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
            // The editor's own sanitizer is the DOMPurify 2.3.3 copy
            // bundled into TOAST UI; use the shared, current one.
            if (window.familiarSanitize) opts.customHTMLSanitizer = window.familiarSanitize.editor;
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
            // Flush any pending save before switching, and wait out one
            // already in flight: if it resolved after the switch it used
            // to repoint localState.note at the old note, so the next
            // keystroke overwrote that note with this one's text.
            if (localState.saveTimer) {
                clearTimeout(localState.saveTimer);
                localState.saveTimer = null;
                await flushSave(true);
            }
            if (localState.savePromise) {
                try { await localState.savePromise; } catch (_) { /* reported by flushSave */ }
            }
            clearConflictBanner();
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
                seedBase(n);
                renderTree();
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

        async function newNote() {
            try {
                const n = await apiJSON("/console/api/books/personal/pages", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ title: "Untitled", content: "" }),
                });
                localState.list.unshift({
                    id: n.id, title: n.title,
                    pinned: false, snippet: "", updated_at: n.updated_at,
                });
                renderTree();
                localState.history.length = 0;
                loadNote(n.id);
                // Surface the new note in the sidebar rail (and any second
                // notes panel) immediately — deleteNote fires notesChanged;
                // create must too, or the rail lagged until reload.
                window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
            } catch (e) {
                notifyErr("Couldn't create note: " + (e.message || String(e)));
            }
        }

        // One-shot guard so a save-failure streak toasts once, not on
        // every debounced retry. Reset on the next successful save.
        let saveFailedNotified = false;
        // Coalesces the sidebar-row refresh after content saves so a body
        // edit's updated-at meta reflects in the rail
        // without refetching it on every 500ms autosave.
        let sidebarMetaTimer = null;
        function currentContent() { return tuiEditor ? tuiEditor.getMarkdown() : ""; }
        function currentTitle() { return titleInput.value || "Untitled"; }

        // seedBase records n as the version the editor is in sync with.
        // The body is read back from the editor (when it is showing n) so
        // Toast UI's markdown normalization doesn't read as an edit.
        function seedBase(n) {
            localState.baseTitle = n.title || "Untitled";
            localState.baseContent = tuiEditor && currentContent() !== null ? currentContent() : (n.content || "");
        }

        function isDirty() {
            // No editor, nothing typed: a destroyed editor reads as empty,
            // and treating that as an edit would save an empty note.
            if (!localState.note || !tuiEditor) return false;
            return currentTitle() !== localState.baseTitle || currentContent() !== localState.baseContent;
        }

        // keepalive: the page is being hidden or unloaded; the browser
        // cancels ordinary requests then, and this one must land.
        function flushSave(immediate, keepalive) {
            if (!localState.note) return Promise.resolve();
            if (localState.saving) {
                // Re-run once the in-flight save returns (see pendingResave).
                localState.pendingResave = true;
                return localState.savePromise || Promise.resolve();
            }
            // A previous save hit a conflict; the banner offers the way
            // out (use theirs / keep mine). Nothing saves until then.
            if (localState.saveBlocked) return Promise.resolve();
            localState.savePromise = doSave(keepalive).finally(() => {
                localState.savePromise = null;
                if (localState.pendingResave) {
                    localState.pendingResave = false;
                    if (!localState.saveBlocked && isDirty()) flushSave();
                }
            });
            return localState.savePromise;
        }

        async function doSave(keepalive) {
            localState.saving = true;
            savedDot.textContent = "Saving…";
            // The note this save is for. If the user switches notes while
            // it is in flight, its response must not touch the new one.
            const savingId = localState.note.id;
            const patch = { content: currentContent() };
            // Send the title only when it changed. An unchanged title in
            // every save made the server treat each save as a rename,
            // ruling out its three-way merge of concurrent edits.
            if (currentTitle() !== localState.baseTitle) patch.title = currentTitle();
            // If-Match carries the version we last synced with. The server
            // merges a concurrent writer's disjoint edit against it, and
            // refuses (409) only when the edits overlap.
            const headers = { "Content-Type": "application/json" };
            if (localState.note.updated_at) {
                headers["If-Match"] = localState.note.updated_at;
            }
            try {
                const resp = await fetch(
                    "/console/api/books/personal/page-by-id/" + encodeURIComponent(savingId),
                    {
                        method: "PATCH",
                        credentials: "include",
                        headers,
                        body: JSON.stringify(patch),
                        keepalive: !!keepalive,
                    },
                );
                const text = await resp.text();
                let respBody = null;
                try { respBody = text ? JSON.parse(text) : null; } catch (e) { /* fall through */ }
                const stillHere = localState.note && localState.note.id === savingId;
                if (resp.status === 404 && stillHere) {
                    notePageGone();
                    return;
                }
                if (resp.status === 409 && respBody && respBody.error === "stale") {
                    if (stillHere) handleSaveConflict(patch, respBody.current);
                    return;
                }
                if (!resp.ok) {
                    throw new Error((respBody && respBody.error) || ("HTTP " + resp.status));
                }
                const n = respBody;
                updateListRow(n);
                if (!stillHere) return;

                keepShareURL(n);
                if (n.merged) {
                    // The server merged our save with another writer's
                    // disjoint edit; the response is the combined document.
                    if (currentContent() === patch.content) {
                        localState.note = n;
                        updateShareIndicator();
                        suppressSave = true;
                        if (tuiEditor) tuiEditor.setMarkdown(n.content || "");
                        suppressSave = false;
                        seedBase(n);
                        savedDot.textContent = "Merged — " + (n.updated_by || "another editor");
                    } else {
                        // The user kept typing during the save. Keep the
                        // old base: the next save is then stale against
                        // the merged version and merges again, keeping
                        // both their new text and the other edit. Adopting
                        // the merged version as the base while keeping the
                        // editor's text would overwrite the other edit.
                        localState.pendingResave = true;
                        savedDot.textContent = "Merging…";
                    }
                } else {
                    localState.note = n;
                    updateShareIndicator();
                    localState.baseTitle = n.title || "Untitled";
                    localState.baseContent = patch.content;
                    savedDot.textContent = isDirty() ? "•" : "Saved";
                    if (isDirty()) localState.pendingResave = true;
                }
                // Publicly shared pages keep their diagram PNGs in
                // step with the content (the share page is script-
                // free, so diagrams ship as pre-rendered bitmaps).
                if (n.share && window.familiarMermaid) {
                    window.familiarMermaid.syncShareRenders(
                        { bookSlug: "personal", pageId: n.id }, n.content || patch.content);
                }
                saveFailedNotified = false;
                setTimeout(() => {
                    if (savedDot.textContent === "Saved" || savedDot.textContent.indexOf("Merged") === 0) {
                        savedDot.textContent = "";
                    }
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

        // updateListRow reflects a saved note in the list — but ONLY
        // re-renders when something the tree actually shows has changed.
        //
        // A row renders title (+ pinned styling) and snippet, and the
        // snippet is deliberately carried over unchanged. So a body-only
        // edit produces byte-identical DOM, and calling renderTree() on
        // every autosave tore down and rebuilt the whole list to draw
        // exactly what was already there. With a 500ms save debounce that
        // is a full rebuild every time you pause mid-sentence, which is
        // what made the sidebar visibly glitch while typing.
        function updateListRow(n) {
            const idx = localState.list.findIndex((x) => x.id === n.id);
            let visibleChange = false;
            if (idx >= 0) {
                const prev = localState.list[idx];
                visibleChange =
                    prev.title !== n.title ||
                    !!prev.pinned !== !!n.pinned;
                localState.list[idx] = {
                    id: n.id, title: n.title,
                    pinned: n.pinned, snippet: prev.snippet,
                    updated_at: n.updated_at,
                };
                if (visibleChange) renderTree();
            }
            // Same reasoning for the sidebar rail. It shows an updated-at
            // meta, so it does want refreshing eventually, but not while
            // the user is still typing. Fire immediately when something
            // visible changed, otherwise wait for a real idle gap so a
            // body-only edit never yanks the rail mid-thought.
            if (sidebarMetaTimer) clearTimeout(sidebarMetaTimer);
            sidebarMetaTimer = setTimeout(() => {
                sidebarMetaTimer = null;
                window.dispatchEvent(new Event("familiar:sidebarRefresh"));
            }, visibleChange ? 0 : 15000);
        }

        // handleSaveConflict reconciles a 409-stale response: another
        // writer's edit overlaps ours, so the server couldn't merge.
        //
        //   1. Our content already matches the server's: nothing to
        //      resolve. Adopt the server's row and carry on.
        //   2. Real divergence: pause saving and show a banner with an
        //      explicit choice. The editor keeps the user's text until
        //      they pick. (The old path told them to click the note in
        //      the sidebar, which did nothing for the open note, while
        //      every later edit was silently dropped.)
        function handleSaveConflict(patch, serverPage) {
            if (serverPage &&
                (serverPage.content || "") === (patch.content || "") &&
                (patch.title === undefined || (serverPage.title || "") === patch.title)) {
                // The conflict payload is the bare page: keep this user's
                // pin and the share, which replacing the note dropped (the
                // globe vanished from a note still public).
                serverPage.pinned = localState.note.pinned;
                serverPage.share = localState.note.share;
                localState.note = serverPage;
                seedBase(serverPage);
                savedDot.textContent = "Saved";
                setTimeout(() => {
                    if (savedDot.textContent === "Saved") savedDot.textContent = "";
                }, 1500);
                return;
            }
            localState.saveBlocked = true;
            savedDot.textContent = "Conflict";
            showConflictBanner(serverPage);
        }

        function showConflictBanner(serverPage) {
            clearConflictBanner();
            const banner = document.createElement("div");
            banner.className = "wiki-sync-banner";
            banner.setAttribute("role", "alert");
            const msg = document.createElement("div");
            msg.className = "wiki-sync-banner-msg";
            msg.textContent = "This note was changed by " +
                ((serverPage && serverPage.updated_by) || "someone else") +
                " in the same place you were editing. Nothing has been saved since.";
            const actions = document.createElement("div");
            actions.className = "wiki-sync-banner-actions";
            const useTheirs = document.createElement("button");
            useTheirs.type = "button";
            useTheirs.className = "wiki-sync-banner-btn";
            useTheirs.textContent = "Use theirs (discard mine)";
            useTheirs.addEventListener("click", () => {
                const id = localState.noteId;
                localState.saveBlocked = false;
                localState.pendingResave = false;
                if (localState.saveTimer) {
                    clearTimeout(localState.saveTimer);
                    localState.saveTimer = null;
                }
                // Reload from the server; discards the local edits.
                localState.note = null;
                loadNote(id);
            });
            const keepMine = document.createElement("button");
            keepMine.type = "button";
            keepMine.className = "wiki-sync-banner-btn wiki-sync-banner-btn-primary";
            keepMine.textContent = "Keep mine (overwrite)";
            keepMine.addEventListener("click", async () => {
                // Save the editor's text over the current version.
                let current = serverPage;
                if (!current) {
                    try {
                        current = await apiJSON("/console/api/books/personal/page-by-id/" +
                            encodeURIComponent(localState.noteId));
                    } catch (e) {
                        notifyErr("Couldn't reach the server: " + (e.message || String(e)));
                        return;
                    }
                }
                localState.note.updated_at = current.updated_at;
                localState.baseTitle = current.title || "Untitled";
                localState.baseContent = current.content || "";
                localState.saveBlocked = false;
                clearConflictBanner();
                flushSave(true);
            });
            actions.append(useTheirs, keepMine);
            banner.append(msg, actions);
            bannerHost.appendChild(banner);
        }

        function clearConflictBanner() {
            bannerHost.innerHTML = "";
        }

        // keepShareURL fills in a save response's share link from the one
        // already known, if the response left it out. The response is the
        // truth about whether the note is shared: carrying the old share
        // forward whenever the response had none hid a share turned off
        // elsewhere.
        function keepShareURL(n) {
            const known = localState.note && localState.note.share;
            if (n.share && !n.share.public_url && known && known.share_key === n.share.share_key) {
                n.share.public_url = known.public_url;
            }
        }

        // notePageGone: the open note was deleted elsewhere (a save 404'd,
        // or a page-deleted event arrived). Saving stops, and the banner
        // offers the text, which nothing else can bring back.
        function notePageGone() {
            if (!localState.note) return;
            localState.saveBlocked = true;
            if (localState.saveTimer) { clearTimeout(localState.saveTimer); localState.saveTimer = null; }
            savedDot.textContent = "Deleted";
            clearConflictBanner();
            const banner = document.createElement("div");
            banner.className = "wiki-sync-banner";
            banner.setAttribute("role", "alert");
            const msg = document.createElement("div");
            msg.className = "wiki-sync-banner-msg";
            msg.textContent = "This note was deleted elsewhere. Nothing you type here will be saved.";
            const actions = document.createElement("div");
            actions.className = "wiki-sync-banner-actions";
            const copy = document.createElement("button");
            copy.type = "button";
            copy.className = "wiki-sync-banner-btn";
            copy.textContent = "Copy the text";
            copy.addEventListener("click", async () => {
                try {
                    await navigator.clipboard.writeText(currentContent());
                    toast("Note text copied", "success");
                } catch (e) {
                    notifyErr("Couldn't copy: " + (e.message || String(e)));
                }
            });
            const recreate = document.createElement("button");
            recreate.type = "button";
            recreate.className = "wiki-sync-banner-btn wiki-sync-banner-btn-primary";
            recreate.textContent = "Save as a new note";
            recreate.addEventListener("click", async () => {
                try {
                    const n = await apiJSON("/console/api/books/personal/pages", {
                        method: "POST",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({ title: currentTitle(), content: currentContent() }),
                    });
                    localState.saveBlocked = false;
                    localState.note = null;
                    localState.history.length = 0;
                    await loadNote(n.id);
                    refreshList();
                    window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
                } catch (e) {
                    notifyErr("Couldn't save: " + (e.message || String(e)));
                }
            });
            actions.append(copy, recreate);
            banner.append(msg, actions);
            bannerHost.appendChild(banner);
        }

        function scheduleSave() {
            if (!localState.note) return;
            // While a conflict is unresolved, keep saying so; "•" read as
            // "saving soon" while nothing was being saved.
            if (localState.saveBlocked) {
                savedDot.textContent = "Conflict";
                return;
            }
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
                // A pin is a per-user preference, not an edit: take only the
                // flag. Adopting the whole response moved the version stamp
                // past edits made elsewhere, so the next save overwrote them.
                if (localState.note && localState.note.id === n.id) localState.note.pinned = n.pinned;
                const idx = localState.list.findIndex((x) => x.id === n.id);
                if (idx >= 0) {
                    localState.list[idx] = { ...localState.list[idx], pinned: n.pinned };
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
            if (!localState.note || localState.shareBusy) return;
            const nextEnabled = !localState.note.share;
            localState.shareBusy = true;
            shareItem.disabled = true;
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
            } finally {
                localState.shareBusy = false;
                shareItem.disabled = false;
            }
        }

        // Copy the current note's public share URL to the clipboard.
        // No-op when nothing's shared (the menu item is hidden in
        // that state, but guard anyway).
        async function copyShareLink() {
            const s = localState.note && localState.note.share;
            if (!s || !s.public_url) {
                notifyErr("Couldn't copy: this note's public link isn't known. Reopen the note and try again.");
                return;
            }
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
                // The user switched notes while this was in flight.
                if (!localState.note || localState.note.id !== n.id) return;
                // Refresh share state regardless of content changes —
                // a remote share toggle should reflect immediately.
                localState.note.share = n.share || null;
                updateShareIndicator();
                // With unsaved edits (typed, saving, or waiting on the
                // debounce), leave the editor and the base version alone:
                // replacing the editor threw those keystrokes away, and
                // adopting the new version while keeping them would make
                // the next save overwrite the change that triggered this.
                // The next save goes out against the old version and the
                // server merges the two edits.
                if (localState.saving || localState.saveTimer || localState.saveBlocked || isDirty()) {
                    refreshList();
                    return;
                }
                localState.note = n;
                // Only touch the editor if the content actually changed
                // (avoids resetting the cursor position).
                if (n.content !== currentContent() || (n.title || "") !== titleInput.value) {
                    titleInput.value = n.title || "";
                    suppressSave = true;
                    if (tuiEditor) {
                        tuiEditor.setMarkdown(n.content || "");
                    }
                    suppressSave = false;
                }
                seedBase(n);
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

            // Send a pending edit when the page goes away or into the
            // background. The unload save was a plain fetch the browser
            // cancelled, sent only while the debounce was pending, and
            // nothing saved when the app was merely backgrounded (a
            // phone may then discard it).
            const flushOnHide = () => {
                if (!localState.note || localState.saveBlocked || !isDirty()) return;
                if (localState.saveTimer) { clearTimeout(localState.saveTimer); localState.saveTimer = null; }
                flushSave(true, /*keepalive=*/true);
            };
            window.addEventListener("beforeunload", flushOnHide, { signal });
            window.addEventListener("pagehide", flushOnHide, { signal });
            document.addEventListener("visibilitychange", () => {
                if (document.visibilityState === "hidden") flushOnHide();
            }, { signal });

            // Live-refresh when the chat surface modifies a note
            // via tool calls (append_to_note, update_note, etc.).
            window.addEventListener("familiar:notesChanged", () => {
                // On the splash, refreshCurrentNote early-returns (no note
                // loaded), so a note added/deleted/moved elsewhere left the
                // Recent/Pinned lists stale. Re-render the splash instead.
                if (root.classList.contains("is-splash")) {
                    renderSplash();
                    return;
                }
                refreshCurrentNote();
            }, { signal });

            // A pin toggled elsewhere (another notes panel, the Home grid, or
            // the AI) only broadcast familiar:pinsChanged, which notes.js did
            // not consume — so a second panel kept a stale pin glyph and the
            // splash's Pinned list lagged. Refresh the affected view.
            window.addEventListener("familiar:pinsChanged", () => {
                if (root.classList.contains("is-splash")) {
                    renderSplash();
                    return;
                }
                refreshList();
            }, { signal });

            // Server-pushed page-saved / page-deleted events. Lets a
            // device idle on a note pick up an edit made on another
            // device without polling. We only act on the page
            // currently open AND only when the local editor is clean
            // (no pending autosave, no in-flight save) — if the user
            // is mid-typing the next save will hit 409 and surface
            // the conflict via the existing path.
            window.addEventListener("familiar:pageEvent", (ev) => {
                const d = ev.detail || {};
                // A note added/deleted on another device or by the AI arrives
                // here with no note open; on the splash, refresh its
                // Recent/Pinned lists (the open-note case continues below).
                if (root.classList.contains("is-splash")) {
                    renderSplash();
                    return;
                }
                if (!localState.note || d.page_id !== localState.note.id) return;
                if (d.kind === "page-deleted") {
                    // Deleted elsewhere: the list drops the row, and the
                    // editor stops saving and offers the text back.
                    refreshList();
                    notePageGone();
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
            }, { signal });
        }

        // dispose releases the shell when its tab closes: its window and
        // document listeners, a pending save timer, and the editor.
        // What was typed goes first: an edit still in the debounce is
        // sent now, and the editor is released only once the save chain
        // (including a re-run for text typed during an in-flight save)
        // has read it.
        function dispose() {
            if (localState.saveTimer) { clearTimeout(localState.saveTimer); localState.saveTimer = null; }
            if (localState.note && !localState.saveBlocked && isDirty()) flushSave(true);
            shellAbort.abort();
            if (sidebarMetaTimer) { clearTimeout(sidebarMetaTimer); sidebarMetaTimer = null; }
            (localState.savePromise || Promise.resolve()).finally(() => {
                localState.note = null;
                if (tuiEditor) {
                    try { tuiEditor.destroy(); } catch (_) { /* already gone */ }
                    tuiEditor = null;
                }
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
        return { root, init, refreshList, loadNote: externalLoadNote, newNote, refreshEditor, enterSplash, dispose };
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

    // Tab closed (or its splash morphed to another surface) — release
    // the shell and drop its entry.
    // Mirrors the wiki surface's listener.
    window.addEventListener("familiar:tabClosed", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "notes") return;
        const entry = shells.get(d.tabId);
        if (entry && entry.model.dispose) entry.model.dispose();
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
