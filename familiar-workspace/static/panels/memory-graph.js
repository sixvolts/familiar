// Memory graph panel — FAMILIAR-CONSOLE-SPEC Phase D.
//
// Lifted out of app.js so the panel's ~450 lines don't bloat the
// main shell. Self-contained inside an IIFE; reaches into
// window.familiarAppHelpers for shared infrastructure (apiJSON,
// toast, setError, ensureCytoscape) and registers itself on
// window.familiarPanels so the panel-switcher in app.js invokes
// init() lazily on first navigation.

(function () {
    "use strict";

    const helpers = window.familiarAppHelpers;
    if (!helpers) {
        console.error("memory-graph: app helpers not loaded; panel disabled");
        return;
    }
    const { apiJSON, toast, setError, ensureCytoscape } = helpers;

    // ── Memory graph (FAMILIAR-CONSOLE-SPEC Phase D) ─────────────

    const graphState = {
        initialized: false,
        cy: null,                  // cytoscape instance
        currentCenter: null,       // entity name we're focused on, or null
        currentLimit: 50,
        searchTimer: null,
        // Detail panel state — tracks what's currently displayed so
        // the delete button knows what to remove.
        detailType: null,          // "entity" | "relationship" | null
        detailId: null,            // entity name or relationship UUID
        // Admin cross-user mode, entered via Users → View graph (or
        // following the memory browser's view-as through Focus in
        // graph). Empty = own graph — the only state a role=user
        // session can be in; the backend ignores the param for them.
        viewingUser: "",
    };

    // withGraphScope appends ?user_id=... to a URL when the admin has
    // picked another user. Non-admins pass through unchanged (the
    // picker is hidden from them, so viewingUser stays empty). Used
    // by loadGraph + searchGraphEntities + openRelationshipDetail so
    // every graph request shares the same scope.
    function withGraphScope(url) {
        if (!graphState.viewingUser) return url;
        const sep = url.includes("?") ? "&" : "?";
        return url + sep + "user_id=" + encodeURIComponent(graphState.viewingUser);
    }

    async function initMemoryGraph() {
        if (graphState.initialized) return;
        graphState.initialized = true;

        document.getElementById("graph-refresh").addEventListener("click", () => loadGraph(graphState.currentCenter));
        document.getElementById("graph-reset").addEventListener("click", () => {
            graphState.currentCenter = null;
            loadGraph(null);
        });
        document.getElementById("graph-detail-close").addEventListener("click", closeGraphDetail);
        document.getElementById("graph-detail-delete").addEventListener("click", deleteGraphItem);
        document.getElementById("graph-search").addEventListener("input", (e) => {
            // Debounced autocomplete — 200ms of quiet before a fetch.
            const q = e.target.value;
            if (graphState.searchTimer) clearTimeout(graphState.searchTimer);
            graphState.searchTimer = setTimeout(() => searchGraphEntities(q), 200);
        });
        document.getElementById("graph-viewas-clear").addEventListener("click", () => viewAsUser(""));

        try {
            await ensureCytoscape();
        } catch (err) {
            setError("graph-error", err);
            return;
        }
        // A focus() request may have arrived before init (the
        // Entities browser's "Focus in graph" handoff) — honor it
        // instead of the default top-N first paint.
        const center = pendingFocus;
        pendingFocus = null;
        if (center) document.getElementById("graph-search").value = center;
        await loadGraph(center || null);
    }

    // pendingFocus holds an entity requested via focus() before the
    // panel finished initializing; initMemoryGraph consumes it.
    let pendingFocus = null;

    // viewAsUser flips the panel between "my graph" and the admin
    // cross-user view. Safe pre-init: state + banner only, the
    // deferred first load picks the scope up via withGraphScope.
    // For role=user sessions the backend ignores the param, so this
    // is cosmetic-only for them — and nothing on the personal page
    // sets it anyway.
    function viewAsUser(userID) {
        userID = userID || "";
        if (userID === graphState.viewingUser) return;
        graphState.viewingUser = userID;
        const banner = document.getElementById("graph-viewas");
        banner.hidden = !userID;
        if (userID) document.getElementById("graph-viewas-user").textContent = userID;
        // Reset center when switching scopes — the previous center
        // may not exist in the new user's graph and would yield an
        // empty canvas.
        graphState.currentCenter = null;
        closeGraphDetail();
        const search = document.getElementById("graph-search");
        if (search) search.value = "";
        if (graphState.initialized) loadGraph(null);
    }

    // focusEntity re-centers the graph on an entity. Called by the
    // memory browser's entity detail; safe to call before the panel
    // has ever been opened.
    function focusEntity(entity) {
        if (!entity) return;
        if (!graphState.initialized) {
            pendingFocus = entity;
            return;
        }
        const search = document.getElementById("graph-search");
        if (search) search.value = entity;
        loadGraph(entity);
    }

    // loadSeq drops stale responses: view-as + focus handoffs can
    // fire two loads back-to-back, and the slower fetch must not
    // paint over the newer one.
    let loadSeq = 0;

    async function loadGraph(center) {
        setError("graph-error", null);
        const seq = ++loadSeq;
        const base = center
            ? "/console/api/memory/graph?center=" + encodeURIComponent(center) + "&depth=2&limit=" + graphState.currentLimit
            : "/console/api/memory/graph?limit=" + graphState.currentLimit;
        try {
            const data = await apiJSON(withGraphScope(base));
            if (seq !== loadSeq) return;
            graphState.currentCenter = center;
            renderGraph(data);
        } catch (err) {
            if (seq !== loadSeq) return;
            setError("graph-error", err);
        }
    }

    function renderGraph(data) {
        const canvas = document.getElementById("graph-canvas");
        // Replace the loading placeholder on first render.
        canvas.innerHTML = "";

        const banner = document.getElementById("graph-banner");
        if (data.truncated) {
            banner.hidden = false;
            banner.textContent = "Showing a slice of your graph — search to focus on a specific area.";
        } else {
            banner.hidden = true;
        }

        if (!data.nodes || data.nodes.length === 0) {
            canvas.innerHTML = '<div class="graph-loading micro">NO ENTITIES YET — TALK TO FAMILIAR ABOUT THINGS YOU CARE ABOUT AND THEY WILL APPEAR HERE</div>';
            return;
        }

        const elements = [];
        for (const n of data.nodes) {
            elements.push({
                data: {
                    id: n.id,
                    label: n.label,
                    degree: n.degree,
                },
            });
        }
        for (const e of data.edges) {
            elements.push({
                data: {
                    id: e.id,
                    source: e.source,
                    target: e.target,
                    label: e.label,
                    confidence: e.confidence,
                },
            });
        }

        // Destroy any previous instance so we don't leak event listeners.
        if (graphState.cy) {
            graphState.cy.destroy();
            graphState.cy = null;
        }

        graphState.cy = window.cytoscape({
            container: canvas,
            elements: elements,
            style: [
                {
                    selector: "node",
                    style: {
                        "background-color": "#FFC000",
                        "label": "data(label)",
                        "color": "#F5F5F5",
                        "font-size": "11px",
                        "text-valign": "bottom",
                        "text-margin-y": 4,
                        // Log-scaled radius by degree so popular nodes
                        // stand out without dominating.
                        "width": "mapData(degree, 1, 30, 18, 60)",
                        "height": "mapData(degree, 1, 30, 18, 60)",
                        "border-width": 1,
                        "border-color": "#917300",
                    },
                },
                {
                    selector: "node:selected",
                    style: {
                        "border-color": "#1EAEDB",
                        "border-width": 3,
                    },
                },
                {
                    selector: "edge",
                    style: {
                        "line-color": "#494949",
                        "target-arrow-color": "#494949",
                        "target-arrow-shape": "triangle",
                        "curve-style": "bezier",
                        // Confidence drives edge thickness.
                        "width": "mapData(confidence, 0.5, 1.0, 1, 3)",
                        "label": "",
                        "font-size": "9px",
                        "color": "#7D7D7D",
                        "text-rotation": "autorotate",
                        "text-background-color": "#000000",
                        "text-background-opacity": 0.8,
                        "text-background-padding": 2,
                    },
                },
                {
                    selector: "edge:selected",
                    style: {
                        "line-color": "#1EAEDB",
                        "target-arrow-color": "#1EAEDB",
                        "label": "data(label)",
                        "color": "#F5F5F5",
                        "width": 3,
                    },
                },
                {
                    selector: "edge.hover",
                    style: { "label": "data(label)" },
                },
            ],
            layout: {
                name: "cose",
                animate: false,
                fit: true,
                padding: 30,
                idealEdgeLength: 80,
                nodeRepulsion: 6000,
            },
            wheelSensitivity: 0.2,
        });

        // Click-to-focus: single click on a node opens the entity
        // detail sidebar. Single click on an edge opens the
        // edge detail in the right sidebar.
        graphState.cy.on("tap", "node", (evt) => {
            const id = evt.target.data("id");
            const label = evt.target.data("label");
            const degree = evt.target.data("degree");
            openEntityDetail(id || label, label, degree);
        });
        graphState.cy.on("tap", "edge", (evt) => {
            const id = evt.target.data("id");
            openRelationshipDetail(id);
        });
        // Double-tap grows the view in place: merge the node's
        // depth-1 neighborhood instead of re-centering, so the user
        // can walk outward without losing what's already laid out.
        graphState.cy.on("dbltap", "node", (evt) => {
            expandNode(evt.target.data("id") || evt.target.data("label"));
        });
        graphState.cy.on("mouseover", "edge", (evt) => evt.target.addClass("hover"));
        graphState.cy.on("mouseout", "edge", (evt) => evt.target.removeClass("hover"));
    }

    // expandNode fetches the depth-1 neighborhood around an entity
    // and adds only the elements the canvas doesn't already have.
    // Nodes go into the array before edges — the backend derives its
    // node list from the edge set, so every endpoint is guaranteed
    // present by the time cytoscape wires the edge up.
    async function expandNode(entityId) {
        if (!graphState.cy) return;
        try {
            const data = await apiJSON(withGraphScope(
                "/console/api/memory/graph?center=" + encodeURIComponent(entityId) + "&depth=1&limit=50"));
            const fresh = [];
            for (const n of data.nodes || []) {
                if (graphState.cy.getElementById(n.id).length === 0) {
                    fresh.push({ data: { id: n.id, label: n.label, degree: n.degree } });
                }
            }
            for (const e of data.edges || []) {
                if (graphState.cy.getElementById(e.id).length === 0) {
                    fresh.push({ data: { id: e.id, source: e.source, target: e.target, label: e.label, confidence: e.confidence } });
                }
            }
            if (!fresh.length) {
                toast('Nothing new around "' + entityId + '"');
                return;
            }
            graphState.cy.add(fresh);
            graphState.cy.layout({
                name: "cose",
                animate: false,
                fit: true,
                padding: 30,
                idealEdgeLength: 80,
                nodeRepulsion: 6000,
            }).run();
        } catch (err) {
            toast("Expand failed: " + (err.message || String(err)), "error");
        }
    }

    async function searchGraphEntities(q) {
        const list = document.getElementById("graph-search-results");
        if (!q || q.length < 2) {
            list.hidden = true;
            list.innerHTML = "";
            return;
        }
        try {
            const data = await apiJSON(withGraphScope("/console/api/memory/entities?q=" + encodeURIComponent(q) + "&limit=10"));
            list.innerHTML = "";
            const items = data.items || [];
            if (!items.length) {
                list.hidden = true;
                return;
            }
            for (const m of items) {
                const row = document.createElement("button");
                row.type = "button";
                row.className = "graph-search-row";
                row.textContent = m.name + "  (" + m.degree + ")";
                row.addEventListener("click", () => {
                    list.hidden = true;
                    document.getElementById("graph-search").value = m.name;
                    loadGraph(m.name);
                });
                list.appendChild(row);
            }
            list.hidden = false;
        } catch (err) {
            // Silent on autocomplete errors — the search is best-effort.
            list.hidden = true;
        }
    }

    async function openRelationshipDetail(id) {
        const aside = document.getElementById("graph-detail");
        const body = document.getElementById("graph-detail-body");
        const labelEl = document.getElementById("graph-detail-label");

        graphState.detailType = "relationship";
        graphState.detailId = id;
        labelEl.textContent = "EDGE DETAIL";
        body.innerHTML = '<div class="micro">LOADING</div>';
        aside.hidden = false;

        try {
            const r = await apiJSON(withGraphScope("/console/api/memory/relationship/" + encodeURIComponent(id)));
            body.innerHTML = "";

            const triple = document.createElement("div");
            triple.className = "graph-detail-triple";
            triple.innerHTML =
                '<code>' + escapeText(r.subject) + '</code> ' +
                '<span class="graph-detail-pred">' + escapeText(r.predicate) + '</span> ' +
                '<code>' + escapeText(r.object) + '</code>';
            body.appendChild(triple);

            const conf = document.createElement("div");
            conf.className = "micro";
            conf.textContent = "Confidence: " + (r.confidence != null ? r.confidence.toFixed(2) : "—");
            body.appendChild(conf);

            // Phase C: inline edge curation — predicate rename +
            // confidence adjust. A predicate collision with an
            // existing (subject, predicate) row comes back as 409.
            const editHeader = document.createElement("div");
            editHeader.className = "label graph-detail-section";
            editHeader.textContent = "EDIT";
            body.appendChild(editHeader);

            const editWrap = document.createElement("div");
            editWrap.className = "graph-edge-edit";
            const predIn = document.createElement("input");
            predIn.className = "input";
            predIn.type = "text";
            predIn.value = r.predicate || "";
            predIn.placeholder = "predicate";
            const confIn = document.createElement("input");
            confIn.className = "input graph-edge-conf";
            confIn.type = "number";
            confIn.min = "0";
            confIn.max = "1";
            confIn.step = "0.05";
            confIn.value = r.confidence != null ? String(r.confidence) : "";
            const saveBtn = document.createElement("button");
            saveBtn.type = "button";
            saveBtn.className = "btn-accent btn-small";
            saveBtn.textContent = "Save";
            saveBtn.addEventListener("click", async () => {
                const patch = {};
                const p = predIn.value.trim();
                if (p && p !== r.predicate) patch.predicate = p;
                const c = parseFloat(confIn.value);
                if (!isNaN(c) && c !== r.confidence) patch.confidence = c;
                if (!Object.keys(patch).length) return;
                try {
                    await apiJSON(withGraphScope("/console/api/memory/relationship/" + encodeURIComponent(id)), {
                        method: "PATCH",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify(patch),
                    });
                    toast("Edge updated", "success");
                    openRelationshipDetail(id);
                    loadGraph(graphState.currentCenter);
                } catch (e) {
                    toast("Edit failed: " + (e.message || String(e)), "error");
                }
            });
            editWrap.append(predIn, confIn, saveBtn);
            body.appendChild(editWrap);

            const factHeader = document.createElement("div");
            factHeader.className = "label graph-detail-section";
            factHeader.textContent = "SUPPORTING FACT";
            body.appendChild(factHeader);

            if (r.supporting_fact) {
                const sf = document.createElement("div");
                sf.className = "graph-detail-fact";
                const content = document.createElement("p");
                content.className = "body";
                content.textContent = r.supporting_fact.content;
                sf.appendChild(content);
                const meta = document.createElement("div");
                meta.className = "micro";
                meta.textContent =
                    "scope=" + (r.supporting_fact.scope || "—") +
                    "  ·  source=" + (r.supporting_fact.source_type || "—");
                sf.appendChild(meta);
                body.appendChild(sf);
            } else {
                const none = document.createElement("div");
                none.className = "micro";
                none.textContent = "(no supporting fact recorded for this edge)";
                body.appendChild(none);
            }
        } catch (err) {
            body.innerHTML = "";
            const e = document.createElement("div");
            e.className = "error";
            e.textContent = "ERROR: " + (err.message || String(err));
            body.appendChild(e);
        }
    }

    function openEntityDetail(entityId, label, degree) {
        const aside = document.getElementById("graph-detail");
        const body = document.getElementById("graph-detail-body");
        const labelEl = document.getElementById("graph-detail-label");

        graphState.detailType = "entity";
        graphState.detailId = entityId;
        labelEl.textContent = "ENTITY DETAIL";
        body.innerHTML = "";
        aside.hidden = false;

        const nameEl = document.createElement("div");
        nameEl.className = "graph-detail-triple";
        nameEl.innerHTML = '<code>' + escapeText(label) + '</code>';
        body.appendChild(nameEl);

        const meta = document.createElement("div");
        meta.className = "micro";
        meta.textContent = "Connections: " + (degree || 0);
        body.appendChild(meta);

        // "Focus graph" button to re-center the graph on this entity.
        const focusBtn = document.createElement("button");
        focusBtn.type = "button";
        focusBtn.className = "btn-ghost btn-small";
        focusBtn.textContent = "Focus graph on this entity";
        focusBtn.style.marginTop = "12px";
        focusBtn.addEventListener("click", () => {
            loadGraph(entityId);
        });
        body.appendChild(focusBtn);

        // List relationships touching this entity.
        const relHeader = document.createElement("div");
        relHeader.className = "label graph-detail-section";
        relHeader.textContent = "RELATIONSHIPS";
        relHeader.style.marginTop = "16px";
        body.appendChild(relHeader);

        const relList = document.createElement("div");
        relList.innerHTML = '<span class="micro">LOADING…</span>';
        body.appendChild(relList);

        // Load relationships by focusing the graph query.
        apiJSON(withGraphScope("/console/api/memory/graph?center=" + encodeURIComponent(entityId) + "&depth=1&limit=50"))
            .then((data) => {
                relList.innerHTML = "";
                const edges = data.edges || [];
                if (edges.length === 0) {
                    relList.innerHTML = '<span class="micro">NONE</span>';
                    return;
                }
                for (const e of edges) {
                    const row = document.createElement("div");
                    row.className = "graph-detail-rel-row";
                    row.innerHTML =
                        '<code>' + escapeText(e.source) + '</code> ' +
                        '<span class="graph-detail-pred">' + escapeText(e.label) + '</span> ' +
                        '<code>' + escapeText(e.target) + '</code>';
                    row.style.cursor = "pointer";
                    row.addEventListener("click", () => openRelationshipDetail(e.id));
                    relList.appendChild(row);
                }
            })
            .catch(() => {
                relList.innerHTML = '<span class="micro">FAILED TO LOAD</span>';
            });
    }

    async function deleteGraphItem() {
        if (!graphState.detailType || !graphState.detailId) return;
        const type = graphState.detailType;
        const id = graphState.detailId;
        const label = type === "entity" ? 'entity "' + id + '" and all its relationships' : "this relationship";
        if (!confirm("Delete " + label + "? This cannot be undone.")) return;
        try {
            if (type === "entity") {
                await apiJSON(withGraphScope("/console/api/memory/entity/" + encodeURIComponent(id)), {
                    method: "DELETE",
                });
            } else {
                await apiJSON(withGraphScope("/console/api/memory/relationship/" + encodeURIComponent(id)), {
                    method: "DELETE",
                });
            }
            closeGraphDetail();
            toast(type === "entity" ? "Entity deleted" : "Relationship deleted", "success");
            loadGraph(graphState.currentCenter);
        } catch (e) {
            toast("Delete failed: " + (e.message || String(e)), "error");
        }
    }

    function closeGraphDetail() {
        document.getElementById("graph-detail").hidden = true;
        graphState.detailType = null;
        graphState.detailId = null;
    }

    // escapeText is a tiny HTML-safe wrapper used by the detail
    // sidebar's innerHTML constructions where we need both safety
    // and inline tags. textContent on individual nodes would be
    // safer but more verbose for the multi-tag triple line.
    function escapeText(s) {
        return String(s == null ? "" : s)
            .replace(/&/g, "&amp;")
            .replace(/</g, "&lt;")
            .replace(/>/g, "&gt;");
    }

    window.familiarPanels = window.familiarPanels || {};
    window.familiarPanels["memory-graph"] = { init: initMemoryGraph, focus: focusEntity, viewAs: viewAsUser };
})();
