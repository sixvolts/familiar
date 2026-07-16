// Wiki surface renderer (BOOKS-WIKI-ARCHITECTURE Phase 1b).
//
// Layout mirrors the Notes panel: left rail with a book-picker
// dropdown and a page list, right rail with a markdown editor
// (same Toast UI instance pattern Notes uses). The two share the
// notes-* CSS classes for the shell so the visual treatment stays
// consistent; wiki-specific add-ons get .wiki-* prefixes.
//
// Per-tab state shape:
//   { bookSlug?: string, pageSlug?: string, pageId?: string }
// The pageId is also stamped onto tab.state so workspace.js's
// getTabDocId() can identify the open doc for layout-restore +
// dedupe — pageSlug is what the API actually uses.
//
// Phase 1b is read/write CRUD only. Phase 1d adds the async
// knowledge-ingestion side-effects on save (embed + extract +
// CommitFacts with scope_tag = 'book:{id}'). Phase 1e adds the
// per-book semantic search input.

(function () {
    "use strict";

    const helpers = window.familiarAppHelpers;
    if (!helpers) {
        console.error("wiki: app helpers not loaded; wiki surface disabled");
        return;
    }

    // Surface an action failure through the toast UI instead of a
    // blocking native alert (EXTERNAL-READINESS-REVIEW.md P2).
    function notifyErr(msg) {
        if (helpers && helpers.toast) helpers.toast(msg, "error");
        else window.alert(msg);
    }
    const { apiJSON, toast } = helpers;

    // Cached per-tab shells so switching surfaces back to wiki
    // doesn't blow away the editor + selection. Same pattern Notes
    // and Chat use.
    const shells = new Map(); // tab.id -> { root, model }

    function escapeHTML(s) {
        return String(s == null ? "" : s)
            .replace(/&/g, "&amp;").replace(/</g, "&lt;")
            .replace(/>/g, "&gt;").replace(/"/g, "&quot;");
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
    function updateStatusBar(content) {
        const sb = window.familiarStatusBar;
        if (!sb || !sb.setContext) return;
        const text = (content || "").trim();
        const words = text === "" ? 0 : text.split(/\s+/).length;
        sb.setContext(words + " word" + (words === 1 ? "" : "s"));
    }

    // ── Markdown pipeline (read-only research view) ───────────────
    //
    // The normal editable page uses Toast UI. The transient
    // "live research — read only" view (RESEARCH-SKILL-SPEC §6.7)
    // renders the hidden evidence page's markdown to static HTML
    // instead — the user must NOT be able to edit system scratch
    // that the workers keep appending to. Reuses the same
    // marked + DOMPurify pipeline chat/notes bootstrap; if either
    // already loaded it, this is a no-op promise.
    const CDN = {
        marked:    "/vendor/marked/marked.min.js",
        dompurify: "/vendor/dompurify/purify.min.js",
        hljsJS:    "/vendor/highlight/core.min.js",
        hljsCSS:   "/vendor/highlight/atom-one-dark.min.css",
    };
    let depsPromise = null;
    function ensureMarkdownDeps() {
        if (depsPromise) return depsPromise;
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
                console.warn("wiki: markdown deps failed, falling back", e);
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

    function render(host, tab) {
        updateStatusBar("");
        // Mark every OTHER shell as background — render() is the
        // workspace's signal that `tab` is the active wiki tab in
        // its panel, so any sibling wiki shell that was foreground
        // a moment ago shouldn't keep its 5s fast-poll going. Sync
        // state on those shells stays — the slow 60s poll keeps
        // running unchanged.
        for (const [otherId, entry] of shells) {
            if (otherId !== tab.id && entry.model.setForeground) {
                entry.model.setForeground(false);
            }
        }
        const cached = shells.get(tab.id);
        if (cached) {
            host.innerHTML = "";
            host.appendChild(cached.root);
            if (cached.model.refreshEditor) cached.model.refreshEditor();
            cached.model.refresh();
            if (cached.model.setForeground) cached.model.setForeground(true);
            return;
        }
        const model = newWikiModel(tab);
        host.innerHTML = "";
        host.appendChild(model.root);
        shells.set(tab.id, { root: model.root, model });
        model.init();
        if (model.setForeground) model.setForeground(true);
    }

    function newWikiModel(tab) {
        const persisted = tab.state || {};
        const localState = {
            // Three view modes — wiki-splash (no book picked, lists
            // every book the user can access), book-splash (book
            // picked, no page; lists the book's pages), page-editor
            // (book + page picked; current Toast UI editor).
            // Resolved from persisted state on first render.
            viewMode: persisted.pageSlug
                ? "page-editor"
                : (persisted.bookSlug ? "book-splash" : "wiki-splash"),
            bookSlug: persisted.bookSlug || null,
            pageSlug: persisted.pageSlug || null,
            book: null,
            books: [],
            pages: [],
            page: null,
            saving: false,
            saveTimer: null,
            suppressSave: false,
            // Per-tab back stack of {bookSlug, pageSlug} entries.
            // Pushed when the user follows a [[link]] from one page
            // to another so a back chevron can return them. Cleared
            // when the user jumps via the page list / book switcher
            // (those are explicit navigation, not a trail).
            history: [],

            // Sync state. baseUpdatedAt is the page's updated_at
            // value as of the last read/write we observed from the
            // server — sent on every PATCH as If-Match so the server
            // can refuse stale writes (409). baseTitle / baseContent
            // are the values that landed with that updated_at; we
            // compare the current editor state against them to know
            // whether the user is "dirty" (typed since the last
            // sync). pollTimer / fastPollTimer are the two cadences
            // — every open page polls at slowPollMs; the foreground
            // tab additionally polls at fastPollMs. Banner state is
            // tracked so we don't stack multiple banners on rapid
            // polls.
            baseUpdatedAt: null,
            baseTitle: "",
            baseContent: "",
            pollTimer: null,
            fastPollTimer: null,
            banner: null,
            isForeground: false,

            // Live-research read-only view (RESEARCH-SKILL-SPEC §6.7).
            // Non-null while this wiki tab is showing the hidden
            // evidence page read-only: { bookSlug, pageSlug, pageId }.
            // pageId is learned from the first fetch so page-saved SSE
            // events can be matched to it. Distinct from the editable
            // page state above — the two never coexist in one tab.
            research: null,
        };

        // ── Shell DOM ─────────────────────────────────────────
        const root = document.createElement("div");
        root.className = "notes-shell wiki-shell";

        // Left rail: book picker on top, page list below.
        const left = document.createElement("aside");
        left.className = "notes-left";

        const leftHead = document.createElement("div");
        leftHead.className = "notes-left-head wiki-left-head";

        const bookSelect = document.createElement("select");
        bookSelect.className = "wiki-book-select";

        const newBookBtn = document.createElement("button");
        newBookBtn.type = "button";
        newBookBtn.className = "chat-new-btn wiki-new-book-btn";
        newBookBtn.textContent = "+ Book";
        newBookBtn.title = "New book";

        leftHead.append(bookSelect, newBookBtn);
        left.appendChild(leftHead);

        // Page list header (shows the current book + new-page).
        const pagesHead = document.createElement("div");
        pagesHead.className = "wiki-pages-head";
        const pagesEyebrow = document.createElement("span");
        pagesEyebrow.className = "wiki-pages-eyebrow";
        pagesEyebrow.textContent = "Pages";
        const newPageBtn = document.createElement("button");
        newPageBtn.type = "button";
        newPageBtn.className = "wiki-new-page-btn";
        newPageBtn.textContent = "+";
        newPageBtn.title = "New page";
        newPageBtn.disabled = true;
        pagesHead.append(pagesEyebrow, newPageBtn);
        left.appendChild(pagesHead);

        const tree = document.createElement("div");
        tree.className = "notes-tree";
        left.appendChild(tree);
        root.appendChild(left);

        // Right rail: header + editor.
        const right = document.createElement("section");
        right.className = "notes-right";

        const header = document.createElement("header");
        // Same layout as notes-header; is-wiki swaps the iris/purple
        // tint for slate/blue so wiki tabs are distinguishable from
        // notes tabs in the page chrome.
        header.className = "notes-header is-wiki";

        // Back chevron — visible only when localState.history has
        // entries (i.e. the user got here by following a [[link]]).
        // Same line-weight as the rest of the sidebar glyphs so it
        // sits inside the page chrome rather than fighting it.
        const backBtn = document.createElement("button");
        backBtn.type = "button";
        backBtn.className = "notes-back";
        backBtn.title = "Back";
        backBtn.setAttribute("aria-label", "Back");
        backBtn.hidden = true;
        backBtn.innerHTML = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 6 L9 12 L15 18"/></svg>';

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

        const overflow = document.createElement("div");
        overflow.className = "notes-overflow";
        const overflowBtn = document.createElement("button");
        overflowBtn.type = "button";
        overflowBtn.className = "notes-overflow-btn";
        overflowBtn.textContent = "⋯";
        overflowBtn.title = "More actions";
        overflowBtn.addEventListener("click", (e) => {
            e.stopPropagation();
            // Refresh role-gated items each time the menu opens so
            // they reflect the current book's caller-role. Delete
            // page is owner/writer only; readers can't see it.
            deletePageItem.style.display = canWritePages() ? "" : "none";
            // Content insertion is a write.
            addImageItem.style.display = canWritePages() ? "" : "none";
            addDiagramItem.style.display = canWritePages() ? "" : "none";
            // Sharing is a write; copy-link shows whenever a share
            // exists (the public URL is fair game for any member).
            // Pin is per-user → shown to everyone; just refresh the label.
            pinItem.textContent = (localState.page && localState.page.pinned) ? "Unpin page" : "Pin page";
            const shared = !!(localState.page && localState.page.share);
            shareItem.style.display = canWritePages() ? "" : "none";
            shareItem.textContent = shared ? "Stop sharing publicly" : "Share publicly";
            copyLinkItem.hidden = !shared;
            overflow.classList.toggle("is-open");
        });
        const overflowMenu = document.createElement("div");
        overflowMenu.className = "notes-overflow-menu";
        // Pin — a per-user preference, so it's available to ANY member
        // (not write-gated like delete/share). Surfaces the page on Home.
        const pinItem = document.createElement("button");
        pinItem.type = "button";
        pinItem.className = "notes-overflow-item";
        pinItem.textContent = "Pin page";
        pinItem.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            togglePinPage();
        });
        overflowMenu.appendChild(pinItem);
        // Content insertion (MEDIA-DIAGRAMS): discoverable entry
        // points for image uploads and mermaid fences.
        const imagePicker = document.createElement("input");
        imagePicker.type = "file";
        imagePicker.accept = "image/png,image/jpeg,image/gif,image/webp";
        imagePicker.hidden = true;
        imagePicker.addEventListener("change", () => {
            const file = imagePicker.files && imagePicker.files[0];
            imagePicker.value = "";
            if (!file || !localState.page || !tuiEditor) return;
            window.familiarWikiLink.uploadImage(
                { bookSlug: localState.bookSlug, pageId: localState.page.id }, file,
            ).then((d) => {
                tuiEditor.exec("addImage", { imageUrl: d.url, altText: d.alt_text || file.name });
            }).catch((e) => {
                notifyErr("Image upload failed: " + (e.message || e));
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
            if (!tuiEditor || !localState.page) return;
            const md = tuiEditor.getMarkdown();
            const fenceIndex = (md.match(/```mermaid/g) || []).length;
            const starter = "graph TD;\n  A[Start] --> B[Next];";
            tuiEditor.setMarkdown(
                md.replace(/\n*$/, "") + "\n\n```mermaid\n" + starter + "\n```\n",
            );
            const ws = window.FamiliarWorkspace;
            if (ws && ws.openDoc) {
                const title = (localState.page.title || "Page") + " · diagram";
                ws.openDoc("diagram", localState.bookSlug + "/" + localState.page.id + "#" + fenceIndex, title, {
                    book_slug: localState.bookSlug,
                    page_id: localState.page.id,
                    fence_index: fenceIndex,
                    source: starter,
                    page_title: localState.page.title || "Page",
                });
            }
        });
        overflowMenu.appendChild(addDiagramItem);
        // Public sharing — toggle + copy-link, same as Notes. Sharing a
        // wiki page is a write action (book writer/owner only); the
        // backend enforces it, and the open handler hides the toggle for
        // readers. The copy-link row appears once a share exists.
        const shareItem = document.createElement("button");
        shareItem.type = "button";
        shareItem.className = "notes-overflow-item";
        shareItem.textContent = "Share publicly";
        shareItem.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            toggleSharePage();
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
            copySharePageLink();
        });
        overflowMenu.appendChild(copyLinkItem);
        // Archive book is a book-level action — it now lives on the
        // book splash's overflow (renderBookSplash), not here in a
        // page's menu.
        const deletePageItem = document.createElement("button");
        deletePageItem.type = "button";
        deletePageItem.className = "notes-overflow-item danger";
        deletePageItem.textContent = "Delete page";
        deletePageItem.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            deletePage();
        });
        overflowMenu.appendChild(deletePageItem);
        overflow.append(overflowBtn, overflowMenu);

        // Globe indicator next to the title — visible only when the page
        // is publicly shared (same component Notes uses).
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

        function updateShareIndicator() {
            shareIndicator.hidden = !(localState.page && localState.page.share);
        }

        // Toggle the page's public share. Mirrors notes.toggleShareNote
        // but keys off the page's own book slug (not "personal"). The
        // endpoint is idempotent server-side, so re-enabling returns the
        // same share key.
        async function toggleSharePage() {
            if (!localState.page) return;
            const nextEnabled = !localState.page.share;
            try {
                const resp = await apiJSON(
                    "/console/api/books/" + encodeURIComponent(localState.bookSlug) +
                        "/page-by-id/" + encodeURIComponent(localState.page.id) + "/share",
                    {
                        method: "POST",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({ enabled: nextEnabled }),
                    },
                );
                if (resp && resp.enabled) {
                    // Pre-render diagrams for the share NOW — the public
                    // page is script-free and can't run mermaid itself.
                    if (window.familiarMermaid && tuiEditor) {
                        window.familiarMermaid.syncShareRenders(
                            { bookSlug: localState.bookSlug, pageId: localState.page.id },
                            tuiEditor.getMarkdown());
                    }
                    localState.page.share = {
                        share_key: resp.share_key,
                        public_url: resp.public_url,
                        visibility: resp.visibility,
                    };
                    toast("Page shared publicly", "success");
                } else {
                    localState.page.share = null;
                    toast("Page is no longer shared", "success");
                }
                updateShareIndicator();
            } catch (e) {
                notifyErr("Couldn't update share: " + (e.message || String(e)));
            }
        }

        async function copySharePageLink() {
            const s = localState.page && localState.page.share;
            if (!s || !s.public_url) return;
            try {
                await navigator.clipboard.writeText(s.public_url);
                toast("Public link copied", "success");
            } catch (e) {
                notifyErr("Couldn't copy: " + (e.message || String(e)));
            }
        }

        // Pin/unpin the page for the current user. Uses the dedicated
        // per-user /pin endpoint (any member, not write-gated) — its
        // response carries pinned but not share, so we patch the flag in
        // place rather than replacing localState.page. Home + the rail
        // re-fetch on familiar:pinsChanged.
        async function togglePinPage() {
            if (!localState.page) return;
            const nextPinned = !localState.page.pinned;
            try {
                const resp = await apiJSON(
                    "/console/api/books/" + encodeURIComponent(localState.bookSlug) +
                        "/page-by-id/" + encodeURIComponent(localState.page.id) + "/pin",
                    {
                        method: "POST",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({ pinned: nextPinned }),
                    },
                );
                localState.page.pinned =
                    resp && typeof resp.pinned === "boolean" ? resp.pinned : nextPinned;
                window.dispatchEvent(new CustomEvent("familiar:pinsChanged"));
                toast(localState.page.pinned ? "Page pinned" : "Page unpinned", "success");
            } catch (e) {
                notifyErr("Couldn't pin: " + (e.message || String(e)));
            }
        }

        // Click-outside closes the menu — same pattern as notes.
        document.addEventListener("click", () => overflow.classList.remove("is-open"));

        header.append(backBtn, titleInput, meta, overflow);

        // updateBackBtn keeps the back chevron in sync with the
        // history stack — visible only when there's somewhere to
        // go back to.
        function updateBackBtn() {
            backBtn.hidden = localState.history.length === 0;
        }

        backBtn.addEventListener("click", async () => {
            const prev = localState.history.pop();
            updateBackBtn();
            if (!prev) return;
            // If the previous page is in a different book, switch
            // book context first — same dance wikiNavigate does.
            if (prev.bookSlug && prev.bookSlug !== localState.bookSlug) {
                if (!localState.books.find((b) => b.slug === prev.bookSlug)) {
                    await refreshBooks();
                }
                if (!localState.books.find((b) => b.slug === prev.bookSlug)) return;
                localState.bookSlug = prev.bookSlug;
                tab.state = { ...(tab.state || {}), bookSlug: prev.bookSlug };
                bookSelect.value = prev.bookSlug;
                try {
                    const b = await apiJSON("/console/api/books/" + encodeURIComponent(prev.bookSlug));
                    localState.book = b;
                } catch (e) { return; }
                await refreshPages();
            }
            loadPage(prev.pageSlug);
        });
        right.appendChild(header);

        // Sync-conflict banner host. The poller / save path drop a
        // banner here when remote state diverged while the editor
        // was dirty; clearBanner() removes it after the user picks
        // a resolution. Empty container takes no layout space when
        // there's nothing to show.
        const bannerHost = document.createElement("div");
        bannerHost.className = "wiki-sync-banner-host";
        right.appendChild(bannerHost);

        const editor = document.createElement("div");
        editor.className = "notes-editor";
        // Toast UI Editor renders into a div (not a textarea) and
        // owns its chrome inside. notes-editor-host is the stable
        // class CSS uses to size the editor surface, distinct from
        // the page-links footer below.
        const editorContainer = document.createElement("div");
        editorContainer.id = "tui-editor-wiki-" + tab.id;
        editorContainer.className = "notes-editor-host";
        editor.appendChild(editorContainer);
        // Page-links footer — outbound + inbound link sections
        // populated by renderPageLinks() on each page load. Hidden
        // until the first non-empty render.
        const linksHost = document.createElement("div");
        linksHost.className = "notes-page-links";
        linksHost.hidden = true;
        editor.appendChild(linksHost);
        right.appendChild(editor);

        // currentBookRole reads the caller's role on the active
        // book from the cached BookSummary list. Used by both the
        // book-splash render (to gate the Manage-users button) and
        // the page-editor overflow menu (to gate the Delete-page
        // item for readers).
        function currentBookRole() {
            if (!localState.bookSlug) return "";
            const b = localState.books.find((x) => x.slug === localState.bookSlug);
            return (b && b.role) || "";
        }
        function canWritePages() {
            const r = currentBookRole();
            return r === "owner" || r === "writer";
        }

        // Splash container — content gets re-rendered whenever the
        // view mode changes. Two layouts share the same shell:
        //   wiki-splash: + New book tile on the left, books list on
        //                the right ("Your books" navigation).
        //   book-splash: + New page tile on the left, pages list on
        //                the right ("Pages in this book").
        // The page-editor view hides this entirely and shows the
        // header + Toast UI editor instead.
        const empty = document.createElement("div");
        empty.className = "notes-empty wiki-splash-host";
        right.appendChild(empty);

        // Live-research read-only host. Fills the right rail (is-splash
        // hides the left rail); a moss banner marks it as a transient,
        // non-editable view and the body renders the evidence page's
        // markdown. Hidden until openResearchReadonly() activates it.
        const researchHost = document.createElement("div");
        researchHost.className = "wiki-research-host";
        researchHost.hidden = true;
        const researchBanner = document.createElement("div");
        researchBanner.className = "wiki-research-banner";
        researchBanner.innerHTML =
            '<span class="chat-research-spinner" aria-hidden="true"></span>' +
            '<span class="wiki-research-badge">Live research — read only</span>' +
            '<span class="wiki-research-topic"></span>';
        const researchBody = document.createElement("div");
        researchBody.className = "wiki-research-body";
        const researchProse = document.createElement("div");
        // Reuse the chat surface's prose styling for the rendered
        // markdown so headings / code / lists look consistent.
        researchProse.className = "chat-msg-body";
        researchBody.appendChild(researchProse);
        researchHost.append(researchBanner, researchBody);
        right.appendChild(researchHost);

        root.appendChild(right);

        // ── Toast UI lazy init ────────────────────────────────
        let tuiEditor = null;
        let currentEditorMode = "wysiwyg"; // "wysiwyg" | "markdown"

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
        // Shared wiki-link navigate function.
        // Navigate to a wiki-linked page. For same-book links, call
        // loadPage directly. For cross-book links, switch the book
        // context first, then loadPage. We bypass loadBook's
        // "restorePage" check because we KNOW the target page slug
        // — we don't need to enumerate the page list to validate it.
        async function wikiNavigate(parsed) {
            const targetBook = parsed.bookSlug || localState.bookSlug;

            // Link follows always load in the current tab —
            // hopping to a duplicate copy in an adjacent panel
            // surprises the user (a tab in panel A flashing into
            // their work in panel B). focusExistingDoc was
            // hijacking the navigation; drop that check.
            //
            // Push the current page onto the back stack so the back
            // chevron in the new page's header can return here.
            // Don't push when there's no current page (a fresh tab
            // following its first link) — nothing to go back to.
            if (localState.bookSlug && localState.pageSlug) {
                localState.history.push({
                    bookSlug: localState.bookSlug,
                    pageSlug: localState.pageSlug,
                });
            }

            if (targetBook !== localState.bookSlug) {
                // Switch book context without loading the splash.
                if (!localState.books.find((b) => b.slug === targetBook)) {
                    await refreshBooks();
                }
                if (!localState.books.find((b) => b.slug === targetBook)) {
                    console.warn("wiki: target book not found:", targetBook);
                    return;
                }
                localState.bookSlug = targetBook;
                tab.state = { ...(tab.state || {}), bookSlug: targetBook };
                bookSelect.value = targetBook;
                try {
                    const b = await apiJSON("/console/api/books/" + encodeURIComponent(targetBook));
                    localState.book = b;
                } catch (e) {
                    console.warn("wiki: fetch book failed:", targetBook, e);
                    showBookSplash();
                    return;
                }
                await refreshPages();
            }
            // Now load the page directly.
            loadPage(parsed.pageSlug);
        }

        function initEditor(mode) {
            mode = mode || currentEditorMode;
            if (tuiEditor) return;
            var opts = window.familiarWikiLink
                ? window.familiarWikiLink.editorOptions(mode, wikiNavigate, function () {
                    return { slug: localState.bookSlug || "", name: (localState.book && localState.book.name) || localState.bookSlug || "" };
                })
                : { height: "100%", initialEditType: mode, theme: "dark", usageStatistics: false, hideModeSwitch: true, toolbarItems: [] };
            opts.el = editorContainer;
            tuiEditor = new toastui.Editor(opts);

            // Click handler for Markdown preview <a> tags.
            if (window.familiarWikiLink) {
                window.familiarWikiLink.wireClickHandler(editorContainer, wikiNavigate);
            }
            if (window.familiarMermaid) {
                window.familiarMermaid.observe(editorContainer);
            }
            if (window.familiarWikiLink && window.familiarWikiLink.wireImageUpload) {
                window.familiarWikiLink.wireImageUpload(tuiEditor, function () {
                    return localState.page && localState.bookSlug
                        ? { bookSlug: localState.bookSlug, pageId: localState.page.id }
                        : null;
                });
            }
            // A rendered mermaid block was clicked — open it in a
            // diagram tab with this page's identity attached.
            editorContainer.addEventListener("familiar:openDiagram", (ev) => {
                const d = ev.detail || {};
                const ws = window.FamiliarWorkspace;
                if (!ws || !ws.openDoc || !localState.page) return;
                const title = (localState.page.title || "Page") + " · diagram";
                ws.openDoc("diagram", localState.bookSlug + "/" + localState.page.id + "#" + (d.fenceIndex || 0), title, {
                    book_slug: localState.bookSlug,
                    page_id: localState.page.id,
                    fence_index: d.fenceIndex || 0,
                    source: d.source || "",
                    page_title: localState.page.title || "Page",
                });
            });

            tuiEditor.on("change", () => {
                if (localState.suppressSave) return;
                updateStatusBar(tuiEditor.getMarkdown());
                scheduleSave();
            });
            // Cmd/Ctrl+S anywhere inside the editor flushes the
            // pending autosave.
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
            // Save content from the current editor.
            var md = tuiEditor ? tuiEditor.getMarkdown() : "";
            // Destroy the old instance.
            if (tuiEditor) {
                tuiEditor.destroy();
                tuiEditor = null;
            }
            // Clear the container (Toast UI leaves DOM behind).
            editorContainer.innerHTML = "";
            // Create the new editor in the target mode.
            currentEditorMode = newMode;
            updateModeBar();
            initEditor(newMode);
            // Restore content.
            if (tuiEditor && md) {
                localState.suppressSave = true;
                tuiEditor.setMarkdown(md);
                localState.suppressSave = false;
            }
        }

        // ── Data flows ────────────────────────────────────────

        async function refreshBooks() {
            try {
                const resp = await apiJSON("/console/api/books");
                localState.books = (resp && resp.items) || [];
            } catch (e) {
                localState.books = [];
                console.warn("wiki: list books failed", e);
            }
            renderBookSelect();
            // Drop the persisted bookSlug if it's no longer
            // accessible (membership revoked, archived, etc.).
            if (localState.bookSlug && !localState.books.find((b) => b.slug === localState.bookSlug)) {
                localState.bookSlug = null;
                localState.pageSlug = null;
                localState.viewMode = "wiki-splash";
            }
            // No automatic default-book pick on first mount: the
            // user lands on the wiki splash and chooses where to
            // go. (Sidebar Wiki click also routes here via
            // resetToWikiSplash.)
            if (localState.viewMode === "wiki-splash") {
                applyViewMode();
            } else if (localState.bookSlug) {
                bookSelect.value = localState.bookSlug;
                await loadBook(localState.bookSlug, /*reuseViewMode*/ true);
            } else {
                applyViewMode();
            }
        }

        function renderBookSelect() {
            bookSelect.innerHTML = "";
            if (localState.books.length === 0) {
                const opt = document.createElement("option");
                opt.value = "";
                opt.textContent = "No books yet";
                opt.disabled = true;
                bookSelect.appendChild(opt);
                bookSelect.disabled = true;
                return;
            }
            bookSelect.disabled = false;
            for (const b of localState.books) {
                const opt = document.createElement("option");
                opt.value = b.slug;
                opt.textContent = b.name + (b.archived_at ? " (archived)" : "");
                bookSelect.appendChild(opt);
            }
        }

        // loadBook fetches book metadata + pages and lands on the
        // book-splash. reuseViewMode=true keeps a persisted
        // page-editor view if the same page is still loadable
        // (used during initial mount from saved state). Default
        // path always lands on the book-splash so sidebar Book
        // clicks behave consistently.
        async function loadBook(slug, reuseViewMode) {
            localState.bookSlug = slug;
            tab.state = { ...(tab.state || {}), bookSlug: slug };
            try {
                const b = await apiJSON("/console/api/books/" + encodeURIComponent(slug));
                localState.book = b;
            } catch (e) {
                localState.book = null;
                showWikiSplash();
                return;
            }
            await refreshPages();
            const restorePage = reuseViewMode &&
                localState.viewMode === "page-editor" &&
                localState.pageSlug &&
                localState.pages.find((p) => p.slug === localState.pageSlug);
            if (restorePage) {
                await loadPage(localState.pageSlug);
            } else {
                localState.pageSlug = null;
                localState.page = null;
                tab.state = { ...(tab.state || {}), pageSlug: null, pageId: null };
                showBookSplash();
            }
            newPageBtn.disabled = false;
            // Tab title: page name when a page is open, otherwise the
            // book name (or "Wiki" while still resolving). loadPage()
            // already stamped the page title in the restorePage branch
            // above; only fall back to the book name when no page is
            // active.
            if (!restorePage && window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                window.FamiliarWorkspace.updateTabTitle(tab.id, (localState.book && localState.book.name) || "Wiki");
            }
        }

        async function refreshPages() {
            try {
                const resp = await apiJSON("/console/api/books/" + encodeURIComponent(localState.bookSlug) + "/pages");
                localState.pages = (resp && resp.items) || [];
            } catch (e) {
                localState.pages = [];
                console.warn("wiki: list pages failed", e);
            }
            renderPageList();
        }

        function renderPageList() {
            tree.innerHTML = "";
            if (localState.pages.length === 0) {
                const stub = document.createElement("div");
                stub.className = "notes-tree-empty";
                stub.textContent = "No pages yet — click + to start.";
                tree.appendChild(stub);
                return;
            }
            for (const p of localState.pages) {
                const row = document.createElement("button");
                row.type = "button";
                row.className = "notes-row";
                if (p.slug === localState.pageSlug) row.classList.add("is-active");
                const t = document.createElement("div");
                t.className = "notes-row-title";
                t.textContent = p.title || "Untitled";
                const m = document.createElement("div");
                m.className = "notes-row-meta";
                m.textContent = relTime(p.updated_at);
                row.append(t, m);
                row.addEventListener("click", () => {
                    const compositeId = localState.bookSlug + "/" + p.slug;
                    const ws = window.FamiliarWorkspace;
                    if (ws && ws.focusExistingDoc("wiki", compositeId)) return;
                    // Sidebar jump — drop any wikilink trail.
                    localState.history.length = 0;
                    loadPage(p.slug);
                });
                tree.appendChild(row);
            }
        }

        async function loadPage(pageSlug) {
            // Detect a RELOAD of the already-open page with unsaved
            // edits (tab re-activation calls loadPage on the same slug).
            // Without this, the setMarkdown below silently clobbers a
            // dirty editor — especially after a failed save, where
            // there's no pending timer to flush.
            const reloadingOpenDirty =
                localState.page && localState.page.slug === pageSlug && isDirty();

            // Flush any pending save before switching.
            if (localState.saveTimer) {
                clearTimeout(localState.saveTimer);
                localState.saveTimer = null;
                await flushSave(true);
            } else if (reloadingOpenDirty) {
                await flushSave(true); // best-effort; keeps a failed edit alive
            }
            localState.pageSlug = pageSlug;
            // Reflect the (possibly just-mutated) history in the
            // chrome. Callers manage the stack themselves —
            // wikiNavigate pushes before calling, the back button
            // pops before calling, and direct jumps clear it.
            updateBackBtn();
            try {
                const p = await apiJSON(
                    "/console/api/books/" + encodeURIComponent(localState.bookSlug) +
                    "/pages/" + encodeURIComponent(pageSlug));
                localState.page = p;
                // If the flush above failed (409 / network), the editor
                // is still dirty on the same page: keep the local edits
                // and let the banner/poll reconcile, rather than
                // overwriting with server content and reseeding.
                const keepLocal = reloadingOpenDirty && isDirty();
                if (!keepLocal) {
                    seedSyncBase(p);
                    clearBanner();
                }
                tab.state = {
                    ...(tab.state || {}),
                    bookSlug: localState.bookSlug,
                    pageSlug: pageSlug,
                    pageId: p.id,
                };
                showEditor();
                initEditor();
                if (!keepLocal) {
                    titleInput.value = p.title || "";
                    if (tuiEditor) {
                        localState.suppressSave = true;
                        tuiEditor.setMarkdown(p.content || "");
                        // Normalize the content baseline to Toast UI's
                        // round-tripped form so its reformatting doesn't
                        // read as a phantom edit (which would false-409
                        // the next remote change into a conflict).
                        localState.baseContent = tuiEditor.getMarkdown();
                        localState.suppressSave = false;
                    }
                }
                updateStatusBar(keepLocal ? currentContent() : (p.content || ""));
                renderPageList();
                updateShareIndicator();
                // Page-links footer (outbound + backlinks) disabled
                // until graph-based UI is designed.
                // renderPageLinks(p.id);
                if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                    window.FamiliarWorkspace.updateTabTitle(tab.id, p.title || "Untitled");
                }
                savedDot.textContent = "";
                startPolling();
                registerUnloadFlush();
            } catch (e) {
                console.warn("wiki: load page failed", e);
                showBookSplash();
            }
        }

        // renderPageLinks fetches outbound + inbound for the page
        // and paints the footer below the editor. Hides the host
        // when both lists are empty so a fresh page doesn't show
        // an empty pane.
        async function renderPageLinks(pageID) {
            linksHost.innerHTML = "";
            linksHost.hidden = true;
            if (!pageID || !localState.bookSlug) return;
            const base = "/console/api/books/" + encodeURIComponent(localState.bookSlug) +
                "/page-by-id/" + encodeURIComponent(pageID);
            let outbound = [], inbound = [];
            try {
                const [linksResp, backResp] = await Promise.all([
                    apiJSON(base + "/links"),
                    apiJSON(base + "/backlinks"),
                ]);
                outbound = (linksResp && linksResp.items) || [];
                inbound = (backResp && backResp.items) || [];
            } catch (e) {
                // Endpoint may 404 on a page that just got
                // recreated by an external write; render empty
                // and try again on the next load.
                console.warn("wiki: renderPageLinks failed", e);
                return;
            }
            if (outbound.length === 0 && inbound.length === 0) return;
            if (outbound.length > 0) {
                linksHost.appendChild(buildLinksSection(
                    "Links from this page", outbound, makeOutboundClick));
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
            // Outbound rows have target_*; inbound have source_*.
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
            // Show book name on cross-book links so the source is
            // clear at a glance.
            if (isOutbound && it.target_book_slug && it.target_book_slug !== localState.bookSlug) {
                const book = document.createElement("span");
                book.className = "notes-page-link-book";
                book.textContent = it.target_book_slug;
                row.appendChild(book);
            } else if (!isOutbound && it.source_book_slug && it.source_book_slug !== localState.bookSlug) {
                const book = document.createElement("span");
                book.className = "notes-page-link-book";
                book.textContent = it.source_book_slug;
                row.appendChild(book);
            }
            if (!isBroken) {
                row.addEventListener("click", makeClick(it));
            }
            return row;
        }

        // makeOutboundClick / makeInboundClick close over `it` and
        // return the click handler. Same-book targets go through
        // loadPage; cross-book targets need a focusBook → loadPage
        // sequence.
        function makeOutboundClick(it) {
            return () => {
                const targetBook = it.target_book_slug || localState.bookSlug;
                if (targetBook === localState.bookSlug) {
                    loadPage(it.target_page_slug);
                } else {
                    focusBook(targetBook).then(() => loadPage(it.target_page_slug));
                }
            };
        }
        function makeInboundClick(it) {
            return () => {
                if (it.source_book_slug === localState.bookSlug) {
                    loadPage(it.source_page_slug);
                } else {
                    focusBook(it.source_book_slug).then(() => loadPage(it.source_page_slug));
                }
            };
        }

        // ── View-mode dispatch ────────────────────────────────
        // applyViewMode is the single entry-point that flips the
        // shell between page-editor and the two splash variants.
        // It owns showing/hiding the editor + header, painting the
        // splash container, and toggling the .is-splash class on
        // the root that hides the left rail in splash mode.

        function applyViewMode() {
            const mode = localState.viewMode;
            // Live-research read-only takes over the whole right rail:
            // no editor, no header, no splash — just the evidence view.
            if (mode === "research") {
                root.classList.add("is-splash"); // hides the left rail
                empty.hidden = true;
                editor.style.display = "none";
                header.style.display = "none";
                researchHost.hidden = false;
                return;
            }
            researchHost.hidden = true;
            if (mode === "page-editor") {
                root.classList.remove("is-splash");
                empty.hidden = true;
                editor.style.display = "";
                header.style.display = "";
                return;
            }
            // Splash modes — left rail hidden, right pane full-
            // width carrying the navigation tile + list.
            root.classList.add("is-splash");
            empty.hidden = false;
            editor.style.display = "none";
            header.style.display = "none";
            if (mode === "book-splash") {
                renderBookSplash();
            } else {
                renderWikiSplash();
            }
        }

        function bookGlyphSVG() {
            return (
                '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" width="32" height="32">' +
                    '<rect x="4" y="6" width="4" height="14" rx="1"/><path d="M4 9.5 H8"/>' +
                    '<rect x="9" y="4.5" width="4" height="15.5" rx="1"/><path d="M9 8 H13"/>' +
                    '<path d="M15.2 6.3 l3.6 -1 a1 1 0 0 1 1.25 0.7 l3.3 12.0 a1 1 0 0 1 -0.7 1.25 l-3.6 1 a1 1 0 0 1 -1.25 -0.7 l-3.3 -12.0 a1 1 0 0 1 0.7 -1.25 Z"/>' +
                    '<path d="M16.6 9.7 L20.2 8.7"/>' +
                '</svg>'
            );
        }
        function pageGlyphSVG() {
            return (
                '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" width="32" height="32">' +
                    '<path d="M6 3 h8 l5 5 v12 a1.5 1.5 0 0 1 -1.5 1.5 H6 a1.5 1.5 0 0 1 -1.5 -1.5 V4.5 A1.5 1.5 0 0 1 6 3 Z"/>' +
                    '<path d="M14 3 v3.5 A1.5 1.5 0 0 0 15.5 8 H19"/>' +
                '</svg>'
            );
        }
        function escapeHTML(s) {
            return String(s == null ? "" : s)
                .replace(/&/g, "&amp;").replace(/</g, "&lt;")
                .replace(/>/g, "&gt;").replace(/"/g, "&quot;");
        }

        // ── Wiki splash (no book selected) ───────────────────
        function renderWikiSplash() {
            empty.innerHTML = "";

            const head = document.createElement("div");
            head.className = "wiki-splash-head";
            head.innerHTML =
                '<h1 class="wiki-splash-title">Your books</h1>';
            empty.appendChild(head);

            const grid = document.createElement("div");
            grid.className = "wiki-splash-grid";

            // Left column — compact "+ New book" button with the
            // user's pinned wiki pages below it (splash rework
            // 2026-06-12).
            const tileCol = document.createElement("div");
            tileCol.className = "wiki-splash-tile-col";
            const tile = document.createElement("button");
            tile.type = "button";
            tile.className = "wiki-empty-tile is-slate is-compact";
            tile.innerHTML =
                '<div class="wiki-empty-tile-glyph">' + bookGlyphSVG() + '</div>' +
                '<div class="wiki-empty-tile-title">New book</div>';
            tile.addEventListener("click", createBook);
            const pinLabel = document.createElement("div");
            pinLabel.className = "wiki-splash-list-label";
            pinLabel.textContent = "Pinned";
            const pinList = document.createElement("div");
            pinList.className = "wiki-splash-list";
            pinList.innerHTML = '<div class="wiki-splash-empty">Loading…</div>';
            tileCol.append(tile, pinLabel, pinList);
            grid.appendChild(tileCol);

            apiJSON("/console/api/home/pins").then((resp) => {
                const items = ((resp && resp.items) || []).filter((it) => it.kind === "wiki");
                pinList.innerHTML = "";
                if (items.length === 0) {
                    pinList.innerHTML = '<div class="wiki-splash-empty">Nothing pinned — pin a page from its book.</div>';
                    return;
                }
                items.forEach((it) => {
                    const row = document.createElement("button");
                    row.type = "button";
                    row.className = "wiki-splash-row";
                    row.innerHTML =
                        '<div class="wiki-splash-row-body">' +
                            '<div class="wiki-splash-row-title">' + escapeHTML(it.title || "Untitled") + '</div>' +
                            (it.book_name ? '<div class="wiki-splash-row-desc">' + escapeHTML(it.book_name) + '</div>' : '') +
                        '</div>';
                    row.style.gridTemplateColumns = "1fr";
                    row.addEventListener("click", async () => {
                        await focusBook(it.book_slug);
                        await loadPageById(it.id);
                    });
                    pinList.appendChild(row);
                });
            }).catch(() => {
                pinList.innerHTML = '<div class="wiki-splash-empty">Couldn’t load pins.</div>';
            });

            // Right column — list of books, one row per book,
            // most-recently-updated first.
            const listCol = document.createElement("div");
            listCol.className = "wiki-splash-list-col";
            const listLabel = document.createElement("div");
            listLabel.className = "wiki-splash-list-label";
            listLabel.textContent = "Books";
            listCol.appendChild(listLabel);

            const list = document.createElement("div");
            list.className = "wiki-splash-list is-dense";
            if (localState.books.length === 0) {
                list.innerHTML = '<div class="wiki-splash-empty">No books yet — create one to start.</div>';
            } else {
                localState.books.forEach((b) => {
                    const row = document.createElement("button");
                    row.type = "button";
                    row.className = "wiki-splash-row";
                    row.innerHTML =
                        '<div class="wiki-splash-row-glyph">' +
                            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round" width="18" height="18">' +
                                '<rect x="4" y="6" width="4" height="14" rx="1"/><path d="M4 9.5 H8"/>' +
                                '<rect x="9" y="4.5" width="4" height="15.5" rx="1"/><path d="M9 8 H13"/>' +
                                '<path d="M15.2 6.3 l3.6 -1 a1 1 0 0 1 1.25 0.7 l3.3 12.0 a1 1 0 0 1 -0.7 1.25 l-3.6 1 a1 1 0 0 1 -1.25 -0.7 l-3.3 -12.0 a1 1 0 0 1 0.7 -1.25 Z"/>' +
                                '<path d="M16.6 9.7 L20.2 8.7"/>' +
                            '</svg>' +
                        '</div>' +
                        '<div class="wiki-splash-row-body">' +
                            '<div class="wiki-splash-row-title">' + escapeHTML(b.name || b.slug) + '</div>' +
                            (b.description
                                ? '<div class="wiki-splash-row-desc">' + escapeHTML(b.description) + '</div>'
                                : '') +
                        '</div>' +
                        '<div class="wiki-splash-row-meta">' + escapeHTML(relTime(b.updated_at)) + '</div>';
                    row.addEventListener("click", () => {
                        focusBook(b.slug);
                    });
                    list.appendChild(row);
                });
            }
            listCol.appendChild(list);
            grid.appendChild(listCol);

            empty.appendChild(grid);
        }

        // ── Book splash (book selected, no page) ─────────────
        function renderBookSplash() {
            empty.innerHTML = "";

            const head = document.createElement("div");
            head.className = "wiki-splash-head";
            const bookName = (localState.book && localState.book.name) || localState.bookSlug || "Book";
            const desc = (localState.book && localState.book.description) || "";

            // Title row — title on the left, overflow ⋯ on the
            // right. The menu is owner-gated and only mounted when
            // there's at least one item to show; readers + writers
            // don't see the trigger at all.
            const titleRow = document.createElement("div");
            titleRow.className = "wiki-splash-title-row";
            const titleEl = document.createElement("h1");
            titleEl.className = "wiki-splash-title";
            titleEl.textContent = bookName;
            titleRow.appendChild(titleEl);

            if (currentBookRole() === "owner") {
                const overflow = document.createElement("div");
                overflow.className = "notes-overflow wiki-splash-overflow";
                const overflowBtn = document.createElement("button");
                overflowBtn.type = "button";
                overflowBtn.className = "notes-overflow-btn";
                overflowBtn.textContent = "⋯";
                overflowBtn.title = "More actions";
                overflowBtn.addEventListener("click", (e) => {
                    e.stopPropagation();
                    overflow.classList.toggle("is-open");
                });
                const menu = document.createElement("div");
                menu.className = "notes-overflow-menu";
                const manageItem = document.createElement("button");
                manageItem.type = "button";
                manageItem.className = "notes-overflow-item";
                manageItem.textContent = "Manage users";
                manageItem.addEventListener("click", (e) => {
                    e.stopPropagation();
                    overflow.classList.remove("is-open");
                    const name = (localState.book && localState.book.name) || localState.bookSlug;
                    openMembersModal(localState.bookSlug, name);
                });
                menu.appendChild(manageItem);
                // Archive lives here — on the book splash — not in a
                // page's overflow, since it's a book-level action.
                const archiveItem = document.createElement("button");
                archiveItem.type = "button";
                archiveItem.className = "notes-overflow-item danger";
                archiveItem.textContent = "Archive book";
                archiveItem.addEventListener("click", (e) => {
                    e.stopPropagation();
                    overflow.classList.remove("is-open");
                    archiveBook();
                });
                menu.appendChild(archiveItem);
                overflow.append(overflowBtn, menu);
                // Click-outside closes — same pattern as the page-
                // editor overflow menu.
                document.addEventListener("click", () => overflow.classList.remove("is-open"));
                titleRow.appendChild(overflow);
            }

            head.appendChild(titleRow);

            if (desc) {
                const sub = document.createElement("div");
                sub.className = "wiki-splash-subtitle";
                sub.textContent = desc;
                head.appendChild(sub);
            }

            empty.appendChild(head);

            const grid = document.createElement("div");
            grid.className = "wiki-splash-grid";

            // Left column — compact "+ New page" button with this
            // book's pinned pages below it (pages carry per-caller
            // `pinned` from the list endpoint; no extra fetch).
            const tileCol = document.createElement("div");
            tileCol.className = "wiki-splash-tile-col";
            const tile = document.createElement("button");
            tile.type = "button";
            tile.className = "wiki-empty-tile is-slate is-compact";
            tile.innerHTML =
                '<div class="wiki-empty-tile-glyph">' + pageGlyphSVG() + '</div>' +
                '<div class="wiki-empty-tile-title">New page</div>';
            tile.addEventListener("click", createPage);
            tileCol.appendChild(tile);

            const pinned = localState.pages.filter((p) => p.pinned);
            const pinLabel = document.createElement("div");
            pinLabel.className = "wiki-splash-list-label";
            pinLabel.textContent = "Pinned";
            const pinList = document.createElement("div");
            pinList.className = "wiki-splash-list";
            if (pinned.length === 0) {
                pinList.innerHTML = '<div class="wiki-splash-empty">Nothing pinned in this book.</div>';
            } else {
                pinned.forEach((p) => {
                    const row = document.createElement("button");
                    row.type = "button";
                    row.className = "wiki-splash-row";
                    row.style.gridTemplateColumns = "1fr";
                    row.innerHTML =
                        '<div class="wiki-splash-row-body">' +
                            '<div class="wiki-splash-row-title">' + escapeHTML(p.title || "Untitled") + '</div>' +
                        '</div>';
                    row.addEventListener("click", () => {
                        const compositeId = localState.bookSlug + "/" + p.slug;
                        const ws = window.FamiliarWorkspace;
                        if (ws && ws.focusExistingDoc("wiki", compositeId)) return;
                        localState.history.length = 0;
                        loadPage(p.slug);
                    });
                    pinList.appendChild(row);
                });
            }
            tileCol.append(pinLabel, pinList);
            grid.appendChild(tileCol);

            const listCol = document.createElement("div");
            listCol.className = "wiki-splash-list-col";
            const listLabel = document.createElement("div");
            listLabel.className = "wiki-splash-list-label";
            listLabel.textContent = "Pages";
            listCol.appendChild(listLabel);

            const list = document.createElement("div");
            list.className = "wiki-splash-list is-dense";
            if (localState.pages.length === 0) {
                list.innerHTML = '<div class="wiki-splash-empty">No pages yet — create one to start.</div>';
            } else {
                localState.pages.forEach((p) => {
                    const row = document.createElement("button");
                    row.type = "button";
                    row.className = "wiki-splash-row";
                    const snippet = (p.snippet || "").slice(0, 100);
                    row.innerHTML =
                        '<div class="wiki-splash-row-glyph">' +
                            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round" width="18" height="18">' +
                                '<path d="M6 3 h8 l5 5 v12 a1.5 1.5 0 0 1 -1.5 1.5 H6 a1.5 1.5 0 0 1 -1.5 -1.5 V4.5 A1.5 1.5 0 0 1 6 3 Z"/>' +
                                '<path d="M14 3 v3.5 A1.5 1.5 0 0 0 15.5 8 H19"/>' +
                            '</svg>' +
                        '</div>' +
                        '<div class="wiki-splash-row-body">' +
                            '<div class="wiki-splash-row-title">' + escapeHTML(p.title || "Untitled") + '</div>' +
                            (snippet
                                ? '<div class="wiki-splash-row-desc">' + escapeHTML(snippet) + '</div>'
                                : '') +
                        '</div>' +
                        '<div class="wiki-splash-row-meta">' + escapeHTML(relTime(p.updated_at)) + '</div>';
                    row.addEventListener("click", () => {
                    const compositeId = localState.bookSlug + "/" + p.slug;
                    const ws = window.FamiliarWorkspace;
                    if (ws && ws.focusExistingDoc("wiki", compositeId)) return;
                    localState.history.length = 0;
                    loadPage(p.slug);
                });
                    list.appendChild(row);
                });
            }
            listCol.appendChild(list);
            grid.appendChild(listCol);

            empty.appendChild(grid);
        }

        function showEditor() {
            localState.viewMode = "page-editor";
            applyViewMode();
        }
        function showWikiSplash() {
            localState.viewMode = "wiki-splash";
            applyViewMode();
        }
        function showBookSplash() {
            localState.viewMode = "book-splash";
            applyViewMode();
        }

        // Reset to wiki splash — clears book + page state and the
        // workspace tab title. Used by the sidebar Wiki click and
        // by archive-book when the last book is gone.
        async function resetToWikiSplash() {
            // Flush any pending save before navigating away.
            if (localState.saveTimer) {
                clearTimeout(localState.saveTimer);
                localState.saveTimer = null;
                await flushSave(true);
            }
            localState.bookSlug = null;
            localState.pageSlug = null;
            localState.book = null;
            localState.page = null;
            localState.pages = [];
            tab.state = { ...(tab.state || {}), bookSlug: null, pageSlug: null, pageId: null };
            if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                window.FamiliarWorkspace.updateTabTitle(tab.id, "Wiki");
            }
            await refreshBooks();
            showWikiSplash();
        }

        // ── Save flow ─────────────────────────────────────────

        function scheduleSave() {
            if (!localState.page) return;
            savedDot.textContent = "Saving…";
            if (localState.saveTimer) clearTimeout(localState.saveTimer);
            localState.saveTimer = setTimeout(() => {
                localState.saveTimer = null;
                flushSave(false);
            }, 500);
        }

        async function flushSave(immediate, keepalive) {
            if (!localState.page || localState.saving) return;
            localState.saving = true;
            const patch = {
                title: titleInput.value || "Untitled",
                content: tuiEditor ? tuiEditor.getMarkdown() : "",
            };
            const url = "/console/api/books/" + encodeURIComponent(localState.bookSlug) +
                "/pages/" + encodeURIComponent(localState.page.slug);
            const headers = { "Content-Type": "application/json" };
            // CHAT-REARCH: send the updated_at we last observed as
            // If-Match. The server (PATCH /pages/{slug}) rejects with
            // 409 + {current: <page>} when the row has moved since,
            // which means another writer landed an edit between our
            // last sync and this save. We never want last-write-wins
            // for wiki content — every conflict surfaces.
            if (localState.baseUpdatedAt) {
                headers["If-Match"] = localState.baseUpdatedAt;
            }
            try {
                const resp = await fetch(url, {
                    method: "PATCH",
                    credentials: "include",
                    headers: headers,
                    body: JSON.stringify(patch),
                    // On unload the browser kills in-flight fetches;
                    // keepalive asks it to complete the save anyway.
                    keepalive: !!keepalive,
                });
                const text = await resp.text();
                let body = null;
                try { body = text ? JSON.parse(text) : null; } catch (_) { /* ignore */ }
                if (resp.status === 409) {
                    handleStaleConflict(body && body.current);
                    return;
                }
                if (!resp.ok) {
                    throw new Error((body && body.error) || ("HTTP " + resp.status));
                }
                const p = body;
                // PATCH responses don't join share state (only GET
                // does) — carry it forward for the render trigger.
                if (!p.share && localState.page && localState.page.share) p.share = localState.page.share;
                localState.page = p;
                seedSyncBase(p);
                clearBanner();
                updateShareIndicator();
                // P2-3: the server auto-merged this save against a
                // concurrent writer's disjoint edit — the response body IS
                // the merged document, and seedSyncBase already adopted it
                // as our baseline. Reflect it in the editor so the user
                // sees the combined result, but ONLY if they haven't typed
                // further since we sent the save (otherwise we'd clobber
                // in-flight keystrokes; their newer edit merges again on
                // the next flush).
                if (p.merged && tuiEditor && currentContent() === patch.content) {
                    localState.suppressSave = true;
                    tuiEditor.setMarkdown(p.content || "");
                    // Normalize the baseline to the editor's round-tripped
                    // form so the next no-keystroke diff isn't dirty.
                    localState.baseContent = tuiEditor.getMarkdown();
                    localState.suppressSave = false;
                }
                // Publicly shared pages keep their diagram PNGs in
                // step with the content (the share page is script-
                // free; diagrams ship as pre-rendered bitmaps).
                if (p.share && window.familiarMermaid) {
                    window.familiarMermaid.syncShareRenders(
                        { bookSlug: localState.bookSlug, pageId: p.id },
                        patch.content || "");
                }
                // Slug may have changed if the rename branch ran.
                localState.pageSlug = p.slug;
                tab.state = { ...(tab.state || {}), pageSlug: p.slug };
                // A merged save gets a distinct, longer-lived notice so
                // the user notices their view just absorbed someone else's
                // change; an ordinary save shows the transient "Saved".
                const savedMsg = p.merged
                    ? ("Merged — " + (p.updated_by || "another editor"))
                    : "Saved";
                savedDot.textContent = savedMsg;
                // Update the row in the list without re-fetching the
                // whole index.
                const idx = localState.pages.findIndex((x) => x.id === p.id);
                if (idx >= 0) {
                    localState.pages[idx] = {
                        id: p.id, slug: p.slug, title: p.title,
                        snippet: localState.pages[idx].snippet,
                        updated_at: p.updated_at, updated_by: p.updated_by,
                    };
                    renderPageList();
                }
                setTimeout(() => {
                    if (savedDot.textContent === savedMsg) savedDot.textContent = "";
                }, p.merged ? 5000 : 1500);
                if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                    window.FamiliarWorkspace.updateTabTitle(tab.id, p.title || "Untitled");
                }
            } catch (e) {
                savedDot.textContent = "Save failed";
                console.warn("wiki: save failed", e);
            } finally {
                localState.saving = false;
            }
        }

        // seedSyncBase records the server's authoritative state as
        // of a successful load or save. Subsequent dirty checks
        // compare against these baselines; isDirty() returns true
        // iff the editor diverges.
        function seedSyncBase(p) {
            if (!p) return;
            localState.baseUpdatedAt = p.updated_at || null;
            localState.baseTitle = p.title || "";
            localState.baseContent = p.content || "";
        }

        function currentTitle() { return titleInput.value || ""; }
        function currentContent() { return tuiEditor ? tuiEditor.getMarkdown() : ""; }

        function isDirty() {
            return currentTitle() !== localState.baseTitle ||
                   currentContent() !== localState.baseContent;
        }

        // ── Background sync ────────────────────────────────────
        //
        // Two cadences. Every open page polls slowly (slowPollMs =
        // 60s) so a long-idle tab eventually picks up other writers'
        // changes. The foreground tab additionally polls fast
        // (fastPollMs = 5s) so the user looking at the page sees
        // updates close to real time. Both call the same poll()
        // function; cadence is the only difference.
        //
        // On each poll:
        //   - dirty editor + remote moved → drop a banner; user
        //     picks "Keep mine" or "Use theirs". No silent overwrite.
        //   - clean editor + remote moved → swap content silently
        //     (toast on the foreground tab so the user sees the
        //     swap happened). Editor is locked under suppressSave
        //     during the swap so the autosave timer doesn't fire.

        const slowPollMs = 60 * 1000;
        const fastPollMs = 5 * 1000;

        function startPolling() {
            stopPolling();
            localState.pollTimer = setInterval(poll, slowPollMs);
            if (localState.isForeground) {
                localState.fastPollTimer = setInterval(poll, fastPollMs);
            }
        }
        function stopPolling() {
            if (localState.pollTimer) { clearInterval(localState.pollTimer); localState.pollTimer = null; }
            if (localState.fastPollTimer) { clearInterval(localState.fastPollTimer); localState.fastPollTimer = null; }
        }

        // registerUnloadFlush attempts one last save when the page/tab
        // is torn down mid-edit — closing the browser within the 500ms
        // autosave debounce (or after a failed save) used to silently
        // drop the edit, since teardown only stops pollers. pagehide
        // covers bfcache/mobile where beforeunload doesn't fire. keepalive
        // asks the browser to let the request outlive the page. Idempotent
        // (registered once per shell); unregisterUnloadFlush removes it on
        // tab close so a closed shell can't flush stale content.
        function registerUnloadFlush() {
            if (localState.unloadHandler) return;
            localState.unloadHandler = () => {
                if (!isDirty()) return;
                if (localState.saveTimer) { clearTimeout(localState.saveTimer); localState.saveTimer = null; }
                flushSave(true, /*keepalive=*/true);
            };
            window.addEventListener("beforeunload", localState.unloadHandler);
            window.addEventListener("pagehide", localState.unloadHandler);
        }
        function unregisterUnloadFlush() {
            if (!localState.unloadHandler) return;
            window.removeEventListener("beforeunload", localState.unloadHandler);
            window.removeEventListener("pagehide", localState.unloadHandler);
            localState.unloadHandler = null;
        }
        // setForeground is invoked by the surface plumbing when this
        // tab becomes (or stops being) the foreground tab in its
        // panel. Toggling foreground also triggers an immediate
        // poll so a freshly-focused tab catches up without waiting
        // for the next slow tick.
        function setForeground(active) {
            if (localState.isForeground === !!active) return;
            localState.isForeground = !!active;
            if (localState.page) {
                startPolling();
                if (active) poll();
            }
        }

        async function poll() {
            if (!localState.page || !localState.bookSlug) return;
            if (localState.saving) return; // a save is in flight; let it land first
            // Snapshot the baseline before the GET so we can detect a
            // save landing while the request is in flight.
            const baseAtRequest = localState.baseUpdatedAt;
            try {
                const fresh = await apiJSON(
                    "/console/api/books/" + encodeURIComponent(localState.bookSlug) +
                    "/pages/" + encodeURIComponent(localState.page.slug));
                if (!fresh || !fresh.updated_at) return;
                // Race guard: if a save started or the baseline moved
                // while this GET was in flight, this response may
                // predate the save — applying it would REVERT the
                // just-saved content (and re-arm a stale base -> 409).
                if (localState.saving || localState.baseUpdatedAt !== baseAtRequest) return;
                if (fresh.updated_at === localState.baseUpdatedAt) return; // no change
                // Never move backwards: a stale/reordered response
                // whose updated_at is older than our base isn't a
                // remote edit. RFC3339 UTC strings sort chronologically.
                if (fresh.updated_at < localState.baseUpdatedAt) return;
                if (isDirty()) {
                    showStaleBanner(fresh);
                } else {
                    swapInRemote(fresh, /*silent=*/!localState.isForeground);
                }
            } catch (e) {
                // Polling is best-effort; a transient network blip
                // shouldn't surface as an error to the user.
                console.debug("wiki: poll failed", e);
            }
        }

        // swapInRemote replaces the editor + title with the
        // server's version. silent=true skips the toast (used by
        // background tabs so a multi-tab user isn't drowned in
        // notifications); the foreground tab gets a short
        // "Synced" toast so the swap is visible.
        function swapInRemote(fresh, silent) {
            localState.page = fresh;
            seedSyncBase(fresh);
            // Slug may have changed if the remote writer renamed.
            localState.pageSlug = fresh.slug;
            tab.state = { ...(tab.state || {}), pageSlug: fresh.slug };
            titleInput.value = fresh.title || "";
            if (tuiEditor) {
                localState.suppressSave = true;
                tuiEditor.setMarkdown(fresh.content || "");
                // Normalize the baseline to the editor's round-tripped
                // form (see loadPage) so a subsequent no-keystroke diff
                // doesn't read as dirty.
                localState.baseContent = tuiEditor.getMarkdown();
                localState.suppressSave = false;
            }
            if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                window.FamiliarWorkspace.updateTabTitle(tab.id, fresh.title || "Untitled");
            }
            updateStatusBar(fresh.content || "");
            // Refresh the row in the index too — its updated_at /
            // title may have moved.
            const idx = localState.pages.findIndex((x) => x.id === fresh.id);
            if (idx >= 0) {
                localState.pages[idx] = {
                    id: fresh.id, slug: fresh.slug, title: fresh.title,
                    snippet: localState.pages[idx].snippet,
                    updated_at: fresh.updated_at, updated_by: fresh.updated_by,
                };
                renderPageList();
            }
            // Surface the sync inline next to the title (where
            // "Saved" appears) instead of a floating toast. Less
            // intrusive and matches the editor's existing chrome.
            // Foreground tabs only — silent=true means a background
            // tab swapped in a remote edit and we don't want to
            // distract the user typing in another panel.
            if (!silent && fresh.updated_by) {
                savedDot.textContent = "Synced — " + fresh.updated_by;
                setTimeout(() => {
                    if (savedDot.textContent.indexOf("Synced") === 0) savedDot.textContent = "";
                }, 4000);
            }
        }

        // showStaleBanner is the dirty-editor conflict UI. Sits
        // above the editor surface, offers a deterministic choice:
        // discard local + reload, or overwrite remote with local.
        // Mid-conflict the editor keeps the user's local content
        // so nothing is lost until they pick.
        function showStaleBanner(fresh) {
            clearBanner();
            const banner = document.createElement("div");
            banner.className = "wiki-sync-banner";
            const msg = document.createElement("div");
            msg.className = "wiki-sync-banner-msg";
            msg.textContent = "This page was updated by " +
                (fresh.updated_by || "someone") +
                " while you were editing.";
            const actions = document.createElement("div");
            actions.className = "wiki-sync-banner-actions";

            const useTheirs = document.createElement("button");
            useTheirs.type = "button";
            useTheirs.className = "wiki-sync-banner-btn";
            useTheirs.textContent = "Use theirs (discard mine)";
            useTheirs.addEventListener("click", () => {
                swapInRemote(fresh, /*silent=*/true);
                clearBanner();
            });

            const keepMine = document.createElement("button");
            keepMine.type = "button";
            keepMine.className = "wiki-sync-banner-btn wiki-sync-banner-btn-primary";
            keepMine.textContent = "Keep mine (overwrite)";
            keepMine.addEventListener("click", () => {
                // Adopt the new updated_at as our base so the next
                // save's If-Match precondition matches. The user's
                // current editor content stays put, and flushSave
                // will write it on top of the remote version.
                localState.baseUpdatedAt = fresh.updated_at;
                clearBanner();
                scheduleSave();
            });

            actions.append(useTheirs, keepMine);
            banner.append(msg, actions);
            bannerHost.appendChild(banner);
            localState.banner = banner;
        }

        // handleStaleConflict is the save-time variant. Server told
        // us 409 with the current remote in the body — surface the
        // same banner the poller uses.
        function handleStaleConflict(current) {
            savedDot.textContent = "Conflict";
            if (current) {
                showStaleBanner(current);
            } else {
                // Server didn't include the current page (storage
                // race). Force a poll which will fetch + branch.
                poll();
            }
        }

        function clearBanner() {
            if (localState.banner) {
                localState.banner.remove();
                localState.banner = null;
            }
            bannerHost.innerHTML = "";
        }

        // ── New / delete / archive ────────────────────────────

        async function createBook() {
            const name = prompt("Book name:");
            if (!name || !name.trim()) return;
            try {
                const b = await apiJSON("/console/api/books", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ name: name.trim() }),
                });
                localState.books.unshift({
                    id: b.id, slug: b.slug, name: b.name,
                    description: b.description || "", role: "owner",
                    updated_at: b.updated_at,
                });
                renderBookSelect();
                bookSelect.value = b.slug;
                await loadBook(b.slug);
            } catch (e) {
                notifyErr("Couldn't create book: " + (e.message || e));
            }
        }

        async function createPage() {
            if (!localState.bookSlug) return;
            try {
                const p = await apiJSON(
                    "/console/api/books/" + encodeURIComponent(localState.bookSlug) + "/pages",
                    { method: "POST", headers: { "Content-Type": "application/json" },
                      body: JSON.stringify({ title: "Untitled", content: "" }) });
                localState.pages.unshift({
                    id: p.id, slug: p.slug, title: p.title,
                    snippet: "", updated_at: p.updated_at, updated_by: p.updated_by,
                });
                localState.history.length = 0;
                await loadPage(p.slug);
                // The sidebar rail caches each book's pages in
                // sidebarWikiPagesCache; notesChanged is what clears
                // that cache + re-renders. Without this the new page
                // doesn't appear in the rail until a manual re-expand
                // (notes.js fires the same event on mutation).
                window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
            } catch (e) {
                notifyErr("Couldn't create page: " + (e.message || e));
            }
        }

        async function deletePage() {
            if (!localState.page) return;
            if (!confirm('Delete "' + (localState.page.title || "Untitled") + '"?')) return;
            try {
                await apiJSON(
                    "/console/api/books/" + encodeURIComponent(localState.bookSlug) +
                    "/pages/" + encodeURIComponent(localState.page.slug),
                    { method: "DELETE" });
                const idx = localState.pages.findIndex((x) => x.id === localState.page.id);
                if (idx >= 0) localState.pages.splice(idx, 1);
                localState.page = null;
                localState.pageSlug = null;
                tab.state = { ...(tab.state || {}), pageSlug: null, pageId: null };
                renderPageList();
                showBookSplash();
                // Drop the deleted row from the sidebar rail (clears
                // the per-book wiki page cache + re-renders).
                window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
            } catch (e) {
                notifyErr("Couldn't delete: " + (e.message || e));
            }
        }

        async function archiveBook() {
            if (!localState.book) return;
            if (!confirm('Archive "' + (localState.book.name || "this book") + '"? It will hide from your list (recoverable).')) return;
            try {
                await apiJSON(
                    "/console/api/books/" + encodeURIComponent(localState.bookSlug),
                    { method: "PATCH", headers: { "Content-Type": "application/json" },
                      body: JSON.stringify({ archive: true }) });
                // Drop from local list and reset to first available.
                localState.books = localState.books.filter((b) => b.slug !== localState.bookSlug);
                localState.bookSlug = null;
                localState.pageSlug = null;
                localState.page = null;
                renderBookSelect();
                if (localState.books.length > 0) {
                    bookSelect.value = localState.books[0].slug;
                    await loadBook(localState.books[0].slug);
                } else {
                    tree.innerHTML = "";
                    newPageBtn.disabled = true;
                    showWikiSplash();
                }
            } catch (e) {
                notifyErr("Couldn't archive: " + (e.message || e));
            }
        }

        // ── Wire interactions ─────────────────────────────────

        bookSelect.addEventListener("change", (ev) => {
            const slug = ev.target.value;
            if (slug && slug !== localState.bookSlug) {
                localState.pageSlug = null;
                localState.page = null;
                localState.history.length = 0;
                tab.state = { ...(tab.state || {}), bookSlug: slug, pageSlug: null, pageId: null };
                loadBook(slug);
            }
        });
        newBookBtn.addEventListener("click", createBook);
        newPageBtn.addEventListener("click", createPage);

        titleInput.addEventListener("input", () => {
            if (!localState.page) return;
            scheduleSave();
            // Live-update the workspace tab label as the user renames.
            if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                window.FamiliarWorkspace.updateTabTitle(tab.id, titleInput.value || "Untitled");
            }
        });

        // Cmd/Ctrl-S in the title input flushes immediately.
        titleInput.addEventListener("keydown", (e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === "s") {
                e.preventDefault();
                if (localState.saveTimer) {
                    clearTimeout(localState.saveTimer);
                    localState.saveTimer = null;
                }
                flushSave(true);
            }
        });

        // A rename only reaches the sidebar rail when the page's cached
        // row is refreshed. Capture the title on focus so blur can tell
        // a real rename from a focus-and-leave (and catch renames the
        // debounced autosave already persisted). The rail caches each
        // book's pages in sidebarWikiPagesCache, so the refresh must go
        // through notesChanged (which clears that cache), not a bare
        // sidebarRefresh. Flush first so the re-fetch sees the new title.
        let titleAtFocus = "";
        titleInput.addEventListener("focus", () => {
            titleAtFocus = titleInput.value || "";
        });
        titleInput.addEventListener("blur", async () => {
            if (!localState.page) return;
            if ((titleInput.value || "") === titleAtFocus) return;
            if (localState.saveTimer) {
                clearTimeout(localState.saveTimer);
                localState.saveTimer = null;
            }
            await flushSave(true);
            window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
        });

        // Initial paint reflects persisted state. refreshBooks
        // (called from init) will resolve which view-mode to land on.
        applyViewMode();

        // Public switch hook for the openDoc event below. Accepts
        // a book slug (focus the book + land on book-splash) or a
        // "bookSlug/pageSlug" pair (deep-link to a specific page).
        async function focusBook(bookSlug, pageSlug) {
            // If the book isn't in the local cache yet (just-created
            // by another shell), pull a fresh list before resolving.
            if (!localState.books.find((b) => b.slug === bookSlug)) {
                await refreshBooks();
            }
            if (!localState.books.find((b) => b.slug === bookSlug)) {
                showWikiSplash();
                return;
            }
            localState.bookSlug = bookSlug;
            localState.pageSlug = pageSlug || null;
            // viewMode reflects the deep-link intent — page-editor
            // when a pageSlug is supplied, book-splash otherwise.
            // loadBook(reuseViewMode=true) honours that.
            localState.viewMode = pageSlug ? "page-editor" : "book-splash";
            tab.state = { ...(tab.state || {}), bookSlug, pageSlug: pageSlug || null };
            bookSelect.value = bookSlug;
            await loadBook(bookSlug, /*reuseViewMode*/ true);
        }

        // Toast UI's WYSIWYG mode is contenteditable-based, not
        // CodeMirror, so the previous detach+reattach blank-editor
        // bug doesn't reproduce. Keep the function as a no-op so
        // callers that still invoke it stay green.
        function refreshEditor() { /* no-op */ }

        // refreshCurrentPage re-fetches the open page and quietly
        // patches the editor if the server-side content drifted —
        // the AI's wiki tools (create_page, update_page, etc.)
        // emit a TOOL_EFFECT signal that chat.js converts into a
        // familiar:notesChanged event; this hook is the listener's
        // entry point. Cursor preservation: we skip setMarkdown
        // entirely when content is byte-identical to what's in the
        // editor, so users mid-typing don't get reset to a stale
        // copy.
        async function refreshCurrentPage() {
            if (!localState.page || !localState.pageSlug || !localState.bookSlug) {
                // No page loaded — just refresh the list so a new
                // page created by the AI appears in the sidebar.
                await refreshPages();
                renderPageList();
                return;
            }
            try {
                const p = await apiJSON(
                    "/console/api/books/" + encodeURIComponent(localState.bookSlug) +
                    "/pages/" + encodeURIComponent(localState.pageSlug));
                // Same discipline as poll(): NEVER overwrite a dirty
                // editor. This fires from many unrelated events (any AI
                // tool effect, another panel's save) and used to
                // setMarkdown() with no dirty check — silently wiping
                // unsaved keystrokes — and without reseeding the sync
                // baseline, so the next save then spuriously 409'd.
                // Route through the shared handling: dirty -> banner,
                // clean -> swapInRemote (which reseeds the baseline).
                if (p && p.updated_at && p.updated_at !== localState.baseUpdatedAt) {
                    if (isDirty()) {
                        showStaleBanner(p);
                    } else {
                        swapInRemote(p, /*silent=*/!localState.isForeground);
                    }
                }
                await refreshPages();
                renderPageList();
            } catch (e) {
                console.warn("wiki: refreshCurrentPage failed", e);
            }
        }

        // loadPageById resolves a page UUID to its slug via the
        // page-by-id endpoint, then hands off to loadPage. Used by
        // the home pin click path where we have the UUID but not
        // the slug.
        async function loadPageById(pageId) {
            if (!pageId || !localState.bookSlug) return;
            try {
                const p = await apiJSON(
                    "/console/api/books/" + encodeURIComponent(localState.bookSlug) +
                    "/page-by-id/" + encodeURIComponent(pageId));
                if (p && p.slug) loadPage(p.slug);
            } catch (e) {
                console.warn("wiki: loadPageById failed", e);
            }
        }

        // ── Live-research read-only view (RESEARCH-SKILL-SPEC §6.7) ──
        //
        // openResearchReadonly points this wiki tab at the hidden
        // research evidence page and renders it read-only, live. The
        // evidence book (research:<uid>) is deliberately absent from
        // the book listing, so we bypass the book-list machinery
        // entirely and fetch the page straight by slug. chat.js drives
        // this via the familiar:openDoc handler below when a run goes
        // live; it swaps the pane to the delivered note when the run
        // finishes. The user cannot edit — this is transient scratch
        // the workers keep appending to (and reap afterwards).
        async function openResearchReadonly(bookSlug, pageSlug, title) {
            if (!bookSlug || !pageSlug) return;
            // Flush + stop any editable-page machinery this tab was
            // running; the research view has no save/poll cadence.
            if (localState.saveTimer) {
                clearTimeout(localState.saveTimer);
                localState.saveTimer = null;
            }
            stopPolling();
            localState.page = null;
            localState.pageSlug = null;
            localState.research = { bookSlug, pageSlug, pageId: null };
            localState.viewMode = "research";
            researchBanner.querySelector(".wiki-research-topic").textContent = title || "";
            if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                window.FamiliarWorkspace.updateTabTitle(tab.id, title || "Research");
            }
            if ((researchProse.textContent || "").trim() === "") {
                researchProse.innerHTML =
                    '<p class="wiki-research-loading">Loading research…</p>';
            }
            applyViewMode();
            await refreshResearch();
        }

        // refreshResearch re-fetches the evidence page and re-renders
        // it in place. Fired on page-saved SSE events (fast) and on
        // chat.js's 5s poll tick (fallback). Sticks to the bottom
        // while the reader is already near it so appended findings
        // scroll into view; otherwise preserves their position.
        async function refreshResearch() {
            const r = localState.research;
            if (!r) return;
            let p;
            try {
                p = await apiJSON(
                    "/console/api/books/" + encodeURIComponent(r.bookSlug) +
                    "/pages/" + encodeURIComponent(r.pageSlug));
            } catch (e) {
                // Evidence page may not exist for the first second or
                // two, or has been reaped — leave the last render.
                return;
            }
            if (localState.research !== r) return; // swapped away mid-flight
            r.pageId = p.id;
            const renderMD = await ensureMarkdownDeps();
            if (localState.research !== r) return;
            const nearBottom =
                researchBody.scrollHeight - researchBody.scrollTop - researchBody.clientHeight <= 80;
            researchProse.innerHTML = renderMD(p.content || "");
            const topicEl = researchBanner.querySelector(".wiki-research-topic");
            if (p.title && topicEl && topicEl.textContent !== p.title) {
                topicEl.textContent = p.title;
            }
            if (nearBottom) researchBody.scrollTop = researchBody.scrollHeight;
        }

        // onPageEvent handles server-pushed page mutations (SSE).
        // Delegates to poll() when the open page matches — poll has
        // the dirty-aware swap logic — and refreshes the page index
        // for unrelated pages so list rows update their updated_at.
        function onPageEvent(detail) {
            if (!detail || (detail.kind !== "page-saved" && detail.kind !== "page-deleted")) return;
            // Live-research read-only: match the evidence page by slug
            // (payload carries book_slug + page_slug) or by the page id
            // we learned on first fetch. Re-render on any save so new
            // findings appear within a second of a worker appending.
            if (localState.research) {
                const r = localState.research;
                const pl = detail.payload || {};
                const match =
                    (pl.book_slug === r.bookSlug && pl.page_slug === r.pageSlug) ||
                    (r.pageId && detail.page_id === r.pageId);
                if (match && detail.kind === "page-saved") refreshResearch();
                return;
            }
            if (localState.page && detail.page_id === localState.page.id) {
                poll();
                return;
            }
            // Different page in the same book (or a book change) —
            // bump the index so its row's updated_at is current.
            refreshPages().then(renderPageList).catch(() => {});
        }

        return {
            root,
            init() { refreshBooks(); },
            refresh() { refreshBooks(); },
            refreshEditor,
            refreshCurrentPage,
            onPageEvent,
            focusBook,
            loadPageById,
            openResearchReadonly,
            refreshResearch,
            resetToWikiSplash,
            createBookFromOutside: createBook,
            // setForeground is called by the workspace shell when
            // this tab becomes / stops being the foreground tab in
            // its panel — drives the 5s fast-poll cadence.
            setForeground,
            // teardown stops the background pollers when a tab is
            // closed so a phantom tab doesn't keep fetching.
            teardown() { stopPolling(); unregisterUnloadFlush(); clearBanner(); localState.research = null; },
        };
    }

    /* -----------------------------------------------------------
       Members modal — single shared instance in index.html. Any
       wiki shell calls openMembersModal(slug, name) to populate
       it; the modal owns its own form / row click handlers from
       module load so the wiring runs once.
       -----------------------------------------------------------*/

    let modalCtx = null; // {bookSlug, name} while open

    function escapeForModal(s) {
        return String(s == null ? "" : s)
            .replace(/&/g, "&amp;").replace(/</g, "&lt;")
            .replace(/>/g, "&gt;").replace(/"/g, "&quot;");
    }

    function showModalError(err) {
        const el = document.getElementById("wiki-members-error");
        if (!el) return;
        if (!err) { el.hidden = true; el.textContent = ""; return; }
        el.hidden = false;
        el.textContent = err.message || String(err);
    }

    async function openMembersModal(bookSlug, bookName) {
        const modal = document.getElementById("wiki-members-modal");
        if (!modal) return;
        modalCtx = { bookSlug, name: bookName };
        const titleEl = document.getElementById("wiki-members-title");
        if (titleEl) titleEl.textContent = "Members of " + (bookName || bookSlug);
        showModalError(null);
        const idInp = document.getElementById("wiki-members-add-id");
        if (idInp) idInp.value = "";
        modal.hidden = false;
        await reloadMembers();
    }

    function closeMembersModal() {
        const modal = document.getElementById("wiki-members-modal");
        if (modal) modal.hidden = true;
        modalCtx = null;
        showModalError(null);
    }

    async function reloadMembers() {
        if (!modalCtx) return;
        const body = document.getElementById("wiki-members-body");
        if (!body) return;
        body.innerHTML = '<div class="wiki-members-loading">Loading…</div>';
        try {
            const resp = await apiJSON("/console/api/books/" + encodeURIComponent(modalCtx.bookSlug) + "/members");
            const items = (resp && resp.items) || [];
            const ownerCount = items.filter((m) => m.role === "owner").length;
            renderMembers(body, items, ownerCount);
        } catch (e) {
            body.innerHTML = '<div class="wiki-members-empty">Couldn\'t load members: ' + escapeForModal(e.message || e) + '</div>';
        }
    }

    function renderMembers(host, items, ownerCount) {
        host.innerHTML = "";
        if (items.length === 0) {
            host.innerHTML = '<div class="wiki-members-empty">No members.</div>';
            return;
        }
        const fmtDate = (iso) => {
            if (!iso) return "";
            const d = new Date(iso);
            return isNaN(d.getTime())
                ? ""
                : d.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
        };
        for (const m of items) {
            const row = document.createElement("div");
            row.className = "wiki-members-row";

            const id = document.createElement("div");
            id.innerHTML =
                '<div class="wiki-members-id">' + escapeForModal(m.user_id) + '</div>' +
                '<span class="wiki-members-id-meta">joined ' + escapeForModal(fmtDate(m.joined_at)) + '</span>';

            const select = document.createElement("select");
            select.className = "wiki-members-role";
            select.dataset.userId = m.user_id;
            for (const r of ["owner", "writer", "reader"]) {
                const opt = document.createElement("option");
                opt.value = r;
                opt.textContent = r.charAt(0).toUpperCase() + r.slice(1);
                if (r === m.role) opt.selected = true;
                select.appendChild(opt);
            }
            // Demoting from the only-owner is blocked at the
            // store; the dropdown stays enabled but the user
            // sees a clear error if they try.

            const removeBtn = document.createElement("button");
            removeBtn.type = "button";
            removeBtn.className = "wiki-members-remove";
            removeBtn.textContent = "✕";
            removeBtn.title = m.role === "owner"
                ? "Demote to writer first to remove an owner"
                : "Remove member";
            removeBtn.dataset.userId = m.user_id;
            removeBtn.dataset.role = m.role;
            // Owners can't be removed directly per backend rule.
            if (m.role === "owner") removeBtn.disabled = true;

            row.append(id, select, removeBtn);
            host.appendChild(row);
        }
    }

    // Module-level wiring — runs once, even though the modal
    // gets used by many tabs over the session.
    document.addEventListener("DOMContentLoaded", () => {
        const modal = document.getElementById("wiki-members-modal");
        if (!modal) return;

        const closeBtn = document.getElementById("wiki-members-close");
        if (closeBtn) closeBtn.addEventListener("click", closeMembersModal);
        // Click outside the card to dismiss.
        modal.addEventListener("click", (ev) => {
            if (ev.target === modal) closeMembersModal();
        });

        // Role-change via dropdown — PATCH /members/{user_id}.
        modal.addEventListener("change", async (ev) => {
            const select = ev.target.closest(".wiki-members-role");
            if (!select || !modalCtx) return;
            const userId = select.dataset.userId;
            const newRole = select.value;
            showModalError(null);
            try {
                await apiJSON(
                    "/console/api/books/" + encodeURIComponent(modalCtx.bookSlug) +
                    "/members/" + encodeURIComponent(userId),
                    { method: "PATCH", headers: { "Content-Type": "application/json" },
                      body: JSON.stringify({ role: newRole }) });
                await reloadMembers();
            } catch (e) {
                showModalError(e);
                // Revert the dropdown to the prior server state.
                await reloadMembers();
            }
        });

        // Remove member — DELETE /members/{user_id}.
        modal.addEventListener("click", async (ev) => {
            const btn = ev.target.closest(".wiki-members-remove");
            if (!btn || !modalCtx || btn.disabled) return;
            const userId = btn.dataset.userId;
            if (!confirm("Remove " + userId + " from this book?")) return;
            showModalError(null);
            try {
                await apiJSON(
                    "/console/api/books/" + encodeURIComponent(modalCtx.bookSlug) +
                    "/members/" + encodeURIComponent(userId),
                    { method: "DELETE" });
                await reloadMembers();
            } catch (e) {
                showModalError(e);
            }
        });

        // Typeahead picker on the user-search input. Debounced GET
        // against /console/api/users/lookup; click a row to lock
        // in a canonical user_id; submit POSTs to /members.
        // Falling back to a raw value (no suggestion clicked) is
        // still allowed — the backend resolves email→id and
        // friendly-errors a missing user_id.
        const inp = document.getElementById("wiki-members-add-id");
        const resolved = document.getElementById("wiki-members-add-resolved");
        const sugBox = document.getElementById("wiki-members-suggestions");
        const addForm = document.getElementById("wiki-members-add");

        let lookupTimer = null;
        let lookupSeq = 0;

        function clearSuggestions() {
            if (sugBox) {
                sugBox.innerHTML = "";
                sugBox.hidden = true;
            }
        }

        function pickSuggestion(item) {
            if (!inp || !resolved) return;
            inp.value = item.display_name || item.email || item.id;
            resolved.value = item.id;
            clearSuggestions();
        }

        async function runLookup(q) {
            if (!sugBox) return;
            const seq = ++lookupSeq;
            try {
                const resp = await apiJSON("/console/api/users/lookup?q=" +
                    encodeURIComponent(q) + "&limit=8");
                if (seq !== lookupSeq) return; // stale response
                renderSuggestions((resp && resp.items) || []);
            } catch (e) {
                if (seq !== lookupSeq) return;
                clearSuggestions();
            }
        }

        function renderSuggestions(items) {
            if (!sugBox) return;
            sugBox.innerHTML = "";
            if (items.length === 0) {
                sugBox.innerHTML = '<div class="wiki-members-suggestion-empty">No matches.</div>';
                sugBox.hidden = false;
                return;
            }
            for (const item of items) {
                const row = document.createElement("button");
                row.type = "button";
                row.className = "wiki-members-suggestion";
                const main = item.display_name && item.display_name !== item.id
                    ? item.display_name
                    : (item.email || item.id);
                const sub = item.email && item.email !== main
                    ? item.email
                    : item.id;
                row.innerHTML =
                    '<span class="wiki-members-suggestion-main">' + escapeForModal(main) + '</span>' +
                    '<span class="wiki-members-suggestion-meta">' + escapeForModal(sub) + '</span>';
                row.addEventListener("mousedown", (ev) => {
                    // mousedown so the input's blur doesn't close
                    // the panel before the click registers.
                    ev.preventDefault();
                    pickSuggestion(item);
                });
                sugBox.appendChild(row);
            }
            sugBox.hidden = false;
        }

        if (inp) {
            inp.addEventListener("input", () => {
                // Manual edits clear any previously-resolved id so a
                // stale picker selection can't sneak through.
                if (resolved) resolved.value = "";
                const q = inp.value.trim();
                if (lookupTimer) clearTimeout(lookupTimer);
                if (q.length < 2) {
                    clearSuggestions();
                    return;
                }
                lookupTimer = setTimeout(() => { lookupTimer = null; runLookup(q); }, 180);
            });
            inp.addEventListener("blur", () => {
                // Tiny delay so the mousedown on a suggestion can
                // run pickSuggestion before the panel hides.
                setTimeout(clearSuggestions, 120);
            });
            inp.addEventListener("keydown", (ev) => {
                if (ev.key === "Escape") clearSuggestions();
            });
        }

        if (addForm) {
            addForm.addEventListener("submit", async (ev) => {
                ev.preventDefault();
                if (!modalCtx) return;
                const role = document.getElementById("wiki-members-add-role").value || "writer";
                const pickedID = (resolved && resolved.value) || "";
                const raw = ((inp && inp.value) || "").trim();
                if (!pickedID && !raw) return;
                const payload = { role };
                if (pickedID) {
                    payload.user_id = pickedID;
                } else if (raw.includes("@")) {
                    payload.email = raw;
                } else {
                    payload.user_id = raw;
                }
                showModalError(null);
                try {
                    await apiJSON(
                        "/console/api/books/" + encodeURIComponent(modalCtx.bookSlug) + "/members",
                        { method: "POST", headers: { "Content-Type": "application/json" },
                          body: JSON.stringify(payload) });
                    if (inp) inp.value = "";
                    if (resolved) resolved.value = "";
                    clearSuggestions();
                    await reloadMembers();
                } catch (e) {
                    showModalError(e);
                }
            });
        }
    });

    if (window.FamiliarWorkspace && window.FamiliarWorkspace.registerSurfaceRenderer) {
        window.FamiliarWorkspace.registerSurfaceRenderer("wiki", render);
    }

    // Sidebar children dispatch familiar:openDoc when a wiki child
    // is clicked. Wiki children are BOOKS — the d.id payload is the
    // book's slug. Optional "bookSlug/pageSlug" form is supported
    // for deep-link cases (e.g. backlink resolution in Phase 2). A
    // null id means "+ New book" was clicked from the sidebar; we
    // route to the wiki splash and immediately trigger the
    // create-book prompt so the click stays one-tap.
    window.addEventListener("familiar:openDoc", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "wiki") return;
        const drive = (entry) => {
            // Live-research read-only open (chat.js drives this when a
            // deep-research run goes live). Bypasses focusBook because
            // the evidence book is hidden from the book listing.
            if (d.research) {
                entry.model.openResearchReadonly(d.bookSlug, d.pageSlug, d.title);
                return;
            }
            if (d.id == null) {
                entry.model.resetToWikiSplash().then(() => entry.model.createBookFromOutside());
                return;
            }
            const parts = String(d.id).split("/");
            if (d.pageId) {
                // Pin click: book slug + page UUID — focus the book, then
                // find the page slug from the list and load it.
                entry.model.focusBook(parts[0]).then(() => {
                    entry.model.loadPageById(d.pageId);
                });
            } else {
                entry.model.focusBook(parts[0], parts[1] || null);
            }
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
            if (!document.body.contains(entry.root)) continue;
            drive(entry);
            return;
        }
    });

    // AI tool calls that mutate wiki pages emit a TOOL_EFFECT
    // signal that chat.js converts into a familiar:notesChanged
    // event. Refresh every live wiki shell so an open page picks
    // up the AI's edits without the user manually reloading.
    window.addEventListener("familiar:notesChanged", () => {
        for (const [, entry] of shells) {
            if (!document.body.contains(entry.root)) continue;
            if (entry.model && entry.model.refreshCurrentPage) {
                entry.model.refreshCurrentPage();
            }
        }
    });

    // Server-pushed page-saved / page-deleted events. Hands the
    // payload to every live wiki shell; each one decides whether to
    // sync (only when its open page matches AND the editor is clean
    // — that's poll()'s responsibility).
    window.addEventListener("familiar:pageEvent", (ev) => {
        const d = ev.detail || {};
        for (const [, entry] of shells) {
            if (!document.body.contains(entry.root)) continue;
            if (entry.model && entry.model.onPageEvent) {
                entry.model.onPageEvent(d);
            }
        }
    });

    // chat.js's research poll re-triggers a read-only refresh every 5s
    // as a fallback for the SSE path — covers the (rare) case where a
    // page-saved event for the hidden evidence book doesn't reach this
    // shell. Scoped to the exact tab chat.js opened for the run.
    window.addEventListener("familiar:researchEvidenceRefresh", (ev) => {
        const d = ev.detail || {};
        const entry = d.tabId && shells.get(d.tabId);
        if (entry && document.body.contains(entry.root) && entry.model.refreshResearch) {
            entry.model.refreshResearch();
        }
    });

    // Sidebar's category-row click (the title area, NOT the chevron)
    // dispatches familiar:surfaceNavRoot. Wiki responds by routing
    // back to the wiki splash so "click Wiki" always lands on the
    // navigation hub, regardless of which book/page was last open.
    window.addEventListener("familiar:surfaceNavRoot", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "wiki") return;
        // Reset the exact tab the workspace just focused (it may
        // have just been created in the leftmost panel). Falls
        // back to first-live-shell when tabId is missing.
        if (d.tabId) {
            const entry = shells.get(d.tabId);
            if (entry) entry.model.resetToWikiSplash();
            return;
        }
        for (const [, entry] of shells) {
            if (!document.body.contains(entry.root)) continue;
            entry.model.resetToWikiSplash();
            return;
        }
    });

    // Tear down background pollers when a wiki tab is closed so a
    // closed shell doesn't keep polling indefinitely. Workspace
    // dispatches this from closeTab().
    window.addEventListener("familiar:tabClosed", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "wiki") return;
        const entry = shells.get(d.tabId);
        if (entry && entry.model.teardown) entry.model.teardown();
        shells.delete(d.tabId);
    });
})();
