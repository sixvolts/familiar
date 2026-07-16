// Memory browser panel — the fact/relationship explorer.
//
// Extracted from app.js as part of the panel modularization.
// Self-contained IIFE; pulls shared infrastructure from
// window.familiarAppHelpers (apiJSON, toast, setError, fmtDate)
// and registers on window.familiarPanels so app.js's
// panel-switcher invokes init() lazily on first navigation.

(function () {
    "use strict";

    const helpers = window.familiarAppHelpers;
    if (!helpers) {
        console.error("memory: app helpers not loaded; panel disabled");
        return;
    }
    const { apiJSON, toast, setError, fmtDate } = helpers;

    // ── Memory browser ──────────────────────────────────────────

    const PAGE_SIZE = 50;
    const memState = {
        initialized: false,
        offset: 0,
        total: 0,
        // kind is the coarse tab: knowledge (default — what retrieval
        // sees) / explicit / extracted / wiki / chunks / all.
        kind: "knowledge",
        currentID: null,
        currentRow: null,
        editing: false,
        // viewUser: admin cross-user mode. Empty = my own memory
        // (the only state a role=user session can be in — the
        // backend enforces self-scope regardless of what we send).
        // Set via Users → "View memory"; shown in the banner.
        viewUser: "",
    };

    // Tab → query-param mapping. "knowledge"/"chunks" use the server's
    // kind param; the per-source tabs pin source_type exactly. The
    // "entities" tab is a mode switch, not a filter — handled apart.
    const KIND_PARAMS = {
        knowledge: { kind: "knowledge" },
        explicit: { source_type: "explicit" },
        extracted: { source_type: "conversation_extraction" },
        wiki: { source_type: "wiki_page" },
        chunks: { kind: "chunks" },
        all: {},
    };

    // Entities mode state (MEMORY-UI-SPEC Phase B).
    const entState = {
        items: [],           // last fetched entity index
        sortKey: "degree",   // name | degree | fact_count | last_seen
        sortDir: -1,         // 1 asc, -1 desc
        current: null,       // entity shown in the detail aside
        searchTimer: null,
    };

    function memQueryString() {
        const form = document.getElementById("memory-filter");
        const fd = new FormData(form);
        const params = new URLSearchParams();
        const q = (fd.get("q") || "").toString().trim();
        if (q) params.set("q", q);
        const scope = (fd.get("scope") || "").toString();
        if (scope) params.set("scope", scope);
        for (const [k, v] of Object.entries(KIND_PARAMS[memState.kind] || {})) {
            params.set(k, v);
        }
        // No user param = own memory (server default for every role).
        // The explicit param only exists in admin view-as mode.
        if (memState.viewUser) params.set("user", memState.viewUser);
        if (fd.get("include_superseded")) params.set("include_superseded", "1");
        params.set("limit", String(PAGE_SIZE));
        params.set("offset", String(memState.offset));
        return params.toString();
    }

    function fmtConf(c) {
        if (typeof c !== "number") return "—";
        return c.toFixed(2);
    }

    function truncate(s, n) {
        if (!s) return "";
        return s.length > n ? s.slice(0, n) + "…" : s;
    }

    function renderMemoryRows(items) {
        const tbody = document.getElementById("memory-rows");
        tbody.innerHTML = "";
        if (!items || items.length === 0) {
            const tr = document.createElement("tr");
            tr.className = "row-empty";
            const td = document.createElement("td");
            td.colSpan = 7;
            td.textContent = "NO RESULTS";
            tr.appendChild(td);
            tbody.appendChild(tr);
            return;
        }
        for (const row of items) {
            const tr = document.createElement("tr");
            if (row.superseded) tr.classList.add("is-superseded");
            tr.dataset.id = row.id;

            const cContent = document.createElement("td");
            cContent.className = "col-content";
            const div = document.createElement("div");
            div.className = "content-cell";
            div.textContent = row.content || "";
            cContent.appendChild(div);

            const cScope = document.createElement("td");
            cScope.className = "col-scope";
            cScope.textContent = row.scope || "—";

            const cUser = document.createElement("td");
            cUser.className = "col-user";
            cUser.textContent = row.user_id || "GLOBAL";

            const cSource = document.createElement("td");
            cSource.className = "col-source";
            cSource.textContent = row.source_type || "—";

            const cConf = document.createElement("td");
            cConf.className = "col-conf";
            cConf.textContent = fmtConf(row.confidence);

            const cCreated = document.createElement("td");
            cCreated.className = "col-created";
            cCreated.textContent = fmtDate(row.created_at);

            const cAct = document.createElement("td");
            cAct.className = "col-actions";
            cAct.textContent = truncate(row.id, 8);

            tr.append(cContent, cScope, cUser, cSource, cConf, cCreated, cAct);
            tr.addEventListener("click", () => openDetail(row.id));
            tbody.appendChild(tr);
        }
    }

    function updatePager() {
        const info = document.getElementById("m-page-info");
        const start = memState.total === 0 ? 0 : memState.offset + 1;
        const end = Math.min(memState.offset + PAGE_SIZE, memState.total);
        info.textContent = memState.total === 0
            ? "0 OF 0"
            : start + "–" + end + " OF " + memState.total;
        document.getElementById("m-prev").disabled = memState.offset <= 0;
        document.getElementById("m-next").disabled = end >= memState.total;
        document.getElementById("memory-total").textContent = String(memState.total);
    }

    async function loadMemories() {
        setError("memory-error", null);
        try {
            const data = await apiJSON("/console/api/memories?" + memQueryString());
            memState.total = data.total || 0;
            renderMemoryRows(data.items || []);
            updatePager();
        } catch (e) {
            setError("memory-error", e);
        }
    }

    function populateSelect(id, values, labelFn) {
        const sel = document.getElementById(id);
        // Keep the first default <option> intact (and "global" for user).
        const keep = [];
        for (const opt of Array.from(sel.options)) {
            if (opt.value === "" || opt.value === "global") keep.push(opt.cloneNode(true));
        }
        sel.innerHTML = "";
        for (const k of keep) sel.appendChild(k);
        for (const v of values || []) {
            const opt = document.createElement("option");
            opt.value = v;
            opt.textContent = labelFn ? labelFn(v) : v.toUpperCase();
            sel.appendChild(opt);
        }
    }

    async function initMemoryBrowser() {
        if (memState.initialized) return;
        let facets;
        try {
            facets = await apiJSON("/console/api/memories/facets");
        } catch (e) {
            return; // no browser — leave section hidden
        }
        if (!facets || !facets.available) return;
        memState.initialized = true;

        populateSelect("m-scope", facets.scopes);

        document.getElementById("memory-section").hidden = false;
        document.getElementById("memory-viewas-clear").addEventListener("click", () => viewAsUser(""));

        document.getElementById("memory-kind-tabs").addEventListener("click", (e) => {
            const tab = e.target.closest(".memory-kind-tab");
            if (!tab) return;
            memState.kind = tab.dataset.kind;
            for (const t of document.querySelectorAll(".memory-kind-tab")) {
                t.classList.toggle("is-active", t === tab);
            }
            const entities = memState.kind === "entities";
            setEntitiesMode(entities);
            if (entities) {
                loadEntities();
            } else {
                memState.offset = 0;
                loadMemories();
            }
        });

        document.getElementById("me-q").addEventListener("input", () => {
            if (entState.searchTimer) clearTimeout(entState.searchTimer);
            entState.searchTimer = setTimeout(loadEntities, 250);
        });
        document.querySelector("#memory-entities-wrap thead").addEventListener("click", (e) => {
            const th = e.target.closest(".me-sort");
            if (!th) return;
            const key = th.dataset.sort;
            if (entState.sortKey === key) {
                entState.sortDir = -entState.sortDir;
            } else {
                entState.sortKey = key;
                entState.sortDir = key === "name" ? 1 : -1;
            }
            renderEntityRows();
        });
        document.getElementById("entity-detail-close").addEventListener("click", closeEntityDetail);
        document.getElementById("entity-detail-graph").addEventListener("click", () => {
            if (!entState.current) return;
            const g = window.familiarPanels && window.familiarPanels["memory-graph"];
            // Carry the admin view-as scope into the graph so the
            // focused entity is looked up in the same user's store.
            if (g && g.viewAs) g.viewAs(memState.viewUser);
            if (g && g.focus) g.focus(entState.current.name);
            if (window.appSwitchPanel) window.appSwitchPanel("memory-graph");
        });
        document.getElementById("entity-merge-go").addEventListener("click", mergeCurrentEntity);
        document.getElementById("detail-collapse-chain").addEventListener("click", collapseCurrentChain);

        loadHealth();

        const form = document.getElementById("memory-filter");
        form.addEventListener("submit", (e) => {
            e.preventDefault();
            memState.offset = 0;
            loadMemories();
        });
        document.getElementById("m-reset").addEventListener("click", () => {
            form.reset();
            memState.offset = 0;
            loadMemories();
        });
        document.getElementById("m-prev").addEventListener("click", () => {
            memState.offset = Math.max(0, memState.offset - PAGE_SIZE);
            loadMemories();
        });
        document.getElementById("m-next").addEventListener("click", () => {
            memState.offset += PAGE_SIZE;
            loadMemories();
        });
        document.getElementById("detail-close").addEventListener("click", closeDetail);
        document.getElementById("detail-delete").addEventListener("click", deleteCurrent);
        document.getElementById("detail-edit").addEventListener("click", enterEditMode);
        document.getElementById("detail-save").addEventListener("click", saveEdit);
        document.getElementById("detail-cancel-edit").addEventListener("click", cancelEditMode);

        loadMemories();
    }

    // ── Entities mode (MEMORY-UI-SPEC Phase B) ──────────────────

    // viewAsUser flips the panel between "my memory" and the admin
    // cross-user view (Users → View memory). Purely cosmetic for
    // non-admins — the backend ignores the params for role=user —
    // but the personal page never sets it, so regular sessions
    // always browse their own store.
    function viewAsUser(userID) {
        userID = userID || "";
        // No-op when the mode isn't changing — the personal subnav
        // entry calls this with "" on every visit and shouldn't
        // trigger a redundant reload.
        if (userID === memState.viewUser) return;
        memState.viewUser = userID;
        const banner = document.getElementById("memory-viewas");
        banner.hidden = !memState.viewUser;
        if (memState.viewUser) {
            document.getElementById("memory-viewas-user").textContent = memState.viewUser;
        }
        closeDetail();
        closeEntityDetail();
        memState.offset = 0;
        if (memState.kind === "entities") {
            loadEntities();
        } else {
            loadMemories();
        }
        loadHealth();
    }

    // setEntitiesMode swaps the fact-browser chrome (filter card,
    // table, pager) for the entity index and back. Both live in the
    // same section so the kind tabs stay put.
    function setEntitiesMode(on) {
        document.getElementById("memory-filter").hidden = on;
        document.getElementById("memory-facts-wrap").hidden = on;
        document.querySelector("#memory-section .memory-pager").hidden = on;
        document.getElementById("memory-entities-wrap").hidden = !on;
        if (!on) closeEntityDetail();
    }

    async function loadEntities() {
        setError("memory-error", null);
        const q = document.getElementById("me-q").value.trim();
        const params = new URLSearchParams({ limit: "100" });
        if (q) params.set("q", q);
        if (memState.viewUser) params.set("user_id", memState.viewUser);
        try {
            const data = await apiJSON("/console/api/memory/entities?" + params.toString());
            entState.items = (data && data.items) || [];
            document.getElementById("memory-total").textContent = String(entState.items.length);
            renderEntityRows();
        } catch (e) {
            setError("memory-error", e);
        }
    }

    function renderEntityRows() {
        const tbody = document.getElementById("memory-entity-rows");
        tbody.innerHTML = "";
        if (entState.items.length === 0) {
            const tr = document.createElement("tr");
            tr.className = "row-empty";
            const td = document.createElement("td");
            td.colSpan = 4;
            td.textContent = "NO ENTITIES";
            tr.appendChild(td);
            tbody.appendChild(tr);
            return;
        }
        const key = entState.sortKey, dir = entState.sortDir;
        const sorted = entState.items.slice().sort((a, b) => {
            const av = a[key] ?? "", bv = b[key] ?? "";
            if (av < bv) return -dir;
            if (av > bv) return dir;
            return 0;
        });
        for (const ent of sorted) {
            const tr = document.createElement("tr");
            const cName = document.createElement("td");
            cName.className = "col-content";
            cName.textContent = ent.name;
            const cDeg = document.createElement("td");
            cDeg.textContent = String(ent.degree || 0);
            const cFacts = document.createElement("td");
            cFacts.textContent = String(ent.fact_count || 0);
            const cSeen = document.createElement("td");
            cSeen.textContent = ent.last_seen ? fmtDate(ent.last_seen) : "—";
            tr.append(cName, cDeg, cFacts, cSeen);
            tr.addEventListener("click", () => openEntityDetail(ent));
            tbody.appendChild(tr);
        }
    }

    async function openEntityDetail(ent) {
        entState.current = ent;
        setError("entity-detail-error", null);
        document.getElementById("entity-detail-name").textContent = ent.name;
        const meta = document.getElementById("entity-detail-meta");
        meta.innerHTML = "";
        const pairs = [
            ["CONNECTIONS", String(ent.degree || 0)],
            ["FACTS", String(ent.fact_count || 0)],
            ["LAST SEEN", ent.last_seen ? fmtDate(ent.last_seen) : "—"],
        ];
        for (const [k, v] of pairs) {
            const kEl = document.createElement("div");
            kEl.className = "k";
            kEl.textContent = k;
            const vEl = document.createElement("div");
            vEl.className = "v";
            vEl.textContent = v;
            meta.append(kEl, vEl);
        }
        const host = document.getElementById("entity-detail-facts");
        host.innerHTML = '<span class="micro">LOADING…</span>';
        document.getElementById("entity-detail").hidden = false;
        try {
            const factsURL = "/console/api/memory/entity/" + encodeURIComponent(ent.name) + "/facts?limit=50" +
                (memState.viewUser ? "&user_id=" + encodeURIComponent(memState.viewUser) : "");
            const data = await apiJSON(factsURL);
            host.innerHTML = "";
            const items = (data && data.items) || [];
            if (items.length === 0) {
                host.innerHTML = '<span class="micro">NO LINKED FACTS — THIS ENTITY ONLY APPEARS IN TRIPLES WITHOUT PROVENANCE</span>';
                return;
            }
            for (const f of items) {
                const row = document.createElement("button");
                row.type = "button";
                row.className = "entity-fact";
                const content = document.createElement("div");
                content.className = "entity-fact-content";
                content.textContent = f.content || "";
                const metaLine = document.createElement("div");
                metaLine.className = "micro";
                metaLine.textContent = (f.source_type || "—") + "  ·  " + fmtDate(f.created_at);
                row.append(content, metaLine);
                row.addEventListener("click", () => {
                    closeEntityDetail();
                    openDetail(f.id);
                });
                host.appendChild(row);
            }
        } catch (e) {
            setError("entity-detail-error", e);
            host.innerHTML = '<span class="micro">FAILED TO LOAD</span>';
        }
    }

    function closeEntityDetail() {
        document.getElementById("entity-detail").hidden = true;
        entState.current = null;
        document.getElementById("entity-merge-target").value = "";
        setError("entity-detail-error", null);
    }

    // mergeCurrentEntity folds the open entity into the typed target
    // (Phase C). Server-side it rewrites subjects/objects, drops
    // collision losers and self-loops.
    async function mergeCurrentEntity() {
        if (!entState.current) return;
        const from = entState.current.name;
        const to = document.getElementById("entity-merge-target").value.trim();
        if (!to) return;
        if (!confirm('Merge "' + from + '" into "' + to + '"? Every triple mentioning it will be rewritten. This cannot be undone.')) return;
        setError("entity-detail-error", null);
        try {
            const res = await apiJSON("/console/api/memory/entity/" + encodeURIComponent(from) + "/merge" +
                (memState.viewUser ? "?user_id=" + encodeURIComponent(memState.viewUser) : ""), {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ into: to }),
            });
            toast("MERGED — " + (res.rewritten || 0) + " TRIPLES NOW REFERENCE " + to.toUpperCase(), "success");
            closeEntityDetail();
            loadEntities();
        } catch (e) {
            setError("entity-detail-error", e);
            toast("MERGE FAILED", "error");
        }
    }

    // loadHealth fills the store-health strip at the bottom of the
    // section (Phase C §5). Best-effort: the strip stays hidden if
    // the endpoint errors.
    async function loadHealth() {
        try {
            const h = await apiJSON("/console/api/memory/health" +
                (memState.viewUser ? "?user_id=" + encodeURIComponent(memState.viewUser) : ""));
            const cells = document.getElementById("memory-health-cells");
            cells.innerHTML = "";
            const items = [
                ["CONVERSATION CHUNKS", String(h.chunks || 0) + (h.oldest_chunk_days ? " · oldest " + h.oldest_chunk_days + "d" : "")],
                ["FACTS WITHOUT EMBEDDING", String(h.missing_embeddings || 0)],
                ["SUPERSEDED ROWS", String(h.superseded_rows || 0)],
                ["ORPHAN EDGES", String(h.orphan_edges || 0)],
            ];
            for (const [k, v] of items) {
                const cell = document.createElement("div");
                cell.className = "memory-health-cell";
                const num = document.createElement("div");
                num.className = "num";
                num.textContent = v;
                const lbl = document.createElement("div");
                lbl.className = "micro";
                lbl.textContent = k;
                cell.append(num, lbl);
                cells.appendChild(cell);
            }
            document.getElementById("memory-health").hidden = false;
        } catch (e) {
            // leave hidden
        }
    }

    async function openDetail(id) {
        setError("detail-error", null);
        memState.currentID = id;
        memState.editing = false;
        const panel = document.getElementById("memory-detail");
        document.getElementById("detail-id").textContent = id;
        document.getElementById("detail-content").textContent = "LOADING…";
        document.getElementById("detail-content").hidden = false;
        document.getElementById("detail-edit-wrap").hidden = true;
        document.getElementById("detail-meta").innerHTML = "";
        document.getElementById("detail-tags").innerHTML = "";
        document.getElementById("detail-versions").innerHTML = '<span class="micro">LOADING…</span>';
        document.getElementById("detail-triples").innerHTML = '<span class="micro">LOADING…</span>';
        panel.hidden = false;
        try {
            const row = await apiJSON("/console/api/memories/" + encodeURIComponent(id));
            memState.currentRow = row;
            renderDetail(row);
            loadVersionHistory(id);
            loadRelationshipTriples(row);
            loadChain(row);
        } catch (e) {
            setError("detail-error", e);
        }
    }

    // loadChain renders the supersede chain when the row is part of
    // one (Phase C). Hidden entirely for standalone rows — most
    // memories never get replaced and don't need the section.
    async function loadChain(row) {
        const wrap = document.getElementById("detail-chain-wrap");
        wrap.hidden = true;
        if (!row || (!row.supersedes && !row.superseded_by)) return;
        try {
            const data = await apiJSON("/console/api/memories/" + encodeURIComponent(row.id) + "/chain");
            const items = (data && data.items) || [];
            if (items.length < 2) return;
            const host = document.getElementById("detail-chain");
            host.innerHTML = "";
            for (const m of items) {
                const el = document.createElement("button");
                el.type = "button";
                el.className = "detail-chain-row" + (m.superseded ? " is-dead" : "") + (m.id === row.id ? " is-current" : "");
                const badge = document.createElement("span");
                badge.className = "detail-chain-badge";
                badge.textContent = m.superseded ? "REPLACED" : "LIVE";
                const content = document.createElement("span");
                content.className = "detail-chain-content";
                content.textContent = truncate(m.content || "", 90);
                const when = document.createElement("span");
                when.className = "micro";
                when.textContent = fmtDate(m.created_at);
                el.append(badge, content, when);
                el.addEventListener("click", () => openDetail(m.id));
                host.appendChild(el);
            }
            wrap.hidden = false;
        } catch (e) {
            // best-effort — section stays hidden
        }
    }

    async function collapseCurrentChain() {
        if (!memState.currentID) return;
        if (!confirm("Collapse this chain? Every replaced version is deleted; only the live fact survives. Version history on the survivor is kept.")) return;
        setError("detail-error", null);
        try {
            const res = await apiJSON("/console/api/memories/" + encodeURIComponent(memState.currentID) + "/chain/collapse", {
                method: "POST",
            });
            toast("CHAIN COLLAPSED — " + (res.deleted || 0) + " ROWS PRUNED", "success");
            if (res.tip) {
                openDetail(res.tip);
            } else {
                closeDetail();
            }
            loadMemories();
        } catch (e) {
            setError("detail-error", e);
            toast("COLLAPSE FAILED", "error");
        }
    }

    function renderDetail(row) {
        document.getElementById("detail-content").textContent = row.content || "";
        const meta = document.getElementById("detail-meta");
        meta.innerHTML = "";
        const pairs = [
            ["SCOPE", row.scope || "—"],
            ["USER", row.user_id || "GLOBAL"],
            ["SOURCE", row.source_type || "—"],
            ["CONF", fmtConf(row.confidence)],
            ["CREATED", fmtDate(row.created_at)],
            ["LAST USED", fmtDate(row.last_accessed)],
            ["EMBEDDING", row.has_embedding ? "YES" : "NO"],
        ];
        if (row.source_ref) pairs.push(["SOURCE REF", row.source_ref]);
        if (row.scope_tag) pairs.push(["SCOPE TAG", row.scope_tag]);
        for (const [k, v] of pairs) {
            const kEl = document.createElement("div");
            kEl.className = "k";
            kEl.textContent = k;
            const vEl = document.createElement("div");
            vEl.className = "v";
            vEl.textContent = v;
            meta.append(kEl, vEl);
        }
        // Supersede chain, navigable in both directions: the row this
        // one replaced, and the row that replaced this one.
        const addLink = (label, id) => {
            const kEl = document.createElement("div");
            kEl.className = "k";
            kEl.textContent = label;
            const vEl = document.createElement("div");
            vEl.className = "v";
            const a = document.createElement("a");
            a.href = "#";
            a.textContent = id.slice(0, 8) + "…";
            a.addEventListener("click", (e) => {
                e.preventDefault();
                openDetail(id);
            });
            vEl.appendChild(a);
            meta.append(kEl, vEl);
        };
        if (row.supersedes) addLink("REPLACED", row.supersedes);
        if (row.superseded_by) addLink("REPLACED BY", row.superseded_by);
        const tags = document.getElementById("detail-tags");
        tags.innerHTML = "";
        for (const t of row.tags || []) {
            const span = document.createElement("span");
            span.className = "tag";
            span.textContent = t;
            tags.appendChild(span);
        }
    }

    async function loadVersionHistory(id) {
        const host = document.getElementById("detail-versions");
        try {
            const versions = await apiJSON("/console/api/memories/" + encodeURIComponent(id) + "/versions");
            host.innerHTML = "";
            if (!versions || versions.length === 0) {
                host.innerHTML = '<span class="micro">NO VERSIONS</span>';
                return;
            }
            for (const v of versions) {
                const entry = document.createElement("div");
                entry.className = "version-entry";
                const num = document.createElement("div");
                num.className = "version-num";
                num.textContent = "V" + v.version;
                const body = document.createElement("div");
                const metaLine = document.createElement("div");
                metaLine.className = "version-meta";
                const typeBadge = document.createElement("span");
                typeBadge.className = "version-type";
                typeBadge.textContent = (v.change_type || "—").toUpperCase();
                const by = document.createElement("span");
                by.textContent = v.changed_by || "—";
                const at = document.createElement("span");
                at.textContent = fmtDate(v.created_at);
                metaLine.append(typeBadge, by, at);
                const content = document.createElement("div");
                content.className = "version-content";
                content.textContent = v.content || "";
                body.append(metaLine, content);
                entry.append(num, body);
                host.appendChild(entry);
            }
        } catch (e) {
            host.innerHTML = '<span class="micro">FAILED TO LOAD</span>';
        }
    }

    // loadRelationshipTriples renders the triples whose provenance
    // (relationships.source_fact) points at this memory — the real
    // link, replacing the old substring-heuristic call to an endpoint
    // that never existed.
    async function loadRelationshipTriples(row) {
        const host = document.getElementById("detail-triples");
        if (!row || !row.id) {
            host.innerHTML = '<span class="micro">NONE</span>';
            return;
        }
        try {
            const data = await apiJSON("/console/api/memories/" + encodeURIComponent(row.id) + "/relationships");
            host.innerHTML = "";
            const items = (data && data.items) || [];
            if (items.length === 0) {
                host.innerHTML = '<span class="micro">NONE</span>';
                return;
            }
            for (const t of items) {
                const el = document.createElement("div");
                el.className = "detail-triple";
                el.innerHTML = esc(t.subject) + '<span class="triple-pred"> → ' + esc(t.predicate) + ' → </span>' + esc(t.object);
                host.appendChild(el);
            }
        } catch (e) {
            host.innerHTML = '<span class="micro">NOT AVAILABLE</span>';
        }
    }

    function esc(s) {
        const d = document.createElement("div");
        d.textContent = s || "";
        return d.innerHTML;
    }

    function enterEditMode() {
        if (!memState.currentRow) return;
        memState.editing = true;
        document.getElementById("detail-content").hidden = true;
        document.getElementById("detail-edit-wrap").hidden = false;
        document.getElementById("detail-edit-textarea").value = memState.currentRow.content || "";
        document.getElementById("detail-edit-textarea").focus();
    }

    function cancelEditMode() {
        memState.editing = false;
        document.getElementById("detail-content").hidden = false;
        document.getElementById("detail-edit-wrap").hidden = true;
    }

    async function saveEdit() {
        if (!memState.currentID) return;
        const newContent = document.getElementById("detail-edit-textarea").value.trim();
        if (!newContent) return;
        setError("detail-error", null);
        try {
            await apiJSON("/console/api/memories/" + encodeURIComponent(memState.currentID), {
                method: "PATCH",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ content: newContent }),
            });
            cancelEditMode();
            toast("MEMORY UPDATED", "success");
            openDetail(memState.currentID);
            loadMemories();
        } catch (e) {
            setError("detail-error", e);
            toast("UPDATE FAILED", "error");
        }
    }

    function closeDetail() {
        document.getElementById("memory-detail").hidden = true;
        memState.currentID = null;
        memState.currentRow = null;
        memState.editing = false;
        setError("detail-error", null);
    }

    async function deleteCurrent() {
        if (!memState.currentID) return;
        if (!confirm("Delete this memory? This cannot be undone.")) return;
        setError("detail-error", null);
        try {
            await apiJSON("/console/api/memories/" + encodeURIComponent(memState.currentID), {
                method: "DELETE",
            });
            closeDetail();
            toast("MEMORY DELETED", "success");
            loadMemories();
        } catch (e) {
            setError("detail-error", e);
            toast("DELETE FAILED", "error");
        }
    }

    window.familiarPanels = window.familiarPanels || {};
    window.familiarPanels["memory"] = {
        init: initMemoryBrowser,
        // resetAndReload jumps the list back to page 1 and reloads.
        resetAndReload() {
            memState.offset = 0;
            loadMemories();
        },
        // viewAsUser enters/leaves the admin cross-user view. The
        // users panel's "View memory" action calls it with a user
        // id; the personal subnav entry and the banner's clear
        // button call it with "".
        viewAsUser,
        // closeOrCancelDetail is app.js's global Escape handler's
        // delegate when a memory/entity detail pane is the topmost
        // overlay: cancel an in-progress edit, else close whichever
        // pane is open (memory detail wins — it stacks on top when
        // opened from an entity's fact list).
        closeOrCancelDetail() {
            const memOpen = !document.getElementById("memory-detail").hidden;
            if (memOpen && memState.editing) {
                cancelEditMode();
            } else if (memOpen) {
                closeDetail();
            } else {
                closeEntityDetail();
            }
        },
    };
})();
