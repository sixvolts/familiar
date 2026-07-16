// Profile panel — the assistant-personality editor plus the
// credential / passkey-enrollment management UI (display name,
// enrolled relying parties, cross-domain enrollment links).
//
// Lifted out of app.js so the panel's ~435 lines don't bloat the
// main shell. Self-contained inside an IIFE; reaches into
// window.familiarAppHelpers for shared infrastructure (apiJSON,
// toast, setError) and registers itself on window.familiarPanels
// so the panel-switcher in app.js invokes init() lazily on first
// navigation.

(function () {
    "use strict";

    const helpers = window.familiarAppHelpers;
    if (!helpers) {
        console.error("profile: app helpers not loaded; panel disabled");
        return;
    }
    const { apiJSON, toast, setError } = helpers;

    // ── Profile (assistant-personality editor) ───────────────────

    const profileState = {
        initialized: false,
        viewingUser: "",          // admin override; empty = own
        lastSavedUserPrompt: "",  // last-saved personality text, for Revert
    };

    async function initProfilePanel() {
        if (profileState.initialized) return;
        profileState.initialized = true;

        document.getElementById("profile-user-prompt-save").addEventListener("click", saveProfile);
        document.getElementById("profile-user-prompt-reset").addEventListener("click", () => {
            const upInput = document.getElementById("profile-user-prompt-input");
            if (upInput) upInput.value = profileState.lastSavedUserPrompt;
            setError("profile-error", null);
        });
        document.getElementById("profile-viewas-clear").addEventListener("click", () => viewAsUser(""));

        await loadProfile();
        // Read-only system-prompt block — only revealed for
        // non-admin users when the admin enabled it. Best-effort;
        // a 403 just leaves the block hidden.
        loadSystemPromptViewer();
        // Self-service display-name editor + per-RP passkey list
        // (CROSS-DOMAIN-ENROLLMENT.md) live alongside the working-
        // context blob. Wire after the blob lands so a slow gateway
        // doesn't delay the more important content.
        wireDisplayNameEditor();
        loadDisplayName();
        wireEnrollmentLinkPanel();
        await loadEnrolledRPs();
    }

    // loadSystemPromptViewer reveals the read-only system-prompt
    // <details> block whenever the admin has the user-visibility
    // toggle on. Admins see the same block as everyone else here —
    // the dedicated admin editor panel is where editing happens,
    // but there's no reason to hide the data from them on the
    // profile page. A 403 (toggle off, non-admin) or any other
    // error leaves the block hidden.
    async function loadSystemPromptViewer() {
        const host = document.getElementById("profile-system-prompt");
        const pre = document.getElementById("profile-system-prompt-text");
        if (!host || !pre) return;
        try {
            const data = await apiJSON("/console/api/system-prompt");
            if (!data.user_visible) { host.hidden = true; return; }
            pre.textContent = data.base || "";
            host.hidden = false;
        } catch (_) {
            host.hidden = true;
        }
    }

    // Self-service display-name editor. Loaded alongside the
    // working-context blob on profile-section activation; saves
    // via PATCH /console/api/profile/me. The any-role endpoint
    // scopes the update to the calling user, so non-admins can
    // safely rename themselves without touching the admin-only
    // /console/api/users/{id} surface.
    function wireDisplayNameEditor() {
        const input = document.getElementById("profile-display-name-input");
        const btn = document.getElementById("profile-display-name-save");
        const statusEl = document.getElementById("profile-display-name-status");
        if (!input || !btn || !statusEl) return;
        async function save() {
            const next = input.value.trim();
            if (!next) {
                statusEl.textContent = "name cannot be empty";
                return;
            }
            btn.disabled = true;
            statusEl.textContent = "saving…";
            try {
                const u = await apiJSON("/console/api/profile/me", {
                    method: "PATCH",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ display_name: next }),
                });
                statusEl.textContent = "saved";
                // Refresh any in-page surfaces that show the
                // signed-in user's name (sidebar avatar + label,
                // chat author label, etc.). The sidebar renderer is
                // initialised in app.js; we trigger a soft refresh
                // by replaying the populate path.
                if (u && u.display_name && typeof refreshSidebarUser === "function") {
                    refreshSidebarUser(u.display_name);
                }
                setTimeout(() => { statusEl.textContent = ""; }, 1500);
            } catch (e) {
                statusEl.textContent = (e && e.message) ? e.message : "save failed";
            } finally {
                btn.disabled = false;
            }
        }
        btn.addEventListener("click", save);
        input.addEventListener("keydown", (e) => {
            if (e.key === "Enter") { e.preventDefault(); save(); }
        });
    }

    async function loadDisplayName() {
        // Read the current value off the unscoped profile fetch so
        // the editor reflects whatever the user (or an admin) saved
        // last. Falls back silently if the endpoint isn't available.
        try {
            const data = await apiJSON("/console/api/auth/status");
            const input = document.getElementById("profile-display-name-input");
            if (input && data && data.display_name) {
                input.value = data.display_name;
            } else if (input && data && data.user_id) {
                input.value = data.user_id;
            }
        } catch (_) { /* anonymous load on a non-auth page */ }
    }

    function refreshSidebarUser(name) {
        // Best-effort: update the sidebar display label if the
        // element exists. The full refresh is owned elsewhere; this
        // shim just keeps the visible name in sync without forcing
        // a page reload.
        const el = document.getElementById("sidebar-user-name");
        if (el) el.textContent = name;
    }

    function wireEnrollmentLinkPanel() {
        const dismiss = document.getElementById("profile-enroll-link-dismiss");
        const copy = document.getElementById("profile-enroll-link-copy");
        if (dismiss) {
            dismiss.addEventListener("click", () => {
                document.getElementById("profile-enroll-link").hidden = true;
            });
        }
        if (copy) {
            copy.addEventListener("click", async () => {
                const url = document.getElementById("profile-enroll-link-url").value;
                if (!url) return;
                try { await navigator.clipboard.writeText(url); }
                catch (_) { /* clipboard blocked — user can select manually */ }
            });
        }
    }

    async function loadEnrolledRPs() {
        const host = document.getElementById("profile-keys-rps");
        const list = document.getElementById("profile-keys-rp-list");
        if (!host || !list) return;
        try {
            const data = await apiJSON("/console/api/auth/passkeys");
            renderCredentialList(data.passkeys || []);
            const enrolled = new Set((data.passkeys || []).map((p) => p.rp_id));
            const available = data.available_rps || [];
            // Build a unified RPID set so the UI lists every RP, with
            // either the enrolled badge or an "Add passkey" affordance.
            const allRPs = new Map();
            for (const p of (data.passkeys || [])) {
                if (!p.rp_id) continue;
                if (!allRPs.has(p.rp_id)) allRPs.set(p.rp_id, { rp_id: p.rp_id, display_name: p.rp_id, origin: "" });
            }
            for (const rp of available) {
                if (!allRPs.has(rp.rp_id)) allRPs.set(rp.rp_id, rp);
            }
            list.innerHTML = "";
            for (const rp of allRPs.values()) {
                const row = document.createElement("div");
                row.className = "rp-row";
                const left = document.createElement("div");
                left.className = "rp-row-left";
                const name = document.createElement("div");
                name.className = "rp-row-name";
                name.textContent = rp.display_name || rp.rp_id;
                const id = document.createElement("div");
                id.className = "rp-row-id";
                id.textContent = rp.rp_id;
                left.append(name, id);
                row.appendChild(left);
                if (enrolled.has(rp.rp_id)) {
                    const badge = document.createElement("span");
                    badge.className = "rp-row-badge rp-row-badge-ok";
                    badge.textContent = "✓ Enrolled";
                    row.appendChild(badge);
                } else {
                    const btn = document.createElement("button");
                    btn.type = "button";
                    btn.className = "btn-ghost btn-small";
                    btn.textContent = "Add passkey";
                    btn.addEventListener("click", () => createEnrollmentLink(rp.rp_id));
                    row.appendChild(btn);
                }
                list.appendChild(row);
            }
            host.hidden = allRPs.size === 0;
        } catch (e) {
            // The endpoint requires auth; on /enroll-style anonymous
            // contexts we'd never reach this panel anyway. Log but
            // don't surface — the rest of the profile page is fine.
            console.warn("enroll: load RPs failed", e);
        }
    }

    // renderCredentialList paints one row per registered passkey
    // into the .profile-keys-creds host. Each row shows a chip
    // (Passkey / Security key / Key), the RPID the credential is
    // bound to, a relative "last used" timestamp, and a Remove
    // button. The gateway refuses to delete a non-admin's last
    // credential — that error surfaces inline as a toast.
    function renderCredentialList(passkeys) {
        const host = document.getElementById("profile-keys-creds");
        const list = document.getElementById("profile-keys-cred-list");
        if (!host || !list) return;
        list.innerHTML = "";
        if (!passkeys.length) {
            host.hidden = true;
            return;
        }
        for (const p of passkeys) {
            const row = document.createElement("div");
            row.className = "cred-row";

            // Type chip — colored by authenticator class. Platform
            // (Touch ID etc.) gets the moss accent, cross-platform
            // (YubiKey etc.) gets sunlamp; unknown gets neutral.
            const chip = document.createElement("span");
            const kind = p.authenticator_type || "unknown";
            chip.className = "cred-chip cred-chip-" + kind.replace(/[^a-z]/g, "-");
            chip.textContent = chipLabelFor(kind, p.transports || []);
            row.appendChild(chip);

            const main = document.createElement("div");
            main.className = "cred-main";
            const title = document.createElement("div");
            title.className = "cred-title";
            title.textContent = p.display_name || "(unnamed)";
            const meta = document.createElement("div");
            meta.className = "cred-meta";
            const rp = p.rp_id || "(unknown domain)";
            const used = p.last_used
                ? "Last used " + relTime(p.last_used)
                : "Never used";
            meta.textContent = rp + " · " + used;
            main.append(title, meta);
            row.appendChild(main);

            const btn = document.createElement("button");
            btn.type = "button";
            btn.className = "btn-ghost btn-small cred-remove";
            btn.textContent = "Remove";
            btn.addEventListener("click", () => removeCredential(p));
            row.appendChild(btn);

            list.appendChild(row);
        }
        host.hidden = false;
    }

    function chipLabelFor(kind, transports) {
        if (kind === "platform") return "Passkey";
        if (kind === "cross-platform") return "Security key";
        // Heuristic for the "unknown" bucket: presence of "internal"
        // in transports is a passkey signal even when Attachment
        // wasn't declared.
        if (transports.indexOf("internal") >= 0) return "Passkey";
        if (transports.length > 0) return "Security key";
        return "Key";
    }

    // relTime returns a compact "5m ago" / "3d ago" / "Jan 14"
    // string for the last_used timestamp. Hour granularity matters
    // for "did I use this today" — coarser for older entries.
    function relTime(iso) {
        if (!iso) return "";
        const t = Date.parse(iso);
        if (isNaN(t)) return "";
        const diff = (Date.now() - t) / 1000;
        if (diff < 60) return "just now";
        if (diff < 3600) return Math.floor(diff / 60) + "m ago";
        if (diff < 86400) return Math.floor(diff / 3600) + "h ago";
        if (diff < 86400 * 7) return Math.floor(diff / 86400) + "d ago";
        const d = new Date(t);
        const mons = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
        return mons[d.getMonth()] + " " + d.getDate();
    }

    async function removeCredential(p) {
        const label = p.display_name || p.credential_id;
        if (!confirm("Remove this credential?\n\n" + label + "\n\nYou won't be able to sign in with this passkey or security key after removal.")) return;
        try {
            await apiJSON("/console/api/auth/passkeys/" + encodeURIComponent(p.credential_id), {
                method: "DELETE",
            });
            if (window.familiarAppHelpers && window.familiarAppHelpers.toast) {
                window.familiarAppHelpers.toast("Credential removed", "ok");
            }
            // Refresh the list so the row disappears and the
            // per-RP rollout summary updates if this was the last
            // credential for that RP.
            loadEnrolledRPs();
        } catch (e) {
            const msg = e && e.message ? e.message : String(e);
            if (window.familiarAppHelpers && window.familiarAppHelpers.toast) {
                window.familiarAppHelpers.toast("Couldn't remove: " + msg, "error");
            } else {
                alert("Couldn't remove: " + msg);
            }
        }
    }

    async function createEnrollmentLink(targetRPID) {
        const linkHost = document.getElementById("profile-enroll-link");
        const msg = document.getElementById("profile-enroll-link-msg");
        const urlInput = document.getElementById("profile-enroll-link-url");
        msg.textContent = "Generating link…";
        urlInput.value = "";
        linkHost.hidden = false;
        try {
            const resp = await apiJSON("/console/api/auth/enrollment-token", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ target_rp_id: targetRPID }),
            });
            urlInput.value = resp.url || "";
            const minsLeft = Math.max(0, Math.round((new Date(resp.expires_at) - Date.now()) / 60000));
            msg.textContent = "Open this link on " + targetRPID + " to register a passkey (expires in " + minsLeft + " min):";
        } catch (e) {
            msg.textContent = "Couldn't generate link: " + (e.message || e);
        }
    }

    // viewAsUser flips the personality editor between "mine" and the
    // admin cross-user view (Users → Edit personality). Only the
    // personality blob is scoped — the passkey/display-name editors
    // below it always operate on the caller's own account. Backend
    // ignores ?user_id= for role=user sessions.
    function viewAsUser(userID) {
        userID = userID || "";
        if (userID === profileState.viewingUser) return;
        profileState.viewingUser = userID;
        const banner = document.getElementById("profile-viewas");
        banner.hidden = !userID;
        if (userID) document.getElementById("profile-viewas-user").textContent = userID;
        if (profileState.initialized) loadProfile();
    }

    function profileURL() {
        return profileState.viewingUser
            ? "/console/api/profile?user_id=" + encodeURIComponent(profileState.viewingUser)
            : "/console/api/profile";
    }

    async function loadProfile() {
        setError("profile-error", null);
        try {
            const data = await apiJSON(profileURL());
            const prompt = typeof data.user_prompt === "string" ? data.user_prompt : "";
            const input = document.getElementById("profile-user-prompt-input");
            if (input) input.value = prompt;
            profileState.lastSavedUserPrompt = prompt;
        } catch (e) {
            setError("profile-error", e);
        }
    }

    async function saveProfile() {
        setError("profile-error", null);
        const input = document.getElementById("profile-user-prompt-input");
        const prompt = input ? input.value : "";
        const btn = document.getElementById("profile-user-prompt-save");
        if (btn) btn.disabled = true;
        try {
            const data = await apiJSON(profileURL(), {
                method: "PATCH",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ user_prompt: prompt }),
            });
            const saved = typeof data.user_prompt === "string" ? data.user_prompt : "";
            if (input) input.value = saved;
            profileState.lastSavedUserPrompt = saved;
            const status = document.getElementById("profile-user-prompt-status");
            if (status) {
                status.textContent = "Saved";
                setTimeout(() => { status.textContent = ""; }, 2500);
            }
        } catch (e) {
            setError("profile-error", e);
        } finally {
            if (btn) btn.disabled = false;
        }
    }


    window.familiarPanels = window.familiarPanels || {};
    window.familiarPanels.profile = { init: initProfilePanel, viewAs: viewAsUser };
})();
