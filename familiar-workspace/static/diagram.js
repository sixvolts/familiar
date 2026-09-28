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
            setDirty(shell, true);
            saveBtn.disabled = !shell.state;
            status.textContent = "unsaved";
            if (shell.renderTimer) clearTimeout(shell.renderTimer);
            shell.renderTimer = setTimeout(renderPreview, 250);
        });

        saveBtn.addEventListener("click", () => saveShell(shell));
        return shell;
    }

    // setDirty mirrors the shell's unsaved state onto its workspace tab,
    // so the tab shows the dirty dot and closing it asks first.
    function setDirty(shell, dirty) {
        shell.dirty = dirty;
        const ws = window.FamiliarWorkspace;
        if (ws && ws.setTabDirty) ws.setTabDirty(shell.tab.id, dirty);
    }

    // fences lists the page's mermaid code blocks the way the editor
    // counts them (the index a diagram tab carries comes from the editor):
    // familiarMermaid.fences, one scanner for both sides.
    function fences(content) {
        const fm = window.familiarMermaid;
        if (!fm || !fm.fences) throw new Error("the diagram renderer isn't loaded");
        return fm.fences(content);
    }

    // fenceBody returns the body of the Nth mermaid fence, or null.
    function fenceBody(content, index) {
        const f = fences(content)[index];
        return f ? f.body : null;
    }

    function sameDiagram(a, b) {
        return (a || "").replace(/\s+$/, "") === (b || "").replace(/\s+$/, "");
    }

    // replaceFence swaps the body of the Nth mermaid fence, keeping its
    // own fence lines (```Mermaid, ~~~mermaid, four backticks).
    // Returns null when the page no longer has that many fences —
    // the diagram moved or was deleted under us.
    function replaceFence(content, index, newSource) {
        const f = fences(content)[index];
        if (!f) return null;
        const body = newSource.endsWith("\n") ? newSource : newSource + "\n";
        return content.slice(0, f.bodyStart) + body + content.slice(f.bodyEnd);
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
            // The fence this tab edits must still hold the diagram it
            // opened. The fresh GET's If-Match alone guarded nothing: a
            // diagram inserted above this one moved the index (so Save
            // overwrote the wrong diagram), and edits made to this one
            // directly in the page were reverted, both reported "saved".
            const current = fenceBody(page.content || "", s.fence_index);
            if (current == null) {
                throw new Error("the page no longer has this diagram — reopen it from the page");
            }
            if (s.base_source != null && !sameDiagram(current, s.base_source)) {
                throw new Error("this diagram was changed on the page since you opened it — reopen it from the page to edit the current version");
            }
            const sent = shell.source.value;
            const next = replaceFence(page.content || "", s.fence_index, sent);
            // If-Match from the GET closes the window between that read and
            // this write; the base_source check above covers the time since
            // the tab opened.
            await api(pageURL, {
                method: "PATCH",
                headers: { "Content-Type": "application/json", "If-Match": page.updated_at },
                body: JSON.stringify({ content: next }),
            });
            s.base_source = sent;
            // Clear dirty only if nothing was typed during the save.
            if (shell.source.value === sent) setDirty(shell, false);
            shell.status.textContent = shell.dirty ? "unsaved" : "saved";
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
            // The fence body this tab was opened with; Save refuses if the
            // page's diagram no longer matches it (see saveShell).
            base_source: d.base_source != null ? d.base_source : (d.source || ""),
        };
        shell.tab.state = { ...(shell.tab.state || {}), ...shell.state };
        shell.crumb.textContent = (d.page_title || "page") + " · diagram " + ((d.fence_index || 0) + 1);
        shell.source.value = d.source || "";
        shell.saveBtn.disabled = false;
        setDirty(shell, !!d.dirty);
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
                    dirty: prev.dirty,
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
                            const f = fences(page.content || "")[tab.state.fence_index || 0];
                            if (f) {
                                loadShell(shell, {
                                    ...tab.state,
                                    source: f.body,
                                    // The fence just read is the baseline;
                                    // the persisted one may predate saves.
                                    base_source: f.body,
                                    page_title: tab.state.page_title || page.title,
                                });
                                return;
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
