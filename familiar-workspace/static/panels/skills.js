// Skills catalog panel — FAMILIAR-CONSOLE-SPEC Phase C.
//
// Extracted from app.js as part of the panel modularization.
// Self-contained IIFE; pulls shared infrastructure from
// window.familiarAppHelpers and registers on
// window.familiarPanels so app.js's panel-switcher invokes
// init() lazily on first navigation.

(function () {
    "use strict";

    const helpers = window.familiarAppHelpers;
    if (!helpers) {
        console.error("skills: app helpers not loaded; panel disabled");
        return;
    }
    const { apiJSON, toast, setError } = helpers;

    // ── Skills catalog (FAMILIAR-CONSOLE-SPEC Phase C) ─────────────

    const skillsState = {
        initialized: false,
        expanded: new Set(),       // skill names currently expanded
        expandedTools: new Set(),  // "<skill>/<tool>" with schema visible
    };

    async function initSkillsCatalog() {
        if (skillsState.initialized) return;
        skillsState.initialized = true;
        const rescanBtn = document.getElementById("skillpacks-rescan");
        if (rescanBtn) rescanBtn.addEventListener("click", rescanSkillpacks);
        const importBtn = document.getElementById("skillpacks-import");
        if (importBtn) importBtn.addEventListener("click", openImportView);
        const myNewBtn = document.getElementById("myskills-new");
        if (myNewBtn) myNewBtn.addEventListener("click", () => openSkillEditor(null));
        document.getElementById("skill-editor-back").addEventListener("click", closeEditorView);
        document.getElementById("skill-editor-save").addEventListener("click", saveSkillEditor);
        document.getElementById("skill-import-back").addEventListener("click", closeImportView);
        document.getElementById("skillpack-import-go").addEventListener("click", previewImport);
        document.getElementById("skillpack-import-approve").addEventListener("click", approveImport);
        // What you approve is what you previewed: any source change
        // revokes the approve button until the next preview.
        document.getElementById("skillpack-import-file").addEventListener("change", invalidatePreview);
        document.getElementById("skillpack-import-url").addEventListener("input", invalidatePreview);
        await Promise.all([loadSkillsCatalog(), loadMySkills(), loadSkillpacks()]);
    }

    async function loadSkillsCatalog() {
        setError("skills-error", null);
        const container = document.getElementById("skills-list");
        try {
            const data = await apiJSON("/console/api/skills");
            renderSkillsCatalog(data.skills || []);
        } catch (e) {
            setError("skills-error", e);
            container.innerHTML = "";
        }
    }

    function renderSkillsCatalog(skills) {
        const container = document.getElementById("skills-list");
        container.innerHTML = "";
        if (!skills.length) {
            const m = document.createElement("div");
            m.className = "micro";
            m.textContent = "NO SKILLS REGISTERED ON THIS INSTANCE";
            container.appendChild(m);
            return;
        }
        for (const skill of skills) {
            container.appendChild(renderSkillEntry(skill));
        }
    }

    function renderSkillEntry(skill) {
        const card = document.createElement("article");
        card.className = "skill-card";

        const header = document.createElement("header");
        header.className = "skill-header";
        header.addEventListener("click", () => {
            if (skillsState.expanded.has(skill.name)) {
                skillsState.expanded.delete(skill.name);
            } else {
                skillsState.expanded.add(skill.name);
            }
            card.replaceWith(renderSkillEntry(skill));
        });

        const name = document.createElement("div");
        name.className = "skill-name";
        name.textContent = skill.name.toUpperCase();
        header.appendChild(name);

        const meta = document.createElement("div");
        meta.className = "skill-meta";
        const toolCount = (skill.tools || []).length;
        meta.textContent =
            (skill.version ? "v" + skill.version + " · " : "") +
            toolCount + " tool" + (toolCount === 1 ? "" : "s");
        header.appendChild(meta);

        const chevron = document.createElement("span");
        chevron.className = "skill-chevron";
        chevron.textContent = skillsState.expanded.has(skill.name) ? "▾" : "▸";
        header.appendChild(chevron);

        card.appendChild(header);

        const desc = document.createElement("p");
        desc.className = "skill-description";
        desc.textContent = skill.description || "";
        card.appendChild(desc);

        if (skillsState.expanded.has(skill.name)) {
            const toolsList = document.createElement("div");
            toolsList.className = "skill-tools-list";
            for (const tool of skill.tools || []) {
                toolsList.appendChild(renderToolRow(skill.name, tool));
            }
            card.appendChild(toolsList);
        }
        return card;
    }

    function renderToolRow(skillName, tool) {
        const key = skillName + "/" + tool.name;
        const row = document.createElement("div");
        row.className = "skill-tool-row";

        const header = document.createElement("div");
        header.className = "skill-tool-header";
        header.addEventListener("click", () => {
            if (skillsState.expandedTools.has(key)) {
                skillsState.expandedTools.delete(key);
            } else {
                skillsState.expandedTools.add(key);
            }
            row.replaceWith(renderToolRow(skillName, tool));
        });

        const name = document.createElement("code");
        name.className = "skill-tool-name";
        name.textContent = tool.name;
        header.appendChild(name);

        const chevron = document.createElement("span");
        chevron.className = "skill-chevron";
        chevron.textContent = skillsState.expandedTools.has(key) ? "▾" : "▸";
        header.appendChild(chevron);

        row.appendChild(header);

        const desc = document.createElement("p");
        desc.className = "skill-tool-description";
        desc.textContent = tool.description || "";
        row.appendChild(desc);

        if (skillsState.expandedTools.has(key)) {
            const schema = document.createElement("pre");
            schema.className = "skill-tool-schema";
            const code = document.createElement("code");
            try {
                code.textContent = JSON.stringify(tool.parameters || {}, null, 2);
            } catch (e) {
                code.textContent = String(tool.parameters);
            }
            schema.appendChild(code);
            row.appendChild(schema);
        }
        return row;
    }


    // ── Imported skills (SKILL-PACKAGES-SPEC Phase 2) ──────────────
    //
    // The library list is visible to every authed user (owners need
    // the catalog to know what they can bind to a shard); management
    // actions are admin-only and the buttons stay hidden otherwise —
    // the backend enforces 403 regardless.

    function isAdmin() {
        return document.body.dataset.role === "admin";
    }

    // ── My skills (USER-SKILLS-SPEC Phase A) ───────────────────────
    //
    // The caller's private library: any user imports / enables /
    // deletes their own skills, no admin involved. Usable by the
    // owner's shards; trusted-path exposure lands in Phase B.

    async function loadMySkills() {
        setError("myskills-error", null);
        const container = document.getElementById("myskills-list");
        if (!container) return;
        try {
            const data = await apiJSON("/console/api/skills/mine");
            renderMySkills(data.items || []);
        } catch (e) {
            if (e && /503|not configured/.test(String(e.message || e))) {
                container.innerHTML = "";
                return;
            }
            setError("myskills-error", e);
            container.innerHTML = "";
        }
    }

    function renderMySkills(items) {
        const container = document.getElementById("myskills-list");
        container.innerHTML = "";
        if (!items.length) {
            const m = document.createElement("div");
            m.className = "micro";
            m.textContent = "NO PERSONAL SKILLS YET — IMPORT A SKILL.MD PACKAGE TO GET STARTED";
            container.appendChild(m);
            return;
        }
        for (const pkg of items) {
            container.appendChild(renderMySkillCard(pkg));
        }
    }

    function renderMySkillCard(pkg) {
        const disabled = !!pkg.disabled_at;
        const card = document.createElement("article");
        card.className = "skill-card" + (disabled ? " is-disabled" : "");

        // is-static: these cards don't expand, so they must not carry
        // the catalog cards' click affordance.
        const header = document.createElement("header");
        header.className = "skill-header is-static";
        const name = document.createElement("div");
        name.className = "skill-name";
        name.textContent = pkg.name.toUpperCase();
        header.appendChild(name);

        // Identity metadata only — state (in chat / disabled) lives in
        // the footer controls, except DISABLED which explains the
        // card's dimming (same as the library cards).
        const meta = document.createElement("div");
        meta.className = "skill-meta";
        const badges = [(pkg.origin || "imported").toUpperCase()];
        if (pkg.version) badges.unshift("v" + pkg.version);
        if (pkg.has_scripts) badges.push("SCRIPTS (INERT)");
        if (disabled) badges.push("DISABLED");
        meta.textContent = badges.join(" · ");
        header.appendChild(meta);
        card.appendChild(header);

        const desc = document.createElement("p");
        desc.className = "skill-description";
        desc.textContent = pkg.description || "";
        card.appendChild(desc);

        // Authored skills were never imported — say "created".
        const fromScratch = pkg.origin === "authored" && !pkg.source_url;
        const prov = document.createElement("div");
        prov.className = "micro";
        prov.textContent =
            "digest " + (pkg.digest || "").slice(0, 12) +
            (pkg.source_url ? " · from " + pkg.source_url : "") +
            (pkg.imported_at
                ? " · " + (fromScratch ? "created " : "imported ") + new Date(pkg.imported_at).toLocaleDateString()
                : "");
        card.appendChild(prov);

        // Footer: the chat opt-in is STATE (checkbox, left); actions
        // are buttons (right).
        const footer = document.createElement("div");
        footer.className = "skill-footer";

        const check = document.createElement("div");
        check.className = "field-check";
        const label = document.createElement("label");
        const cb = document.createElement("input");
        cb.type = "checkbox";
        cb.checked = !!pkg.chat_enabled;
        cb.disabled = disabled;
        cb.addEventListener("change", () => toggleMySkillChat(pkg));
        label.appendChild(cb);
        label.appendChild(document.createTextNode(" Use in chat"));
        if (disabled) label.title = "Enable the skill first";
        check.appendChild(label);
        footer.appendChild(check);

        const row = document.createElement("div");
        row.className = "button-row";
        const mk = (label, danger, fn) => {
            const b = document.createElement("button");
            b.type = "button";
            b.className = "btn-ghost btn-small" + (danger ? " is-danger" : "");
            b.textContent = label;
            b.addEventListener("click", fn);
            row.appendChild(b);
        };
        if (pkg.origin === "authored") {
            mk("Edit", false, () => openSkillEditor(pkg));
        } else {
            mk("Duplicate to edit", false, () => duplicateMySkill(pkg));
        }
        mk("Export", false, () => exportMySkill(pkg));
        mk(disabled ? "Enable" : "Disable", false, () => toggleMySkill(pkg, !disabled));
        mk("Delete", true, () => deleteMySkill(pkg));
        footer.appendChild(row);
        card.appendChild(footer);
        return card;
    }

    // ── Sub-views (USER-SKILLS-SPEC Phase C) ────────────────────────
    //
    // The editor (user panel) and importer (admin panel) are full-page
    // views — the same list→detail pattern shards and scheduled use.
    // .is-editing on the owning panel root hides its home section.

    function openEditorView() {
        document.getElementById("skill-editor-view").hidden = false;
        document.getElementById("panel-skills").classList.add("is-editing");
    }
    function closeEditorView() {
        document.getElementById("skill-editor-view").hidden = true;
        document.getElementById("panel-skills").classList.remove("is-editing");
    }
    function closeImportView() {
        document.getElementById("skill-import-view").hidden = true;
        document.getElementById("panel-system-skills").classList.remove("is-editing");
    }

    // ── Skill editor ────────────────────────────────────────────────
    //
    // Authors a SKILL.md without the zip round-trip. The server
    // composes the frontmatter from name+description and refuses
    // anything that doesn't round-trip through the parser, so the
    // body here is pure markdown instructions.

    const editorState = { name: null }; // null = creating; set = editing that skill

    async function openSkillEditor(pkg) {
        setError("skill-editor-error", null);
        const nameInput = document.getElementById("skill-editor-name");
        const descInput = document.getElementById("skill-editor-desc");
        const bodyInput = document.getElementById("skill-editor-body");
        if (!pkg) {
            editorState.name = null;
            document.getElementById("skill-editor-title").textContent = "New skill";
            nameInput.value = "";
            nameInput.disabled = false;
            descInput.value = "";
            bodyInput.value = "";
            openEditorView();
            nameInput.focus();
            return;
        }
        try {
            const c = await apiJSON("/console/api/skills/mine/" + encodeURIComponent(pkg.id) + "/content");
            editorState.name = c.name;
            document.getElementById("skill-editor-title").textContent = "Edit " + c.name;
            nameInput.value = c.name;
            nameInput.disabled = true; // the name is the identity — fixed after creation
            descInput.value = c.description || "";
            bodyInput.value = c.body || "";
            openEditorView();
            bodyInput.focus();
        } catch (e) {
            setError("myskills-error", e);
        }
    }

    async function saveSkillEditor() {
        setError("skill-editor-error", null);
        const name = editorState.name || document.getElementById("skill-editor-name").value.trim();
        const description = document.getElementById("skill-editor-desc").value.trim();
        const body = document.getElementById("skill-editor-body").value;
        if (!name) return setError("skill-editor-error", new Error("name is required"));
        if (!description) return setError("skill-editor-error", new Error("description is required"));
        if (!body.trim()) return setError("skill-editor-error", new Error("instructions are required"));
        try {
            const pkg = await apiJSON("/console/api/skills/mine/" + encodeURIComponent(name), {
                method: "PUT",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ description, body }),
            });
            toast("Skill " + pkg.name + " saved", "success");
            closeEditorView();
            await loadMySkills();
        } catch (e) {
            setError("skill-editor-error", e);
        }
    }

    async function duplicateMySkill(pkg) {
        const suggested = pkg.name + "-mine";
        const name = prompt(
            'Duplicate "' + pkg.name + '" as an editable skill.\n\nName for your copy ' +
            "(lowercase letters, digits, hyphens):", suggested);
        if (!name) return;
        setError("myskills-error", null);
        try {
            const copy = await apiJSON("/console/api/skills/mine/" + encodeURIComponent(pkg.id) + "/duplicate", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ name: name.trim() }),
            });
            toast("Duplicated as " + copy.name, "success");
            await loadMySkills();
        } catch (e) {
            setError("myskills-error", e);
        }
    }

    // exportMySkill downloads the zip via fetch→blob so the session
    // cookie rides along and the filename is honored.
    async function exportMySkill(pkg) {
        setError("myskills-error", null);
        try {
            const resp = await fetch("/console/api/skills/mine/" + encodeURIComponent(pkg.id) + "/export", {
                credentials: "include",
            });
            if (!resp.ok) throw new Error("export failed: HTTP " + resp.status);
            const blob = await resp.blob();
            const a = document.createElement("a");
            a.href = URL.createObjectURL(blob);
            a.download = pkg.name + ".zip";
            document.body.appendChild(a);
            a.click();
            a.remove();
            URL.revokeObjectURL(a.href);
        } catch (e) {
            setError("myskills-error", e);
        }
    }

    // toggleMySkillChat flips the trusted-path opt-in. Enabling a
    // skill with FOREIGN content gets a provenance warning: imported
    // skills, and duplicates of imported skills (origin becomes
    // 'authored' for editability but source_url survives the copy).
    // From-scratch authored skills flip silently — the user wrote them.
    async function toggleMySkillChat(pkg) {
        const enabling = !pkg.chat_enabled;
        if (enabling && (pkg.origin !== "authored" || pkg.source_url)) {
            const src = pkg.source_url ? "from " + pkg.source_url : "from an imported package";
            if (!confirm(
                'Use "' + pkg.name + '" in your chats?\n\n' +
                "This skill was imported " + src + ". Its instructions will be read by " +
                "your assistant during normal conversations, where it can use your full " +
                "toolset (memory, notes, web). Only enable skills you trust.")) {
                // The checkbox already flipped visually — re-render to
                // revert it.
                await loadMySkills();
                return;
            }
        }
        setError("myskills-error", null);
        try {
            await apiJSON("/console/api/skills/mine/" + encodeURIComponent(pkg.id) + "/chat", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ enabled: enabling }),
            });
            toast("Skill " + pkg.name + (enabling ? " available in chat" : " removed from chat"), "success");
            await loadMySkills();
        } catch (e) {
            setError("myskills-error", e);
        }
    }

    async function toggleMySkill(pkg, disable) {
        setError("myskills-error", null);
        try {
            await apiJSON(
                "/console/api/skills/mine/" + encodeURIComponent(pkg.id) + "/" + (disable ? "disable" : "enable"),
                { method: "POST" },
            );
            toast("Skill " + pkg.name + " " + (disable ? "disabled" : "enabled"), "success");
            await loadMySkills();
        } catch (e) {
            setError("myskills-error", e);
        }
    }

    async function deleteMySkill(pkg) {
        if (!confirm('Delete your skill "' + pkg.name + '"? Its files are removed and any shard bindings drop. This cannot be undone.')) return;
        setError("myskills-error", null);
        try {
            await apiJSON("/console/api/skills/mine/" + encodeURIComponent(pkg.id), { method: "DELETE" });
            toast("Skill " + pkg.name + " deleted", "success");
            await loadMySkills();
        } catch (e) {
            setError("myskills-error", e);
        }
    }

    async function loadSkillpacks() {
        setError("skillpacks-error", null);
        const container = document.getElementById("skillpacks-list");
        if (!container) return;
        try {
            const data = await apiJSON("/console/api/skillpacks");
            renderSkillpacks(data.items || []);
        } catch (e) {
            // 503 = the library isn't configured on this deploy —
            // show a quiet note rather than an error banner.
            if (e && /503|not configured/.test(String(e.message || e))) {
                container.innerHTML = "";
                const m = document.createElement("div");
                m.className = "micro";
                m.textContent = "IMPORTED SKILLS NOT CONFIGURED ON THIS DEPLOY";
                container.appendChild(m);
                return;
            }
            setError("skillpacks-error", e);
            container.innerHTML = "";
        }
    }

    function renderSkillpacks(items) {
        const container = document.getElementById("skillpacks-list");
        container.innerHTML = "";
        if (!items.length) {
            const m = document.createElement("div");
            m.className = "micro";
            m.textContent = "LIBRARY EMPTY — DROP A SKILL DIRECTORY INTO THE LIBRARY FOLDER AND RESCAN";
            container.appendChild(m);
            return;
        }
        for (const pkg of items) {
            container.appendChild(renderSkillpackCard(pkg));
        }
    }

    function renderSkillpackCard(pkg) {
        const disabled = !!pkg.disabled_at;
        const card = document.createElement("article");
        card.className = "skill-card" + (disabled ? " is-disabled" : "");

        const header = document.createElement("header");
        header.className = "skill-header is-static";

        const name = document.createElement("div");
        name.className = "skill-name";
        name.textContent = pkg.name.toUpperCase();
        header.appendChild(name);

        const meta = document.createElement("div");
        meta.className = "skill-meta";
        // Builtins are versioned gateway source — "UNSIGNED" would be
        // misleading, so the origin badge replaces the signature one
        // (same slot the My-skills cards use for origin).
        const builtin = pkg.origin === "builtin";
        const badges = [builtin ? "BUILT-IN" : pkg.signature_status.toUpperCase()];
        if (pkg.version) badges.unshift("v" + pkg.version);
        if (pkg.has_scripts) badges.push("SCRIPTS (INERT)");
        if (pkg.has_wasm) badges.push("WASM");
        if (disabled) badges.push("DISABLED");
        meta.textContent = badges.join(" · ");
        header.appendChild(meta);

        card.appendChild(header);

        const desc = document.createElement("p");
        desc.className = "skill-description";
        desc.textContent = pkg.description || "";
        card.appendChild(desc);

        // Advisory allowed-tools mapping (spec decision: map what
        // matches, note the rest).
        const matched = pkg.tools_matched || [];
        const unmatched = pkg.tools_unmatched || [];
        if (matched.length || unmatched.length) {
            const tools = document.createElement("div");
            tools.className = "micro";
            const parts = [];
            if (matched.length) parts.push("requests tools: " + matched.join(", "));
            if (unmatched.length) parts.push("not available here: " + unmatched.join(", "));
            tools.textContent = parts.join(" — ");
            card.appendChild(tools);
        }

        // Builtins were never imported — say where they came from.
        const prov = document.createElement("div");
        prov.className = "micro";
        prov.textContent =
            "digest " + (pkg.digest || "").slice(0, 12) +
            (pkg.source_url ? " · from " + pkg.source_url : "") +
            (builtin
                ? " · shipped with the gateway"
                : (pkg.imported_at ? " · imported " + new Date(pkg.imported_at).toLocaleDateString() : ""));
        card.appendChild(prov);

        if (isAdmin()) {
            const row = document.createElement("div");
            row.className = "button-row";
            const mk = (label, danger, fn) => {
                const b = document.createElement("button");
                b.type = "button";
                b.className = "btn-ghost btn-small" + (danger ? " is-danger" : "");
                b.textContent = label;
                b.addEventListener("click", fn);
                row.appendChild(b);
            };
            mk(disabled ? "Enable" : "Disable", false, () => toggleSkillpack(pkg, !disabled));
            if (builtin) {
                // Delete would just undo itself at the next boot sync,
                // so it's refused (backend too) — Disable is the off
                // switch.
                const del = document.createElement("button");
                del.type = "button";
                del.className = "btn-ghost btn-small is-danger";
                del.textContent = "Delete";
                del.disabled = true;
                del.title = "built-in — re-syncs at boot";
                row.appendChild(del);
                // Trusted-chat exposure, same footer layout as the
                // My-skills cards. No provenance warning (unlike
                // toggleMySkillChat): the body is first-party source.
                const footer = document.createElement("div");
                footer.className = "skill-footer";
                const check = document.createElement("div");
                check.className = "field-check";
                const label = document.createElement("label");
                const cb = document.createElement("input");
                cb.type = "checkbox";
                cb.checked = !!pkg.chat_enabled;
                cb.disabled = disabled;
                // The checkbox is the source of truth for the desired
                // state — deriving it from pkg.chat_enabled goes stale
                // after a failed POST and the next click re-sends the
                // original value, landing the server opposite the UI.
                cb.addEventListener("change", () => toggleSkillpackChat(pkg, cb));
                label.appendChild(cb);
                label.appendChild(document.createTextNode(" Use in chat"));
                if (disabled) label.title = "Enable the skill first";
                check.appendChild(label);
                footer.appendChild(check);
                footer.appendChild(row);
                card.appendChild(footer);
            } else {
                mk("Delete", true, () => deleteSkillpack(pkg));
                card.appendChild(row);
            }
        }
        return card;
    }

    async function rescanSkillpacks() {
        setError("skillpacks-error", null);
        try {
            const res = await apiJSON("/console/api/skillpacks/rescan", { method: "POST" });
            const errs = res.errors || [];
            const missing = res.missing || [];
            toast(
                "Rescan: " + (res.added || 0) + " added, " + (res.updated || 0) + " updated" +
                (missing.length ? ", " + missing.length + " missing (disabled)" : "") +
                (errs.length ? ", " + errs.length + " error(s)" : ""),
                errs.length ? "error" : "success",
            );
            if (missing.length) {
                setError("skillpacks-error", new Error(
                    "Disabled — directory gone from the library: " + missing.join(", ")));
            }
            if (errs.length) setError("skillpacks-error", new Error(errs.join("; ")));
            await loadSkillpacks();
        } catch (e) {
            setError("skillpacks-error", e);
        }
    }

    // ── Import with preview-then-approve ───────────────────────────
    //
    // Admin-only: zip / URL packages land in the instance library
    // (#panel-system-skills). Users author markdown skills instead —
    // there is no user-facing zip import. The endpoint dry-runs by
    // default; Approve sends confirm=true.

    const importState = { previewed: false };

    function importEndpoint() {
        return "/console/api/skillpacks/import";
    }

    function openImportView() {
        document.getElementById("skillpack-import-file").value = "";
        document.getElementById("skillpack-import-url").value = "";
        invalidatePreview();
        setError("skillpack-import-error", null);
        document.getElementById("skill-import-view").hidden = false;
        document.getElementById("panel-system-skills").classList.add("is-editing");
    }

    function invalidatePreview() {
        importState.previewed = false;
        const prev = document.getElementById("skillpack-import-preview");
        prev.hidden = true;
        prev.innerHTML = "";
        document.getElementById("skillpack-import-approve").hidden = true;
        document.getElementById("skillpack-import-go").hidden = false;
    }

    // buildImportRequest assembles the apiJSON options for the import
    // endpoint from whichever source is filled in. Multipart for a
    // file (the browser sets the boundary — no manual Content-Type),
    // JSON for a URL.
    function buildImportRequest(confirm) {
        const fileInput = document.getElementById("skillpack-import-file");
        const url = document.getElementById("skillpack-import-url").value.trim();
        const file = fileInput.files && fileInput.files[0];
        if (file && url) throw new Error("pick a file OR a URL, not both");
        if (file) {
            const fd = new FormData();
            fd.append("file", file);
            fd.append("confirm", confirm ? "true" : "false");
            return { method: "POST", body: fd };
        }
        if (url) {
            return {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ url, confirm: !!confirm }),
            };
        }
        throw new Error("choose a zip file or enter a URL");
    }

    async function previewImport() {
        setError("skillpack-import-error", null);
        try {
            const res = await apiJSON(importEndpoint(), buildImportRequest(false));
            renderImportPreview(res);
            importState.previewed = true;
            document.getElementById("skillpack-import-approve").hidden = false;
            document.getElementById("skillpack-import-go").hidden = true;
        } catch (e) {
            invalidatePreview();
            setError("skillpack-import-error", e);
        }
    }

    async function approveImport() {
        if (!importState.previewed) return;
        setError("skillpack-import-error", null);
        try {
            const pkg = await apiJSON(importEndpoint(), buildImportRequest(true));
            toast("Skill " + pkg.name + " imported", "success");
            closeImportView();
            await loadSkillpacks();
        } catch (e) {
            setError("skillpack-import-error", e);
        }
    }

    // renderImportPreview paints the dry-run payload. Everything is
    // textContent — package metadata is untrusted remote input.
    function renderImportPreview(res) {
        const host = document.getElementById("skillpack-import-preview");
        host.innerHTML = "";
        const fm = res.frontmatter || {};

        const title = document.createElement("div");
        title.className = "skill-name";
        title.textContent = (fm.name || "?").toUpperCase();
        host.appendChild(title);

        const badges = ["UNSIGNED"];
        if (res.has_scripts) badges.push("SCRIPTS (INERT)");
        if (res.has_wasm) badges.push("WASM");
        const meta = document.createElement("div");
        meta.className = "skill-meta";
        meta.textContent = badges.join(" · ") + " · digest " + (res.digest || "").slice(0, 12);
        host.appendChild(meta);

        const desc = document.createElement("p");
        desc.className = "skill-description";
        desc.textContent = fm.description || "";
        host.appendChild(desc);

        const matched = res.tools_matched || [];
        const unmatched = res.tools_unmatched || [];
        if (matched.length || unmatched.length) {
            const tools = document.createElement("div");
            tools.className = "micro";
            const parts = [];
            if (matched.length) parts.push("requests tools: " + matched.join(", "));
            if (unmatched.length) parts.push("not available here: " + unmatched.join(", "));
            tools.textContent = parts.join(" — ");
            host.appendChild(tools);
        }

        if (res.body_preview) {
            const pre = document.createElement("pre");
            pre.className = "skillpack-import-body";
            pre.textContent = res.body_preview;
            host.appendChild(pre);
        }
        host.hidden = false;
    }

    // toggleSkillpackChat flips trusted-path exposure for a built-in
    // package. Builtin-only affordance: other instance skills stay
    // shard-only in v1 (USER-SKILLS-SPEC §3.2) and the backend refuses
    // them regardless.
    async function toggleSkillpackChat(pkg, cb) {
        const enabling = cb.checked;
        setError("skillpacks-error", null);
        try {
            await apiJSON("/console/api/skillpacks/" + encodeURIComponent(pkg.id) + "/chat", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ enabled: enabling }),
            });
            pkg.chat_enabled = enabling;
            toast("Skill " + pkg.name + (enabling ? " available in chat" : " removed from chat"), "success");
            await loadSkillpacks();
        } catch (e) {
            // Revert the checkbox so the UI keeps telling the truth
            // about the server's state.
            cb.checked = !enabling;
            setError("skillpacks-error", e);
        }
    }

    async function toggleSkillpack(pkg, disable) {
        setError("skillpacks-error", null);
        try {
            await apiJSON(
                "/console/api/skillpacks/" + encodeURIComponent(pkg.id) + "/" + (disable ? "disable" : "enable"),
                { method: "POST" },
            );
            toast("Skill " + pkg.name + " " + (disable ? "disabled" : "enabled"), "success");
            await loadSkillpacks();
        } catch (e) {
            setError("skillpacks-error", e);
        }
    }

    async function deleteSkillpack(pkg) {
        if (!confirm('Delete imported skill "' + pkg.name + '"? Its files are removed from the library and any shard bindings drop. This cannot be undone.')) return;
        setError("skillpacks-error", null);
        try {
            await apiJSON("/console/api/skillpacks/" + encodeURIComponent(pkg.id), { method: "DELETE" });
            toast("Skill " + pkg.name + " deleted", "success");
            await loadSkillpacks();
        } catch (e) {
            setError("skillpacks-error", e);
        }
    }

    // The one module drives both the user Skills panel (My skills) and
    // the admin System skills panel (catalog + imported library). init
    // is idempotent (skillsState.initialized), so either section opening
    // first wires both panels' controls.
    window.familiarPanels = window.familiarPanels || {};
    window.familiarPanels["skills"] = { init: initSkillsCatalog };
    window.familiarPanels["system-skills"] = { init: initSkillsCatalog };
})();
