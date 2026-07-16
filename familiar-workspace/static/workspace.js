// Workspace panel-grid + tab system (FAMILIAR-WORKSPACE-SPEC Phase 1b).
//
// State machine — flat tab array, panels reference tabs by id. The
// flat shape is easier to serialize and restore from localStorage
// than a nested layout-tree.
//
//     {
//       layout: "split-2x1",            // grid template
//       activePanelSlot: "A",           // for sidebar focus rules
//       panels: { A: { activeTabId, tabIds: [] }, B: ..., ... },
//       tabs:   { t1: { id, surface, panelSlot, title, dirty, state }, ... },
//       trackSizes: { col1: 0.5, col2: 0.5, row1: 0.5, row2: 0.5 },
//     }
//
// Layout names map to CSS Grid templates via the .ws-grid-<name>
// class in app.css. Resize handles update CSS custom properties on
// the grid container; the grid recomputes track sizes from the
// vars on the next paint.
//
// Phase 1c replaces the surface placeholders with real Chat /
// Notes / Wiki UIs; the contract for that integration is the
// renderTabContent(tab) function below — surfaces register their
// renderer there.

(function () {
    "use strict";

    // ── Constants ──────────────────────────────────────────────

    // The 8 layouts the spec calls out. Each entry names the slots
    // that exist in that layout (so renderGrid knows how many panels
    // to draw and how to label them) plus a human label for the
    // toolbar readout.
    // Minimal layout set. Stacked / quad / t-bottom / t-left /
    // t-right were dropped — they didn't earn their picker slot.
    // split-1x2 is the working "side-by-side rotated 90°" — full-
    // width top space, full-width bottom space.
    const LAYOUTS = {
        "single":    { slots: ["A"],            label: "single" },
        "split-2x1": { slots: ["A", "B"],       label: "side by side" },
        "split-1x2": { slots: ["A", "B"],       label: "stacked" },
        "t-top":     { slots: ["A", "B", "C"],  label: "T-top" },
    };

    // Aliases so localStorage entries from removed layouts don't
    // leave the workspace in an unloadable state. State written
    // before this commit had stacked-1x2 / quad / t-bottom /
    // t-left / t-right; remap to the closest survivor.
    const LAYOUT_ALIASES = {
        "stacked-1x2": "split-1x2",
        "quad":        "split-2x1",
        "t-bottom":    "t-top",
        "t-left":      "t-top",
        "t-right":     "t-top",
    };

    // Surface metadata. title is the default tab title for a fresh
    // tab; placeholder is the inline content shown in Phase 1b
    // before each surface gets its real implementation. Phase 1c
    // wires real renderers via registerSurfaceRenderer.
    const SURFACES = {
        chat:   { title: "Chat",   placeholder: "Chat surface lands in Phase 1c. Will stream from /api/chat (native protocol) and persist via /console/api/conversations." },
        notes:  { title: "Notes",  placeholder: "Notes surface lands in Phase 2." },
        wiki:   { title: "Wiki",   placeholder: "Loading…" },
        shards: { title: "Shards", placeholder: "Shards in tabs lands in Phase 4 (the existing Shards panel still works as a secondary surface)." },
        diagram: { title: "Diagram", placeholder: "Open a diagram by clicking a rendered mermaid block in a note or wiki page." },
    };

    const STORAGE_KEY = "familiar.workspace.v1";

    const MIN_TRACK_FRACTION = 0.15; // 15% of grid extent — keeps
                                     // resize handles from collapsing
                                     // a panel below the spec's
                                     // 300×200 minimum on most
                                     // viewports.

    // ── State ──────────────────────────────────────────────────

    const state = loadState();

    // Surface renderers register here. Phase 1c populates this map.
    // Default renderer renders the placeholder text — visible until
    // a real surface is wired.
    const surfaceRenderers = {};

    // ── Public API ─────────────────────────────────────────────

    // exposed on window so Phase 1c can register chat surface,
    // Phase 2 notes, etc., without re-entering this IIFE.
    window.FamiliarWorkspace = {
        registerSurfaceRenderer(name, fn) { surfaceRenderers[name] = fn; },
        focusSurface,                       // sidebar primary nav clicks
        openTab,
        closeTab,
        switchTab,
        setLayout,
        getState() { return JSON.parse(JSON.stringify(state)); },
        // Update a tab's display title (called by surfaces when a
        // doc loads or is renamed). Re-renders just the tab label.
        updateTabTitle(tabId, title) {
            const tab = state.tabs[tabId];
            if (!tab || tab.title === title) return;
            tab.title = title;
            saveState();
            const btn = document.querySelector('.ws-tab[data-tab-id="' + tabId + '"] .ws-tab-label');
            if (btn) btn.textContent = title;
        },
        // Try to focus an existing tab with this document open.
        // Returns true if found and focused, false if not found.
        // Surfaces call this before loading a doc to avoid opening
        // a duplicate tab.
        focusExistingDoc(surface, docId) {
            const existing = findTabWithDoc(surface, docId);
            if (!existing) return false;
            const panel = state.panels[existing.slot];
            panel.activeTabId = existing.tabId;
            state.activePanelSlot = existing.slot;
            saveState();
            renderGrid();
            return true;
        },
        // THE public doc-open entry point. Every "open this doc"
        // path (sidebar rows, home pins, [[wiki-links]], shard Chat
        // buttons) should come through here so the tab-targeting
        // contract holds everywhere: never override a doc-bearing
        // tab, reuse only the same surface's splash, otherwise new
        // tab in the left-most panel — and the openDoc event always
        // carries the prepared tab's id.
        openDoc(surface, id, title, extra) {
            openDocFromSidebar(surface, id, title, extra);
        },
    };

    // ── State load/save ────────────────────────────────────────

    function loadState() {
        try {
            const raw = localStorage.getItem(STORAGE_KEY);
            if (raw) {
                const parsed = JSON.parse(raw);
                if (parsed && parsed.layout) {
                    // Remap dropped-layout entries to their
                    // closest survivor before validating.
                    if (LAYOUT_ALIASES[parsed.layout]) {
                        parsed.layout = LAYOUT_ALIASES[parsed.layout];
                    }
                    if (LAYOUTS[parsed.layout]) {
                        return normalize(parsed);
                    }
                }
            }
        } catch (e) { /* fall through to defaults */ }
        return defaultState();
    }

    function defaultState() {
        // Spec default: 2x1 side-by-side, left = Chat, right = Notes.
        const tab1 = makeTab("chat", "A");
        const tab2 = makeTab("notes", "B");
        return {
            layout: "split-2x1",
            activePanelSlot: "A",
            panels: {
                A: { activeTabId: tab1.id, tabIds: [tab1.id] },
                B: { activeTabId: tab2.id, tabIds: [tab2.id] },
            },
            tabs: { [tab1.id]: tab1, [tab2.id]: tab2 },
            trackSizes: { col1: 0.5, col2: 0.5, row1: 0.5, row2: 0.5 },
        };
    }

    // normalize trims state to the layout's slot set and drops
    // dangling tab references — defends against schema drift if a
    // layout is removed or a tab is orphaned mid-edit.
    function normalize(s) {
        const layout = LAYOUTS[s.layout] ? s.layout : "split-2x1";
        const validSlots = new Set(LAYOUTS[layout].slots);
        const panels = {};
        for (const slot of validSlots) {
            const p = (s.panels && s.panels[slot]) || { activeTabId: null, tabIds: [] };
            const tabIds = (p.tabIds || []).filter((id) => s.tabs && s.tabs[id]);
            const activeTabId = tabIds.includes(p.activeTabId) ? p.activeTabId : tabIds[0] || null;
            panels[slot] = { activeTabId, tabIds };
        }
        const tabs = {};
        for (const slot of validSlots) {
            for (const id of panels[slot].tabIds) {
                const t = s.tabs[id];
                tabs[id] = { ...t, panelSlot: slot };
            }
        }
        return {
            layout,
            activePanelSlot: validSlots.has(s.activePanelSlot) ? s.activePanelSlot : LAYOUTS[layout].slots[0],
            panels,
            tabs,
            trackSizes: Object.assign({ col1: 0.5, col2: 0.5, row1: 0.5, row2: 0.5 }, s.trackSizes || {}),
        };
    }

    function saveState() {
        try {
            localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
        } catch (e) { /* quota or disabled — ignore */ }
    }

    // ── Tab + panel helpers ────────────────────────────────────

    function makeTab(surface, panelSlot, opts) {
        opts = opts || {};
        const surf = SURFACES[surface] || { title: surface };
        return {
            id: "t-" + Math.random().toString(36).slice(2, 10),
            surface,
            panelSlot,
            title: opts.title || surf.title,
            dirty: false,
            state: opts.state || null,
        };
    }

    function openTab(surface, panelSlot, opts) {
        if (!SURFACES[surface]) {
            console.warn("workspace: unknown surface", surface);
            return null;
        }
        const slots = LAYOUTS[state.layout].slots;
        if (!panelSlot || !slots.includes(panelSlot)) {
            panelSlot = state.activePanelSlot && slots.includes(state.activePanelSlot)
                ? state.activePanelSlot
                : slots[0];
        }
        const tab = makeTab(surface, panelSlot, opts);
        state.tabs[tab.id] = tab;
        state.panels[panelSlot].tabIds.push(tab.id);
        state.panels[panelSlot].activeTabId = tab.id;
        state.activePanelSlot = panelSlot;
        saveState();
        renderGrid();
        return tab.id;
    }

    function closeTab(tabId) {
        const tab = state.tabs[tabId];
        if (!tab) return;
        const panel = state.panels[tab.panelSlot];
        if (!panel) return;
        const idx = panel.tabIds.indexOf(tabId);
        if (idx === -1) return;
        panel.tabIds.splice(idx, 1);
        delete state.tabs[tabId];
        // Activate the neighbor: prefer the one to the left, fall
        // back to the right, then null when empty.
        if (panel.activeTabId === tabId) {
            panel.activeTabId = panel.tabIds[idx - 1] || panel.tabIds[idx] || null;
        }
        saveState();
        renderGrid();
        // Notify surface modules so background work (pollers,
        // editor instances, etc.) can teardown. Surfaces opt-in by
        // listening; nothing breaks if none does.
        window.dispatchEvent(new CustomEvent("familiar:tabClosed", {
            detail: { tabId, surface: tab.surface },
        }));
    }

    function switchTab(panelSlot, tabId) {
        const panel = state.panels[panelSlot];
        if (!panel || !panel.tabIds.includes(tabId)) return;
        panel.activeTabId = tabId;
        state.activePanelSlot = panelSlot;
        saveState();
        renderGrid();
    }

    // Move a tab from its current panel to targetSlot, optionally
    // inserting before a specific tab. If beforeTabId is null, the
    // tab appends to the end of the target panel's tab list.
    function moveTab(tabId, targetSlot, beforeTabId) {
        const tab = state.tabs[tabId];
        if (!tab) return;
        const srcPanel = state.panels[tab.panelSlot];
        const dstPanel = state.panels[targetSlot];
        if (!srcPanel || !dstPanel) return;

        // Remove from source.
        const srcIdx = srcPanel.tabIds.indexOf(tabId);
        if (srcIdx !== -1) srcPanel.tabIds.splice(srcIdx, 1);

        // Fix source panel's active tab if we just removed it.
        if (srcPanel.activeTabId === tabId) {
            srcPanel.activeTabId = srcPanel.tabIds[srcIdx - 1]
                || srcPanel.tabIds[srcIdx]
                || srcPanel.tabIds[0]
                || null;
        }

        // Insert into destination.
        if (beforeTabId) {
            const dstIdx = dstPanel.tabIds.indexOf(beforeTabId);
            if (dstIdx !== -1) {
                dstPanel.tabIds.splice(dstIdx, 0, tabId);
            } else {
                dstPanel.tabIds.push(tabId);
            }
        } else {
            dstPanel.tabIds.push(tabId);
        }

        // Update tab metadata + activate in destination.
        tab.panelSlot = targetSlot;
        dstPanel.activeTabId = tabId;
        state.activePanelSlot = targetSlot;

        saveState();
        renderGrid();
    }

    // Get the document ID from a tab's state, based on surface type.
    function getTabDocId(tab) {
        if (!tab || !tab.state) return null;
        switch (tab.surface) {
            case "chat":   return tab.state.conversationId || null;
            case "notes":  return tab.state.noteId || null;
            case "wiki": {
                // Composite slug key for dedup: "book/page". A tab
                // sitting at a book's splash (no page) keys as the
                // bare book slug so a second sidebar click on that
                // book refocuses it instead of minting another tab.
                const b = tab.state.bookSlug || "";
                const p = tab.state.pageSlug || "";
                return (b && p) ? b + "/" + p : (b || null);
            }
            case "shards": return tab.state.shardId || null;
            case "diagram": {
                // One tab per fence: book/page#index.
                const s = tab.state;
                return (s.book_slug && s.page_id)
                    ? s.book_slug + "/" + s.page_id + "#" + (s.fence_index || 0)
                    : null;
            }
            default:       return null;
        }
    }

    // Find an existing tab that has a specific doc open.
    // Returns { tabId, slot } or null.
    function findTabWithDoc(surface, docId) {
        if (!docId) return null;
        for (const slot of LAYOUTS[state.layout].slots) {
            const panel = state.panels[slot];
            for (const tabId of panel.tabIds) {
                const tab = state.tabs[tabId];
                if (tab && tab.surface === surface && getTabDocId(tab) === docId) {
                    return { tabId, slot };
                }
            }
        }
        return null;
    }

    // Check if a tab has no document loaded (fresh/empty state).
    // Surface-specific: chat needs a conversationId, notes needs
    // a noteId, etc. A tab with no state or no doc ID is empty.
    function isTabEmpty(tab) {
        if (!tab) return true;
        const s = tab.state || {};
        switch (tab.surface) {
            case "chat":   return !s.conversationId;
            case "notes":  return !s.noteId;
            // A wiki tab parked at a book's splash is NOT empty — the
            // user navigated to that book; reusing or morphing it
            // would discard their place. Only the top-level wiki
            // splash (no book, no page) is reusable.
            case "wiki":   return !s.pageId && !s.bookSlug;
            case "shards": return !s.shardId;
            case "diagram": return !s.page_id;
            default:       return Object.keys(s).length === 0;
        }
    }

    // focusSurface is the entry point for sidebar primary clicks.
    // Walks the panels left-to-right looking for any tab of this
    // surface — first hit wins, and we focus it without resetting,
    // so a click on Chat/Notes/Wiki returns the user to whatever
    // they had open last. Only opens a new tab when no tab of this
    // surface exists anywhere; the explicit "+" tab-add button
    // covers the "I want a new one" case.
    //
    // Returns { tabId, isNew } so the caller can decide whether
    // to fire familiar:surfaceNavRoot — we only signal a splash
    // reset when we actually created a fresh tab.
    function focusSurface(surface) {
        const slots = LAYOUTS[state.layout].slots;
        for (const slot of slots) {
            const panel = state.panels[slot];
            if (!panel) continue;
            // panel.tabIds is in display order, so the first match
            // is the leftmost tab of this surface in this panel.
            const existing = panel.tabIds.find((id) => {
                const t = state.tabs[id];
                return t && t.surface === surface;
            });
            if (existing) {
                panel.activeTabId = existing;
                state.activePanelSlot = slot;
                saveState();
                renderGrid();
                return { tabId: existing, isNew: false };
            }
        }
        // No tab of this surface exists. Single-splash policy:
        // before minting a new tab, see if any other empty/splash
        // tab is sitting around (e.g. the Notes splash when the
        // user clicks Chat). If so, morph it to this surface
        // instead of stacking a second splash on top.
        const morphed = morphEmptyTab(surface);
        if (morphed) {
            return { tabId: morphed.tabId, isNew: true };
        }
        const tabId = openTab(surface, slots[0]);
        return { tabId, isNew: true };
    }


    // findEmptyTab returns the first tab (any surface) with no
    // doc loaded — i.e. a "splash" tab. Used by the single-splash
    // policy: clicking Notes when only a Chat splash is open
    // morphs that splash to Notes rather than stacking another.
    function findEmptyTab() {
        for (const slot of LAYOUTS[state.layout].slots) {
            const panel = state.panels[slot];
            if (!panel) continue;
            for (const tabId of panel.tabIds) {
                const tab = state.tabs[tabId];
                if (tab && isTabEmpty(tab)) {
                    return { tabId, slot };
                }
            }
        }
        return null;
    }

    // morphEmptyTab converts an existing splash tab to the
    // requested surface. Returns the focus info or null when no
    // empty tab is available. Surface-specific state is reset so
    // the old surface's splash UI (pinned-list scroll, filters,
    // …) doesn't leak into the new one. Doc-bearing tabs are
    // never touched — only splashes are reusable.
    function morphEmptyTab(surface) {
        const found = findEmptyTab();
        if (!found) return null;
        return morphTabTo(found, surface);
    }

    // morphTabTo repurposes an already-empty tab to `surface` and
    // activates it. `found` is {tabId, slot} from findEmptyTab /
    // emptyTabInSlot.
    function morphTabTo(found, surface) {
        const tab = state.tabs[found.tabId];
        if (tab.surface !== surface) {
            // The old surface's shell is going away — same teardown
            // signal closeTab fires, so the module drops its shells
            // entry (pollers, listeners) instead of leaking it.
            window.dispatchEvent(new CustomEvent("familiar:tabClosed", {
                detail: { tabId: found.tabId, surface: tab.surface },
            }));
            tab.surface = surface;
            tab.state = {};
        }
        state.panels[found.slot].activeTabId = found.tabId;
        state.activePanelSlot = found.slot;
        saveState();
        renderGrid();
        return found;
    }

    // emptyTabInSlot returns an empty tab in `slot` — of `surface` when
    // given, else any — or null. Used to reuse a splash in a SPECIFIC
    // panel so new opens land where we want (the left-most panel).
    function emptyTabInSlot(slot, surface) {
        const panel = state.panels[slot];
        if (!panel) return null;
        for (const tabId of panel.tabIds) {
            const tab = state.tabs[tabId];
            if (tab && isTabEmpty(tab) && (!surface || tab.surface === surface)) {
                return { tabId, slot };
            }
        }
        return null;
    }

    function setLayout(name) {
        if (!LAYOUTS[name]) return;
        const newSlots = LAYOUTS[name].slots;
        const oldSlots = LAYOUTS[state.layout].slots;
        // Migrate tabs from removed slots into the first surviving
        // slot so the user doesn't lose state when they shrink the
        // grid. The receiving panel's tab order = its existing
        // tabs followed by the migrated ones.
        const fallbackSlot = newSlots[0];
        for (const slot of oldSlots) {
            if (newSlots.includes(slot)) continue;
            const panel = state.panels[slot];
            if (!panel) continue;
            const fb = state.panels[fallbackSlot] || (state.panels[fallbackSlot] = { activeTabId: null, tabIds: [] });
            for (const id of panel.tabIds) {
                fb.tabIds.push(id);
                if (state.tabs[id]) state.tabs[id].panelSlot = fallbackSlot;
            }
            if (!fb.activeTabId && panel.activeTabId) fb.activeTabId = panel.activeTabId;
            delete state.panels[slot];
        }
        // Ensure every new slot has a panel object.
        for (const slot of newSlots) {
            if (!state.panels[slot]) {
                state.panels[slot] = { activeTabId: null, tabIds: [] };
            }
        }
        state.layout = name;
        if (!newSlots.includes(state.activePanelSlot)) {
            state.activePanelSlot = newSlots[0];
        }
        saveState();
        renderGrid();
    }

    // ── Rendering ──────────────────────────────────────────────

    function renderGrid() {
        const host = document.getElementById("ws-grid");
        if (!host) return;
        const layout = state.layout;
        const slots = LAYOUTS[layout].slots;

        // Update the layout-name readout + active layout button.
        const nameEl = document.getElementById("ws-layout-name");
        if (nameEl) nameEl.textContent = LAYOUTS[layout].label;
        for (const btn of document.querySelectorAll(".ws-layout-btn")) {
            btn.classList.toggle("is-active", btn.dataset.layout === layout);
        }

        // Apply layout class. The CSS owns grid-template-* per
        // layout name; JS only swaps the class.
        host.className = "ws-grid ws-grid-" + layout;

        // Track sizes — set CSS custom properties so the grid can
        // reflect resize-handle drags.
        host.style.setProperty("--col-1-fr", state.trackSizes.col1 + "fr");
        host.style.setProperty("--col-2-fr", state.trackSizes.col2 + "fr");
        host.style.setProperty("--row-1-fr", state.trackSizes.row1 + "fr");
        host.style.setProperty("--row-2-fr", state.trackSizes.row2 + "fr");

        // Re-render: clear, rebuild, attach handles. Re-rendering
        // on every change is fine at this size — < 4 panels.
        host.innerHTML = "";
        for (const slot of slots) {
            host.appendChild(renderPanel(slot));
        }
        attachResizeHandles(host, layout);
    }

    function clearDropIndicators(bar) {
        for (const el of bar.querySelectorAll(".ws-tab-drop-before, .ws-tab-drop-after")) {
            el.classList.remove("ws-tab-drop-before", "ws-tab-drop-after");
        }
        bar.classList.remove("ws-tab-bar-drop-target");
    }

    function renderPanel(slot) {
        const panel = state.panels[slot];
        const wrapper = document.createElement("div");
        wrapper.className = "ws-panel";
        wrapper.dataset.slot = slot;
        if (slot === state.activePanelSlot) wrapper.classList.add("is-active");
        wrapper.style.gridArea = "p" + slot;

        // Tab bar. Layout: [‹] [scrollable strip of tabs] [›] [+]
        // The scrollable strip hides its native horizontal scrollbar
        // (which renders on top of tabs at small sizes). Chevrons
        // appear only when the strip overflows; clicking them
        // scrolls by ~one tab width. Tab labels also auto-truncate
        // down to a min of 8ch via a CSS variable set by
        // applyTabFit() before chevrons kick in.
        const bar = document.createElement("div");
        bar.className = "ws-tab-bar";

        const leftBtn = document.createElement("button");
        leftBtn.type = "button";
        leftBtn.className = "ws-tab-scroll ws-tab-scroll-left";
        leftBtn.innerHTML = "&#8249;"; // ‹
        leftBtn.title = "Scroll tabs left";

        const strip = document.createElement("div");
        strip.className = "ws-tab-strip";
        for (const tabId of panel.tabIds) {
            const tab = state.tabs[tabId];
            if (!tab) continue;
            strip.appendChild(renderTab(tab, slot, panel.activeTabId === tabId));
        }

        const rightBtn = document.createElement("button");
        rightBtn.type = "button";
        rightBtn.className = "ws-tab-scroll ws-tab-scroll-right";
        rightBtn.innerHTML = "&#8250;"; // ›
        rightBtn.title = "Scroll tabs right";

        const addBtn = document.createElement("button");
        addBtn.type = "button";
        addBtn.className = "ws-tab-add";
        addBtn.textContent = "+";
        addBtn.title = "New tab in this panel";
        addBtn.addEventListener("click", (e) => {
            e.stopPropagation();
            // Open a new tab matching the panel's current surface type.
            // Falls back to "chat" if the panel is empty.
            const activeTab = panel.activeTabId && state.tabs[panel.activeTabId];
            const surface = (activeTab && activeTab.surface) || "chat";
            // Single-splash policy: if a splash tab already exists
            // anywhere, reuse it (morph to this surface if needed)
            // rather than stacking another. The "+" button is the
            // third splash-creation path alongside the sidebar
            // surface header and "+ New <surface>" — all three
            // need the same dedup.
            if (!morphEmptyTab(surface)) {
                openTab(surface, slot);
            }
        });

        bar.appendChild(leftBtn);
        bar.appendChild(strip);
        bar.appendChild(rightBtn);
        bar.appendChild(addBtn);

        // Chevron + truncation logic. Runs once on render, again
        // whenever the bar resizes (panel splits, window resize).
        setupTabBarOverflow(bar, strip, leftBtn, rightBtn);

        // ── Drop zone for tab drag-and-drop ────────────────
        bar.addEventListener("dragover", (e) => {
            if (!e.dataTransfer.types.includes("text/x-familiar-tab")) return;
            e.preventDefault();
            e.dataTransfer.dropEffect = "move";

            // Show a drop indicator. Find the tab we're hovering
            // over and decide left-or-right based on cursor position.
            clearDropIndicators(bar);
            const targetTab = e.target.closest(".ws-tab");
            if (targetTab) {
                const rect = targetTab.getBoundingClientRect();
                const midX = rect.left + rect.width / 2;
                if (e.clientX < midX) {
                    targetTab.classList.add("ws-tab-drop-before");
                } else {
                    targetTab.classList.add("ws-tab-drop-after");
                }
            } else {
                // Hovering over empty bar area — drop at end.
                bar.classList.add("ws-tab-bar-drop-target");
            }
        });
        bar.addEventListener("dragleave", (e) => {
            // Only clear if we actually left the bar (not just
            // moving between children).
            if (!bar.contains(e.relatedTarget)) {
                clearDropIndicators(bar);
            }
        });
        bar.addEventListener("drop", (e) => {
            if (!e.dataTransfer.types.includes("text/x-familiar-tab")) return;
            e.preventDefault();
            clearDropIndicators(bar);

            const draggedTabId = e.dataTransfer.getData("text/x-familiar-tab");
            if (!draggedTabId || !state.tabs[draggedTabId]) return;

            // Figure out insertion point.
            let beforeTabId = null;
            const targetTab = e.target.closest(".ws-tab");
            if (targetTab) {
                const rect = targetTab.getBoundingClientRect();
                const midX = rect.left + rect.width / 2;
                const hoveredId = targetTab.dataset.tabId;
                if (e.clientX < midX) {
                    // Drop before the hovered tab.
                    beforeTabId = hoveredId;
                } else {
                    // Drop after the hovered tab — find the next
                    // tab in the panel, or null for end.
                    const panel = state.panels[slot];
                    const idx = panel.tabIds.indexOf(hoveredId);
                    beforeTabId = panel.tabIds[idx + 1] || null;
                }
            }

            // Same-panel reorder or cross-panel move — moveTab
            // handles both (it splices from source and inserts
            // at the target position).
            moveTab(draggedTabId, slot, beforeTabId);
        });

        wrapper.appendChild(bar);

        // Content area.
        const content = document.createElement("div");
        content.className = "ws-panel-content";
        if (panel.activeTabId && state.tabs[panel.activeTabId]) {
            renderTabContent(content, state.tabs[panel.activeTabId]);
        } else {
            content.innerHTML = '<div class="ws-empty">No tabs in this panel.<br><br>Click + above or pick a surface from the sidebar.</div>';
        }
        wrapper.appendChild(content);

        // Click anywhere on the panel marks it active (so + button
        // and sidebar focus targeting know which panel is in focus).
        wrapper.addEventListener("click", () => {
            if (state.activePanelSlot !== slot) {
                state.activePanelSlot = slot;
                saveState();
                for (const el of document.querySelectorAll(".ws-panel")) {
                    el.classList.toggle("is-active", el.dataset.slot === slot);
                }
            }
        });

        return wrapper;
    }

    // Tab-bar overflow handling — truncate labels first, then expose
    // chevron scroll buttons when even minimum-width tabs overflow.
    function setupTabBarOverflow(bar, strip, leftBtn, rightBtn) {
        function applyTabFit() {
            const tabs = strip.querySelectorAll(".ws-tab");
            if (!tabs.length) {
                strip.style.setProperty("--ws-tab-label-max", "20ch");
                return;
            }
            // Reset to natural; if it fits at 20ch, no truncation
            // needed and we're done.
            strip.style.setProperty("--ws-tab-label-max", "20ch");
            if (strip.scrollWidth <= strip.clientWidth) return;
            // Shrink one ch at a time until the strip fits or we
            // hit the 8ch floor. Each style set + scrollWidth read
            // forces a reflow, but the loop is bounded to 12
            // iterations and runs only on resize / tab change, so
            // the cost is negligible. Iterating beats a per-tab
            // chrome formula because real tab widths depend on
            // category, dirty-dot presence, font metrics, etc.
            for (let ch = 19; ch >= 8; ch--) {
                strip.style.setProperty("--ws-tab-label-max", ch + "ch");
                if (strip.scrollWidth <= strip.clientWidth) return;
            }
            // Floored at 8ch — chevrons will surface for the rest.
        }
        function updateChevrons() {
            const overflow = strip.scrollWidth > strip.clientWidth + 1;
            bar.classList.toggle("has-overflow", overflow);
            const atStart = strip.scrollLeft <= 0;
            const atEnd = strip.scrollLeft + strip.clientWidth >= strip.scrollWidth - 1;
            leftBtn.classList.toggle("is-disabled", atStart);
            rightBtn.classList.toggle("is-disabled", atEnd);
        }
        function refresh() { applyTabFit(); updateChevrons(); }

        leftBtn.addEventListener("click", (e) => {
            e.stopPropagation();
            strip.scrollBy({ left: -160, behavior: "smooth" });
        });
        rightBtn.addEventListener("click", (e) => {
            e.stopPropagation();
            strip.scrollBy({ left: 160, behavior: "smooth" });
        });
        strip.addEventListener("scroll", updateChevrons, { passive: true });

        // ResizeObserver fires on initial connect + every layout change
        // (panel splits, window resize, sidebar collapse, etc.).
        if (typeof ResizeObserver !== "undefined") {
            const ro = new ResizeObserver(refresh);
            ro.observe(bar);
        }
        // Initial pass once the bar is in the DOM. requestAnimationFrame
        // is enough since renderPanel synchronously appends to the grid.
        requestAnimationFrame(() => {
            refresh();
            // Scroll the active tab into view so it doesn't start
            // hidden behind a chevron after a re-render.
            const active = strip.querySelector(".ws-tab.is-active");
            if (active) {
                active.scrollIntoView({ block: "nearest", inline: "nearest" });
            }
        });
    }

    function renderTab(tab, slot, isActive) {
        // Tab structure per DESIGN.md: flat 32px rectangle,
        // 2px top stripe in the category color, label centered,
        // dirty dot OR × in the trailing slot. The × shows on
        // hover; the dirty dot shows otherwise. CSS owns the
        // swap via :hover.
        const btn = document.createElement("button");
        btn.type = "button";
        btn.className = "ws-tab" + (isActive ? " is-active" : "");
        btn.dataset.tabId = tab.id;
        // Category drives the top-stripe color via CSS attribute
        // selector. Surfaces map 1:1 to the four named categories
        // (notes / wiki / chat / shards).
        btn.dataset.category = tab.surface;

        const label = document.createElement("span");
        label.className = "ws-tab-label";
        label.textContent = tab.title;
        btn.appendChild(label);

        // Trailing slot: dirty dot + close, both rendered, CSS
        // toggles which is visible on hover.
        const trail = document.createElement("span");
        trail.className = "ws-tab-trail";
        if (tab.dirty) {
            const dot = document.createElement("span");
            dot.className = "ws-tab-dirty";
            trail.appendChild(dot);
        }
        const close = document.createElement("span");
        close.className = "ws-tab-close";
        close.textContent = "×";
        close.title = "Close tab";
        close.addEventListener("click", (e) => {
            e.stopPropagation();
            closeTab(tab.id);
        });
        trail.appendChild(close);
        btn.appendChild(trail);

        btn.addEventListener("click", () => switchTab(slot, tab.id));
        btn.addEventListener("auxclick", (e) => {
            // Middle-click = close; matches browser tab convention.
            if (e.button === 1) {
                e.preventDefault();
                closeTab(tab.id);
            }
        });

        // ── Tab context menu: right-click or touch long-press ──
        // HTML5 drag between panels is unreliable on iPad Safari,
        // so the menu is the touch-first way to move a tab. Both
        // inputs land on the same menu; showing it twice is benign
        // (the component replaces any open instance).
        btn.addEventListener("contextmenu", (e) => {
            e.preventDefault();
            e.stopPropagation();
            showTabMenu(e.clientX, e.clientY, tab.id, slot);
        });
        let pressTimer = null;
        let pressStart = null;
        let longPressed = false;
        const cancelPress = () => {
            if (pressTimer) {
                clearTimeout(pressTimer);
                pressTimer = null;
            }
        };
        btn.addEventListener("pointerdown", (e) => {
            if (e.pointerType !== "touch") return;
            pressStart = { x: e.clientX, y: e.clientY };
            longPressed = false;
            pressTimer = setTimeout(() => {
                pressTimer = null;
                longPressed = true;
                showTabMenu(pressStart.x, pressStart.y, tab.id, slot);
            }, 450);
        });
        btn.addEventListener("pointermove", (e) => {
            // A real drag/scroll intent cancels the press.
            if (!pressTimer || !pressStart) return;
            if (Math.abs(e.clientX - pressStart.x) > 10 ||
                Math.abs(e.clientY - pressStart.y) > 10) {
                cancelPress();
            }
        });
        btn.addEventListener("pointerup", cancelPress);
        btn.addEventListener("pointercancel", cancelPress);
        // Swallow the synthetic click that follows a long-press so
        // it doesn't switch tabs under the open menu. Capture phase
        // so it runs before the switchTab click handler above.
        btn.addEventListener("click", (e) => {
            if (longPressed) {
                e.preventDefault();
                e.stopPropagation();
                longPressed = false;
            }
        }, true);

        // ── Drag to move between panels ────────────────────
        btn.draggable = true;
        btn.addEventListener("dragstart", (e) => {
            e.dataTransfer.effectAllowed = "move";
            e.dataTransfer.setData("text/x-familiar-tab", tab.id);
            e.dataTransfer.setData("text/x-familiar-slot", slot);
            btn.classList.add("is-dragging");
        });
        btn.addEventListener("dragend", () => {
            btn.classList.remove("is-dragging");
            // Clean up any lingering drop indicators.
            for (const el of document.querySelectorAll(".ws-tab-drop-before, .ws-tab-drop-after, .ws-tab-bar-drop-target")) {
                el.classList.remove("ws-tab-drop-before", "ws-tab-drop-after", "ws-tab-bar-drop-target");
            }
        });

        return btn;
    }

    // Positional panel names for the tab context menu, per layout —
    // "Move to right panel" beats "Move to panel B".
    const SLOT_NAMES = {
        "single":    { A: "panel" },
        "split-2x1": { A: "left panel", B: "right panel" },
        "split-1x2": { A: "top panel", B: "bottom panel" },
        "t-top":     { A: "top panel", B: "bottom-left panel", C: "bottom-right panel" },
    };

    // showTabMenu paints the tab context menu (right-click / touch
    // long-press): one "Move to <panel>" entry per other slot in the
    // current layout, plus Close. Reuses the sidebar context-menu
    // component — same look, same dismiss rules.
    function showTabMenu(x, y, tabId, slot) {
        const items = [];
        const names = SLOT_NAMES[state.layout] || {};
        for (const s of LAYOUTS[state.layout].slots) {
            if (s === slot) continue;
            items.push({
                label: "Move to " + (names[s] || "panel " + s),
                onClick: () => moveTab(tabId, s, null),
            });
        }
        if (items.length) items.push({ divider: true });
        items.push({ label: "Close tab", danger: true, onClick: () => closeTab(tabId) });
        showSidebarContextMenu(x, y, items);
    }

    function renderTabContent(host, tab) {
        const renderer = surfaceRenderers[tab.surface];
        if (renderer) {
            try {
                renderer(host, tab);
                return;
            } catch (e) {
                console.error("workspace: surface renderer error", tab.surface, e);
                host.innerHTML = '<div class="ws-error">Surface renderer threw: ' + escapeHTML(String(e)) + '</div>';
                return;
            }
        }
        // Default: placeholder text from SURFACES.
        const surf = SURFACES[tab.surface];
        host.innerHTML = '';
        const ph = document.createElement("div");
        ph.className = "ws-placeholder";
        const title = document.createElement("h2");
        title.className = "ws-placeholder-title";
        title.textContent = tab.title;
        ph.appendChild(title);
        const body = document.createElement("p");
        body.className = "ws-placeholder-body";
        body.textContent = (surf && surf.placeholder) || "(no content)";
        ph.appendChild(body);
        host.appendChild(ph);
    }

    function escapeHTML(s) {
        return s
            .replace(/&/g, "&amp;").replace(/</g, "&lt;")
            .replace(/>/g, "&gt;").replace(/"/g, "&quot;");
    }

    // ── Resize handles ─────────────────────────────────────────

    // attachResizeHandles inspects the layout and inserts the
    // appropriate row/column drag handles. The handles use pointer
    // events for cross-input compatibility (touch + mouse + pen).
    // Drag updates the corresponding trackSizes entry, which
    // propagates to the grid via the CSS custom properties.
    //
    // Handles are layout-specific:
    //   split-2x1, t-top (bottom row), t-bottom (top row), quad   → vertical handle between cols
    //   stacked-1x2, t-left (right col), t-right (left col), quad → horizontal handle between rows
    //   t-top, t-bottom → horizontal handle between full and split rows
    //   t-left, t-right → vertical handle between full and split cols
    //
    // Rather than enumerate exhaustively, we always place a single
    // vertical and a single horizontal handle inside the grid
    // container; the layout's grid-template-areas decides which
    // ones are visible (handles assigned to areas not in the
    // layout collapse to zero size).
    function attachResizeHandles(host, layout) {
        const colHandle = document.createElement("div");
        colHandle.className = "ws-resize ws-resize-col";
        colHandle.style.gridArea = "rc";
        colHandle.addEventListener("pointerdown", (e) => beginColResize(e, host));
        host.appendChild(colHandle);

        const rowHandle = document.createElement("div");
        rowHandle.className = "ws-resize ws-resize-row";
        rowHandle.style.gridArea = "rr";
        rowHandle.addEventListener("pointerdown", (e) => beginRowResize(e, host));
        host.appendChild(rowHandle);
    }

    function beginColResize(e, host) {
        e.preventDefault();
        const rect = host.getBoundingClientRect();
        const startX = e.clientX;
        const startCol1 = state.trackSizes.col1;
        const startCol2 = state.trackSizes.col2;
        const total = startCol1 + startCol2;

        function onMove(ev) {
            const dx = ev.clientX - startX;
            const frac = clamp((startCol1 * rect.width + dx) / rect.width, MIN_TRACK_FRACTION, total - MIN_TRACK_FRACTION);
            state.trackSizes.col1 = frac;
            state.trackSizes.col2 = total - frac;
            host.style.setProperty("--col-1-fr", frac + "fr");
            host.style.setProperty("--col-2-fr", (total - frac) + "fr");
        }
        function onUp() {
            window.removeEventListener("pointermove", onMove);
            window.removeEventListener("pointerup", onUp);
            saveState();
        }
        window.addEventListener("pointermove", onMove);
        window.addEventListener("pointerup", onUp);
    }

    function beginRowResize(e, host) {
        e.preventDefault();
        const rect = host.getBoundingClientRect();
        const startY = e.clientY;
        const startRow1 = state.trackSizes.row1;
        const startRow2 = state.trackSizes.row2;
        const total = startRow1 + startRow2;

        function onMove(ev) {
            const dy = ev.clientY - startY;
            const frac = clamp((startRow1 * rect.height + dy) / rect.height, MIN_TRACK_FRACTION, total - MIN_TRACK_FRACTION);
            state.trackSizes.row1 = frac;
            state.trackSizes.row2 = total - frac;
            host.style.setProperty("--row-1-fr", frac + "fr");
            host.style.setProperty("--row-2-fr", (total - frac) + "fr");
        }
        function onUp() {
            window.removeEventListener("pointermove", onMove);
            window.removeEventListener("pointerup", onUp);
            saveState();
        }
        window.addEventListener("pointermove", onMove);
        window.addEventListener("pointerup", onUp);
    }

    function clamp(x, lo, hi) {
        return Math.max(lo, Math.min(hi, x));
    }

    // ── Wiring ─────────────────────────────────────────────────

    // ── Sidebar category children (DESIGN.md) ─────
    //
    // Clicking a category row toggles its expanded state. When
    // expanded, children (recent docs of that category) are
    // fetched on demand and rendered inline below the row.
    // Clicking a child dispatches a `familiar:openDoc` custom
    // event that the surface modules (chat.js, notes.js) consume
    // to focus the right tab + load the doc.
    //
    // State is volatile: which categories are expanded, and the
    // cached children list. Children refetch on each expand (cheap
    // — small lists). Persisted across page reload via workspace
    // state's `expandedCategories` Set.

    const sidebarCatState = {
        expanded: new Set(),
        // Cached child lists keyed by category. Refetched on each
        // expand to keep the list fresh; old caches stick around
        // for paint until the new data arrives.
        cache: {},
    };

    async function fetchCategoryChildren(category) {
        const helpers = window.familiarAppHelpers;
        if (!helpers || !helpers.apiJSON) return { kind: "flat", items: [] };
        try {
            if (category === "notes") {
                // Pages double as folders — return the full personal-
                // book page set with parent_id/sort_order so the tree
                // renderer can build the hierarchy in one pass. 200
                // is a generous upper bound for a personal notebook.
                const resp = await helpers.apiJSON("/console/api/books/personal/pages?limit=200");
                return {
                    kind: "tree",
                    items: ((resp && resp.items) || []).map((n) => ({
                        id: n.id,
                        title: n.title || "Untitled",
                        meta: agoOrDateStr(n.updated_at),
                        parent_id: n.parent_id || null,
                        sort_order: n.sort_order || 0,
                    })),
                };
            }
            if (category === "shards") {
                // Every active shard becomes a child. Chat-enabled
                // ones open a conversation bound to the shard
                // (SKILL-PACKAGES-SPEC Phase 1); the rest open the
                // Shards panel so an api/console-only shard is still
                // reachable from the rail. chatEnabled rides along so
                // the click handler can branch.
                const resp = await helpers.apiJSON("/console/api/shards");
                return {
                    kind: "flat",
                    items: ((resp && resp.items) || [])
                        .filter((sh) => sh.active !== false)
                        .map((sh) => ({
                            id: sh.id,
                            title: sh.name || sh.id,
                            meta: sh.chat_enabled === false
                                ? "no chat"
                                : (sh.persistence === "ephemeral" ? "ephemeral" : "persistent"),
                            chatEnabled: sh.chat_enabled !== false,
                        })),
                };
            }
            if (category === "scheduled") {
                // Scheduled actions as children: a click opens the
                // Actions panel. (The list endpoint is owner-scoped;
                // admins see every action.)
                const resp = await helpers.apiJSON("/console/api/actions");
                return {
                    kind: "flat",
                    items: ((resp && resp.items) || []).map((a) => ({
                        id: a.id,
                        title: a.name || "Untitled action",
                        meta: a.enabled === false ? "paused" : (a.last_status || ""),
                    })),
                };
            }
            if (category === "chat") {
                // Conversations group under chat folders ("projects").
                // Fetch both in parallel; a missing folders endpoint
                // (older deploys) degrades to all-uncategorized.
                const [foldersResp, convsResp] = await Promise.all([
                    helpers.apiJSON("/console/api/chat/folders").catch(() => ({ items: [] })),
                    helpers.apiJSON("/console/api/conversations?limit=50"),
                ]);
                return {
                    kind: "folder-grouped",
                    folders: ((foldersResp && foldersResp.items) || []).map((f) => ({
                        id: f.id,
                        name: f.name,
                        sort_order: f.sort_order || 0,
                    })),
                    items: ((convsResp && convsResp.items) || []).map((c) => ({
                        id: c.id,
                        title: c.title || "New conversation",
                        meta: agoOrDateStr(c.updated_at),
                        folder_id: c.folder_id || null,
                    })),
                };
            }
            if (category === "wiki") {
                // Wiki children at the rail level are books the user
                // is a member of. Each book is treated as a foldable
                // "folder" — expanding it lazily fetches that book's
                // pages and renders them as a nested tree under the
                // book row. See renderWikiChildren below.
                const resp = await helpers.apiJSON("/console/api/books");
                return {
                    kind: "wiki",
                    books: ((resp && resp.items) || []).map((b) => ({
                        id: b.slug, title: b.name || b.slug,
                        meta: agoOrDateStr(b.updated_at),
                    })),
                };
            }
            return { kind: "flat", items: [] };
        } catch (e) {
            return { kind: "flat", items: [] };
        }
    }

    function agoOrDateStr(iso) {
        if (!iso) return "";
        const t = new Date(iso).getTime();
        if (!t) return "";
        const diff = Date.now() - t;
        if (diff < 60_000) return "now";
        if (diff < 3_600_000) return Math.round(diff / 60_000) + "m";
        if (diff < 86_400_000) return Math.round(diff / 3_600_000) + "h";
        if (diff < 7 * 86_400_000) return Math.round(diff / 86_400_000) + "d";
        return new Date(t).toLocaleDateString(undefined, { month: "short", day: "numeric" });
    }

    async function toggleCategoryExpand(catRow) {
        const category = catRow.dataset.category;
        if (!category) return;
        const wasExpanded = sidebarCatState.expanded.has(category);
        if (wasExpanded) {
            sidebarCatState.expanded.delete(category);
            catRow.classList.remove("is-expanded");
            const existing = catRow.nextElementSibling;
            if (existing && existing.classList.contains("sidebar-children")) {
                existing.remove();
            }
            return;
        }
        sidebarCatState.expanded.add(category);
        catRow.classList.add("is-expanded");

        // Render a placeholder children list immediately so the
        // sidebar feels responsive; replace with real data when
        // the fetch resolves.
        const childList = document.createElement("div");
        childList.className = "sidebar-children sidebar-cat-" + category;
        childList.dataset.category = category;
        childList.innerHTML = '<div class="sidebar-children-loading">Loading…</div>';
        catRow.insertAdjacentElement("afterend", childList);

        const items = await fetchCategoryChildren(category);
        sidebarCatState.cache[category] = items;
        renderCategoryChildren(childList, category, items);
    }

    // Per-category sets of "currently expanded" identifiers — used
    // by the tree (notes) and folder-grouped (chat) renderers to
    // remember which nodes are open across a refresh. Keys are
    // category names; values are Sets of ids (page ids or folder
    // ids respectively). The wiki tree reuses sidebarTreeExpanded
    // with category "wiki" — book slugs and page UUIDs share that
    // set without colliding.
    const sidebarTreeExpanded = new Map();
    const sidebarFolderExpanded = new Map();

    // Lazy cache of pages per wiki book. Keyed by book slug; value
    // is the array returned by /console/api/books/{slug}/pages. The
    // first expansion of a book populates it; subsequent toggles
    // re-use the cache so collapse/re-expand is instant.
    const sidebarWikiPagesCache = new Map();

    async function ensureWikiPagesFetched(bookSlug) {
        if (sidebarWikiPagesCache.has(bookSlug)) return;
        // `helpers` is function-local everywhere in this file — grab
        // our own. Referencing a sibling scope's const here threw a
        // ReferenceError that the catch below ate, which cached []
        // and rendered every book as "Empty" (2026-06-12 bug).
        const helpers = window.familiarAppHelpers;
        if (!helpers || !helpers.apiJSON) return;
        try {
            const resp = await helpers.apiJSON(
                "/console/api/books/" + encodeURIComponent(bookSlug) + "/pages?limit=200"
            );
            const items = (resp && resp.items) || [];
            sidebarWikiPagesCache.set(bookSlug, items);
            // Diagnostic: when a book renders as "Empty" but the
            // user is sure pages exist, the console makes the
            // failure mode obvious (zero rows vs fetch error vs
            // shape mismatch). Cheap to keep.
            if (items.length === 0) {
                console.info("[wiki sidebar] no pages for book", bookSlug,
                    "(response:", resp, ")");
            }
        } catch (e) {
            console.warn("[wiki sidebar] pages fetch failed for", bookSlug, ":", e);
            sidebarWikiPagesCache.set(bookSlug, []);
        }
    }

    function getSetFor(map, category) {
        let s = map.get(category);
        if (!s) {
            s = new Set();
            map.set(category, s);
        }
        return s;
    }

    function renderCategoryChildren(host, category, data) {
        host.innerHTML = "";
        // Back-compat: callers that still pass a bare array get the
        // flat renderer.
        if (Array.isArray(data)) {
            data = { kind: "flat", items: data };
        }
        const kind = (data && data.kind) || "flat";
        if (kind === "tree") {
            renderTreeChildren(host, category, data.items || []);
        } else if (kind === "wiki") {
            renderWikiChildren(host, data.books || []);
        } else if (kind === "folder-grouped") {
            renderFolderGroupedChildren(host, category, data.folders || [], data.items || []);
        } else {
            renderFlatChildren(host, category, data.items || []);
        }
    }

    // renderWikiChildren paints the rail's wiki section: one row
    // per book, each foldable. Expanding a book row reveals that
    // book's pages as an indented tree (parent_id-bucketed, same
    // shape as the notes tree). Empty books still get a caret so
    // every book has the same affordance — clicking it shows
    // "Empty" until a page is added. Pages are lazy-fetched on
    // first expand and cached in sidebarWikiPagesCache.
    function renderWikiChildren(host, books) {
        if (books.length === 0) {
            const empty = document.createElement("div");
            empty.className = "sidebar-children-empty";
            empty.textContent = "No books yet.";
            host.appendChild(empty);
            return;
        }
        const expanded = getSetFor(sidebarTreeExpanded, "wiki");
        for (const book of books) {
            // hasChildren=true unconditionally — wiki books are
            // foldable by default (see comment above).
            host.appendChild(buildChildRow("wiki", book, 0, true));
            if (!expanded.has(book.id)) continue;
            const pages = sidebarWikiPagesCache.get(book.id);
            if (pages === undefined) {
                // Cache miss — first expand (or post-invalidation
                // re-render). Kick off the fetch; another refresh
                // fires when pages arrive and that render swaps
                // "Loading…" for the real tree.
                ensureWikiPagesFetched(book.id).then(refreshSidebarChildren);
                const loading = document.createElement("div");
                loading.className = "sidebar-children-loading";
                loading.textContent = "Loading…";
                host.appendChild(loading);
                continue;
            }
            if (pages.length === 0) {
                const empty = document.createElement("div");
                empty.className = "sidebar-children-empty sidebar-folder-empty";
                empty.textContent = "Empty";
                host.appendChild(empty);
                continue;
            }
            appendWikiPagesTree(host, book.id, pages, 1);
        }
    }

    // parentsFirst orders a sibling group so items that have their own
    // children float above leaf items, keeping the server sort_order
    // within each group (Array.sort is stable). Applied per level in
    // the sidebar trees so the expandable parents are easy to find at a
    // glance instead of being scattered among leaves.
    function parentsFirst(kids, childrenOf) {
        return kids.slice().sort((a, b) => {
            const aRank = (childrenOf.get(a.id) || []).length > 0 ? 0 : 1;
            const bRank = (childrenOf.get(b.id) || []).length > 0 ? 0 : 1;
            return aRank - bRank;
        });
    }

    // appendWikiPagesTree walks one book's pages and appends rows
    // to host starting at startDepth. Mirrors renderTreeChildren's
    // parent_id-bucketing algorithm; the only difference is each
    // row is tagged with bookSlug so buildChildRow's click handler
    // routes to the page (not a top-level book open).
    function appendWikiPagesTree(host, bookSlug, pages, startDepth) {
        const childrenOf = new Map();
        for (const p of pages) {
            const key = p.parent_id || "";
            if (!childrenOf.has(key)) childrenOf.set(key, []);
            childrenOf.get(key).push(p);
        }
        const expanded = getSetFor(sidebarTreeExpanded, "wiki");
        const walk = (parentKey, depth) => {
            const kids = parentsFirst(childrenOf.get(parentKey) || [], childrenOf);
            for (const kid of kids) {
                const hasKids = (childrenOf.get(kid.id) || []).length > 0;
                const item = {
                    id: kid.id,
                    title: kid.title || "(untitled)",
                    parent_id: kid.parent_id || null,
                    meta: agoOrDateStr(kid.updated_at),
                    bookSlug,
                };
                host.appendChild(buildChildRow("wiki", item, depth, hasKids));
                if (hasKids && expanded.has(kid.id)) walk(kid.id, depth + 1);
            }
        };
        walk("", startDepth);
    }

    function renderFlatChildren(host, category, items) {
        if (items.length === 0) {
            const empty = document.createElement("div");
            empty.className = "sidebar-children-empty";
            empty.textContent = category === "wiki" ? "No books yet." : "Nothing yet.";
            host.appendChild(empty);
            return;
        }
        for (const it of items) {
            host.appendChild(buildChildRow(category, it, 0, false));
        }
    }

    function buildChildRow(category, item, depth, hasChildren) {
        const row = document.createElement("a");
        row.href = "#";
        row.className = "sidebar-child";
        row.dataset.docId = item.id;
        row.dataset.category = category;
        if (depth > 0) row.dataset.depth = String(depth);
        // CSS reads --depth to size the indent; capping at 6 levels
        // matches the visible padding budget on a 260px sidebar.
        row.style.setProperty("--depth", String(Math.min(depth, 6)));

        // Caret slot — always present so titles align across rows
        // whether or not the page has children.
        const caret = document.createElement("span");
        caret.className = "sidebar-tree-caret";
        caret.setAttribute("aria-hidden", "true");
        if (hasChildren) {
            const isOpen = getSetFor(sidebarTreeExpanded, category).has(item.id);
            // Same chevron as the category rows: › that rotates 90°
            // when open. The old ▸/▾ pair read as a dot at row size.
            caret.textContent = "›";
            if (isOpen) caret.classList.add("is-open");
            caret.classList.add("has-children");
            caret.addEventListener("click", (e) => {
                e.preventDefault();
                e.stopPropagation();
                const set = getSetFor(sidebarTreeExpanded, category);
                if (set.has(item.id)) set.delete(item.id);
                else set.add(item.id);
                // Wiki books fetch their pages lazily on demand —
                // renderWikiChildren spots the cache miss and kicks
                // off the fetch; we don't need to do it here.
                refreshSidebarChildren();
            });
        }
        row.appendChild(caret);

        const title = document.createElement("span");
        title.className = "sidebar-child-title";
        title.textContent = item.title;
        const meta = document.createElement("span");
        meta.className = "sidebar-child-meta";
        meta.textContent = item.meta || "";
        row.append(title, meta);
        row.addEventListener("click", (e) => {
            e.preventDefault();
            e.stopPropagation();
            if (category === "wiki" && item.bookSlug) {
                // Wiki page row — focus the parent book and load
                // this specific page (mirrors how pin clicks reach
                // the wiki surface).
                openDocFromSidebar("wiki", item.bookSlug, item.title, { pageId: item.id });
            } else if (category === "shards") {
                if (item.chatEnabled === false) {
                    // No chat surface for this shard — open the panel
                    // so it's still reachable from the rail.
                    if (window.appSwitchPanel) window.appSwitchPanel("shards");
                } else {
                    // A shard child opens a chat bound to that shard,
                    // not the admin panel — the panel stays one click
                    // away on the category row itself.
                    if (window.appSwitchPanel) window.appSwitchPanel("workspace");
                    const nav = focusSurface("chat");
                    window.dispatchEvent(new CustomEvent("familiar:openDoc", {
                        detail: {
                            surface: "chat", shard_id: item.id, shard_name: item.title,
                            tabId: nav && nav.tabId,
                        },
                    }));
                }
            } else if (category === "scheduled") {
                // Actions are panel-managed, not workspace docs —
                // a child click opens the Actions panel.
                if (window.appSwitchPanel) window.appSwitchPanel("scheduled");
            } else {
                openDocFromSidebar(category, item.id, item.title);
            }
        });

        // ── Context menu ──────────────────────────────────────────
        // Right-click any tree row to get scope-appropriate actions.
        // Today the menu is intentionally small — just the un-nest
        // operations. Future work (rename, delete, pin, share) will
        // hang off the same menu so the right-click vocabulary is
        // one place.
        row.addEventListener("contextmenu", (e) => {
            const items = [];
            if (category === "notes" && item.parent_id) {
                items.push({
                    label: "Move to top level",
                    onClick: () => moveNote(item.id, ""),
                });
            }
            if (category === "wiki" && item.bookSlug && item.parent_id) {
                items.push({
                    label: "Move to top level",
                    onClick: () => moveWikiPage(item.bookSlug, item.id, ""),
                });
            }
            if (category === "chat" && item.folder_id) {
                items.push({
                    label: "Move out of folder",
                    onClick: () => moveChat(item.id, ""),
                });
            }
            if (items.length === 0) return; // let the native menu show
            e.preventDefault();
            showSidebarContextMenu(e.clientX, e.clientY, items);
        });

        // ── Drag-and-drop ─────────────────────────────────────────
        // The dataTransfer type encodes the category so dragover
        // targets can accept/reject without reading the payload
        // (browsers don't expose getData during dragover for
        // security; only types[]). Two categories carry drag /
        // drop wiring today: notes (reparent under another page)
        // and chat (move into a folder).
        if (category === "notes" || category === "chat") {
            row.draggable = true;
            row.addEventListener("dragstart", (e) => {
                e.dataTransfer.setData(dndType(category), JSON.stringify({ id: item.id }));
                e.dataTransfer.effectAllowed = "move";
                row.classList.add("is-dragging");
            });
            row.addEventListener("dragend", () => row.classList.remove("is-dragging"));
        }
        if (category === "notes") {
            // Drop another note here → that note becomes a child
            // of this one. Self-drops are rejected at drop time.
            row.addEventListener("dragover", (e) => {
                if (!e.dataTransfer.types.includes(dndType("notes"))) return;
                e.preventDefault();
                e.dataTransfer.dropEffect = "move";
                row.classList.add("is-drop-target");
            });
            row.addEventListener("dragleave", () => row.classList.remove("is-drop-target"));
            row.addEventListener("drop", (e) => {
                row.classList.remove("is-drop-target");
                if (!e.dataTransfer.types.includes(dndType("notes"))) return;
                e.preventDefault();
                const payload = parseDndPayload(e.dataTransfer, "notes");
                if (!payload || payload.id === item.id) return;
                moveNote(payload.id, item.id);
            });
        }

        // Wiki pages reparent the same way notes do — same table,
        // same move endpoint, per-book. The dataTransfer TYPE carries
        // the book slug (types are all dragover can see), so drops
        // only light up within the page's own book.
        if (category === "wiki" && item.bookSlug) {
            const wikiType = dndType("wiki--" + item.bookSlug);
            row.draggable = true;
            row.addEventListener("dragstart", (e) => {
                e.dataTransfer.setData(wikiType, JSON.stringify({ id: item.id }));
                e.dataTransfer.effectAllowed = "move";
                row.classList.add("is-dragging");
            });
            row.addEventListener("dragend", () => row.classList.remove("is-dragging"));
            row.addEventListener("dragover", (e) => {
                if (!e.dataTransfer.types.includes(wikiType)) return;
                e.preventDefault();
                e.dataTransfer.dropEffect = "move";
                row.classList.add("is-drop-target");
            });
            row.addEventListener("dragleave", () => row.classList.remove("is-drop-target"));
            row.addEventListener("drop", (e) => {
                row.classList.remove("is-drop-target");
                if (!e.dataTransfer.types.includes(wikiType)) return;
                e.preventDefault();
                const payload = parseDndPayload(e.dataTransfer, "wiki--" + item.bookSlug);
                if (!payload || payload.id === item.id) return;
                moveWikiPage(item.bookSlug, payload.id, item.id);
            });
        }
        // A BOOK row accepts drops of its own pages → top level
        // (the book is the tree root, mirroring the notes "Move to
        // top level" affordance as a drag).
        if (category === "wiki" && !item.bookSlug) {
            const wikiType = dndType("wiki--" + item.id);
            row.addEventListener("dragover", (e) => {
                if (!e.dataTransfer.types.includes(wikiType)) return;
                e.preventDefault();
                e.dataTransfer.dropEffect = "move";
                row.classList.add("is-drop-target");
            });
            row.addEventListener("dragleave", () => row.classList.remove("is-drop-target"));
            row.addEventListener("drop", (e) => {
                row.classList.remove("is-drop-target");
                if (!e.dataTransfer.types.includes(wikiType)) return;
                e.preventDefault();
                const payload = parseDndPayload(e.dataTransfer, "wiki--" + item.id);
                if (!payload) return;
                moveWikiPage(item.id, payload.id, "");
            });
        }
        return row;
    }

    function renderTreeChildren(host, category, items) {
        if (items.length === 0) {
            const empty = document.createElement("div");
            empty.className = "sidebar-children-empty";
            empty.textContent = "Nothing yet.";
            host.appendChild(empty);
            return;
        }
        // Bucket by parent_id (null = top-level) preserving the
        // server's sort_order. childrenOf maps "" → roots, "id" →
        // direct children of that id.
        const childrenOf = new Map();
        for (const it of items) {
            const key = it.parent_id || "";
            if (!childrenOf.has(key)) childrenOf.set(key, []);
            childrenOf.get(key).push(it);
        }
        const expanded = getSetFor(sidebarTreeExpanded, category);
        const walk = (parentKey, depth) => {
            const kids = parentsFirst(childrenOf.get(parentKey) || [], childrenOf);
            for (const kid of kids) {
                const hasKids = (childrenOf.get(kid.id) || []).length > 0;
                host.appendChild(buildChildRow(category, kid, depth, hasKids));
                if (hasKids && expanded.has(kid.id)) walk(kid.id, depth + 1);
            }
        };
        walk("", 0);
    }

    function renderFolderGroupedChildren(host, category, folders, items) {
        if (folders.length === 0 && items.length === 0) {
            const empty = document.createElement("div");
            empty.className = "sidebar-children-empty";
            empty.textContent = "Nothing yet.";
            host.appendChild(empty);
            return;
        }
        const expanded = getSetFor(sidebarFolderExpanded, category);
        // Bucket conversations by folder_id; null bucket is the
        // "Uncategorized" tail section.
        const byFolder = new Map();
        const uncategorized = [];
        for (const it of items) {
            if (!it.folder_id) {
                uncategorized.push(it);
            } else {
                if (!byFolder.has(it.folder_id)) byFolder.set(it.folder_id, []);
                byFolder.get(it.folder_id).push(it);
            }
        }
        const sortedFolders = folders.slice().sort((a, b) =>
            (a.sort_order || 0) - (b.sort_order || 0) || a.name.localeCompare(b.name));

        const buildFolderHeader = (folder) => {
            const header = document.createElement("div");
            header.className = "sidebar-folder-header";
            header.dataset.folderId = folder.id;
            const caret = document.createElement("span");
            caret.className = "sidebar-folder-caret";
            const isOpen = expanded.has(folder.id);
            caret.textContent = isOpen ? "▾" : "▸";
            const name = document.createElement("span");
            name.className = "sidebar-folder-name";
            name.textContent = folder.name;
            const count = document.createElement("span");
            count.className = "sidebar-folder-count";
            const n = (byFolder.get(folder.id) || []).length;
            if (n > 0) count.textContent = String(n);
            header.append(caret, name, count);
            header.addEventListener("click", (e) => {
                e.preventDefault();
                e.stopPropagation();
                if (expanded.has(folder.id)) expanded.delete(folder.id);
                else expanded.add(folder.id);
                refreshSidebarChildren();
            });
            // Drop a chat onto this folder header → move it in.
            // Sentinel "_unfiled" id means uncategorized (clears
            // folder_id).
            header.addEventListener("dragover", (e) => {
                if (!e.dataTransfer.types.includes(dndType("chat"))) return;
                e.preventDefault();
                e.dataTransfer.dropEffect = "move";
                header.classList.add("is-drop-target");
            });
            header.addEventListener("dragleave", () =>
                header.classList.remove("is-drop-target"));
            header.addEventListener("drop", (e) => {
                header.classList.remove("is-drop-target");
                if (!e.dataTransfer.types.includes(dndType("chat"))) return;
                e.preventDefault();
                const payload = parseDndPayload(e.dataTransfer, "chat");
                if (!payload) return;
                const folderID = folder.id === "_unfiled" ? "" : folder.id;
                moveChat(payload.id, folderID);
            });
            return header;
        };

        for (const folder of sortedFolders) {
            host.appendChild(buildFolderHeader(folder));
            if (!expanded.has(folder.id)) continue;
            const convs = byFolder.get(folder.id) || [];
            if (convs.length === 0) {
                const empty = document.createElement("div");
                empty.className = "sidebar-children-empty sidebar-folder-empty";
                empty.textContent = "Empty";
                host.appendChild(empty);
                continue;
            }
            for (const c of convs) host.appendChild(buildChildRow(category, c, 1, false));
        }

        // Uncategorized — shown only when something lives there OR
        // there are no folders yet. Keeps the rail clean.
        if (uncategorized.length > 0 || sortedFolders.length === 0) {
            if (sortedFolders.length > 0) {
                const sep = document.createElement("div");
                sep.className = "sidebar-folder-header sidebar-folder-uncategorized";
                const caret = document.createElement("span");
                caret.className = "sidebar-folder-caret";
                // The uncategorized "folder" uses the sentinel "_unfiled"
                // key in the expanded set so its open/closed state
                // persists across refreshes alongside real folders.
                const isOpen = expanded.has("_unfiled") || sortedFolders.length === 0;
                caret.textContent = isOpen ? "▾" : "▸";
                const name = document.createElement("span");
                name.className = "sidebar-folder-name";
                name.textContent = "Uncategorized";
                const count = document.createElement("span");
                count.className = "sidebar-folder-count";
                if (uncategorized.length > 0) count.textContent = String(uncategorized.length);
                sep.append(caret, name, count);
                sep.addEventListener("click", (e) => {
                    e.preventDefault();
                    e.stopPropagation();
                    if (expanded.has("_unfiled")) expanded.delete("_unfiled");
                    else expanded.add("_unfiled");
                    refreshSidebarChildren();
                });
                host.appendChild(sep);
                if (!isOpen) return;
            }
            for (const c of uncategorized) host.appendChild(buildChildRow(category, c, 1, false));
        }
        // The "+ New …" affordance lives on the category header row
        // (.sidebar-cat-new next to the chevron), not at the bottom
        // of the expanded child list.
    }

    // Refresh sidebar children for all currently-expanded categories.
    // Called when titles change or docs are created/deleted so the
    // sidebar list stays current without a page refresh.
    async function refreshSidebarChildren() {
        for (const category of sidebarCatState.expanded) {
            const childList = document.querySelector('.sidebar-children[data-category="' + category + '"]');
            if (!childList) continue;
            const items = await fetchCategoryChildren(category);
            sidebarCatState.cache[category] = items;
            renderCategoryChildren(childList, category, items);
        }
    }

    // ── Sidebar context menu ────────────────────────────────────
    //
    // One floating menu, summoned by right-click on a sidebar row.
    // The caller passes items as [{label, onClick, danger?}]; a
    // {divider:true} entry renders a hairline separator. Outside
    // click or Escape dismisses. Position clamps to the viewport.

    function showSidebarContextMenu(x, y, items) {
        document.querySelectorAll(".sidebar-ctxmenu").forEach((n) => n.remove());
        const menu = document.createElement("div");
        menu.className = "sidebar-ctxmenu";
        menu.style.left = x + "px";
        menu.style.top = y + "px";
        for (const it of items) {
            if (it.divider) {
                const d = document.createElement("div");
                d.className = "sidebar-ctxmenu-divider";
                menu.appendChild(d);
                continue;
            }
            const btn = document.createElement("button");
            btn.type = "button";
            btn.className = "sidebar-ctxmenu-item";
            if (it.danger) btn.classList.add("is-danger");
            btn.textContent = it.label;
            btn.addEventListener("click", () => {
                it.onClick();
                menu.remove();
            });
            menu.appendChild(btn);
        }
        document.body.appendChild(menu);
        // Clamp into viewport — re-measure after attach.
        const r = menu.getBoundingClientRect();
        if (r.right > window.innerWidth) {
            menu.style.left = Math.max(4, window.innerWidth - r.width - 4) + "px";
        }
        if (r.bottom > window.innerHeight) {
            menu.style.top = Math.max(4, window.innerHeight - r.height - 4) + "px";
        }
        // Dismiss on outside click or Escape. Defer the click handler
        // attach so the right-click that opened the menu doesn't
        // immediately close it.
        const dismiss = (ev) => {
            if (ev.type === "keydown" && ev.key !== "Escape") return;
            if (ev.type === "click" && menu.contains(ev.target)) return;
            menu.remove();
            document.removeEventListener("click", dismiss);
            document.removeEventListener("keydown", dismiss);
            document.removeEventListener("contextmenu", dismiss);
        };
        setTimeout(() => {
            document.addEventListener("click", dismiss);
            document.addEventListener("keydown", dismiss);
            document.addEventListener("contextmenu", dismiss);
        }, 0);
    }

    // ── Sidebar drag-and-drop ───────────────────────────────────
    //
    // Two operations supported today; both desktop-only (mobile UX
    // is deferred). Notes drag onto another note to reparent;
    // notes drag onto the Notes category header to unparent.
    // Chats drag onto a folder header to move into that folder
    // (sentinel "_unfiled" header clears folder_id).
    //
    // Reordering siblings via DnD isn't wired yet — the backend
    // accepts sort_order on moves, the frontend just doesn't
    // compute it from drop position. Future work.

    function dndType(category) {
        return "application/x-familiar-" + category;
    }

    function parseDndPayload(dt, category) {
        try {
            const raw = dt.getData(dndType(category));
            if (!raw) return null;
            return JSON.parse(raw);
        } catch (e) {
            return null;
        }
    }

    // moveWikiPage reparents a page within its book via the same
    // /move endpoint moveNote uses (notes ARE personal-book pages —
    // one table, one contract). The book's sidebar page cache is
    // stale after a move, so it's dropped before the re-render.
    async function moveWikiPage(bookSlug, pageID, parentID) {
        const helpers = window.familiarAppHelpers;
        if (!helpers || !helpers.apiJSON) return;
        try {
            await helpers.apiJSON(
                "/console/api/books/" + encodeURIComponent(bookSlug) +
                    "/page-by-id/" + encodeURIComponent(pageID) + "/move",
                {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ parent_id: parentID }),
                },
            );
            sidebarWikiPagesCache.delete(bookSlug);
            refreshSidebarChildren();
            // Wiki shells listen for this and refresh their page
            // lists (same signal AI page edits use).
            window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
        } catch (e) {
            if (helpers.toast) helpers.toast("Couldn't move page: " + (e.message || e), "error");
        }
    }

    async function moveNote(noteID, parentID) {
        const helpers = window.familiarAppHelpers;
        if (!helpers || !helpers.apiJSON) return;
        try {
            await helpers.apiJSON(
                "/console/api/books/personal/page-by-id/" +
                    encodeURIComponent(noteID) + "/move",
                {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ parent_id: parentID }),
                },
            );
            refreshSidebarChildren();
            // Tell in-surface trees (notes.js) to reload too.
            window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
        } catch (e) {
            if (helpers.toast) helpers.toast("Couldn't move note: " + (e.message || e), "error");
        }
    }

    async function moveChat(convID, folderID) {
        const helpers = window.familiarAppHelpers;
        if (!helpers || !helpers.apiJSON) return;
        try {
            await helpers.apiJSON(
                "/console/api/conversations/" +
                    encodeURIComponent(convID) + "/move",
                {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ folder_id: folderID }),
                },
            );
            refreshSidebarChildren();
        } catch (e) {
            if (helpers.toast) helpers.toast("Couldn't move chat: " + (e.message || e), "error");
        }
    }

    // Drop a note onto the static Notes category row to unparent
    // it back to top-level. Document-level listeners so we don't
    // have to re-attach on every sidebar redraw.
    document.addEventListener("dragover", (e) => {
        if (!e.dataTransfer || !e.dataTransfer.types) return;
        if (!e.dataTransfer.types.includes(dndType("notes"))) return;
        const target = e.target.closest('a.sidebar-cat[data-category="notes"]');
        if (!target) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = "move";
        target.classList.add("is-drop-target");
    });
    document.addEventListener("dragleave", (e) => {
        const target = e.target.closest && e.target.closest('a.sidebar-cat[data-category="notes"]');
        if (target) target.classList.remove("is-drop-target");
    });
    document.addEventListener("drop", (e) => {
        if (!e.dataTransfer || !e.dataTransfer.types) return;
        if (!e.dataTransfer.types.includes(dndType("notes"))) return;
        const target = e.target.closest('a.sidebar-cat[data-category="notes"]');
        if (!target) return;
        e.preventDefault();
        target.classList.remove("is-drop-target");
        const payload = parseDndPayload(e.dataTransfer, "notes");
        if (!payload) return;
        moveNote(payload.id, "");
    });

    // Listen for sidebar refresh requests from surface modules.
    window.addEventListener("familiar:sidebarRefresh", refreshSidebarChildren);

    // AI tool calls that mutate notes / pages dispatch
    // familiar:notesChanged via chat.js when the gateway emits the
    // TOOL_EFFECT signal. Refresh the sidebar children too so a
    // brand new note created by the AI shows up in the expanded
    // list without manual re-expand. Also drop the per-book wiki
    // pages cache — a created/renamed/deleted wiki page invalidates
    // every cached book, so the next render re-fetches.
    window.addEventListener("familiar:notesChanged", () => {
        sidebarWikiPagesCache.clear();
        refreshSidebarChildren();
    });

    // A chat was deleted — drop its row from the expanded "Chat"
    // category in the rail. Without this the deleted conversation
    // lingers in the sidebar (closing its tab doesn't touch the
    // rail) and clicking it 404s until a full page refresh.
    window.addEventListener("familiar:conversationDeleted", refreshSidebarChildren);

    // openDocFromSidebar focuses (or creates) a workspace tab of
    // the right surface, then dispatches `familiar:openDoc` so the
    // surface module can load the doc into that tab. id=null means
    // "open a new blank doc" — the surface module decides what
    // that does (creates a conversation, opens an Untitled note,
    // etc.).
    // findTabWithPageId is the wiki-specific dedup for sidebar page
    // rows, which carry the page UUID (not the slug the composite
    // docId key uses). Loaded wiki tabs stamp state.pageId, so a
    // direct scan finds an already-open page regardless of slugs.
    function findTabWithPageId(pageId) {
        if (!pageId) return null;
        for (const slot of LAYOUTS[state.layout].slots) {
            for (const tabId of state.panels[slot].tabIds) {
                const t = state.tabs[tabId];
                if (t && t.surface === "wiki" && t.state && t.state.pageId === pageId) {
                    return { tabId, slot };
                }
            }
        }
        return null;
    }

    function focusTabRef(ref) {
        state.panels[ref.slot].activeTabId = ref.tabId;
        state.activePanelSlot = ref.slot;
        saveState();
        renderGrid();
    }

    function openDocFromSidebar(surface, id, title, extra) {
        if (window.appSwitchPanel) window.appSwitchPanel("workspace");
        // The tab this open targets. Threaded into the openDoc event
        // so the surface module loads the doc into THIS tab — not
        // whichever same-surface shell happens to be mounted first
        // (with two panels showing the same surface, that heuristic
        // overwrote a visible doc; 2026-06-12 report).
        let targetTabId = null;
        if (id === null) {
            // New doc / splash. New tabs ALWAYS land in the left-most
            // panel (never the active/right one). Single-splash policy:
            // reuse an empty tab already in the left-most panel instead
            // of stacking a second splash; else mint one there. Opening a
            // new tab never overrides a doc-bearing tab (openTab appends).
            const leftSlot = LAYOUTS[state.layout].slots[0];
            const emptyLeft = emptyTabInSlot(leftSlot);
            if (emptyLeft) {
                morphTabTo(emptyLeft, surface);
                targetTabId = emptyLeft.tabId;
            } else {
                targetTabId = openTab(surface, leftSlot);
            }
        } else {
            // Already open? Just focus that tab — don't create a
            // duplicate. Wiki sidebar page rows dedup by page UUID;
            // everything else by docId.
            // Exact-page match (wiki sidebar rows carry the page
            // UUID): the page is on screen in that tab — focus it
            // and stop, no reload.
            const byPage = extra && extra.pageId && findTabWithPageId(extra.pageId);
            if (byPage) {
                focusTabRef(byPage);
                return;
            }
            const existing = findTabWithDoc(surface, id);
            if (existing) {
                focusTabRef(existing);
                targetTabId = existing.tabId;
                // Doc loaded — skip the dispatch UNLESS the caller
                // passed `extra` to navigate inside the tab (e.g. a
                // page click whose parent book tab is sitting at the
                // book splash must still load that page).
                if (!extra || Object.keys(extra).length === 0) {
                    return;
                }
            } else {
                // Not open anywhere. New tabs land in the LEFT-MOST
                // panel. NEVER reuse a doc-bearing tab — sidebar opens
                // must not override what's on screen. An empty tab of the
                // SAME surface (its splash) IN THE LEFT-MOST PANEL can
                // take the doc — that overrides nothing. Otherwise mint a
                // new tab there. (Other surfaces' splashes are left alone:
                // a note opening where the chat splash sat is its own
                // surprise.)
                const leftSlot = LAYOUTS[state.layout].slots[0];
                const emptySame = emptyTabInSlot(leftSlot, surface);
                if (emptySame) {
                    focusTabRef(emptySame);
                    targetTabId = emptySame.tabId;
                } else {
                    targetTabId = openTab(surface, leftSlot);
                }
            }
        }
        // Notify the surface module to load this doc into the
        // target tab. Microtask delay so the surface render
        // pass that the open/morph triggered gets to run first.
        setTimeout(() => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", {
                detail: { surface, id, title, tabId: targetTabId, ...(extra || {}) },
            }));
        }, 0);
    }

    function wire() {
        // Layout picker buttons.
        const picker = document.getElementById("ws-layout-picker");
        if (picker) {
            picker.addEventListener("click", (e) => {
                const btn = e.target.closest(".ws-layout-btn");
                if (!btn) return;
                setLayout(btn.dataset.layout);
            });
        }

        // Zoom picker — S/M/L buttons next to the layout picker.
        const zoomPicker = document.getElementById("ws-zoom-picker");
        if (zoomPicker) {
            // Mark the active level on load.
            const current = window.familiarZoom ? window.familiarZoom.get() : "small";
            zoomPicker.querySelectorAll(".ws-zoom-btn").forEach((b) => {
                b.classList.toggle("is-active", b.dataset.zoom === current);
            });
            zoomPicker.addEventListener("click", (e) => {
                const btn = e.target.closest(".ws-zoom-btn");
                if (!btn || !btn.dataset.zoom) return;
                if (window.familiarZoom) window.familiarZoom.set(btn.dataset.zoom);
                zoomPicker.querySelectorAll(".ws-zoom-btn").forEach((b) => {
                    b.classList.toggle("is-active", b === btn);
                });
            });
        }

        // Sidebar mode-switching nav. Three click targets here
        // (per DESIGN.md):
        //
        //   data-surface=*  → switch to workspace mode + focus
        //                     the matching surface tab
        //   data-mode=home  → switch to home mode (Home row +
        //                     the F-mark in sidebar-mark both
        //                     carry this attribute)
        //   data-mode=user  → switch to user mode (Phase 3c lands
        //                     the User panel; for now this also
        //                     just flips the panel and active
        //                     state — the panel-user section is
        //                     not yet wired so the click is a
        //                     visible no-op)
        //
        // Active-state management: only one of Home / cat / User
        // shows is-active at a time.
        document.addEventListener("click", (e) => {
            const surfaceLink = e.target.closest("[data-surface]");
            if (surfaceLink) {
                e.preventDefault();
                // Three click targets on a category row, separate
                // intents. The "+ New …" button creates a fresh doc
                // of this surface kind. The chevron toggles expand.
                // The title / glyph / count area opens / focuses
                // the surface tab. Splitting these keeps the
                // tab-open click predictable while still giving
                // each affordance a one-tap behavior.
                if (e.target.closest(".sidebar-cat-new")) {
                    e.stopPropagation();
                    // Panel-based categories (shards, scheduled) open
                    // their panel + trigger the "new" flow instead of
                    // opening a workspace tab.
                    const surf = surfaceLink.dataset.surface;
                    if (surf === "scheduled" && window.appSwitchPanel) {
                        window.appSwitchPanel("scheduled");
                        setSidebarActive(surfaceLink, "category");
                        // Trigger the new-action form (the panel's
                        // "New action" button is #actions-new).
                        const btn = document.getElementById("actions-new");
                        if (btn) btn.click();
                        return;
                    }
                    openDocFromSidebar(surfaceLink.dataset.surface, null, null);
                    return;
                }
                if (e.target.closest(".sidebar-row-chevron")) {
                    toggleCategoryExpand(surfaceLink);
                    return;
                }
                // Shards is the one category whose primary surface
                // isn't a workspace tab kind — clicking it opens the
                // admin Shards panel directly so users land on the
                // CRUD form instead of a placeholder workspace tab.
                if (surfaceLink.dataset.surface === "shards" && window.appSwitchPanel) {
                    window.appSwitchPanel("shards");
                    setSidebarActive(surfaceLink, "category");
                    return;
                }
                // Scheduled actions: same panel-direct treatment as
                // Shards — there's no workspace tab kind for it.
                if (surfaceLink.dataset.surface === "scheduled" && window.appSwitchPanel) {
                    window.appSwitchPanel("scheduled");
                    setSidebarActive(surfaceLink, "category");
                    return;
                }
                if (window.appSwitchPanel) window.appSwitchPanel("workspace");
                setSidebarActive(surfaceLink, "category");
                const navResult = focusSurface(surfaceLink.dataset.surface);
                // Only fire surfaceNavRoot when we minted a fresh
                // tab — that's the splash-reset signal. Reusing an
                // existing tab must NOT reset it; otherwise the
                // sidebar click would yank the user out of an open
                // note / chat / page, which is exactly the
                // behavior we're moving away from.
                if (navResult && navResult.isNew) {
                    window.dispatchEvent(new CustomEvent("familiar:surfaceNavRoot", {
                        detail: { surface: surfaceLink.dataset.surface, tabId: navResult.tabId },
                    }));
                }
                return;
            }
            const modeLink = e.target.closest("[data-mode]");
            if (modeLink) {
                e.preventDefault();
                const mode = modeLink.dataset.mode;
                if (mode === "home" && window.appSwitchPanel) {
                    window.appSwitchPanel("home");
                    setSidebarActive(document.getElementById("sidebar-home-row"), "home");
                } else if (mode === "user" && window.appSwitchPanel) {
                    window.appSwitchPanel("user");
                    setSidebarActive(document.getElementById("sidebar-user-row"), "user");
                }
                return;
            }
        }, true /* capture so we run before app.js's nav-item handler */);

        // setSidebarActive flips is-active across the three
        // sidebar-row classes so only one shows at a time.
        // section is "category" | "home" | "user".
        function setSidebarActive(activeEl, section) {
            for (const el of document.querySelectorAll(".sidebar-cat")) {
                el.classList.toggle("is-active", section === "category" && el === activeEl);
            }
            const home = document.getElementById("sidebar-home-row");
            if (home) home.classList.toggle("is-active", section === "home");
            const user = document.getElementById("sidebar-user-row");
            if (user) user.classList.toggle("is-active", section === "user");
            for (const el of document.querySelectorAll("[data-panel]")) {
                el.classList.remove("is-active");
            }
        }

        // ── Sidebar resize handle ──────────────────────────────
        // Show the grab handle when any sidebar label overflows.
        // One-way: once visible, it stays until page reload.
        (function initSidebarResize() {
            const handle = document.getElementById("sidebar-resize");
            const sidebar = document.getElementById("sidebar");
            const shell = document.querySelector(".view-shell");
            if (!handle || !sidebar || !shell) return;

            let revealed = false;

            function checkOverflow() {
                if (revealed) return;
                // Check category labels, child title spans, AND their
                // parent containers — overflow:hidden on the parent
                // clips the text without the span itself overflowing.
                const els = sidebar.querySelectorAll(
                    ".sidebar-row-label, .sidebar-child-title, .sidebar-child"
                );
                for (const el of els) {
                    if (el.scrollWidth > el.clientWidth + 1) {
                        handle.classList.add("visible");
                        revealed = true;
                        return;
                    }
                }
            }

            // Check on load + whenever sidebar children change.
            checkOverflow();
            const cats = sidebar.querySelector(".sidebar-categories");
            if (cats) {
                new MutationObserver(checkOverflow)
                    .observe(cats, { childList: true, subtree: true });
            }

            // Drag to resize.
            let startX = 0, startW = 0;

            handle.addEventListener("mousedown", (e) => {
                e.preventDefault();
                startX = e.clientX;
                startW = sidebar.getBoundingClientRect().width;
                handle.classList.add("dragging");
                document.addEventListener("mousemove", onDrag);
                document.addEventListener("mouseup", onUp);
            });

            function onDrag(e) {
                const newW = Math.max(180, Math.min(480, startW + (e.clientX - startX)));
                shell.style.setProperty("--sidebar-w", newW + "px");
            }

            function onUp() {
                handle.classList.remove("dragging");
                document.removeEventListener("mousemove", onDrag);
                document.removeEventListener("mouseup", onUp);
                // Re-check after resize in case content now fits
                // (but don't hide — one-way reveal).
            }
        })();

        // Render now that DOM is parsed.
        renderGrid();
    }

    if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", wire);
    } else {
        wire();
    }
})();
