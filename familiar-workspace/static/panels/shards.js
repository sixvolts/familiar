// Shards panel — FAMILIAR-SHARDS-PHASE1-SPEC Steps 8-9 + token
// subview + SHARD-AUTH-SPEC shard-passkey enrollment.
//
// Lifted out of app.js so the panel's ~740 lines don't bloat the
// main shell. Self-contained inside an IIFE; reaches into
// window.familiarAppHelpers for shared infrastructure (apiJSON,
// toast, setError, plus the WebAuthn ceremony helpers
// requireWebAuthn / b64urlToBuf / bufToB64url for the passkey
// enroll flow) and registers itself on window.familiarPanels so
// the panel-switcher in app.js invokes init() lazily on first
// navigation.

(function () {
    "use strict";

    const helpers = window.familiarAppHelpers;
    if (!helpers) {
        console.error("shards: app helpers not loaded; panel disabled");
        return;
    }
    const {
        apiJSON, toast, setError,
        requireWebAuthn, b64urlToBuf, bufToB64url,
    } = helpers;

    // ── Shards (FAMILIAR-SHARDS-PHASE1-SPEC Steps 8-9) ───────────

    const shardsState = {
        initialized: false,
        toolCatalog: [],           // [{name, write_capable}]
        currentShard: null,        // full DTO when editing
        mode: "create",            // "create" | "edit"
    };

    async function initShardsBrowser() {
        if (shardsState.initialized) return;
        // Probe — 503 means shards aren't wired on this deploy.
        try {
            await apiJSON("/console/api/shards");
        } catch (e) {
            return;
        }
        shardsState.initialized = true;

        // Wire form events once.
        document.getElementById("shards-new").addEventListener("click", openShardCreate);
        document.getElementById("shard-detail-back").addEventListener("click", closeShardDetail);
        document.getElementById("shard-form").addEventListener("submit", submitShardForm);
        document.getElementById("shard-disable").addEventListener("click", () => toggleShardDisabled(true));
        document.getElementById("shard-enable").addEventListener("click", () => toggleShardDisabled(false));
        document.getElementById("shard-delete").addEventListener("click", deleteCurrentShard);
        document.getElementById("shard-mint-token").addEventListener("click", mintTokenForCurrent);
        document.getElementById("shard-chat").addEventListener("click", () => {
            // Owner-side shard chat (SKILL-PACKAGES-SPEC Phase 1):
            // jump to the workspace chat surface bound to this shard,
            // through the doc-open contract so no open chat tab gets
            // overridden.
            const s = shardsState.currentShard;
            if (!s) return;
            const ws = window.FamiliarWorkspace;
            if (ws && ws.openDoc) {
                ws.openDoc("chat", null, s.name, { shard_id: s.id, shard_name: s.name });
                return;
            }
            if (window.appSwitchPanel) window.appSwitchPanel("workspace");
            if (ws && ws.focusSurface) ws.focusSurface("chat");
            window.dispatchEvent(new CustomEvent("familiar:openDoc", {
                detail: { surface: "chat", shard_id: s.id, shard_name: s.name },
            }));
        });
        document.getElementById("token-modal-close").addEventListener("click", closeTokenModal);
        document.getElementById("token-modal-copy").addEventListener("click", copyTokenModalValue);

        // Shard passkeys (SHARD-AUTH-SPEC Phase 1).
        document.getElementById("shard-add-passkey").addEventListener("click", openPasskeyLabelModal);
        document.getElementById("shard-passkey-label-cancel").addEventListener("click", closePasskeyLabelModal);
        document.getElementById("shard-passkey-label-go").addEventListener("click", enrollShardPasskey);

        // Toggle the prompt requirement when chat/api kill-switches
        // flip — a console-only shard doesn't need a system prompt.
        for (const id of ["shard-chat-enabled", "shard-api-enabled"]) {
            const el = document.getElementById(id);
            if (el) el.addEventListener("change", updateShardPromptState);
        }

        // Re-validate tool checkboxes when persistence flips between
        // ephemeral and persistent: write-capable tools are greyed
        // out (and unchecked) for ephemeral shards.
        for (const radio of document.querySelectorAll('input[name="persistence"]')) {
            radio.addEventListener("change", updateToolAvailability);
        }

        // Skill bind-time warnings track the tool allowlist live
        // (delegated — the checklist re-renders).
        document.getElementById("shard-tools").addEventListener("change", updateSkillpackToolWarnings);

        // Load the tool catalog once — it's static across a process
        // lifetime. If this fails the checklist just stays empty.
        try {
            const tools = await apiJSON("/console/api/skills/tools");
            shardsState.toolCatalog = tools.items || [];
        } catch (e) {
            shardsState.toolCatalog = [];
        }
        renderToolChecklist();
        // Pull the owner's books once for the book-access picker.
        // Empty list is fine — owner just sees an empty checklist
        // and the field works as "inherit" by default.
        try {
            const books = await apiJSON("/console/api/books");
            shardsState.bookCatalog = (books && books.items) || [];
        } catch (e) {
            shardsState.bookCatalog = [];
        }
        // Imported-skills library (SKILL-PACKAGES-SPEC Phase 2). A 503
        // means the library isn't configured on this deploy — the
        // checklist just renders its "none" note.
        await refreshSkillpackCatalog();
        loadShards();
    }

    // refreshShardBookCatalog pulls /console/api/books fresh.
    // Called from openShardCreate / openShardDetail so a book
    // created earlier in the same session shows up in the
    // checklist without re-initing the whole panel.
    async function refreshShardBookCatalog() {
        try {
            const books = await apiJSON("/console/api/books");
            shardsState.bookCatalog = (books && books.items) || [];
        } catch (e) {
            shardsState.bookCatalog = shardsState.bookCatalog || [];
        }
    }

    // renderShardBookAccess paints the book-access checklist with
    // the cached book catalog and pre-checks the IDs in `selected`.
    // Stores book IDs as the checkbox value so the form payload
    // matches what the backend stores in shards.book_access (UUIDs,
    // not slugs or display names).
    function renderShardBookAccess(selected) {
        const host = document.getElementById("shard-book-access");
        if (!host) return;
        host.innerHTML = "";
        const set = new Set(selected || []);
        const books = (shardsState && shardsState.bookCatalog) || [];
        if (!books.length) {
            const empty = document.createElement("div");
            empty.className = "micro";
            empty.textContent = "(no books yet — create one in the Wiki panel first)";
            host.appendChild(empty);
            return;
        }
        for (const b of books) {
            const label = document.createElement("label");
            label.className = "shard-checklist-option";
            const cb = document.createElement("input");
            cb.type = "checkbox";
            cb.value = b.id;
            cb.checked = set.has(b.id);
            label.appendChild(cb);
            label.appendChild(document.createTextNode(" " + (b.name || b.slug || b.id)));
            host.appendChild(label);
        }
    }

    // refreshSkillpackCatalog pulls the biddable-skills catalog:
    // the instance library plus the caller's private skills
    // (USER-SKILLS-SPEC Phase A). Refreshed on every detail/create
    // open (like the book catalog) so a package admitted in the
    // Skills panel shows up without a full panel re-init. Note the
    // "mine" half is the CALLER's — when an admin opens another
    // user's shard, a checked personal skill would be refused by the
    // PUT (cross-owner binding); the server stays the boundary.
    async function refreshSkillpackCatalog() {
        try {
            const [inst, mine] = await Promise.all([
                apiJSON("/console/api/skillpacks").catch(() => null),
                apiJSON("/console/api/skills/mine").catch(() => null),
            ]);
            const items = ((inst && inst.items) || []).concat(
                (((mine && mine.items) || []).map((p) => ({ ...p, mine: true }))),
            );
            shardsState.skillpackCatalog = items;
        } catch (e) {
            shardsState.skillpackCatalog = shardsState.skillpackCatalog || [];
        }
    }

    // renderShardSkillpacks paints the imported-skills binding
    // checklist. Disabled packages can't be (re)bound — the PUT
    // endpoint refuses them — so their checkboxes are inert; a
    // disabled-but-still-bound package is shown checked + greyed and
    // its binding is dropped on the next save (the runtime already
    // ignores it).
    function renderShardSkillpacks(selected) {
        const host = document.getElementById("shard-skillpacks");
        if (!host) return;
        host.innerHTML = "";
        const set = new Set(selected || []);
        const pkgs = shardsState.skillpackCatalog || [];
        if (!pkgs.length) {
            const empty = document.createElement("div");
            empty.className = "micro";
            empty.textContent = "(no imported skills — admins admit them in the Skills panel)";
            host.appendChild(empty);
            return;
        }
        for (const p of pkgs) {
            const disabled = !!p.disabled_at;
            const row = document.createElement("div");
            row.className = "shard-skillpack-row";
            const label = document.createElement("label");
            label.className = "shard-checklist-option" + (disabled ? " is-disabled" : "");
            label.title = p.description || "";
            const cb = document.createElement("input");
            cb.type = "checkbox";
            cb.value = p.id;
            cb.checked = set.has(p.id);
            cb.disabled = disabled;
            cb.addEventListener("change", updateSkillpackToolWarnings);
            label.appendChild(cb);
            label.appendChild(document.createTextNode(
                " " + p.name + (p.mine ? " (yours)" : "") + (disabled ? " (disabled)" : "")));
            row.appendChild(label);
            const warn = document.createElement("div");
            warn.className = "micro shard-skillpack-warn";
            warn.hidden = true;
            row.appendChild(warn);
            host.appendChild(row);
        }
        updateSkillpackToolWarnings();
    }

    // updateSkillpackToolWarnings cross-references each CHECKED
    // skill's advisory tools_matched against the tool allowlist
    // checkboxes above. A skill whose instructions reference a tool
    // the shard can't call still binds fine — dispatch refuses the
    // call, which is the sandbox working — but the model burns a
    // turn finding out, so the operator gets told at bind time.
    function updateSkillpackToolWarnings() {
        const host = document.getElementById("shard-skillpacks");
        if (!host) return;
        const allowed = new Set(
            Array.from(document.querySelectorAll(".shard-tool-option input:checked")).map((cb) => cb.value),
        );
        const byID = new Map((shardsState.skillpackCatalog || []).map((p) => [p.id, p]));
        for (const row of host.querySelectorAll(".shard-skillpack-row")) {
            const cb = row.querySelector('input[type="checkbox"]');
            const warn = row.querySelector(".shard-skillpack-warn");
            const pkg = byID.get(cb.value);
            const missing = (cb.checked && pkg ? pkg.tools_matched || [] : [])
                .filter((t) => !allowed.has(t));
            if (missing.length) {
                warn.textContent = "⚠ requests " + missing.join(", ") +
                    " — not in this shard's tool allowlist";
                warn.hidden = false;
            } else {
                warn.hidden = true;
            }
        }
    }

    async function loadShards() {
        setError("shards-error", null);
        try {
            const data = await apiJSON("/console/api/shards");
            renderShardsTable(data.items || []);
        } catch (e) {
            setError("shards-error", e);
        }
    }

    function renderShardsTable(items) {
        const tbody = document.getElementById("shards-rows");
        tbody.innerHTML = "";
        if (!items.length) {
            const tr = document.createElement("tr");
            tr.className = "row-empty";
            const td = document.createElement("td");
            td.colSpan = 8;
            td.textContent = "NO SHARDS — CLICK NEW SHARD TO CREATE ONE";
            tr.appendChild(td);
            tbody.appendChild(tr);
            return;
        }
        for (const s of items) {
            const tr = document.createElement("tr");
            tr.dataset.id = s.id;
            const addTd = (text, cls) => {
                const td = document.createElement("td");
                td.textContent = text || "—";
                if (cls) td.className = cls;
                tr.appendChild(td);
            };
            addTd(s.name, "col-name");
            addTd(s.id, "col-id");
            addTd((s.persistence || "").toUpperCase(), "col-status");
            addTd((s.visibility || "").toUpperCase(), "col-status");
            addTd(String(s.tool_allowlist ? s.tool_allowlist.length : 0), "col-status");
            addTd(String(s.token_count || 0), "col-status");
            const status = s.active ? "ACTIVE" : "DISABLED";
            addTd(status, "col-status");
            addTd(s.updated_at ? new Date(s.updated_at).toLocaleDateString() : "—", "col-created");
            tr.addEventListener("click", () => openShardDetail(s.id));
            tbody.appendChild(tr);
        }
    }

    function renderToolChecklist() {
        const container = document.getElementById("shard-tools");
        container.innerHTML = "";
        if (!shardsState.toolCatalog.length) {
            const m = document.createElement("div");
            m.className = "micro";
            m.textContent = "NO TOOLS REGISTERED ON THIS DEPLOY";
            container.appendChild(m);
            return;
        }
        for (const t of shardsState.toolCatalog) {
            const lbl = document.createElement("label");
            lbl.className = "shard-tool-option";
            lbl.dataset.name = t.name;
            lbl.dataset.writeCapable = t.write_capable ? "1" : "0";
            const cb = document.createElement("input");
            cb.type = "checkbox";
            cb.value = t.name;
            cb.name = "tool";
            lbl.appendChild(cb);
            const span = document.createElement("span");
            span.textContent = t.name;
            lbl.appendChild(span);
            if (t.write_capable) {
                const badge = document.createElement("span");
                badge.className = "micro shard-tool-write-badge";
                badge.textContent = " (WRITE)";
                lbl.appendChild(badge);
            }
            container.appendChild(lbl);
        }
        updateToolAvailability();
    }

    // Disable write-capable tools when persistence=ephemeral. Uses the
    // checkbox's data-write-capable attribute (written by
    // renderToolChecklist from the backend's annotation) so the rule
    // stays in lockstep with shards.IsWriteCapable in Go.
    function updateToolAvailability() {
        const eph = document.querySelector('input[name="persistence"]:checked').value === "ephemeral";
        for (const lbl of document.querySelectorAll(".shard-tool-option")) {
            const write = lbl.dataset.writeCapable === "1";
            const cb = lbl.querySelector('input[type="checkbox"]');
            if (eph && write) {
                cb.checked = false;
                cb.disabled = true;
                lbl.classList.add("is-disabled");
                lbl.title = "write-capable tools are forbidden on ephemeral shards";
            } else {
                cb.disabled = false;
                lbl.classList.remove("is-disabled");
                lbl.title = "";
            }
        }
        // Programmatic unchecks above don't fire change events, so
        // re-derive the skill warnings explicitly.
        updateSkillpackToolWarnings();
    }

    async function openShardCreate() {
        await refreshShardBookCatalog();
        await refreshSkillpackCatalog();
        shardsState.mode = "create";
        shardsState.currentShard = null;
        document.getElementById("shard-detail-title").textContent = "New shard";
        setError("shard-form-error", null);

        const form = document.getElementById("shard-form");
        form.reset();
        // Re-set the radios to defaults after reset (some browsers
        // drop the defaults on reset if they're not in the form HTML
        // attribute).
        document.querySelector('input[name="persistence"][value="ephemeral"]').checked = true;
        document.querySelector('input[name="visibility"][value="isolated"]').checked = true;
        document.getElementById("shard-max-tokens").value = "2048";
        document.getElementById("shard-temperature").value = "0.7";
        document.getElementById("shard-id").disabled = false;

        // Hide edit-only buttons + tokens subview on create.
        document.getElementById("shard-disable").hidden = true;
        document.getElementById("shard-enable").hidden = true;
        document.getElementById("shard-delete").hidden = true;
        document.getElementById("shard-chat").hidden = true;
        document.getElementById("shard-tokens-section").hidden = true;
        document.getElementById("shard-passkeys-section").hidden = true;

        for (const cb of document.querySelectorAll(".shard-tool-option input")) cb.checked = false;
        updateToolAvailability();

        // SHARD-AUTH-SPEC scoping defaults: console-off until the
        // owner explicitly opts in, but chat/api on (matches the
        // DB column defaults).
        document.getElementById("shard-console-access").checked = false;
        document.getElementById("shard-chat-enabled").checked = true;
        document.getElementById("shard-api-enabled").checked = true;
        for (const cb of document.querySelectorAll('#shard-console-panels input')) cb.checked = false;
        renderShardBookAccess([]);
        renderShardSkillpacks([]);
        document.getElementById("shard-session-max-age").value = "";

        document.getElementById("shard-detail").hidden = false;
        document.getElementById("panel-shards").classList.add("is-editing");
        updateShardPromptState();
    }

    async function openShardDetail(id) {
        await refreshShardBookCatalog();
        await refreshSkillpackCatalog();
        try {
            const s = await apiJSON("/console/api/shards/" + encodeURIComponent(id));
            shardsState.mode = "edit";
            shardsState.currentShard = s;
            document.getElementById("shard-detail-title").textContent = s.name || s.id;
            setError("shard-form-error", null);

            document.getElementById("shard-id").value = s.id;
            document.getElementById("shard-id").disabled = true; // immutable
            document.getElementById("shard-name").value = s.name || "";
            document.getElementById("shard-description").value = s.description || "";
            document.querySelector('input[name="persistence"][value="' + s.persistence + '"]').checked = true;
            document.querySelector('input[name="visibility"][value="' + s.visibility + '"]').checked = true;
            document.getElementById("shard-scope").value = s.scope_tag || "";
            document.getElementById("shard-model").value = s.model_preference || "";
            document.getElementById("shard-tier").value = s.tier_preference || "";
            document.getElementById("shard-max-tokens").value = String(s.max_tokens || 2048);
            document.getElementById("shard-temperature").value = String(s.temperature != null ? s.temperature : 0.7);
            document.getElementById("shard-prompt").value = s.system_prompt || "";
            document.getElementById("shard-input-schema").value = s.input_schema ? JSON.stringify(s.input_schema, null, 2) : "";
            document.getElementById("shard-output-schema").value = s.output_schema ? JSON.stringify(s.output_schema, null, 2) : "";

            const allowed = new Set(s.tool_allowlist || []);
            for (const cb of document.querySelectorAll(".shard-tool-option input")) {
                cb.checked = allowed.has(cb.value);
            }
            updateToolAvailability();

            // SHARD-AUTH-SPEC scoping fields.
            document.getElementById("shard-console-access").checked = !!s.console_access;
            document.getElementById("shard-chat-enabled").checked = s.chat_enabled !== false;
            document.getElementById("shard-api-enabled").checked = s.api_enabled !== false;
            const panels = new Set(s.console_panels || []);
            for (const cb of document.querySelectorAll('#shard-console-panels input')) {
                cb.checked = panels.has(cb.value);
            }
            renderShardBookAccess(s.book_access || []);
            // Bound imported skills live in their own join table —
            // fetched separately; tolerate a 503/404 as "none".
            let boundSkills = [];
            try {
                const bound = await apiJSON("/console/api/shards/" + encodeURIComponent(id) + "/skills");
                boundSkills = ((bound && bound.items) || []).map((p) => p.id);
            } catch (e) { /* library not configured */ }
            renderShardSkillpacks(boundSkills);
            document.getElementById("shard-session-max-age").value = s.session_max_age != null ? String(s.session_max_age) : "";

            document.getElementById("shard-disable").hidden = !s.active;
            document.getElementById("shard-enable").hidden = s.active;
            document.getElementById("shard-delete").hidden = false;
            document.getElementById("shard-chat").hidden = !(s.active && s.chat_enabled !== false);
            document.getElementById("shard-tokens-section").hidden = false;
            loadShardTokens(s.id);
            document.getElementById("shard-passkeys-section").hidden = false;
            loadShardPasskeys(s.id);

            document.getElementById("shard-detail").hidden = false;
        document.getElementById("panel-shards").classList.add("is-editing");
        updateShardPromptState();
        } catch (e) {
            setError("shards-error", e);
        }
    }

    function closeShardDetail() {
        document.getElementById("shard-detail").hidden = true;
        document.getElementById("panel-shards").classList.remove("is-editing");
        shardsState.currentShard = null;
    }

    // updateShardPromptState toggles the system_prompt textarea
    // between "required + active" and "optional + zombie" based on
    // the chat / api kill-switches. Both off ⇒ console-only shard
    // (a kiosk that never runs inference); skip the prompt
    // requirement and grey the field out so the form stays
    // submittable. Either on ⇒ we'll route real prompts through
    // here, so it's required and editable.
    function updateShardPromptState() {
        const chatEl = document.getElementById("shard-chat-enabled");
        const apiEl = document.getElementById("shard-api-enabled");
        const promptEl = document.getElementById("shard-prompt");
        if (!chatEl || !apiEl || !promptEl) return;
        const zombie = !chatEl.checked && !apiEl.checked;
        if (zombie) {
            promptEl.required = false;
            promptEl.disabled = true;
            promptEl.placeholder = "zombie shard — console only";
            promptEl.value = "";
        } else {
            promptEl.required = true;
            promptEl.disabled = false;
            promptEl.placeholder = "You are a specialized agent that...";
        }
    }

    async function submitShardForm(e) {
        e.preventDefault();
        setError("shard-form-error", null);

        const persistence = document.querySelector('input[name="persistence"]:checked').value;
        const visibility = document.querySelector('input[name="visibility"]:checked').value;
        const toolAllowlist = Array.from(
            document.querySelectorAll('.shard-tool-option input:checked')
        ).map(cb => cb.value);

        const model = document.getElementById("shard-model").value.trim();
        const tier = document.getElementById("shard-tier").value;
        if (model && tier) {
            setError("shard-form-error", new Error("model preference and tier preference are mutually exclusive"));
            return;
        }

        // SHARD-AUTH-SPEC scoping fields. Empty arrays explicitly
        // mean "no panels / no books"; absence means "inherit
        // everything", which the form represents as no checkboxes
        // ticked + a blank book-access input. Server-side, the
        // patch shape uses pointer fields so we send arrays only
        // when the user has actually picked a subset.
        const consolePanels = Array.from(
            document.querySelectorAll('#shard-console-panels input:checked')
        ).map(cb => cb.value);
        const bookAccess = Array.from(
            document.querySelectorAll('#shard-book-access input:checked')
        ).map(cb => cb.value);
        const sessionMaxRaw = document.getElementById("shard-session-max-age").value.trim();
        const sessionMaxAge = sessionMaxRaw ? parseInt(sessionMaxRaw, 10) : null;

        const body = {
            id: document.getElementById("shard-id").value.trim(),
            name: document.getElementById("shard-name").value.trim(),
            description: document.getElementById("shard-description").value.trim(),
            persistence: persistence,
            visibility: visibility,
            scope_tag: document.getElementById("shard-scope").value.trim(),
            tool_allowlist: toolAllowlist,
            system_prompt: document.getElementById("shard-prompt").value,
            model_preference: model,
            tier_preference: tier,
            max_tokens: parseInt(document.getElementById("shard-max-tokens").value, 10) || 2048,
            temperature: parseFloat(document.getElementById("shard-temperature").value) || 0,
            console_access: document.getElementById("shard-console-access").checked,
            chat_enabled: document.getElementById("shard-chat-enabled").checked,
            api_enabled: document.getElementById("shard-api-enabled").checked,
            // Always send the arrays — empty means "inherit", which
            // the backend treats as nil (matches len()==0 path in
            // loadShardPermissions).
            console_panels: consolePanels,
            book_access: bookAccess,
        };
        if (sessionMaxAge && sessionMaxAge > 0) {
            body.session_max_age = sessionMaxAge;
        }

        // Parse optional schema blobs; empty string stays empty.
        const inSchema = document.getElementById("shard-input-schema").value.trim();
        if (inSchema) {
            try { body.input_schema = JSON.parse(inSchema); }
            catch (err) { setError("shard-form-error", new Error("input_schema: invalid JSON")); return; }
        }
        const outSchema = document.getElementById("shard-output-schema").value.trim();
        if (outSchema) {
            try { body.output_schema = JSON.parse(outSchema); }
            catch (err) { setError("shard-form-error", new Error("output_schema: invalid JSON")); return; }
        }

        // Imported-skill bindings ride along with the form but live in
        // their own join table. Snapshot the checklist BEFORE the
        // save (openShardDetail re-renders it) and only sync when the
        // checklist actually rendered packages — an empty/unconfigured
        // library has nothing to replace.
        const skillpackInputs = document.querySelectorAll("#shard-skillpacks input");
        const skillIDs = Array.from(skillpackInputs)
            .filter((cb) => cb.checked && !cb.disabled)
            .map((cb) => cb.value);
        const syncSkills = async (shardID) => {
            if (!skillpackInputs.length) return;
            await apiJSON("/console/api/shards/" + encodeURIComponent(shardID) + "/skills", {
                method: "PUT",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ skill_ids: skillIDs }),
            });
        };

        try {
            if (shardsState.mode === "create") {
                const created = await apiJSON("/console/api/shards", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(body),
                });
                await syncSkills(created.id);
                toast("Shard " + created.id + " created", "success");
                await openShardDetail(created.id);
            } else {
                const id = shardsState.currentShard.id;
                // Server expects PATCH shape with every top-level field
                // optional; we just send the whole body and the server
                // overwrites matching fields.
                delete body.id; // URL carries the id
                await apiJSON("/console/api/shards/" + encodeURIComponent(id), {
                    method: "PATCH",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(body),
                });
                await syncSkills(id);
                toast("Shard " + id + " updated", "success");
                await openShardDetail(id);
            }
            loadShards();
        } catch (err) {
            setError("shard-form-error", err);
        }
    }

    async function toggleShardDisabled(disable) {
        if (!shardsState.currentShard) return;
        const id = shardsState.currentShard.id;
        const action = disable ? "disable" : "enable";
        try {
            await apiJSON("/console/api/shards/" + encodeURIComponent(id) + "/" + action, { method: "POST" });
            toast("Shard " + action + "d", "success");
            await openShardDetail(id);
            loadShards();
        } catch (e) {
            setError("shard-form-error", e);
        }
    }

    async function deleteCurrentShard() {
        if (!shardsState.currentShard) return;
        const id = shardsState.currentShard.id;
        if (!confirm("Delete shard \"" + id + "\"? Tokens will be revoked. This cannot be undone.")) return;
        try {
            await apiJSON("/console/api/shards/" + encodeURIComponent(id), { method: "DELETE" });
            toast("Shard " + id + " deleted", "success");
            closeShardDetail();
            loadShards();
        } catch (e) {
            setError("shard-form-error", e);
        }
    }

    // ── Token subview ──────────────────────────────────────────

    async function loadShardTokens(shardID) {
        setError("shard-tokens-error", null);
        try {
            const data = await apiJSON("/console/api/shards/" + encodeURIComponent(shardID) + "/tokens");
            renderShardTokensTable(data.items || []);
        } catch (e) {
            setError("shard-tokens-error", e);
        }
    }

    function renderShardTokensTable(items) {
        const tbody = document.getElementById("shard-tokens-rows");
        tbody.innerHTML = "";
        if (!items.length) {
            const tr = document.createElement("tr");
            tr.className = "row-empty";
            const td = document.createElement("td");
            td.colSpan = 6;
            td.textContent = "NO TOKENS — MINT ONE TO INVOKE THIS SHARD";
            tr.appendChild(td);
            tbody.appendChild(tr);
            return;
        }
        for (const t of items) {
            const tr = document.createElement("tr");
            const addTd = (text, cls) => {
                const td = document.createElement("td");
                td.textContent = text || "—";
                if (cls) td.className = cls;
                tr.appendChild(td);
            };
            addTd(t.label, "col-name");
            addTd(t.token_prefix + "…", "col-id");
            addTd(t.created_at ? new Date(t.created_at).toLocaleString() : "—", "col-created");
            addTd(t.last_used_at ? new Date(t.last_used_at).toLocaleString() : "NEVER", "col-created");
            addTd(t.revoked_at ? "REVOKED" : "ACTIVE", "col-status");

            const actionsTd = document.createElement("td");
            actionsTd.className = "col-actions";
            if (!t.revoked_at) {
                const btn = document.createElement("button");
                btn.type = "button";
                btn.className = "btn-ghost btn-small";
                btn.textContent = "REVOKE";
                btn.addEventListener("click", (e) => {
                    e.stopPropagation();
                    revokeTokenByID(t.id);
                });
                actionsTd.appendChild(btn);
            }
            tr.appendChild(actionsTd);
            tbody.appendChild(tr);
        }
    }

    async function mintTokenForCurrent() {
        if (!shardsState.currentShard) return;
        const id = shardsState.currentShard.id;
        const label = prompt("Token label (e.g. 'cannonball-prod'):", "") || "";
        try {
            const data = await apiJSON("/console/api/shards/" + encodeURIComponent(id) + "/tokens", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ label: label.trim() }),
            });
            showTokenModal(data.plaintext);
            loadShardTokens(id);
        } catch (e) {
            setError("shard-tokens-error", e);
        }
    }

    async function revokeTokenByID(tokenID) {
        if (!confirm("Revoke this token? Invocations using it will return 403.")) return;
        try {
            await apiJSON("/console/api/shard_tokens/" + encodeURIComponent(tokenID) + "/revoke", {
                method: "POST",
            });
            toast("Token revoked", "success");
            if (shardsState.currentShard) {
                loadShardTokens(shardsState.currentShard.id);
            }
        } catch (e) {
            setError("shard-tokens-error", e);
        }
    }

    // ── Shard passkeys (SHARD-AUTH-SPEC Phase 1) ────────────────
    //
    // The four CRUD endpoints under /console/api/shards/{id}/passkeys
    // mirror the user passkey CRUD but key on shard_id. Enrollment is
    // standard WebAuthn registration with no authenticatorAttachment
    // hint — that lets the browser offer the cross-device QR flow
    // when the kiosk device isn't the one the owner is enrolling
    // from. The label modal collects a device name BEFORE the
    // ceremony fires, since the WebAuthn dialog can't be paused
    // mid-flight to ask.

    async function loadShardPasskeys(shardID) {
        setError("shard-passkeys-error", null);
        try {
            const data = await apiJSON("/console/api/shards/" + encodeURIComponent(shardID) + "/passkeys");
            renderShardPasskeysTable(data.items || []);
        } catch (e) {
            setError("shard-passkeys-error", e);
        }
    }

    function renderShardPasskeysTable(items) {
        const tbody = document.getElementById("shard-passkeys-rows");
        tbody.innerHTML = "";
        if (!items.length) {
            const tr = document.createElement("tr");
            tr.className = "row-empty";
            const td = document.createElement("td");
            td.colSpan = 6;
            td.textContent = "NO PASSKEYS — ADD ONE TO LET A DEVICE LOG IN AS THIS SHARD";
            tr.appendChild(td);
            tbody.appendChild(tr);
            return;
        }
        for (const p of items) {
            const tr = document.createElement("tr");
            const addTd = (text, cls) => {
                const td = document.createElement("td");
                td.textContent = text || "—";
                if (cls) td.className = cls;
                tr.appendChild(td);
            };
            addTd(p.label || "(unnamed)", "col-name");
            addTd((p.transports || []).join(", ") || "—", "col-id");
            addTd(p.created_by || "—", "col-created");
            addTd(p.created_at ? new Date(p.created_at).toLocaleString() : "—", "col-created");
            addTd(p.last_used_at ? new Date(p.last_used_at).toLocaleString() : "NEVER", "col-created");

            const actionsTd = document.createElement("td");
            actionsTd.className = "col-actions";
            const btn = document.createElement("button");
            btn.type = "button";
            btn.className = "btn-ghost btn-small";
            btn.textContent = "REVOKE";
            btn.addEventListener("click", (e) => {
                e.stopPropagation();
                revokeShardPasskey(p.id);
            });
            actionsTd.appendChild(btn);
            tr.appendChild(actionsTd);
            tbody.appendChild(tr);
        }
    }

    function openPasskeyLabelModal() {
        if (!shardsState.currentShard) return;
        const input = document.getElementById("shard-passkey-label-input");
        input.value = "";
        document.getElementById("shard-passkey-label-modal").hidden = false;
        setTimeout(() => input.focus(), 0);
    }

    function closePasskeyLabelModal() {
        document.getElementById("shard-passkey-label-modal").hidden = true;
    }

    async function enrollShardPasskey() {
        if (!shardsState.currentShard) return;
        const id = shardsState.currentShard.id;
        const label = (document.getElementById("shard-passkey-label-input").value || "").trim();
        if (!label) {
            toast("Label required", "error");
            return;
        }
        closePasskeyLabelModal();
        setError("shard-passkeys-error", null);
        try {
            // Begin — server returns PublicKeyCredentialCreationOptions
            // with a pending cookie that finish/{id} will consume.
            const creation = await apiJSON("/console/api/shards/" + encodeURIComponent(id) + "/passkeys/begin", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ label }),
            });
            const pk = creation.publicKey;
            pk.challenge = b64urlToBuf(pk.challenge);
            // user.id is the shard ID (UTF-8 string per
            // EncodeUserIDAsString=true on the backend).
            pk.user.id = new TextEncoder().encode(pk.user.id).buffer;
            if (Array.isArray(pk.excludeCredentials)) {
                for (const c of pk.excludeCredentials) {
                    c.id = b64urlToBuf(c.id);
                }
            }
            requireWebAuthn();
            const credential = await navigator.credentials.create({ publicKey: pk });
            if (!credential) throw new Error("authenticator returned no credential");

            const finishBody = {
                id: credential.id,
                rawId: bufToB64url(credential.rawId),
                type: credential.type,
                response: {
                    attestationObject: bufToB64url(credential.response.attestationObject),
                    clientDataJSON: bufToB64url(credential.response.clientDataJSON),
                },
                clientExtensionResults: credential.getClientExtensionResults
                    ? credential.getClientExtensionResults()
                    : {},
            };
            await apiJSON("/console/api/shards/" + encodeURIComponent(id) + "/passkeys/finish", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify(finishBody),
            });
            toast("Passkey enrolled", "success");
            loadShardPasskeys(id);
        } catch (e) {
            setError("shard-passkeys-error", e);
        }
    }

    async function revokeShardPasskey(passkeyID) {
        if (!shardsState.currentShard) return;
        if (!confirm("Revoke this passkey? Any session it minted is invalidated; the device will need a new passkey to log in again.")) return;
        const id = shardsState.currentShard.id;
        try {
            await apiJSON("/console/api/shards/" + encodeURIComponent(id) + "/passkeys/" + encodeURIComponent(passkeyID), {
                method: "DELETE",
            });
            toast("Passkey revoked", "success");
            loadShardPasskeys(id);
        } catch (e) {
            setError("shard-passkeys-error", e);
        }
    }

    // One-shot token modal. The plaintext is shown exactly once and
    // cleared from the DOM as soon as the modal closes.
    function showTokenModal(plaintext) {
        const modal = document.getElementById("token-modal");
        const valueEl = document.getElementById("token-modal-value");
        valueEl.textContent = plaintext;
        modal.hidden = false;
    }

    function closeTokenModal() {
        const modal = document.getElementById("token-modal");
        const valueEl = document.getElementById("token-modal-value");
        valueEl.textContent = "";
        modal.hidden = true;
    }

    async function copyTokenModalValue() {
        const valueEl = document.getElementById("token-modal-value");
        const plaintext = valueEl.textContent;
        if (!plaintext) return;
        try {
            await navigator.clipboard.writeText(plaintext);
            toast("Copied to clipboard — save it now", "success");
        } catch (e) {
            // Fallback: select the text so the user can copy manually.
            const range = document.createRange();
            range.selectNodeContents(valueEl);
            const sel = window.getSelection();
            sel.removeAllRanges();
            sel.addRange(range);
            toast("Select + copy manually — clipboard blocked", "info");
        }
    }

    window.familiarPanels = window.familiarPanels || {};
    window.familiarPanels.shards = { init: initShardsBrowser };
})();
