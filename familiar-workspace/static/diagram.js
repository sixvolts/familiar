// diagram.js — the Diagram workspace tab (MEDIA-DIAGRAMS Phase 0b).
// Clicking a rendered mermaid block in Rich Text opens its fence
// here: a mermaid-live-editor-style split (source left, live render
// right) inside one normal workspace tab. Save patches the fence
// back into the owning note/wiki page by index; the inline block and
// any open editor refresh through the existing notesChanged signal.
//
// Phase 3 hosts draw.io in this same tab kind — different fence
// language, different editor pane, same chrome.
(function () {
    "use strict";

    const helpers = () => window.familiarAppHelpers || {};

    // Per-tab shells, keyed by tab id (same pattern as chat/notes).
    const shells = new Map();

    function buildShell(host, tab) {
        host.innerHTML = "";
        const root = document.createElement("div");
        root.className = "diagram-shell";

        const head = document.createElement("div");
        head.className = "diagram-head";
        const crumb = document.createElement("div");
        crumb.className = "diagram-crumb";
        crumb.textContent = "No diagram loaded";
        const saveBtn = document.createElement("button");
        saveBtn.type = "button";
        saveBtn.className = "btn-accent btn-small";
        saveBtn.textContent = "Save to page";
        saveBtn.disabled = true;
        const status = document.createElement("span");
        status.className = "micro diagram-status";
        head.append(crumb, status, saveBtn);
        root.appendChild(head);

        const split = document.createElement("div");
        split.className = "diagram-split";
        const source = document.createElement("textarea");
        source.className = "diagram-source";
        source.placeholder = "graph TD;\n  A --> B;";
        source.spellcheck = false;
        const preview = document.createElement("div");
        preview.className = "diagram-preview";
        split.append(source, preview);
        root.appendChild(split);
        host.appendChild(root);

        const shell = {
            tab, root, crumb, saveBtn, status, source, preview,
            state: null,   // { book_slug, page_id, fence_index, page_title }
            dirty: false,
            renderTimer: null,
        };

        const renderPreview = () => {
            preview.innerHTML = "";
            const block = document.createElement("div");
            block.className = "mermaid-block";
            block.textContent = source.value;
            preview.appendChild(block);
            if (window.familiarMermaid) {
                window.familiarMermaid.renderBlock(block, source.value);
            }
        };
        shell.renderPreview = renderPreview;

        source.addEventListener("input", () => {
            shell.dirty = true;
            saveBtn.disabled = !shell.state;
            status.textContent = "unsaved";
            if (shell.renderTimer) clearTimeout(shell.renderTimer);
            shell.renderTimer = setTimeout(renderPreview, 250);
        });

        saveBtn.addEventListener("click", () => saveShell(shell));
        return shell;
    }

    // replaceFence swaps the body of the Nth ```mermaid fence.
    // Returns null when the page no longer has that many fences —
    // the diagram moved or was deleted under us.
    function replaceFence(content, index, newSource) {
        const re = /```mermaid[^\n]*\n([\s\S]*?)```/g;
        let i = -1;
        let result = null;
        result = content.replace(re, (match) => {
            i++;
            if (i !== index) return match;
            const body = newSource.endsWith("\n") ? newSource : newSource + "\n";
            return "```mermaid\n" + body + "```";
        });
        return i >= index ? result : null;
    }

    async function saveShell(shell) {
        const api = helpers().apiJSON;
        if (!api || !shell.state) return;
        const s = shell.state;
        shell.status.textContent = "saving…";
        try {
            // Fetch fresh content so we never clobber edits made
            // elsewhere since this tab opened.
            const pageURL = "/console/api/books/" + encodeURIComponent(s.book_slug) +
                "/page-by-id/" + encodeURIComponent(s.page_id);
            const page = await api(pageURL);
            const next = replaceFence(page.content || "", s.fence_index, shell.source.value);
            if (next == null) {
                throw new Error("the page no longer has this diagram — reopen it from the page");
            }
            await api(pageURL, {
                method: "PATCH",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ content: next }),
            });
            shell.dirty = false;
            shell.status.textContent = "saved";
            // Shared pages get fresh diagram PNGs immediately — the
            // page object we just fetched carries the share state.
            if (page.share && window.familiarMermaid) {
                window.familiarMermaid.syncShareRenders(
                    { bookSlug: s.book_slug, pageId: s.page_id }, next);
            }
            if (helpers().toast) helpers().toast("Diagram saved to " + (s.page_title || "page"), "success");
            // Open editors (and the inline block) pick up the change.
            window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
        } catch (e) {
            shell.status.textContent = "";
            if (helpers().toast) helpers().toast("Couldn't save diagram: " + (e.message || e), "error");
        }
    }

    function loadShell(shell, d) {
        shell.state = {
            book_slug: d.book_slug,
            page_id: d.page_id,
            fence_index: d.fence_index || 0,
            page_title: d.page_title || "page",
        };
        shell.tab.state = { ...(shell.tab.state || {}), ...shell.state };
        shell.crumb.textContent = (d.page_title || "page") + " · diagram " + ((d.fence_index || 0) + 1);
        shell.source.value = d.source || "";
        shell.saveBtn.disabled = false;
        shell.dirty = false;
        shell.status.textContent = "";
        shell.renderPreview();
        const ws = window.FamiliarWorkspace;
        if (ws && ws.updateTabTitle) {
            ws.updateTabTitle(shell.tab.id, (d.page_title || "Diagram") + " · diagram");
        }
    }

    function register() {
        const ws = window.FamiliarWorkspace;
        if (!ws || !ws.registerSurfaceRenderer) {
            setTimeout(register, 50);
            return;
        }
        ws.registerSurfaceRenderer("diagram", (host, tab) => {
            let shell = shells.get(tab.id);
            // Rebuild the DOM on every mount (panels re-render), but
            // keep the shell's loaded state across mounts.
            const prev = shell;
            shell = buildShell(host, tab);
            shells.set(tab.id, shell);
            if (prev && prev.state) {
                loadShell(shell, {
                    ...prev.state,
                    source: prev.source.value,
                    page_title: prev.state.page_title,
                });
                shell.dirty = prev.dirty;
                if (shell.dirty) shell.status.textContent = "unsaved";
            } else if (tab.state && tab.state.page_id) {
                // Restored from persisted workspace state: we have
                // identity but not the source — pull it from the page.
                const api = helpers().apiJSON;
                if (api) {
                    api("/console/api/books/" + encodeURIComponent(tab.state.book_slug) +
                        "/page-by-id/" + encodeURIComponent(tab.state.page_id))
                        .then((page) => {
                            const re = /```mermaid[^\n]*\n([\s\S]*?)```/g;
                            let m, i = -1;
                            while ((m = re.exec(page.content || ""))) {
                                i++;
                                if (i === (tab.state.fence_index || 0)) {
                                    loadShell(shell, {
                                        ...tab.state,
                                        source: m[1],
                                        page_title: tab.state.page_title || page.title,
                                    });
                                    return;
                                }
                            }
                            shell.crumb.textContent = "Diagram no longer on the page";
                        })
                        .catch(() => { shell.crumb.textContent = "Couldn't load diagram"; });
                }
            }
        });
    }
    register();

    // The workspace prepared a tab and dispatched openDoc with its
    // id — load the fence into exactly that shell.
    window.addEventListener("familiar:openDoc", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "diagram") return;
        if (d.tabId) {
            const shell = shells.get(d.tabId);
            if (shell && document.body.contains(shell.root)) {
                loadShell(shell, d);
                return;
            }
        }
        for (const [, shell] of shells) {
            if (document.body.contains(shell.root)) {
                loadShell(shell, d);
                return;
            }
        }
    });

    window.addEventListener("familiar:tabClosed", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "diagram") return;
        shells.delete(d.tabId);
    });
})();
