// Scheduled-actions panel — SCHEDULED-ACTIONS-SPEC Phase 1.
// List / create / edit / toggle / run-now / run history against
// /console/api/actions. Same IIFE + window.familiarPanels pattern as
// shards.js; reaches into window.familiarAppHelpers for apiJSON +
// toast + setError.

(function () {
    "use strict";

    const helpers = window.familiarAppHelpers;
    if (!helpers) {
        console.error("scheduled: app helpers not loaded; panel disabled");
        return;
    }
    const { apiJSON, toast, setError } = helpers;

    const state = {
        initialized: false,
        current: null, // full action DTO in edit mode; null in create mode
    };

    function $(id) { return document.getElementById(id); }

    async function init() {
        if (!state.initialized) {
            state.initialized = true;
            $("actions-refresh").addEventListener("click", loadActions);
            $("actions-new").addEventListener("click", openCreate);
            $("action-detail-back").addEventListener("click", closeDetail);
            $("action-form").addEventListener("submit", submitForm);
            $("action-run-now").addEventListener("click", runNow);
            $("action-enable").addEventListener("click", () => toggleEnabled(true));
            $("action-disable").addEventListener("click", () => toggleEnabled(false));
            $("action-delete").addEventListener("click", deleteCurrent);
            $("action-target-kind").addEventListener("change", updateTargetInputs);
            $("action-target-book").addEventListener("change", loadPageChoices);
            $("action-trigger").addEventListener("change", updateTriggerInputs);
            for (const a of document.querySelectorAll(".action-cron-preset")) {
                a.addEventListener("click", (e) => {
                    e.preventDefault();
                    $("action-cron").value = a.dataset.cron;
                });
            }
        }
        closeDetail();
        loadActions();
    }

    async function loadActions() {
        setError("actions-error", null);
        try {
            const data = await apiJSON("/console/api/actions");
            renderRows(data.items || []);
        } catch (e) {
            setError("actions-error", e);
        }
    }

    function describeSchedule(a) {
        if (a.trigger_kind === "page_saved") return "on save: " + (a.watch_book_slug || "?");
        if (a.trigger_kind === "webhook") return "webhook";
        if (a.cron) return a.cron + (a.timezone && a.timezone !== "UTC" ? " (" + a.timezone + ")" : "");
        if (a.run_at) return "once @ " + new Date(a.run_at).toLocaleString();
        return "—";
    }

    function updateTriggerInputs() {
        const kind = $("action-trigger").value;
        $("action-trigger-cron").hidden = kind !== "cron";
        $("action-trigger-page").hidden = kind !== "page_saved";
        $("action-trigger-webhook").hidden = kind !== "webhook";
        $("action-interval-wrap").hidden = kind === "cron";
    }

    function describeTargets(a) {
        const names = { none: "nowhere", page: "note/wiki", conversation: "chat thread", slack: "Slack", slack_dm: "Slack DM", push: "push", notify: "push", log: "log" };
        return (a.report_targets || []).map((t) => names[t.kind] || t.kind).join(", ") || "—";
    }

    // statusCell builds the dot+label span used in the list's State /
    // Last-run columns and the run history's Status column.
    function statusCell(text, klass) {
        const span = document.createElement("span");
        span.className = "action-status " + (klass || "");
        span.textContent = text;
        return span;
    }

    function statusClass(status) {
        switch (status) {
            case "ok": return "is-ok";
            case "error":
            case "timeout": return "is-error";
            case "skipped_quiet": return "is-quiet";
            case "running": return "is-running";
            default: return "is-off";
        }
    }

    function statusLabel(status) {
        const map = {
            ok: "ok",
            error: "error",
            timeout: "timeout",
            running: "running",
            skipped_quiet: "quiet",
            skipped_overlap: "overlap skip",
            skipped_owner: "owner skip",
            skipped_budget: "over budget",
        };
        return map[status] || status || "—";
    }

    function renderRows(items) {
        const tbody = $("actions-rows");
        tbody.innerHTML = "";
        if (!items.length) {
            const tr = document.createElement("tr");
            tr.className = "row-empty";
            const td = document.createElement("td");
            td.colSpan = 7;
            td.textContent = "NO SCHEDULED ACTIONS — CLICK NEW ACTION TO CREATE ONE";
            tr.appendChild(td);
            tbody.appendChild(tr);
            return;
        }
        for (const a of items) {
            const tr = document.createElement("tr");
            tr.dataset.id = a.id;
            const addTd = (text, cls) => {
                const td = document.createElement("td");
                td.textContent = text || "—";
                if (cls) td.className = cls;
                tr.appendChild(td);
            };
            addTd(a.name, "col-name");
            addTd(describeSchedule(a), "col-status");
            addTd(
                a.envelope === "shard" || a.shard_id ? "shard"
                    : a.envelope === "ephemeral" ? "ephemeral"
                    : "as you",
                "col-status",
            );
            addTd(describeTargets(a), "col-status");
            const stateTd = document.createElement("td");
            stateTd.className = "col-status";
            stateTd.appendChild(statusCell(a.enabled ? "enabled" : "disabled", a.enabled ? "is-ok" : "is-off"));
            tr.appendChild(stateTd);
            const lastTd = document.createElement("td");
            lastTd.className = "col-status";
            lastTd.appendChild(
                a.last_status
                    ? statusCell(statusLabel(a.last_status), statusClass(a.last_status))
                    : statusCell("never ran", "is-off"),
            );
            tr.appendChild(lastTd);
            addTd(a.last_run_at ? new Date(a.last_run_at).toLocaleString() : "—", "col-created");
            tr.addEventListener("click", () => openDetail(a.id));
            tbody.appendChild(tr);
        }
    }

    // ── Detail / form ─────────────────────────────────────────────

    // loadEnvelopeChoices fills the "Run as" select: the two built-in
    // envelopes (run-as-you full scope, ephemeral prompt-only) plus
    // one entry per user shard. Values: "user" | "ephemeral" |
    // "shard:<id>".
    async function loadEnvelopeChoices(selected) {
        const sel = $("action-shard");
        sel.innerHTML =
            '<option value="user">Run as you — full scope</option>' +
            '<option value="ephemeral">Ephemeral — no scope beyond the prompt</option>';
        try {
            const data = await apiJSON("/console/api/shards");
            const items = data.items || [];
            if (items.length) {
                const group = document.createElement("optgroup");
                group.label = "Your shards";
                for (const s of items) {
                    const opt = document.createElement("option");
                    opt.value = "shard:" + s.id;
                    opt.textContent = s.name + " (" + s.id + ")";
                    group.appendChild(opt);
                }
                sel.appendChild(group);
            }
        } catch (e) { /* shards optional — built-ins remain */ }
        sel.value = selected || "user";
        if (!sel.value) sel.value = "user"; // selected shard no longer listed
    }

    async function loadPageChoices(selectedID) {
        const sel = $("action-target-pageid");
        sel.innerHTML = "";
        const slug = ($("action-target-book").value || "personal").trim();
        try {
            const data = await apiJSON("/console/api/books/" + encodeURIComponent(slug) + "/pages?limit=100");
            for (const p of data.items || []) {
                const opt = document.createElement("option");
                opt.value = p.id;
                opt.textContent = p.title || p.slug;
                if (p.id === selectedID) opt.selected = true;
                sel.appendChild(opt);
            }
            if (!(data.items || []).length) {
                sel.innerHTML = '<option value="">— no pages in ' + slug + " —</option>";
            }
        } catch (e) {
            sel.innerHTML = '<option value="">— book not found —</option>';
        }
    }

    function updateTargetInputs() {
        const kind = $("action-target-kind").value;
        $("action-target-page").hidden = kind !== "page";
        $("action-target-slack").hidden = kind !== "slack";
        $("action-target-slackdm").hidden = kind !== "slack_dm";
        $("action-target-conversation").hidden = kind !== "conversation";
        if (kind === "page" && !$("action-target-pageid").options.length) loadPageChoices();
    }

    // setTimezone selects the action's timezone in the dropdown. If the
    // stored zone predates the curated list (or was set via the API), add
    // a one-off option so editing the action doesn't silently snap it to
    // UTC on the next save.
    function setTimezone(tz) {
        const sel = $("action-timezone");
        if (![...sel.options].some((o) => o.value === tz)) {
            sel.add(new Option(tz + " (custom)", tz));
        }
        sel.value = tz;
    }

    function openCreate() {
        state.current = null;
        $("action-detail-title").textContent = "New Action";
        $("action-form").reset();
        $("action-timezone").value = "UTC";
        $("action-timeout").value = "600";
        $("action-budget").value = "0";
        $("action-policy").value = "always";
        $("action-target-book").value = "personal";
        $("action-conversation-hint").textContent =
            'A dedicated "Scheduled: …" conversation is created with the action; reports land there as assistant messages you can reply to.';
        $("action-trigger").value = "cron";
        $("action-interval").value = "60";
        $("action-watch-book").value = "personal";
        $("action-webhook-hint").textContent =
            "A secret webhook URL is generated when you save; POST to it to fire this action.";
        updateTriggerInputs();
        for (const id of ["action-run-now", "action-enable", "action-disable", "action-delete"]) {
            $(id).hidden = true;
        }
        $("action-runs-section").hidden = true;
        setError("action-form-error", null);
        loadEnvelopeChoices("user");
        loadPageChoices();
        updateTargetInputs();
        showDetail();
    }

    async function openDetail(id) {
        setError("actions-error", null);
        try {
            const a = await apiJSON("/console/api/actions/" + encodeURIComponent(id));
            state.current = a;
            $("action-detail-title").textContent = a.name;
            $("action-name").value = a.name;
            $("action-prompt").value = a.prompt;
            $("action-cron").value = a.cron || "";
            setTimezone(a.timezone || "UTC");
            $("action-timeout").value = String(a.timeout_seconds || 600);
            $("action-policy").value = a.delivery_policy || "always";
            $("action-budget").value = String(a.max_runs_per_day || 0);
            $("action-trigger").value = a.trigger_kind || "cron";
            $("action-interval").value = String(a.min_interval_seconds || 60);
            $("action-watch-book").value = a.watch_book_slug || "personal";
            if (a.trigger_kind === "webhook" && a.webhook_token) {
                $("action-webhook-hint").textContent =
                    "POST " + location.origin + "/console/api/actions/hooks/" + a.webhook_token;
            }
            updateTriggerInputs();
            const targets = a.report_targets || [];
            let notify = targets.some((x) => x.kind === "notify");
            let t = targets.find((x) => x.kind !== "notify") || { kind: "none" };
            // Legacy "push" was chat-thread + notify combined; present it as
            // the conversation destination with the notify box checked.
            if (t.kind === "push") {
                t = { kind: "conversation", conversation_id: t.conversation_id };
                notify = true;
            }
            $("action-notify-push").checked = notify;
            $("action-target-kind").value = t.kind;
            if (t.kind === "page") {
                $("action-target-book").value = t.book_slug || "personal";
                await loadPageChoices(t.page_id);
            } else if (t.kind === "slack") {
                $("action-target-channel").value = t.channel_id || "";
            } else if (t.kind === "conversation") {
                $("action-conversation-hint").textContent =
                    "Reports land in conversation " + (t.conversation_id || "?") + " — find it as \"Scheduled: " + a.name + "\" in Chat.";
            }
            await loadEnvelopeChoices(
                a.envelope === "shard" || a.shard_id
                    ? "shard:" + a.shard_id
                    : (a.envelope || "user"),
            );
            updateTargetInputs();
            $("action-run-now").hidden = false;
            $("action-enable").hidden = a.enabled;
            $("action-disable").hidden = !a.enabled;
            $("action-delete").hidden = false;
            $("action-runs-section").hidden = false;
            setError("action-form-error", null);
            showDetail();
            loadRuns(a.id);
        } catch (e) {
            setError("actions-error", e);
        }
    }

    function showDetail() {
        // Take over the panel — .is-editing hides the whole list view
        // (#scheduled-list) so the form reads as its own page rather
        // than expanding under the existing UI.
        $("panel-scheduled").classList.add("is-editing");
        $("action-detail").hidden = false;
    }

    function closeDetail() {
        $("panel-scheduled").classList.remove("is-editing");
        $("action-detail").hidden = true;
        state.current = null;
    }

    function formBody() {
        const kind = $("action-target-kind").value;
        let target = { kind };
        if (kind === "page") {
            target.book_slug = ($("action-target-book").value || "personal").trim();
            target.page_id = $("action-target-pageid").value;
        } else if (kind === "slack") {
            target.channel_id = $("action-target-channel").value.trim();
        } else if (kind === "conversation") {
            // Keep the existing thread on edit (including a legacy "push"
            // target's thread we're migrating to conversation); omit on
            // create so the backend mints "Scheduled: <name>".
            const prevTargets = (state.current && state.current.report_targets) || [];
            const prev = prevTargets.find((x) => x.kind === "conversation" || x.kind === "push");
            if (prev && prev.conversation_id) {
                target.conversation_id = prev.conversation_id;
            }
        }
        // "Also notify me via push" is an independent ping that rides
        // alongside the destination.
        const targets = [target];
        if ($("action-notify-push").checked) targets.push({ kind: "notify" });
        const trigger = $("action-trigger").value;
        // "Run as" select: user | ephemeral | shard:<id>.
        const env = $("action-shard").value || "user";
        const isShard = env.indexOf("shard:") === 0;
        return {
            name: $("action-name").value.trim(),
            prompt: $("action-prompt").value,
            trigger_kind: trigger,
            cron: trigger === "cron" ? $("action-cron").value.trim() : "",
            watch_book_slug: trigger === "page_saved" ? ($("action-watch-book").value || "personal").trim() : "",
            min_interval_seconds: parseInt($("action-interval").value, 10) || 60,
            timezone: $("action-timezone").value.trim() || "UTC",
            envelope: isShard ? "shard" : env,
            shard_id: isShard ? env.slice(6) : "",
            report_targets: targets,
            delivery_policy: $("action-policy").value,
            timeout_seconds: parseInt($("action-timeout").value, 10) || 600,
            max_runs_per_day: parseInt($("action-budget").value, 10) || 0,
        };
    }

    async function submitForm(e) {
        e.preventDefault();
        setError("action-form-error", null);
        const body = formBody();
        try {
            if (state.current) {
                await apiJSON("/console/api/actions/" + encodeURIComponent(state.current.id), {
                    method: "PATCH",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(body),
                });
                toast("Action saved", "success");
            } else {
                await apiJSON("/console/api/actions", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(body),
                });
                toast("Action created", "success");
            }
            closeDetail();
            loadActions();
        } catch (err) {
            setError("action-form-error", err);
        }
    }

    async function runNow() {
        if (!state.current) return;
        try {
            await apiJSON("/console/api/actions/" + encodeURIComponent(state.current.id) + "/run", {
                method: "POST",
            });
            toast("Run started", "success");
            // The run is async; give it a beat before refreshing.
            setTimeout(() => loadRuns(state.current && state.current.id), 1500);
        } catch (e) {
            setError("action-form-error", e);
        }
    }

    async function toggleEnabled(enabled) {
        if (!state.current) return;
        try {
            const a = await apiJSON(
                "/console/api/actions/" + encodeURIComponent(state.current.id) + (enabled ? "/enable" : "/disable"),
                { method: "POST" },
            );
            state.current = a;
            $("action-enable").hidden = a.enabled;
            $("action-disable").hidden = !a.enabled;
            toast(enabled ? "Enabled" : "Disabled", "success");
        } catch (e) {
            setError("action-form-error", e);
        }
    }

    async function deleteCurrent() {
        if (!state.current) return;
        if (!confirm("Delete this action and its run history?")) return;
        try {
            await apiJSON("/console/api/actions/" + encodeURIComponent(state.current.id), { method: "DELETE" });
            toast("Action deleted", "success");
            closeDetail();
            loadActions();
        } catch (e) {
            setError("action-form-error", e);
        }
    }

    async function loadRuns(actionID) {
        if (!actionID) return;
        try {
            const data = await apiJSON("/console/api/actions/" + encodeURIComponent(actionID) + "/runs");
            const tbody = $("action-runs-rows");
            tbody.innerHTML = "";
            const items = data.items || [];
            if (!items.length) {
                tbody.innerHTML = '<tr class="row-empty"><td colspan="6">No runs yet — click Run now.</td></tr>';
                return;
            }
            for (const r of items) {
                const tr = document.createElement("tr");
                tr.dataset.expandable = "1";
                const addTd = (text, cls) => {
                    const td = document.createElement("td");
                    td.textContent = text || "—";
                    if (cls) td.className = cls;
                    tr.appendChild(td);
                };
                addTd(new Date(r.started_at).toLocaleString(), "col-created");
                const statusTd = document.createElement("td");
                statusTd.className = "col-status";
                statusTd.appendChild(statusCell(statusLabel(r.status), statusClass(r.status)));
                tr.appendChild(statusTd);
                addTd(r.trigger, "col-status");
                addTd(r.model_id, "col-status");
                addTd(r.duration_ms ? (r.duration_ms / 1000).toFixed(1) + "s" : "—", "col-status");
                const summary = r.error || (r.output || "").slice(0, 160);
                addTd(summary, "col-name");
                // Click → toggle a full-detail row (whole output +
                // per-target delivery results) under this one.
                tr.addEventListener("click", () => {
                    const next = tr.nextElementSibling;
                    if (next && next.classList.contains("action-run-detail")) {
                        next.remove();
                        return;
                    }
                    const detail = document.createElement("tr");
                    detail.className = "action-run-detail";
                    const td = document.createElement("td");
                    td.colSpan = 6;
                    const pre = document.createElement("pre");
                    let body = r.error ? "ERROR: " + r.error + "\n\n" : "";
                    body += r.output || "(no output)";
                    if (r.deliveries) {
                        try {
                            const dels = typeof r.deliveries === "string" ? JSON.parse(r.deliveries) : r.deliveries;
                            if (Array.isArray(dels) && dels.length) {
                                body += "\n\nDeliveries:";
                                for (const d of dels) {
                                    body += "\n  " + d.kind + ": " + (d.ok ? "ok" : "FAILED — " + (d.error || "?"));
                                }
                            }
                        } catch (e) { /* leave raw */ }
                    }
                    pre.textContent = body;
                    td.appendChild(pre);
                    detail.appendChild(td);
                    tr.insertAdjacentElement("afterend", detail);
                });
                tbody.appendChild(tr);
            }
        } catch (e) { /* runs table is best-effort */ }
    }

    window.familiarPanels = window.familiarPanels || {};
    window.familiarPanels.scheduled = { init };
})();
