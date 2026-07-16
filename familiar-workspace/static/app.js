// FAMILIAR ADMIN — WebAuthn client.
//
// Boot flow:
//   1. GET /console/api/auth/status
//      → 200 => dashboard
//      → 401 => GET /console/api/auth/register/status
//                 → requires_auth=false => first-time setup
//                 → requires_auth=true  => login
//

// ── UI Zoom ────────────────────────────────────────────────────
// Persists a Small / Medium / Large zoom level in localStorage.
// Applied to <html> via CSS zoom so layout reflows and everything
// stays fitted to the viewport.
(function () {
    var ZOOM_KEY = "familiar-zoom";
    var LEVELS = { small: 1.0, medium: 1.10, large: 1.20 };
    function apply(level) {
        document.documentElement.style.zoom = LEVELS[level] || 1.0;
    }
    function get() {
        return localStorage.getItem(ZOOM_KEY) || "small";
    }
    function set(level) {
        localStorage.setItem(ZOOM_KEY, level);
        apply(level);
    }
    apply(get());
    window.familiarZoom = { get: get, set: set, LEVELS: LEVELS };
})();
// Registration and login ceremonies follow the standard two-step
// begin/finish pattern. The server stashes SessionData in memory keyed
// by a short-lived pending cookie so we don't need to round-trip it.

(function () {
    "use strict";

    // ── base64url <-> ArrayBuffer ────────────────────────────────

    function b64urlToBuf(s) {
        const padded = s.replace(/-/g, "+").replace(/_/g, "/");
        const pad = (4 - (padded.length % 4)) % 4;
        const raw = atob(padded + "=".repeat(pad));
        const buf = new Uint8Array(raw.length);
        for (let i = 0; i < raw.length; i++) buf[i] = raw.charCodeAt(i);
        return buf.buffer;
    }

    function bufToB64url(buf) {
        const bytes = new Uint8Array(buf);
        let s = "";
        for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
        return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=/g, "");
    }

    // ── API helpers ─────────────────────────────────────────────

    // apiJSON is the shared fetch wrapper for JSON CRUD calls (NOT the
    // /api/chat SSE stream, which manages its own fetch in chat.js).
    //
    // Robustness for a wider audience (EXTERNAL-READINESS-REVIEW.md P2):
    //   - a client-side timeout (opts.timeoutMs, default 30s) so a
    //     hung request surfaces a clear error instead of spinning
    //     forever on a flaky connection;
    //   - a 401 handler that, when a logged-in session expires
    //     mid-use, drops the user back to the login view instead of
    //     leaving a dead UI. It only fires while the dashboard is
    //     showing, so the bootstrap/auth flow (where a 401 is expected
    //     and handled by the caller) is untouched.
    let unauthorizedHandled = false;
    function handleUnauthorized() {
        const dash = document.getElementById("view-dashboard");
        if (!dash || dash.hidden) return; // bootstrap/auth flow — leave to caller
        if (unauthorizedHandled) return;
        unauthorizedHandled = true;
        try { toast("Your session expired — please sign in again.", "error"); } catch (_) {}
        show("login");
        setTimeout(() => { unauthorizedHandled = false; }, 3000);
    }

    // ── Session watchdog ────────────────────────────────────────
    // Expiry used to surface as mystery breakage: API calls failing
    // quietly behind whatever screen was open until a refresh dumped
    // the user at login. The watchdog probes /auth/status (a) the
    // moment a sleeping device wakes — the kiosk-iPad case — and
    // (b) every 90s while visible. A 401 lands the user on the login
    // view immediately, with the toast; a 200 renews the sliding
    // session AND rolls the cookie (the endpoint re-sets Max-Age),
    // so a visible kiosk never idles out mid-use.
    function startSessionWatchdog() {
        const probe = async () => {
            if (document.hidden) return;
            const dash = document.getElementById("view-dashboard");
            if (!dash || dash.hidden) return; // not signed in
            try {
                const resp = await fetch("/console/api/auth/status", { credentials: "include" });
                if (resp.status === 401) { handleUnauthorized(); return; }
                if (resp.ok) {
                    // Re-evaluate the maintenance banner on every probe
                    // so it appears/clears live (incl. auto-recovery)
                    // without a reload.
                    const s = await resp.json().catch(() => null);
                    if (s) applyMaintenanceBanner(s.maintenance);
                }
            } catch (_) { /* network blip — don't kick a working UI */ }
        };
        document.addEventListener("visibilitychange", () => {
            if (!document.hidden) probe();
        });
        window.addEventListener("focus", probe);
        setInterval(probe, 90_000);
    }
    startSessionWatchdog();

    async function apiJSON(path, opts) {
        opts = opts || {};
        const timeoutMs = opts.timeoutMs || 30000;
        let controller, timer;
        let signal = opts.signal;
        if (!signal && typeof AbortController !== "undefined") {
            controller = new AbortController();
            signal = controller.signal;
            timer = setTimeout(() => controller.abort(), timeoutMs);
        }
        let resp;
        try {
            resp = await fetch(path, Object.assign({ credentials: "include" }, opts, { signal }));
        } catch (e) {
            if (e && e.name === "AbortError") {
                throw new Error("Request timed out — check your connection and try again.");
            }
            throw new Error("Network error — check your connection.");
        } finally {
            if (timer) clearTimeout(timer);
        }
        if (resp.status === 401) {
            handleUnauthorized();
            throw new Error("Your session expired. Please sign in again.");
        }
        const text = await resp.text();
        let body = null;
        try { body = text ? JSON.parse(text) : null; } catch (e) { /* ignore */ }
        if (!resp.ok) {
            const msg = (body && body.error) || ("HTTP " + resp.status);
            throw new Error(msg);
        }
        return body;
    }

    // ── Toast notifications ────────────────────────────────────────

    function toast(message, type) {
        type = type || "info";
        const container = document.getElementById("toast-container");
        if (!container) return;
        const el = document.createElement("div");
        el.className = "toast toast-" + type;
        el.textContent = message;
        container.appendChild(el);
        setTimeout(() => {
            el.classList.add("toast-out");
            setTimeout(() => el.remove(), 220);
        }, 3000);
    }

    // ── Loading overlay helper ─────────────────────────────────────

    function withLoading(targetEl, fn) {
        return async function () {
            if (!targetEl) return fn.apply(this, arguments);
            targetEl.classList.add("is-loading");
            const overlay = document.createElement("div");
            overlay.className = "loading-overlay";
            // Brand F-mark loader (static F + pulsing yellow dot),
            // same markup as the boot screen. SVG namespace so the
            // path/circle render — createElement("svg") would build
            // an inert HTML element.
            overlay.innerHTML =
                '<svg class="fmark-loader" viewBox="78 56 96 110" aria-hidden="true">' +
                '<g transform="translate(-82 0)">' +
                '<path d="M 165.41 64.56 L 244.48 64.56 L 233.02 79.97 L 180.48 79.97 L 180.48 100.21 L 218.40 100.21 L 206.94 115.63 L 181.04 115.63 L 181.04 154.56 L 165.41 154.56 Z" fill="var(--accent, #6A4CE0)"/>' +
                '<circle class="fmark-loader-dot" cx="198.69" cy="146.74" r="7.817" fill="#E8BE55"/>' +
                '</g></svg>';
            targetEl.appendChild(overlay);
            try {
                return await fn.apply(this, arguments);
            } finally {
                overlay.remove();
                targetEl.classList.remove("is-loading");
            }
        };
    }

    // ── View switching ──────────────────────────────────────────

    const views = ["loading", "setup", "login", "dashboard"];
    function show(name) {
        for (const v of views) {
            const el = document.getElementById("view-" + v);
            if (el) el.hidden = v !== name;
        }
    }

    function setError(id, err) {
        const el = document.getElementById(id);
        if (!el) return;
        if (!err) {
            el.hidden = true;
            el.textContent = "";
            return;
        }
        el.hidden = false;
        const raw = err.message || String(err);
        // Account-status gates (login/register) get human copy instead
        // of a raw "ERROR: account pending approval". These are the
        // states a freshly-onboarded beta user actually hits.
        const lower = raw.toLowerCase();
        if (lower.includes("pending approval")) {
            el.textContent = "Your account is awaiting admin approval. You'll be able to sign in once it's approved.";
        } else if (lower.includes("account disabled")) {
            el.textContent = "This account has been disabled. Contact the admin if you think that's a mistake.";
        } else if (lower.includes("access denied")) {
            el.textContent = "Your access request was denied. Contact the admin if you think that's a mistake.";
        } else {
            el.textContent = "ERROR: " + raw;
        }
    }

    // requireWebAuthn throws a readable error when navigator
    // .credentials isn't available. WebAuthn requires a secure
    // context — HTTPS, or http://localhost. The most common cause
    // of this firing in development is opening the workspace via a
    // LAN IP (http://192.168.x.x:port) which the browser refuses to
    // expose the credentials API to.
    function requireWebAuthn() {
        if (typeof navigator === "undefined" || !navigator.credentials) {
            throw new Error(
                "Passkeys aren't available on this page. Open the workspace over HTTPS, or via http://localhost — " +
                "browsers gate the WebAuthn API behind a secure context."
            );
        }
    }

    // ── Registration ceremony ───────────────────────────────────

    async function doRegister(errorId, params) {
        // Phase 4 — registerBegin requires {email} (and optional
        // display_name on first-run). Callers pass whichever values
        // they've collected; if the caller doesn't provide anything
        // the backend returns 400 and the UI surfaces the message
        // via the supplied errorId.
        const body = {};
        if (params && params.email) body.email = params.email;
        if (params && params.displayName) body.display_name = params.displayName;
        const creation = await apiJSON("/console/api/auth/register/begin", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(body),
        });

        // Convert base64url fields to ArrayBuffers for the browser API.
        const pk = creation.publicKey;
        pk.challenge = b64urlToBuf(pk.challenge);
        // user.id is a plain string (EncodeUserIDAsString=true in the
        // Go config). Always UTF-8 encode — never base64url-decode,
        // because some IDs (e.g. "canyon_r") happen to be valid
        // base64url and silently produce wrong bytes.
        pk.user.id = new TextEncoder().encode(pk.user.id).buffer;
        if (Array.isArray(pk.excludeCredentials)) {
            for (const c of pk.excludeCredentials) {
                c.id = b64urlToBuf(c.id);
            }
        }

        requireWebAuthn();
        const credential = await navigator.credentials.create({ publicKey: pk });
        if (!credential) throw new Error("authenticator returned no credential");

        // Second request body for the /register/finish POST. Named
        // distinctly from the `body` declared above for /register/begin
        // so the two `const` declarations don't collide at parse time.
        const finishBody = {
            id: credential.id,
            rawId: bufToB64url(credential.rawId),
            type: credential.type,
            response: {
                attestationObject: bufToB64url(credential.response.attestationObject),
                clientDataJSON:    bufToB64url(credential.response.clientDataJSON),
            },
            clientExtensionResults: credential.getClientExtensionResults
                ? credential.getClientExtensionResults()
                : {},
        };

        return apiJSON("/console/api/auth/register/finish", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(finishBody),
        });
    }

    // ── Login ceremony ──────────────────────────────────────────

    async function doLogin() {
        const assertion = await apiJSON("/console/api/auth/login/begin", { method: "POST" });

        const pk = assertion.publicKey;
        pk.challenge = b64urlToBuf(pk.challenge);
        if (Array.isArray(pk.allowCredentials)) {
            for (const c of pk.allowCredentials) {
                c.id = b64urlToBuf(c.id);
            }
        }

        requireWebAuthn();
        const credential = await navigator.credentials.get({ publicKey: pk });
        if (!credential) throw new Error("authenticator returned no credential");

        const body = {
            id: credential.id,
            rawId: bufToB64url(credential.rawId),
            type: credential.type,
            response: {
                authenticatorData: bufToB64url(credential.response.authenticatorData),
                clientDataJSON:    bufToB64url(credential.response.clientDataJSON),
                signature:         bufToB64url(credential.response.signature),
                userHandle: credential.response.userHandle
                    ? bufToB64url(credential.response.userHandle)
                    : null,
            },
            clientExtensionResults: credential.getClientExtensionResults
                ? credential.getClientExtensionResults()
                : {},
        };

        return apiJSON("/console/api/auth/login/finish", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(body),
        });
    }

    // ── Dashboard rendering ─────────────────────────────────────

    // applyMaintenanceBanner renders (or clears) the warning strip that
    // tells everyone the big model is offline and a slower fallback is
    // answering. Fed by the `maintenance` block on /auth/status — only
    // present when maintenance is active — from both boot and the 90s
    // watchdog, so it shows and auto-clears without a reload. Idempotent:
    // re-rendering the same message is a no-op (no flicker per probe).
    function applyMaintenanceBanner(m) {
        const host = document.getElementById("maintenance-banner-host");
        if (!host) return;
        if (!m || !m.active) {
            if (host.dataset.msg) { host.innerHTML = ""; host.dataset.msg = ""; }
            return;
        }
        const msg = m.message || ("Maintenance mode — using " + (m.model || "a fallback model"));
        if (host.dataset.msg === msg) return;
        host.dataset.msg = msg;
        host.innerHTML = "";
        const banner = document.createElement("div");
        banner.className = "maintenance-banner";
        const icon = document.createElement("span");
        icon.className = "maintenance-banner-icon";
        icon.textContent = "⚠";
        const text = document.createElement("span");
        text.className = "maintenance-banner-msg";
        text.textContent = msg;
        banner.append(icon, text);
        host.appendChild(banner);
    }

    function renderDashboard(session) {
        // Phase-3 role-conditional bootstrap. `session` is the full
        // auth-status response: {user, display_name, role, email,
        // authenticated, principal_type, shard_id, shard_name,
        // permissions}. We stash it on document.body.dataset.role
        // (CSS picks this up to hide .admin-only elements) and on
        // window.FAMILIAR_SESSION for JS callers that need the full
        // record (the memory panel reads it).
        //
        // SHARD-AUTH-SPEC Phase 1: shard sessions get extra body
        // data-* attributes the CSS uses to hide off-limits panels.
        // The backend enforces the same envelope regardless — these
        // rules just keep the chrome from advertising panels that
        // would 403 on click.
        const role = (session && session.role) || "";
        const user = session && session.user;
        const displayName = (session && session.display_name) || user || "";
        const principalType = (session && session.principal_type) || "user";
        const perms = (session && session.permissions) || null;

        document.body.dataset.role = role;
        document.body.dataset.principalType = principalType;
        applyPermissionEnvelope(perms);
        window.FAMILIAR_SESSION = session || {};
        applyMaintenanceBanner(session && session.maintenance);

        // DESIGN.md: user identity now lives in the
        // sidebar footer, not the title-bar nav strip. Populate the
        // footer's name + initials avatar; the rest (vault label,
        // chevron) is static markup. SHARD-AUTH-SPEC: shard sessions
        // show the shard's name + a yellow diamond glyph instead of
        // the owner's name + initials, so a kiosk surface reads as
        // its own identity rather than borrowing the owner's.
        const sidebarName = document.getElementById("sidebar-user-name");
        const avatar = document.getElementById("sidebar-user-avatar");
        if (principalType === "shard") {
            // Strip data-mode="user" so workspace.js's capture-phase
            // click listener doesn't beat ours to the User panel
            // switch — capture fires ancestors (document) before
            // descendants (the row), so without removing the
            // attribute the panel flashes open behind the popover.
            const userRow = document.getElementById("sidebar-user-row");
            if (userRow) userRow.removeAttribute("data-mode");

            const shardLabel = session.shard_name || session.shard_id || "shard";
            if (sidebarName) sidebarName.textContent = shardLabel;
            if (avatar) {
                avatar.textContent = "";
                avatar.classList.add("is-shard");
                avatar.innerHTML = '<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
                    '<path d="M14 2.2 L20.5 9.6 L17.2 21.6 L7.4 19.0 L4.2 12.4 Z"/>' +
                    '<path d="M14 2.2 L10.4 11.6 L4.2 12.4"/>' +
                    '<path d="M10.4 11.6 L17.2 21.6"/>' +
                    '<path d="M10.4 11.6 L20.5 9.6"/>' +
                    '</svg>';
            }
        } else {
            if (sidebarName) sidebarName.textContent = displayName || "—";
            if (avatar) {
                const initials = (displayName || "?")
                    .split(/\s+/).filter(Boolean).slice(0, 2)
                    .map((s) => s[0].toUpperCase()).join("") || "?";
                avatar.textContent = initials;
            }
        }

        // Title bar removed; sidebar header now owns the F-mark
        // and search affordance. The view-shell grid collapsed
        // to two rows — sidebar/content + statusbar.

        show("dashboard");
        wireSidebar();

        // DESIGN.md: default landing is Home (the
        // launchpad), not the workspace grid. Home is a mode, not
        // a tab kind — switchPanel("home") shows panel-home and
        // hides the workspace + every secondary surface. The
        // workspace's own state still restores from localStorage
        // when the user clicks a category.
        switchPanel("home");
        const homeRow = document.getElementById("sidebar-home-row");
        if (homeRow) homeRow.classList.add("is-active");
    }

    // ── Sidebar panel switching ─────────────────────────────────

    const panelLoaders = {
        // Home, workspace, and user are the three top-level modes
        // per DESIGN.md. All three are listed here so
        // switchPanel accepts them as targets. The workspace and
        // user panels' content is rendered by their respective
        // IIFEs / sub-section logic; this loader only fires when
        // panel-user is first revealed and triggers
        // initUserPanel() (sub-nav wireup, default section).
        home: () => initHome(),
        workspace: () => {},
        user: () => initUserPanel(),
        dashboard: () => initDashboard(),
        "system-status": () => initSystemStatus(),
        users: () => initUsersBrowser(),
        // Shards lives in /panels/shards.js — registers itself on
        // window.familiarPanels at load time.
        shards: () => {
            const p = window.familiarPanels && window.familiarPanels.shards;
            if (p && p.init) p.init();
            else console.error("shards panel module not loaded");
        },
        // Scheduled actions lives in /panels/scheduled.js.
        scheduled: () => {
            const p = window.familiarPanels && window.familiarPanels.scheduled;
            if (p && p.init) p.init();
            else console.error("scheduled panel module not loaded");
        },
        // Skills catalog lives in /panels/skills.js. The one module
        // drives both the user Skills panel and the admin System skills
        // panel; its init is idempotent.
        skills: () => {
            const p = window.familiarPanels && window.familiarPanels.skills;
            if (p && p.init) p.init();
            else console.error("skills panel module not loaded");
        },
        "system-skills": () => {
            const p = window.familiarPanels && window.familiarPanels.skills;
            if (p && p.init) p.init();
            else console.error("skills panel module not loaded");
        },
        // Memory browser lives in /panels/memory.js.
        memory: () => {
            const p = window.familiarPanels && window.familiarPanels.memory;
            if (p && p.init) p.init();
            else console.error("memory panel module not loaded");
        },
        // Memory graph lives in /panels/memory-graph.js — registers
        // itself on window.familiarPanels at load time.
        "memory-graph": () => {
            const p = window.familiarPanels && window.familiarPanels["memory-graph"];
            if (p && p.init) p.init();
            else console.error("memory-graph panel module not loaded");
        },
        // Profile lives in /panels/profile.js — registers itself on
        // window.familiarPanels at load time.
        profile: () => {
            const p = window.familiarPanels && window.familiarPanels.profile;
            if (p && p.init) p.init();
            else console.error("profile panel module not loaded");
        },
        // Admin system-prompt editor lives in /panels/system-prompt.js.
        "system-prompt": () => {
            const p = window.familiarPanels && window.familiarPanels["system-prompt"];
            if (p && p.init) p.init();
            else console.error("system-prompt panel module not loaded");
        },
    };
    const panelLoaded = { home: false, workspace: false, user: false, dashboard: false, "system-status": false, users: false, shards: false, skills: false, memory: false, "memory-graph": false, profile: false, "system-prompt": false };
    let sidebarWired = false;
    let currentPanel = "dashboard";

    function wireSidebar() {
        if (sidebarWired) return;
        sidebarWired = true;
        for (const link of document.querySelectorAll(".nav-item")) {
            link.addEventListener("click", (e) => {
                // Primary surfaces (data-surface=*) are handled by
                // workspace.js's capture-phase listener. Skip them
                // here so we don't double-handle.
                if (link.dataset.surface) return;
                e.preventDefault();
                const target = link.dataset.panel;
                if (target) switchPanel(target);
            });
        }

        // Shard sessions: intercept the sidebar profile click on a
        // higher-priority capture-phase listener so workspace.js
        // doesn't drop the user into the User panel (which a shard
        // can't meaningfully use). Show a tiny popover with just
        // Sign out — same affordance as the mobile "..." menus.
        const userRow = document.getElementById("sidebar-user-row");
        if (userRow) {
            userRow.addEventListener("click", (e) => {
                const sess = window.FAMILIAR_SESSION || {};
                if (sess.principal_type !== "shard") return;
                e.preventDefault();
                e.stopPropagation();
                openShardUserPopover(userRow);
            }, true);
        }
    }

    // openShardUserPopover puts a one-button popover (Sign out)
    // anchored above the sidebar profile row. Click-outside / Esc
    // closes it. Reused on every click so the position picks up
    // any layout shifts since last time.
    function openShardUserPopover(anchor) {
        // Toggle off if already open.
        const existing = document.querySelector(".shard-user-popover");
        if (existing) {
            existing.remove();
            return;
        }
        const pop = document.createElement("div");
        pop.className = "shard-user-popover";
        const btn = document.createElement("button");
        btn.type = "button";
        btn.className = "is-danger";
        btn.textContent = "Sign out";
        btn.addEventListener("click", async () => {
            try {
                await apiJSON("/console/api/auth/logout", { method: "POST" });
            } catch (e) { /* ignore — reload below kicks user back to login */ }
            location.reload();
        });
        pop.appendChild(btn);
        document.body.appendChild(pop);

        // Anchor above the sidebar row so it doesn't get clipped by
        // the sidebar's overflow.
        const r = anchor.getBoundingClientRect();
        pop.style.left = (r.left + 8) + "px";
        pop.style.bottom = (window.innerHeight - r.top + 4) + "px";

        // Dismiss on outside click / Esc. Use capture for the click
        // so any in-popover button click runs first and the listener
        // sees that "outside" wasn't actually inside.
        const dismiss = (ev) => {
            if (ev.type === "keydown" && ev.key !== "Escape") return;
            if (ev.type === "click" && pop.contains(ev.target)) return;
            pop.remove();
            document.removeEventListener("click", dismiss, true);
            document.removeEventListener("keydown", dismiss);
        };
        // Defer so the click that opened the popover doesn't
        // immediately close it.
        setTimeout(() => {
            document.addEventListener("click", dismiss, true);
            document.addEventListener("keydown", dismiss);
        }, 0);
    }

    function switchPanel(name) {
        // SHARD-AUTH-SPEC: refuse to switch into a panel the
        // session's permission envelope hides. Backend enforces the
        // same boundary; this guard keeps a curl/inspector poke or a
        // stale URL hash from briefly rendering a 403'd panel before
        // the API call fails. Falls back to the home surface.
        if (!isPanelAccessible(name)) {
            name = "home";
        }
        // Scope to top-level panels only (direct children of
        // .content). Phase 3c relocates secondary panels inside
        // panel-user; without this scope, switching to user mode
        // would also hide the nested panel that's the whole point
        // of being in user mode.
        //
        // If the target itself was relocated into the user host
        // earlier (a subnav visit moved it), pull it back to the
        // content root first — otherwise it would stay invisible
        // inside the now-hidden panel-user.
        const target = document.getElementById("panel-" + name);
        const content = document.querySelector(".content");
        if (target && content && target.parentElement !== content) {
            content.appendChild(target);
        }
        for (const p of document.querySelectorAll(".content > .panel")) {
            p.hidden = p.id !== ("panel-" + name);
        }
        for (const link of document.querySelectorAll(".nav-item")) {
            link.classList.toggle("is-active", link.dataset.panel === name);
        }
        currentPanel = name;
        if (!panelLoaded[name]) {
            panelLoaded[name] = true;
            const loader = panelLoaders[name];
            if (loader) loader();
        }
        // The dashboard is a personal surface: normal navigation
        // always lands on your own. The Users-panel jump re-enters
        // view-as immediately after this call, so the reset here
        // never fights it.
        if (name === "dashboard") dashViewAs("");
    }

    // ── Permission envelope (SHARD-AUTH-SPEC Phase 1) ────────────
    //
    // applyPermissionEnvelope walks the session's permission record
    // and stamps the body with hide-flags the CSS turns into
    // display:none rules. Called once in renderDashboard after the
    // auth/status response lands. User sessions pass null and short-
    // circuit — only shard sessions carry an envelope.
    //
    // The flags written here:
    //   data-can-chat="false"      — hide the Chat sidebar row + chat
    //                                shortcut buttons elsewhere
    //   data-panel-{name}="hidden" — hide the named top-level panel
    //                                from the sidebar / user-subnav
    //
    // Allowed panel names (must match the backend envelope keys):
    //   chat, notes, books (= wiki), shards, dashboard, memory
    //
    // The envelope's `panels` field is the ALLOWED set — if it's nil
    // (i.e. omitted) every panel is allowed. A non-nil list with no
    // entries hides all panels.
    function applyPermissionEnvelope(perms) {
        if (!perms) {
            // User session or shard with no envelope override.
            return;
        }
        // Canonical surface → panel-name mapping. The sidebar
        // categories use data-surface; map each to the envelope's
        // panel name so the same `panels` array gates both kinds.
        const SURFACE_PANEL = {
            chat: "chat",
            notes: "notes",
            wiki: "books",
            shards: "shards",
        };
        // Hide the Chat sidebar row when chat is gated off.
        if (perms.can_chat === false) {
            document.body.dataset.canChat = "false";
        }
        if (Array.isArray(perms.panels)) {
            const allowed = new Set(perms.panels);
            // Hide sidebar surface rows whose mapped panel isn't
            // in the envelope.
            for (const row of document.querySelectorAll(".sidebar-cat[data-surface]")) {
                const panelName = SURFACE_PANEL[row.dataset.surface];
                if (panelName && !allowed.has(panelName)) {
                    row.hidden = true;
                }
            }
            // user-subnav sections that map to panels in the
            // envelope: memory* → memory. Other sections (profile,
            // sessions, skills) aren't panel-gated and stay visible.
            const SECTION_PANEL = {
                memory: "memory",
                "memory-graph": "memory",
            };
            for (const row of document.querySelectorAll("#user-subnav .user-subnav-row")) {
                const name = row.dataset.userSection;
                const panelName = SECTION_PANEL[name];
                if (panelName && !allowed.has(panelName)) {
                    row.hidden = true;
                }
            }
            // Top-level data-panel links (Dashboard cards' "View →"
            // shortcuts) get the same treatment so a click into a
            // hidden panel doesn't sneak through.
            for (const link of document.querySelectorAll("[data-panel]")) {
                const target = link.dataset.panel;
                // Skip the meta panels — home/user are always
                // accessible.
                if (target === "home" || target === "user") continue;
                if (!allowed.has(target)) {
                    link.hidden = true;
                }
            }
            // Stash the allowed set as a body data-* prefix so CSS
            // (and isPanelAccessible below) can read it without
            // re-parsing the JSON each time.
            document.body.dataset.allowedPanels = perms.panels.join(",");
        }
    }

    // isPanelAccessible mirrors the backend's CanAccessPanel.
    // Returns true when no envelope is in play (user session) or
    // when the panel is in the allowed list. The "home" panel and
    // the "user" panel are always accessible — they're the
    // fallback shells, not surface panels.
    function isPanelAccessible(name) {
        if (name === "home" || name === "user") return true;
        // Scheduled actions are owner-only: every /console/api/actions
        // route refuses shard sessions, so the panel hides for ANY
        // shard principal — even one whose envelope inherits all
        // panels. Rendering it would just be a wall of 403s.
        if (name === "scheduled" &&
            (window.FAMILIAR_SESSION || {}).principal_type === "shard") {
            return false;
        }
        const allowed = document.body.dataset.allowedPanels;
        if (!allowed) return true; // no envelope → all allowed
        // Map workspace surface panels to their envelope names.
        const ALIAS = { workspace: "books", wiki: "books" };
        const checkName = ALIAS[name] || name;
        return allowed.split(",").indexOf(checkName) !== -1;
    }

    // Expose for workspace.js — it needs to flip the .panel host
    // when a sidebar primary surface is clicked.
    window.appSwitchPanel = switchPanel;

    // Expose apiJSON + helpers so workspace surface modules
    // (chat.js, etc.) can reuse the existing fetch wrapper rather
    // than re-implementing credential + JSON handling. Read-only
    // bag — surfaces should not mutate this.
    // ensureCytoscape lazy-loads cytoscape.js from CDN on first use.
    // Single shared promise across panels (dashboard graph preview +
    // memory-graph panel) so the page loads cytoscape exactly once
    // regardless of which surface needs it first. Moved out of the
    // memory-graph section so panel extraction doesn't strand the
    // dashboard caller.
    let _cytoscapeLoadingPromise = null;
    function ensureCytoscape() {
        if (window.cytoscape) return Promise.resolve(window.cytoscape);
        if (_cytoscapeLoadingPromise) return _cytoscapeLoadingPromise;
        _cytoscapeLoadingPromise = new Promise((resolve, reject) => {
            const s = document.createElement("script");
            s.src = "/vendor/cytoscape/cytoscape.min.js";
            s.async = true;
            s.onload = () => resolve(window.cytoscape);
            s.onerror = () => reject(new Error("failed to load cytoscape.js"));
            document.head.appendChild(s);
        });
        return _cytoscapeLoadingPromise;
    }

    // fmtDate renders an ISO timestamp as "YYYY-MM-DD HH:MM" (UTC).
    // Shared helper — exported below; the memory-browser panel uses
    // it. Defined here (rather than inside a panel section) so panel
    // extraction can't strand the helpers export.
    function fmtDate(iso) {
        if (!iso) return "—";
        try {
            const d = new Date(iso);
            return d.toISOString().replace("T", " ").slice(0, 16);
        } catch (e) { return iso; }
    }

    // truncate caps a string at n chars with an ellipsis. Used by
    // the users-browser identity-chip render. (The memory panel
    // module carries its own copy.)
    function truncate(s, n) {
        if (!s) return "";
        return s.length > n ? s.slice(0, n) + "…" : s;
    }

    window.familiarAppHelpers = {
        apiJSON,
        toast,
        setError,
        ensureCytoscape,
        // WebAuthn ceremony helpers — used by register/login here in
        // app.js and by the shard-passkey enroll flow in
        // panels/shards.js. Same implementations either way; exposing
        // them so the panel module doesn't have to duplicate.
        requireWebAuthn,
        b64urlToBuf,
        bufToB64url,
        fmtDate,
    };

    // ── Page events (SSE push) ───────────────────────────────────
    //
    // Singleton EventSource subscribed to /console/api/events/pages.
    // Every page-saved / page-deleted that the server fires for a
    // book the user is a member of gets dispatched as a
    // `familiar:pageEvent` CustomEvent so surface modules (notes.js,
    // wiki.js) can refresh-if-clean. One connection per shell — the
    // server-side filter handles fanout.
    //
    // EventSource auto-reconnects on close; we don't manage retries
    // beyond letting the browser do its thing. Shard sessions skip
    // SSE entirely (they don't run editors and a shard session
    // shouldn't be holding a long-lived auth channel anyway).
    (function wirePageEventsSSE() {
        const sess = window.FAMILIAR_SESSION || {};
        if (sess.principal_type === "shard") return;
        if (typeof EventSource !== "function") return; // ancient browser
        let es;
        try {
            es = new EventSource("/console/api/events/pages", { withCredentials: true });
        } catch (e) {
            console.warn("page events: EventSource failed:", e);
            return;
        }
        const dispatch = (raw, kind) => {
            try {
                const data = JSON.parse(raw);
                window.dispatchEvent(new CustomEvent("familiar:pageEvent", {
                    detail: { kind, ...data },
                }));
            } catch (e) { /* malformed event — ignore */ }
        };
        es.addEventListener("page-saved", (ev) => dispatch(ev.data, "page-saved"));
        es.addEventListener("page-deleted", (ev) => dispatch(ev.data, "page-deleted"));
        es.addEventListener("error", () => {
            // EventSource will auto-reconnect; don't log on every
            // hiccup or the console drowns in noise during sleep/wake.
        });
        // Keep a handle for debug + so future code can re-open after
        // logout. Read-only.
        window.familiarPageEvents = es;
    })();

    // Status bar context setter (DESIGN.md). Surface
    // modules call this with their per-category context string
    // when they become the active tab. The right cluster of the
    // status bar reflects the active document kind:
    //   Notes   → "N words · N backlinks"
    //   Wiki    → "N linked pages · reviewed YYYY-MM-DD"
    //   Chat    → "model · ctx N/N"
    //   Shards  → "last run · status"
    // Empty string clears the slot.
    window.familiarStatusBar = {
        setContext(text) {
            const el = document.getElementById("statusbar-context");
            if (el) el.textContent = text || "";
        },
    };

    // ── Dashboard (FAMILIAR-DASHBOARD-SPEC Phase G) ─────────────
    //
    // The user-facing landing page. Card grid fed by the seven
    // /console/api/dashboard/* endpoints; admins see a user picker
    // that re-scopes every card via ?user_id=. Non-admins see their
    // own data — the server silently filters regardless of what the
    // URL says, so no client-side enforcement is needed.

    const dashState = {
        initialized: false,
        viewingUser: "",       // admin picker: "" = own, else userID
        cy: null,              // preview cytoscape instance
    };

    // dashScope mirrors withGraphScope — one helper per panel keeps
    // the admin-override flow explicit at each call site.
    function dashScope(url) {
        if (!dashState.viewingUser) return url;
        const sep = url.includes("?") ? "&" : "?";
        return url + sep + "user_id=" + encodeURIComponent(dashState.viewingUser);
    }

    async function initDashboard() {
        if (dashState.initialized) return;
        dashState.initialized = true;

        document.getElementById("dash-refresh").addEventListener("click", () => loadDashboard());
        document.getElementById("dash-viewas-clear").addEventListener("click", () => dashViewAs(""));

        // Card "View → " links route to the corresponding panel via
        // switchPanel. Delegated so late-rendered DOM (recent-writes
        // rows that link to memory detail) picks up the same handler.
        document.getElementById("panel-dashboard").addEventListener("click", (e) => {
            const link = e.target.closest("[data-panel]");
            if (!link) return;
            // Ignore the nav-left brand click bubbling through — it
            // has its own handler on the sidebar path.
            if (!link.classList.contains("dash-card-link") &&
                !link.classList.contains("dash-graph-preview")) {
                return;
            }
            e.preventDefault();
            switchPanel(link.dataset.panel);
        });

        await loadDashboard();
    }

    // dashViewAs flips the dashboard between "mine" and the admin
    // cross-user view (Users → View dashboard). The backend ignores
    // ?user_id= for role=user sessions, and nothing on the personal
    // path sets this.
    function dashViewAs(userID) {
        userID = userID || "";
        if (userID === dashState.viewingUser) return;
        dashState.viewingUser = userID;
        const banner = document.getElementById("dash-viewas");
        banner.hidden = !userID;
        if (userID) document.getElementById("dash-viewas-user").textContent = userID;
        if (dashState.initialized) loadDashboard();
    }

    // loadDashboard fetches every card's data in parallel and renders
    // them independently. Each render is fault-tolerant: a single
    // endpoint's failure does not prevent the rest from rendering.
    async function loadDashboard() {
        setError("dash-global-error", null);

        const calls = [
            ["overview",          "/console/api/dashboard/overview",          renderHero],
            ["memory-stats",      "/console/api/dashboard/overview",          renderMemoryStats],
            ["entity-breakdown",  "/console/api/dashboard/entity_breakdown",  renderEntityBreakdown],
            ["recent-sessions",   "/console/api/dashboard/recent_sessions?limit=5", renderRecentSessions],
            ["recent-writes",     "/console/api/dashboard/recent_writes?limit=5",   renderRecentWrites],
            ["shard-summary",     "/console/api/dashboard/shard_summary",     renderShardSummary],
            ["graph-preview",     "/console/api/dashboard/graph_preview?limit=15",  renderGraphPreview],
            ["growth-sparkline",  "/console/api/dashboard/growth_sparkline?days=30", renderSparkline],
        ];

        // Dedupe GETs to /overview (both hero + memory-stats read it).
        const seen = new Map();
        const tasks = calls.map(([key, path, render]) => {
            const url = dashScope(path);
            const p = seen.has(url) ? seen.get(url) : apiJSON(url);
            seen.set(url, p);
            return p.then(
                (data) => render(data),
                (err) => console.warn("dashboard " + key + ": " + err.message),
            );
        });
        await Promise.allSettled(tasks);
    }

    // ── Renderers ──────────────────────────────────────────────

    function renderHero(overview) {
        const title = document.getElementById("dash-hero-title");
        const sub = document.getElementById("dash-hero-sub");
        const name = overview.display_name || overview.user_id || "there";

        title.textContent = "Hi " + name + ".";

        if (!overview.last_chat_at) {
            sub.textContent = "No chat history yet — say hi to get started.";
            return;
        }
        const ago = humanizeTimeAgo(overview.last_chat_at);
        const facts = (overview.fact_count || 0).toLocaleString();
        sub.textContent = "Last chat " + ago + ". Familiar knows " + facts + " facts about your world.";
    }

    function renderMemoryStats(overview) {
        document.getElementById("dash-fact-count").textContent = fmtInt(overview.fact_count || 0);
        document.getElementById("dash-entity-count").textContent = fmtInt(overview.entity_count || 0);
        document.getElementById("dash-rel-count").textContent = fmtInt(overview.relationship_count || 0);

        const empty = (overview.fact_count || 0) === 0 &&
                      (overview.entity_count || 0) === 0 &&
                      (overview.relationship_count || 0) === 0;
        document.getElementById("dash-memory-stats").hidden = empty;
        document.getElementById("dash-memory-empty").hidden = !empty;
    }

    function renderEntityBreakdown(data) {
        const host = document.getElementById("dash-entity-breakdown");
        const empty = document.getElementById("dash-entities-empty");
        host.innerHTML = "";
        const rows = (data && data.breakdown) || [];
        if (rows.length === 0) {
            host.hidden = true;
            empty.hidden = false;
            return;
        }
        host.hidden = false;
        empty.hidden = true;
        const max = Math.max(...rows.map((r) => r.count), 1);
        for (const r of rows) {
            const row = document.createElement("div");
            row.className = "dash-breakdown-row";
            const label = document.createElement("span");
            label.className = "dash-breakdown-label";
            label.textContent = r.type;
            const bar = document.createElement("span");
            bar.className = "dash-breakdown-bar";
            const fill = document.createElement("span");
            fill.className = "dash-breakdown-fill";
            fill.style.width = ((r.count / max) * 100).toFixed(1) + "%";
            bar.appendChild(fill);
            const num = document.createElement("span");
            num.className = "dash-breakdown-num";
            num.textContent = fmtInt(r.count);
            row.append(label, bar, num);
            host.appendChild(row);
        }
    }

    function renderRecentSessions(data) {
        const list = document.getElementById("dash-sessions-list");
        const empty = document.getElementById("dash-sessions-empty");
        list.innerHTML = "";
        const items = (data && data.items) || [];
        if (items.length === 0) {
            list.hidden = true;
            empty.hidden = false;
            return;
        }
        list.hidden = false;
        empty.hidden = true;
        for (const s of items) {
            const li = document.createElement("li");
            li.className = "dash-list-row";
            const head = document.createElement("div");
            head.className = "dash-list-head";
            const platform = document.createElement("span");
            platform.className = "dash-list-tag";
            platform.textContent = s.platform || "session";
            const ago = document.createElement("span");
            ago.className = "dash-list-ago";
            ago.textContent = humanizeTimeAgo(s.last_active);
            head.append(platform, ago);
            const sub = document.createElement("div");
            sub.className = "dash-list-sub";
            sub.textContent = (s.turns || 0) + " turn" + (s.turns === 1 ? "" : "s") + " · " + (s.channel_id || s.id);
            li.append(head, sub);
            list.appendChild(li);
        }
    }

    function renderRecentWrites(data) {
        const list = document.getElementById("dash-writes-list");
        const empty = document.getElementById("dash-writes-empty");
        list.innerHTML = "";
        const items = (data && data.items) || [];
        if (items.length === 0) {
            list.hidden = true;
            empty.hidden = false;
            return;
        }
        list.hidden = false;
        empty.hidden = true;
        for (const w of items) {
            const li = document.createElement("li");
            li.className = "dash-list-row";
            const body = document.createElement("div");
            body.className = "dash-list-body";
            body.textContent = w.snippet;
            const meta = document.createElement("div");
            meta.className = "dash-list-sub";
            const src = w.source_type || "save";
            meta.textContent = src + " · " + humanizeTimeAgo(w.created_at);
            li.append(body, meta);
            list.appendChild(li);
        }
    }

    function renderShardSummary(data) {
        const items = (data && data.shards) || [];
        const num = document.getElementById("dash-shard-summary-num");
        const meta = document.getElementById("dash-shard-summary-meta");
        const empty = document.getElementById("dash-shards-empty");
        if (items.length === 0) {
            num.hidden = true;
            meta.hidden = true;
            empty.hidden = false;
            return;
        }
        num.hidden = false;
        meta.hidden = false;
        empty.hidden = true;
        num.textContent = fmtInt(items.length);
        const active = items.filter((s) => !s.disabled).length;
        meta.textContent = active === items.length
            ? "all active"
            : active + " active · " + (items.length - active) + " disabled";
    }

    // renderSparkline — hand-rolled SVG line chart. Two polylines
    // (facts + entities) on a shared x-axis. Y-axis is scaled per
    // series independently so one line doesn't flatten the other.
    // No axes, no gridlines, no legend — dashboard sparklines
    // communicate trend, not precision.
    function renderSparkline(data) {
        const wrap = document.getElementById("dash-sparkline-wrap");
        const delta = document.getElementById("dash-sparkline-delta");
        const empty = document.getElementById("dash-sparkline-empty");
        const series = (data && data.series) || [];

        if (series.length < 2 || series[series.length - 1].fact_count === 0) {
            wrap.innerHTML = "";
            wrap.hidden = true;
            delta.hidden = true;
            empty.hidden = false;
            return;
        }
        wrap.hidden = false;
        delta.hidden = false;
        empty.hidden = true;

        const W = 320, H = 80, pad = 4;
        const n = series.length;
        const scaleX = (i) => pad + (i / (n - 1)) * (W - 2 * pad);
        const scaleY = (v, max) => {
            if (max <= 0) return H - pad;
            return H - pad - (v / max) * (H - 2 * pad);
        };
        const maxF = Math.max(...series.map((p) => p.fact_count), 1);
        const maxE = Math.max(...series.map((p) => p.entity_count), 1);

        const pts = (getter, max) =>
            series.map((p, i) => scaleX(i).toFixed(1) + "," + scaleY(getter(p), max).toFixed(1)).join(" ");

        const factsPts = pts((p) => p.fact_count, maxF);
        const entsPts  = pts((p) => p.entity_count, maxE);

        // Minimal SVG — one line per series + an end-of-line dot on
        // the fact series so the "today" value has a visual anchor.
        const lastX = scaleX(n - 1).toFixed(1);
        const lastY = scaleY(series[n - 1].fact_count, maxF).toFixed(1);

        wrap.innerHTML =
            '<svg class="dash-sparkline" viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none">' +
                '<polyline class="dash-sparkline-line dash-sparkline-ents" points="' + entsPts + '"/>' +
                '<polyline class="dash-sparkline-line dash-sparkline-facts" points="' + factsPts + '"/>' +
                '<circle class="dash-sparkline-dot" cx="' + lastX + '" cy="' + lastY + '" r="2.5"/>' +
            '</svg>';

        const dFacts = series[n - 1].fact_count - series[0].fact_count;
        const dEnts  = series[n - 1].entity_count - series[0].entity_count;
        delta.textContent = fmtSigned(dFacts) + " facts, " + fmtSigned(dEnts) + " entities over " + (n - 1) + " days";
    }

    // renderGraphPreview reuses the shared ensureCytoscape loader
    // (single cached promise across panels), then
    // renders a static read-only view. Clicking anywhere inside the
    // preview navigates to the full graph panel — the preview is a
    // thumbnail, not an interactive toy.
    async function renderGraphPreview(data) {
        const host = document.getElementById("dash-graph-preview");
        const empty = document.getElementById("dash-graph-empty");
        const nodes = (data && data.nodes) || [];
        if (nodes.length === 0) {
            host.innerHTML = "";
            host.hidden = true;
            empty.hidden = false;
            return;
        }
        host.hidden = false;
        empty.hidden = true;

        let cy;
        try {
            cy = await ensureCytoscape();
        } catch (err) {
            host.innerHTML = '<div class="dash-empty">Graph preview unavailable (cytoscape failed to load).</div>';
            return;
        }

        host.innerHTML = "";
        const elements = [];
        for (const n of nodes) {
            elements.push({ data: { id: n.id, label: n.label, degree: n.degree } });
        }
        for (const e of (data.edges || [])) {
            elements.push({ data: { id: e.id, source: e.source, target: e.target } });
        }

        // Destroy any previous preview so listeners don't leak on
        // refresh / user-picker change.
        if (dashState.cy) {
            dashState.cy.destroy();
            dashState.cy = null;
        }
        dashState.cy = cy({
            container: host,
            elements: elements,
            userZoomingEnabled: false,
            userPanningEnabled: false,
            boxSelectionEnabled: false,
            autoungrabify: true,
            style: [
                {
                    selector: "node",
                    style: {
                        "background-color": "#6A4CE0",
                        "label": "data(label)",
                        "color": "#C7C7D1",
                        "font-size": 10,
                        "font-family": "Geist Mono, monospace",
                        "text-valign": "bottom",
                        "text-margin-y": 4,
                        "width": 18,
                        "height": 18,
                    },
                },
                {
                    selector: "edge",
                    style: {
                        "width": 1,
                        "line-color": "rgba(255,255,255,0.16)",
                        "curve-style": "bezier",
                    },
                },
            ],
            layout: { name: "cose", animate: false, padding: 12, fit: true },
        });

        // Whole-card click → full graph panel. Using the host element
        // (not cy.on tap) so clicks on empty canvas also navigate.
        host.onclick = () => switchPanel("memory-graph");
    }

    // ── Helpers ────────────────────────────────────────────────

    function humanizeTimeAgo(ts) {
        if (!ts) return "";
        const d = new Date(ts);
        const diffSec = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
        if (diffSec < 45) return "just now";
        if (diffSec < 90) return "a minute ago";
        const m = Math.floor(diffSec / 60);
        if (m < 60) return m + "m ago";
        const h = Math.floor(m / 60);
        if (h < 24) return h + "h ago";
        const days = Math.floor(h / 24);
        if (days < 30) return days + "d ago";
        return d.toLocaleDateString();
    }

    function fmtSigned(n) {
        if (n > 0) return "+" + n.toLocaleString();
        return n.toLocaleString();
    }

    // ── System status ───────────────────────────────────────────
    //
    // The admin-only telemetry board (gateway uptime, model health,
    // memory totals, registered skills). Phase G adds a separate
    // user-facing Dashboard; this panel stays as operator-only.

    // ── User / Config panel (DESIGN.md) ──────────────
    //
    // The User panel re-homes the secondary surfaces (Profile,
    // Memory, Memory graph, Hot memory, Sessions, Skills, Users,
    // System status) inside a single grouped sub-nav. Sub-section
    // panels stay where they are in the DOM until the user clicks
    // their row, at which point JS relocates the existing
    // panel-* element into user-content-host. One-time move; the
    // existing init/render logic for each panel keeps working
    // because the IDs are unchanged.

    let userPanelWired = false;
    let lastUserSection = "profile";
    const sectionToPanelLoader = {
        profile: "profile",
        memory: "memory",
        "memory-graph": "memory-graph",
        skills: "skills",
        "system-skills": "skills",
        users: "users",
        "system-status": "system-status",
        "system-prompt": "system-prompt",
    };

    function initUserPanel() {
        if (!userPanelWired) {
            userPanelWired = true;
            wireUserPanel();
        }
        // Default to the last section the user opened, or
        // Profile on first visit.
        switchUserSection(lastUserSection);
    }

    function wireUserPanel() {
        const subnav = document.getElementById("user-subnav");
        if (subnav) {
            subnav.addEventListener("click", (e) => {
                const row = e.target.closest(".user-subnav-row");
                if (!row) return;
                e.preventDefault();
                const name = row.dataset.userSection;
                if (!name) return;
                switchUserSection(name);
            });
        }
        const signout = document.getElementById("user-signout");
        if (signout) {
            signout.addEventListener("click", async (e) => {
                e.preventDefault();
                try {
                    await apiJSON("/console/api/auth/logout", { method: "POST" });
                } catch (e2) { /* ignore */ }
                location.reload();
            });
        }
    }

    function switchUserSection(name) {
        lastUserSection = name;
        const host = document.getElementById("user-content-host");
        if (!host) return;

        // Update sub-nav active state.
        for (const row of document.querySelectorAll("#user-subnav .user-subnav-row")) {
            row.classList.toggle("is-active", row.dataset.userSection === name);
        }

        // Find the matching panel-* element. If it's not yet a
        // child of host, relocate it here. Then hide every
        // sibling so only this one shows.
        const targetId = "panel-" + name;
        const target = document.getElementById(targetId);
        if (!target) {
            // Section not yet implemented (e.g. preferences,
            // rules, audit, flags). Show empty placeholder.
            const empty = document.getElementById("user-content-empty");
            if (empty) {
                empty.hidden = false;
                empty.textContent = name + " section is a placeholder; coming in a polish pass.";
            }
            return;
        }

        // Empty placeholder hides once a real section opens.
        const empty = document.getElementById("user-content-empty");
        if (empty) empty.hidden = true;

        // Move the panel into the user-content host on first
        // render. After that, just toggle visibility.
        if (target.parentElement !== host) {
            host.appendChild(target);
        }
        for (const child of host.children) {
            // Only show the chosen target panel; everything else
            // (other relocated panel-* siblings, the empty
            // placeholder) is hidden.
            child.hidden = child !== target;
        }
        // Re-show the chosen one (we just hid it above when
        // looping).
        target.hidden = false;

        // Trigger the underlying panel's loader so the section
        // initializes its data fetch (memory rows, sessions list,
        // etc.). Reuse panelLoaded so we don't double-init.
        const loaderKey = sectionToPanelLoader[name];
        if (loaderKey && !panelLoaded[loaderKey]) {
            panelLoaded[loaderKey] = true;
            const fn = panelLoaders[loaderKey];
            if (fn) fn();
        }

        // The personal Memory/Graph sections are always self-scoped
        // — if an admin left either panel in cross-user view-as mode
        // (entered via the Users panel), coming here resets it.
        if (name === "memory") {
            const mp = window.familiarPanels && window.familiarPanels.memory;
            if (mp && mp.viewAsUser) mp.viewAsUser("");
        }
        if (name === "memory-graph") {
            const gp = window.familiarPanels && window.familiarPanels["memory-graph"];
            if (gp && gp.viewAs) gp.viewAs("");
        }
        if (name === "profile") {
            const pp = window.familiarPanels && window.familiarPanels.profile;
            if (pp && pp.viewAs) pp.viewAs("");
        }
    }

    // Expose so workspace.js's mode router can call switchPanel
    // + initUserPanel correctly when data-mode=user is clicked.
    window.appUserPanel = { switchSection: switchUserSection };

    // ── Home — 4-quadrant launchpad ─────────────────────────────
    //
    // Pinned · Quad (search + action cards) — Recent · Weather.
    // A greeting line sits on top. Pinned + Recent pull live data;
    // the Quad wires note/chat/shard creation; Weather calls the
    // home weather endpoint once the browser shares a location.

    let homeWired = false;

    function initHome() {
        if (!homeWired) {
            homeWired = true;
            wireHomeQuad();
        }
        loadHome();
    }

    async function loadHome() {
        renderGreeting();
        // Shard sessions skip the personal pinned + recent lists +
        // weather — all owner-scoped, and a kiosk identity can't
        // meaningfully act on them.
        const sess = window.FAMILIAR_SESSION || {};
        if (sess.principal_type === "shard") {
            return;
        }
        loadHomeWeather();
        await Promise.allSettled([loadHomeRecent(), loadHomePins()]);
    }

    // Inline SVG for a surface kind, stroked in its category color.
    // Colors come from a fixed table, so innerHTML insertion is safe.
    function homeKindIcon(kind) {
        const map = {
            chat:   ["var(--moss-400)", '<path d="M4 5.5 a2 2 0 0 1 2 -2 h12 a2 2 0 0 1 2 2 v9 a2 2 0 0 1 -2 2 H9 l-4 3.5 V5.5 Z"/>'],
            note:   ["var(--iris-400)", '<path d="M6 3 h8 l5 5 v12 a1.5 1.5 0 0 1 -1.5 1.5 H6 a1.5 1.5 0 0 1 -1.5 -1.5 V4.5 A1.5 1.5 0 0 1 6 3 Z"/><path d="M14 3 v3.5 A1.5 1.5 0 0 0 15.5 8 H19"/>'],
            wiki:   ["var(--slate-400)", '<rect x="4" y="6" width="4" height="14" rx="1"/><path d="M4 9.5 H8"/><rect x="9" y="4.5" width="4" height="15.5" rx="1"/><path d="M9 8 H13"/><path d="M15.2 6.3 l3.6 -1 a1 1 0 0 1 1.25 0.7 l3.3 12 a1 1 0 0 1 -0.7 1.25 l-3.6 1 a1 1 0 0 1 -1.25 -0.7 l-3.3 -12 a1 1 0 0 1 0.7 -1.25 Z"/>'],
            shards: ["var(--sunlamp-400)", '<path d="M14 2.2 L20.5 9.6 L17.2 21.6 L7.4 19 L4.2 12.4 Z"/><path d="M14 2.2 L10.4 11.6 L4.2 12.4"/><path d="M10.4 11.6 L17.2 21.6"/>'],
        };
        const spec = map[kind] || map.note;
        return '<svg class="home-rec-ico" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="' +
            spec[0] + '" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">' + spec[1] + '</svg>';
    }

    // /console/api/home/pins returns pinned notes + chats + wiki
    // pages, newest-updated first. Each renders as a rec-list row
    // that routes to the right surface on click.
    async function loadHomePins() {
        const list = document.getElementById("home-pinned-grid");
        if (!list) return;
        let items = [];
        try {
            const resp = await apiJSON("/console/api/home/pins");
            items = (resp && resp.items) || [];
        } catch (e) {
            return; // leave the placeholder rather than blanking
        }
        list.innerHTML = "";
        if (items.length === 0) {
            const empty = document.createElement("div");
            empty.className = "home-empty";
            empty.textContent = "Nothing pinned yet. Pin a chat or note from its ⋯ menu.";
            list.appendChild(empty);
            return;
        }
        for (const it of items) {
            const row = document.createElement("button");
            row.type = "button";
            row.className = "home-rec-item";
            row.innerHTML = homeKindIcon(it.kind);
            const title = document.createElement("span");
            title.className = "home-rec-title";
            title.textContent = it.title || "Untitled";
            row.appendChild(title);
            row.addEventListener("click", () => openPin(it));
            list.appendChild(row);
        }
    }

    // Routes a pin click to the right workspace surface — opens (or
    // focuses) a tab and asks that surface to load the record. Same
    // path the sidebar children use: focusSurface → familiar:openDoc.
    function openPin(it) {
        let surface = "notes";
        if (it.kind === "chat") surface = "chat";
        else if (it.kind === "wiki") surface = "wiki";
        const ws = window.FamiliarWorkspace;
        if (ws && ws.openDoc) {
            // The workspace's tab-targeting contract (dedup → same-
            // surface splash → new left-most tab) + tabId-threaded
            // dispatch. The old focusSurface+dispatch recipe could
            // override a doc-bearing tab in split layouts.
            ws.openDoc(
                surface,
                it.kind === "wiki" ? it.book_slug : it.id,
                it.title || null,
                it.kind === "wiki" ? { pageId: it.id } : undefined,
            );
            return;
        }
        // Legacy fallback (workspace not mounted — shouldn't happen).
        if (window.appSwitchPanel) window.appSwitchPanel("workspace");
        if (ws && ws.focusSurface) ws.focusSurface(surface);
        window.dispatchEvent(new CustomEvent("familiar:openDoc", {
            detail: {
                id: it.kind === "wiki" ? it.book_slug : it.id,
                surface: surface,
                pageId: it.kind === "wiki" ? it.id : undefined,
            },
        }));
    }

    // When any surface flips a pin, refresh the home pins list (if
    // home-pinned-grid is in the DOM). Cheap: one GET when the user
    // toggles a pin.
    window.addEventListener("familiar:pinsChanged", () => {
        loadHomePins();
    });

    function renderGreeting() {
        const sess = window.FAMILIAR_SESSION || {};
        const line = document.getElementById("home-greeting-line");
        if (!line) return;

        // Shard sessions get a different greeting — they're a kiosk
        // identity, not a person who'd appreciate "Good morning."
        // Lead with the shard's name so a kitchen iPad reads as its
        // own identity.
        if (sess.principal_type === "shard") {
            const shardName = sess.shard_name || sess.shard_id || "Shard";
            line.textContent = shardName + ".";
            return;
        }

        const name = sess.display_name || sess.user || "there";
        const hour = new Date().getHours();
        let timeOfDay = "Hello";
        if (hour < 5) timeOfDay = "Up late";
        else if (hour < 12) timeOfDay = "Good morning";
        else if (hour < 17) timeOfDay = "Good afternoon";
        else timeOfDay = "Good evening";
        // One uniform line — the name is not visually set apart.
        line.textContent = timeOfDay + ", " + name + ".";
    }

    async function loadHomeRecent() {
        const list = document.getElementById("home-recent-list");
        if (!list) return;
        list.innerHTML = "";

        // Pull a unified recent feed from the existing dashboard
        // endpoints. Two streams: recent_writes (notes/memories
        // saved recently) + recent_sessions (chat conversations).
        // Merge by timestamp, render up to 12 rows.
        let writes = [];
        let sessions = [];
        try {
            const resp = await apiJSON("/console/api/dashboard/recent_writes?limit=8");
            writes = (resp && resp.items) || [];
        } catch (e) { /* skill or endpoint not available — soft-fail */ }
        try {
            const resp = await apiJSON("/console/api/dashboard/recent_sessions?limit=8");
            sessions = (resp && resp.items) || [];
        } catch (e) { /* same — Home keeps rendering with what it has */ }

        const rows = [];
        for (const w of writes) {
            rows.push({
                kind: "note",
                title: w.snippet || w.source_type || "memory",
                ts: new Date(w.created_at).getTime() || 0,
            });
        }
        for (const s of sessions) {
            rows.push({
                kind: "chat",
                title: s.channel_id || s.id || "session",
                ts: new Date(s.last_active).getTime() || 0,
            });
        }
        rows.sort((a, b) => b.ts - a.ts);
        const top = rows.slice(0, 12);

        if (top.length === 0) {
            const li = document.createElement("li");
            li.className = "home-empty";
            li.textContent = "Nothing yet — start a chat or write a note to fill this in.";
            list.appendChild(li);
            return;
        }
        for (const r of top) {
            const li = document.createElement("li");
            li.className = "home-rec-item";
            li.innerHTML = homeKindIcon(r.kind);
            const title = document.createElement("span");
            title.className = "home-rec-title";
            title.textContent = r.title;
            const ago = document.createElement("span");
            ago.className = "home-rec-ago";
            ago.textContent = agoOrDateLocal(r.ts);
            li.append(title, ago);
            li.addEventListener("click", () => openNewDoc(r.kind === "chat" ? "chat" : "notes"));
            list.appendChild(li);
        }
    }

    // ── Quad — search box + four action cards ───────────────────

    function wireHomeQuad() {
        const noteBtn  = document.getElementById("home-act-note");
        const chatBtn  = document.getElementById("home-act-chat");
        const schedBtn = document.getElementById("home-act-scheduled");
        const shardBtn = document.getElementById("home-act-shard");
        const search   = document.getElementById("home-search-input");
        const searchBox = document.getElementById("home-search-box");

        if (noteBtn) noteBtn.addEventListener("click", () => openNewDoc("notes"));
        if (chatBtn) chatBtn.addEventListener("click", () => openNewDoc("chat"));
        if (shardBtn) shardBtn.addEventListener("click", () => {
            // Shards already have a "new shard" affordance on the
            // Shards surface — route there rather than duplicating it.
            if (window.appSwitchPanel) window.appSwitchPanel("workspace");
            const ws = window.FamiliarWorkspace;
            if (ws && ws.focusSurface) ws.focusSurface("shards");
        });
        if (schedBtn) schedBtn.addEventListener("click", () => {
            // SCHEDULED-ACTIONS-SPEC Phase 1: the card opens the
            // scheduled-actions panel.
            if (window.appSwitchPanel) window.appSwitchPanel("scheduled");
        });
        if (searchBox && search) {
            searchBox.addEventListener("click", () => search.focus());
            search.addEventListener("keydown", (e) => {
                if (e.key === "Enter" && search.value.trim()) {
                    toast("Global search is coming soon.", "info");
                }
            });
        }
    }

    // Open (or focus) a notes/chat tab and ask the surface to start
    // a fresh document. Mirrors openPin: focusSurface guarantees a
    // live shell, then a no-id openDoc triggers newNote() /
    // newConversation() on that surface.
    function openNewDoc(surface) {
        const ws = window.FamiliarWorkspace;
        if (ws && ws.openDoc) {
            ws.openDoc(surface, null, null);
            return;
        }
        if (window.appSwitchPanel) window.appSwitchPanel("workspace");
        if (ws && ws.focusSurface) ws.focusSurface(surface);
        window.dispatchEvent(new CustomEvent("familiar:openDoc", {
            detail: { surface: surface },
        }));
    }

    // ── Weather quadrant ────────────────────────────────────────
    //
    // The widget needs a location. The server keeps no per-user
    // location, so coordinates come from browser geolocation. iOS
    // Safari (and a home-screen PWA) does NOT persist the geolocation
    // permission across launches — relying on it re-prompts the user
    // on every visit (the iPad-kiosk bug). So once the browser grants
    // a position we remember the COORDINATES on this device and reuse
    // them on subsequent loads, never re-prompting. A fixed device's
    // location doesn't move; "Share location" forces a fresh ask.

    const WX_LOC_KEY = "familiar.weather.loc";

    function cachedWeatherLoc() {
        try {
            const o = JSON.parse(localStorage.getItem(WX_LOC_KEY) || "null");
            if (o && typeof o.lat === "number" && typeof o.lon === "number") return o;
        } catch (_) { /* corrupt entry — ignore */ }
        return null;
    }
    function saveWeatherLoc(lat, lon) {
        try {
            localStorage.setItem(WX_LOC_KEY, JSON.stringify({ lat: lat, lon: lon, t: Date.now() }));
        } catch (_) { /* storage disabled / full — degrade to re-prompting */ }
    }

    async function loadHomeWeather(forceLocate) {
        const host = document.getElementById("home-weather");
        if (!host) return;

        // 1. Stored-location path — no params, no permission prompt.
        // (The endpoint currently returns no_location; kept so a future
        // server-side location resolves here without a client change.)
        let data = null;
        try {
            data = await apiJSON("/console/api/home/weather");
        } catch (e) { /* fall through to remembered/browser geolocation */ }
        if (data && !data.error) {
            renderHomeWeather(host, data);
            return;
        }

        // 2. Coordinates this device already shared — reuse silently so
        // we don't re-trigger the permission prompt every launch.
        if (!forceLocate) {
            const loc = cachedWeatherLoc();
            if (loc) {
                fetchHomeWeather(host, loc.lat, loc.lon);
                return;
            }
        }

        // 3. First time (or a forced re-locate) — ask the browser once,
        // then remember the answer for next time.
        if (!navigator.geolocation) {
            renderWeatherPrompt(host, "Location isn't available in this browser.");
            return;
        }
        navigator.geolocation.getCurrentPosition(
            (pos) => {
                saveWeatherLoc(pos.coords.latitude, pos.coords.longitude);
                fetchHomeWeather(host, pos.coords.latitude, pos.coords.longitude);
            },
            ()    => renderWeatherPrompt(host),
            { timeout: 8000, maximumAge: 30 * 60 * 1000 },
        );
    }

    function renderWeatherPrompt(host, msg) {
        host.innerHTML = "";
        const wrap = document.createElement("div");
        wrap.className = "home-wx-prompt";
        const p = document.createElement("p");
        p.textContent = msg ||
            "Share your location with your familiar to see your local forecast.";
        const btn = document.createElement("button");
        btn.type = "button";
        btn.textContent = "Share location";
        btn.addEventListener("click", () => {
            host.innerHTML = '<div class="home-empty" style="padding:22px">Loading…</div>';
            loadHomeWeather(true); // explicit user action → re-ask the browser
        });
        wrap.append(p, btn);
        host.appendChild(wrap);
    }

    async function fetchHomeWeather(host, lat, lon) {
        let data;
        try {
            data = await apiJSON("/console/api/home/weather?lat=" +
                encodeURIComponent(lat.toFixed(4)) +
                "&lon=" + encodeURIComponent(lon.toFixed(4)));
        } catch (e) {
            renderWeatherPrompt(host, "Couldn't load the forecast right now.");
            return;
        }
        renderHomeWeather(host, data);
    }

    function renderHomeWeather(host, d) {
        host.innerHTML = "";
        const body = document.createElement("div");
        body.className = "home-wx-body";

        const top = document.createElement("div");
        top.className = "home-wx-top";
        top.innerHTML =
            '<div class="home-wx-icon">' + weatherGlyph(d.icon) + "</div>" +
            '<div class="home-wx-temp">' + Math.round(d.temp_f || 0) +
            '<span class="deg">°F</span></div>';

        const cond = document.createElement("div");
        cond.className = "home-wx-cond";
        const label = document.createElement("span");
        label.className = "label";
        label.textContent = d.condition || d.summary || "—";
        const sub = document.createElement("span");
        sub.className = "sub";
        sub.textContent = "feels " + Math.round(d.feels_f || 0) + "° · " +
            Math.round(d.wind_mph || 0) + " mph" + (d.wind_dir ? " " + d.wind_dir : "");
        const hilo = document.createElement("span");
        hilo.className = "hilo";
        const hi = document.createElement("span");
        hi.className = "hi";
        hi.textContent = "↑ " + Math.round(d.high_f || 0) + "°";
        const lo = document.createElement("span");
        lo.className = "lo";
        lo.textContent = "↓ " + Math.round(d.low_f || 0) + "°";
        hilo.append(hi, lo);
        cond.append(label, sub, hilo);
        top.appendChild(cond);
        body.appendChild(top);

        if (d.description) {
            const desc = document.createElement("p");
            desc.className = "home-wx-desc";
            desc.textContent = d.description;
            body.appendChild(desc);
        }

        const hours = (d.hourly || []).slice(0, 8);
        if (hours.length) {
            const strip = document.createElement("div");
            strip.className = "home-wx-hourly";
            hours.forEach((h, i) => {
                const cell = document.createElement("div");
                cell.className = "home-wx-hour" + (i === 0 ? " now" : "");
                const t = document.createElement("div");
                t.className = "t";
                t.textContent = i === 0 ? "NOW" : hourLabel(h.time);
                const v = document.createElement("div");
                v.className = "v";
                v.textContent = Math.round(h.temp_f || 0) + "°";
                cell.append(t, v);
                strip.appendChild(cell);
            });
            body.appendChild(strip);
        }
        host.appendChild(body);
    }

    // Local "3 PM"-style label for an hourly bucket (unix seconds).
    function hourLabel(ts) {
        const dt = new Date((ts || 0) * 1000);
        let h = dt.getHours();
        const ap = h < 12 ? "AM" : "PM";
        h = h % 12;
        if (h === 0) h = 12;
        return h + " " + ap;
    }

    // Coarse weather glyph keyed off the Pirate Weather icon string.
    // Three buckets — sun, cloud, rain — keep the widget readable
    // without shipping a full icon set.
    function weatherGlyph(icon) {
        const sun =
            '<g stroke="var(--sunlamp-400)" stroke-width="1.6" stroke-linecap="round" fill="none">' +
            '<circle cx="18" cy="18" r="6"/>' +
            '<path d="M18 5v3M18 28v3M5 18h3M28 18h3M9 9l2 2M25 25l2 2M27 9l-2 2M11 25l-2 2"/></g>';
        const cloud =
            '<path d="M11 25h14a5 5 0 0 0 0-10 7 7 0 0 0-13.5-1A4 4 0 0 0 11 25z" ' +
            'stroke="var(--graphite-300)" stroke-width="1.6" fill="none"/>';
        const rain = cloud +
            '<g stroke="var(--slate-400)" stroke-width="1.6" stroke-linecap="round">' +
            '<path d="M14 28l-1.5 3M20 28l-1.5 3M26 28l-1.5 3"/></g>';
        let inner = cloud;
        if (icon && icon.indexOf("clear") === 0) inner = sun;
        else if (icon === "rain" || icon === "sleet" || icon === "snow" ||
                 icon === "hail" || icon === "thunderstorm") inner = rain;
        else if (icon && icon.indexOf("partly-cloudy") === 0)
            inner = '<g transform="translate(-3 -4) scale(0.7)">' + sun + "</g>" +
                    '<g transform="translate(4 3)">' + cloud + "</g>";
        return '<svg width="40" height="40" viewBox="0 0 36 36" fill="none">' + inner + "</svg>";
    }

    function agoOrDateLocal(ts) {
        if (!ts) return "";
        const diff = Date.now() - ts;
        if (diff < 60_000) return "just now";
        if (diff < 3_600_000) return Math.round(diff / 60_000) + "m ago";
        if (diff < 86_400_000) return Math.round(diff / 3_600_000) + "h ago";
        if (diff < 30 * 86_400_000) return Math.round(diff / 86_400_000) + "d ago";
        return new Date(ts).toLocaleDateString();
    }

    // ── System status ───────────────────────────────────────────

    const DASH_REFRESH_MS = 30000;
    let dashTimer = null;

    function initSystemStatus() {
        loadSystemStatus();
        if (dashTimer) clearInterval(dashTimer);
        dashTimer = setInterval(() => {
            if (currentPanel === "system-status") loadSystemStatus();
        }, DASH_REFRESH_MS);
    }

    async function loadSystemStatus() {
        setError("dash-error", null);
        let data;
        try {
            data = await apiJSON("/console/api/status");
        } catch (e) {
            setError("dash-error", e);
            return;
        }
        const uptimeSecs = data.gateway && data.gateway.uptime_seconds;
        const uptimeEl = document.getElementById("dash-uptime");
        uptimeEl.innerHTML = "";
        const uptimeDot = document.createElement("span");
        uptimeDot.className = "health-dot " + (typeof uptimeSecs === "number" ? "healthy" : "critical");
        uptimeEl.appendChild(uptimeDot);
        uptimeEl.appendChild(document.createTextNode(fmtUptime(uptimeSecs)));

        document.getElementById("dash-memory").textContent = fmtInt(data.memory && data.memory.total);
        const u = data.users || {};
        document.getElementById("dash-users-total").textContent = fmtInt(u.total);
        document.getElementById("dash-users-detail").textContent =
            (u.approved != null ? u.approved : "—") + " / " + (u.pending != null ? u.pending : "—");

        const sessCount = data.sessions && data.sessions.active;
        const sessEl = document.getElementById("dash-sessions");
        sessEl.innerHTML = "";
        const sessDot = document.createElement("span");
        sessDot.className = "health-dot " + (typeof sessCount === "number" && sessCount > 0 ? "healthy" : "warning");
        sessEl.appendChild(sessDot);
        sessEl.appendChild(document.createTextNode(fmtInt(sessCount)));

        renderModelsTable(data.models || []);
        renderSkills(data.skills || []);
        renderMaintenanceControl(data.models || []);
    }

    // Maintenance-mode admin control (System Status panel). Populates
    // the fallback-model dropdown from the live model catalog and
    // reflects the current state; the toggle POSTs the new state. POST
    // is admin-only server-side; the card itself is .admin-only.
    let maintWired = false;
    async function renderMaintenanceControl(models) {
        const sel = document.getElementById("maint-model");
        const toggle = document.getElementById("maint-toggle");
        const stateEl = document.getElementById("maint-state");
        if (!sel || !toggle || !stateEl) return;

        // Populate options once (don't clobber a selection mid-edit).
        if (sel.options.length === 0 && models.length) {
            for (const m of models) {
                const opt = document.createElement("option");
                opt.value = m.id;
                opt.textContent = m.display_name || m.id;
                sel.appendChild(opt);
            }
        }

        let state;
        try {
            state = await apiJSON("/console/api/maintenance");
        } catch (e) { return; }
        if (!state || state.available === false) return;

        // Reflect the server's selection unless the admin is mid-choice.
        if (document.activeElement !== sel && state.model_id) sel.value = state.model_id;

        if (state.active) {
            const how = state.reason === "auto"
                ? "Auto: primary model offline" : "Enabled";
            stateEl.innerHTML = "";
            const dot = document.createElement("span");
            dot.className = "health-dot warning";
            stateEl.appendChild(dot);
            stateEl.appendChild(document.createTextNode(
                how + " — chat is using " + (state.model || state.model_id || "a fallback")));
        } else {
            const primary = state.primary_model || state.primary_id || "the primary model";
            stateEl.textContent = state.primary_offline
                ? ("Primary (" + primary + ") is offline — enable a fallback below.")
                : ("Inactive — chat is using " + primary + ".");
        }
        // The toggle reflects the MANUAL switch (auto can't be toggled off).
        toggle.textContent = state.enabled ? "Disable" : "Enable";
        toggle.dataset.enabled = state.enabled ? "1" : "";

        if (maintWired) return;
        maintWired = true;
        toggle.addEventListener("click", async () => {
            const err = document.getElementById("maint-error");
            if (err) { err.hidden = true; err.textContent = ""; }
            const enable = toggle.dataset.enabled !== "1";
            if (enable && !sel.value) {
                if (err) { err.hidden = false; err.textContent = "Select a fallback model first."; }
                return;
            }
            toggle.disabled = true;
            try {
                await apiJSON("/console/api/maintenance", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ enabled: enable, model_id: sel.value }),
                });
                toast(enable ? "Maintenance mode enabled" : "Maintenance mode disabled", "info");
                await loadSystemStatus();
                // Refresh the banner immediately for this admin too.
                try {
                    const s = await apiJSON("/console/api/auth/status");
                    applyMaintenanceBanner(s && s.maintenance);
                } catch (_) { /* banner will catch up on next probe */ }
            } catch (e) {
                if (err) { err.hidden = false; err.textContent = (e && e.message) || "Couldn't update maintenance mode."; }
            } finally {
                toggle.disabled = false;
            }
        });
    }

    function renderModelsTable(models) {
        const tbody = document.getElementById("dash-models-rows");
        tbody.innerHTML = "";
        if (!models.length) {
            const tr = document.createElement("tr");
            tr.className = "row-empty";
            const td = document.createElement("td");
            td.colSpan = 4;
            td.textContent = "NO MODELS REGISTERED";
            tr.appendChild(td);
            tbody.appendChild(tr);
            return;
        }
        for (const m of models) {
            const tr = document.createElement("tr");
            const cId = document.createElement("td");
            cId.className = "col-content";
            cId.textContent = m.id || "—";
            const cProv = document.createElement("td");
            cProv.className = "col-source";
            cProv.textContent = (m.provider || "").toUpperCase();
            const cStatus = document.createElement("td");
            const dot = document.createElement("span");
            dot.className = "health-dot " + (m.status === "online" ? "healthy" : m.status === "offline" ? "critical" : "warning");
            cStatus.appendChild(dot);
            const pill = document.createElement("span");
            pill.className = "status-pill " + statusClass(m.status);
            pill.textContent = (m.status || "UNKNOWN").toUpperCase();
            cStatus.appendChild(pill);
            const cEnd = document.createElement("td");
            cEnd.className = "col-created";
            cEnd.textContent = m.endpoint || "—";
            tr.append(cId, cProv, cStatus, cEnd);
            tbody.appendChild(tr);
        }
    }

    function renderSkills(skills) {
        const host = document.getElementById("dash-skills");
        host.innerHTML = "";
        if (!skills.length) {
            const span = document.createElement("span");
            span.className = "label";
            span.textContent = "NO SKILLS REGISTERED";
            host.appendChild(span);
            return;
        }
        for (const s of skills) {
            const chip = document.createElement("span");
            chip.className = "link-chip";
            chip.textContent = s.toUpperCase();
            host.appendChild(chip);
        }
    }

    function statusClass(s) {
        if (s === "online") return "approved";
        if (s === "offline") return "denied";
        return "pending";
    }

    function fmtInt(n) {
        return (typeof n === "number") ? String(n) : "—";
    }

    function fmtUptime(secs) {
        if (typeof secs !== "number" || secs < 0) return "—";
        const d = Math.floor(secs / 86400);
        const h = Math.floor((secs % 86400) / 3600);
        const m = Math.floor((secs % 3600) / 60);
        if (d > 0) return d + "D " + h + "H";
        if (h > 0) return h + "H " + m + "M";
        return m + "M";
    }

    // ── Users browser ───────────────────────────────────────────

    const usersState = {
        initialized: false,
        filter: "",
        currentID: null,
        currentUser: null,
    };

    async function initUsersBrowser() {
        if (usersState.initialized) return;
        // Probe the endpoint; 503 means user management isn't wired.
        try {
            await apiJSON("/console/api/users");
        } catch (e) {
            return;
        }
        usersState.initialized = true;
        document.getElementById("users-section").hidden = false;

        for (const tab of document.querySelectorAll(".user-tab")) {
            tab.addEventListener("click", () => {
                for (const t of document.querySelectorAll(".user-tab")) {
                    t.classList.toggle("is-active", t === tab);
                }
                usersState.filter = tab.dataset.status || "";
                loadUsers();
            });
        }

        wireInviteForm();

        document.getElementById("user-detail-close").addEventListener("click", closeUserDetail);
        document.getElementById("user-approve").addEventListener("click", () => setUserStatus("approved"));
        document.getElementById("user-deny").addEventListener("click", () => setUserStatus("denied"));
        document.getElementById("user-disable").addEventListener("click", () => setUserStatus("disabled"));
        document.getElementById("user-reinstate").addEventListener("click", () => setUserStatus("approved"));
        document.getElementById("user-link-form").addEventListener("submit", submitLink);

        // Cross-domain enrollment-link generator (admin-only flow,
        // gated server-side; the rendering is harmless for non-admins).
        const enrollGo = document.getElementById("user-enroll-go");
        if (enrollGo) enrollGo.addEventListener("click", adminGenerateEnrollmentLink);
        const enrollCopy = document.getElementById("user-enroll-copy");
        if (enrollCopy) {
            enrollCopy.addEventListener("click", async () => {
                const url = document.getElementById("user-enroll-url").value;
                if (!url) return;
                try { await navigator.clipboard.writeText(url); } catch (_) { /* clipboard blocked */ }
            });
        }
        const enrollDismiss = document.getElementById("user-enroll-dismiss");
        if (enrollDismiss) {
            enrollDismiss.addEventListener("click", () => {
                document.getElementById("user-enroll-result").hidden = true;
            });
        }
        document.getElementById("user-view-memories").addEventListener("click", (e) => {
            e.preventDefault();
            if (!usersState.currentUser) return;
            const targetUser = usersState.currentUser.id || "";
            closeUserDetail();
            // Admin cross-user entry point: the memory panel opens
            // in view-as mode with a visible banner. The backend
            // only honors the user param for admin sessions.
            switchPanel("memory");
            const mp = window.familiarPanels && window.familiarPanels.memory;
            if (mp && mp.viewAsUser) mp.viewAsUser(targetUser);
        });
        document.getElementById("user-view-graph").addEventListener("click", (e) => {
            e.preventDefault();
            if (!usersState.currentUser) return;
            const targetUser = usersState.currentUser.id || "";
            closeUserDetail();
            // Same admin cross-user contract as View memories.
            switchPanel("memory-graph");
            const gp = window.familiarPanels && window.familiarPanels["memory-graph"];
            if (gp && gp.viewAs) gp.viewAs(targetUser);
        });
        document.getElementById("user-view-dashboard").addEventListener("click", (e) => {
            e.preventDefault();
            if (!usersState.currentUser) return;
            const targetUser = usersState.currentUser.id || "";
            closeUserDetail();
            switchPanel("dashboard");
            dashViewAs(targetUser);
        });
        document.getElementById("user-edit-profile").addEventListener("click", (e) => {
            e.preventDefault();
            if (!usersState.currentUser) return;
            const targetUser = usersState.currentUser.id || "";
            closeUserDetail();
            switchPanel("profile");
            const pp = window.familiarPanels && window.familiarPanels.profile;
            if (pp && pp.viewAs) pp.viewAs(targetUser);
        });

        loadUsers();
    }

    // ── Invite user (admin-only) ───────────────────────────
    //
    // POST /console/api/users creates an admin-invited row by email
    // and (optionally) mints a one-shot enrollment-token link the
    // admin can share with the new user. The RP dropdown reuses the
    // available_rps list from /console/api/auth/passkeys so any
    // configured RP shows up; selecting one toggles
    // generate_enrollment_link=true on the POST body.
    function wireInviteForm() {
        const btn = document.getElementById("users-invite-btn");
        const form = document.getElementById("users-invite-form");
        if (!btn || !form) return;
        btn.addEventListener("click", () => {
            form.hidden = !form.hidden;
            if (!form.hidden) {
                document.getElementById("users-invite-email").focus();
                populateInviteRPsOnce();
            }
        });
        document.getElementById("users-invite-cancel").addEventListener("click", () => {
            form.hidden = true;
            resetInviteForm();
        });
        document.getElementById("users-invite-go").addEventListener("click", submitInvite);
        document.getElementById("users-invite-email").addEventListener("keydown", (e) => {
            if (e.key === "Enter") { e.preventDefault(); submitInvite(); }
        });
        document.getElementById("users-invite-name").addEventListener("keydown", (e) => {
            if (e.key === "Enter") { e.preventDefault(); submitInvite(); }
        });
        document.getElementById("users-invite-dismiss").addEventListener("click", () => {
            form.hidden = true;
            resetInviteForm();
        });
        document.getElementById("users-invite-copy").addEventListener("click", async () => {
            const url = document.getElementById("users-invite-url").value;
            if (!url) return;
            try { await navigator.clipboard.writeText(url); } catch (_) { /* clipboard blocked */ }
        });
    }

    let inviteRPsLoaded = false;
    async function populateInviteRPsOnce() {
        if (inviteRPsLoaded) return;
        const sel = document.getElementById("users-invite-rp");
        if (!sel) return;
        try {
            const data = await apiJSON("/console/api/auth/passkeys");
            const rps = new Map();
            for (const p of (data.passkeys || [])) {
                if (p.rp_id && !rps.has(p.rp_id)) rps.set(p.rp_id, p.rp_id);
            }
            for (const rp of (data.available_rps || [])) {
                if (rp.rp_id) rps.set(rp.rp_id, rp.display_name || rp.rp_id);
            }
            for (const [id, label] of rps.entries()) {
                const opt = document.createElement("option");
                opt.value = id;
                opt.textContent = "Enrollment link · " + label;
                sel.appendChild(opt);
            }
            inviteRPsLoaded = true;
        } catch (e) {
            console.warn("invite: load RPs failed", e);
        }
    }

    function resetInviteForm() {
        document.getElementById("users-invite-email").value = "";
        document.getElementById("users-invite-name").value = "";
        document.getElementById("users-invite-role").value = "user";
        document.getElementById("users-invite-rp").value = "";
        document.getElementById("users-invite-result").hidden = true;
        const err = document.getElementById("users-invite-error");
        err.hidden = true;
        err.textContent = "";
    }

    async function submitInvite() {
        const email = document.getElementById("users-invite-email").value.trim();
        const name = document.getElementById("users-invite-name").value.trim();
        const role = document.getElementById("users-invite-role").value || "user";
        const rpid = document.getElementById("users-invite-rp").value;
        const err = document.getElementById("users-invite-error");
        err.hidden = true;
        err.textContent = "";
        if (!email) {
            err.textContent = "Email is required.";
            err.hidden = false;
            return;
        }
        const body = { email, display_name: name, role };
        if (rpid) {
            body.generate_enrollment_link = true;
            body.target_rp_id = rpid;
        }
        try {
            const resp = await apiJSON("/console/api/users", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify(body),
            });
            // Surface the result inline. If we asked for an enrollment
            // link, render it for copy/paste — otherwise just toast.
            if (resp && resp.enrollment_url) {
                const result = document.getElementById("users-invite-result");
                const msg = document.getElementById("users-invite-msg");
                const urlInput = document.getElementById("users-invite-url");
                msg.textContent = "Invited " + (resp.user && resp.user.display_name ? resp.user.display_name : email) +
                    " — share this link to register their passkey:";
                urlInput.value = resp.enrollment_url;
                result.hidden = false;
            } else {
                if (window.familiarAppHelpers && window.familiarAppHelpers.toast) {
                    window.familiarAppHelpers.toast("Invited " + email, "ok");
                }
                document.getElementById("users-invite-form").hidden = true;
                resetInviteForm();
            }
            loadUsers();
        } catch (e2) {
            err.textContent = (e2 && e2.message) ? e2.message : String(e2);
            err.hidden = false;
        }
    }

    async function loadUsers() {
        setError("users-error", null);
        const qs = usersState.filter ? ("?status=" + encodeURIComponent(usersState.filter)) : "";
        try {
            const data = await apiJSON("/console/api/users" + qs);
            renderUsersTable(data.items || []);
            // Always fetch the unfiltered pending count for the header.
            if (usersState.filter !== "pending") {
                const all = await apiJSON("/console/api/users?status=pending");
                document.getElementById("users-pending-count").textContent = String((all.items || []).length);
            } else {
                document.getElementById("users-pending-count").textContent = String((data.items || []).length);
            }
        } catch (e) {
            setError("users-error", e);
        }
    }

    function renderUsersTable(items) {
        const tbody = document.getElementById("users-rows");
        tbody.innerHTML = "";
        if (!items || items.length === 0) {
            const tr = document.createElement("tr");
            tr.className = "row-empty";
            const td = document.createElement("td");
            td.colSpan = 6;
            td.textContent = "NO USERS";
            tr.appendChild(td);
            tbody.appendChild(tr);
            return;
        }
        for (const u of items) {
            const tr = document.createElement("tr");
            tr.dataset.id = u.id;

            const cName = document.createElement("td");
            cName.className = "col-name";
            cName.textContent = u.display_name || u.id;

            const cId = document.createElement("td");
            cId.className = "col-id";
            cId.textContent = u.id;

            const cStatus = document.createElement("td");
            cStatus.className = "col-status";
            const pill = document.createElement("span");
            pill.className = "status-pill " + (u.status || "");
            pill.textContent = u.status || "—";
            cStatus.appendChild(pill);
            // Flag invited-but-not-yet-enrolled accounts so the admin can
            // spot who still needs an enrollment link at a glance.
            if (!(u.passkey_count > 0)) {
                const np = document.createElement("span");
                np.className = "status-pill no-passkey";
                np.textContent = "no passkey";
                np.title = "This user hasn't registered a passkey yet — open them to send an enrollment link.";
                cStatus.appendChild(np);
            }

            const cLinks = document.createElement("td");
            cLinks.className = "col-links";
            for (const l of u.identities || []) {
                const chip = document.createElement("span");
                chip.className = "link-chip";
                const p = document.createElement("span");
                p.className = "p";
                p.textContent = (l.platform || "").toUpperCase();
                const pid = document.createElement("span");
                pid.textContent = truncate(l.platform_id || "", 16);
                chip.append(p, pid);
                cLinks.appendChild(chip);
            }
            if (!u.identities || u.identities.length === 0) {
                cLinks.textContent = "—";
                cLinks.style.color = "var(--ash)";
            }

            const cCreated = document.createElement("td");
            cCreated.className = "col-created";
            cCreated.textContent = fmtDate(u.created_at);

            const cAct = document.createElement("td");
            cAct.className = "col-actions";
            if (u.status === "pending") {
                cAct.textContent = "REVIEW →";
                cAct.style.color = "var(--gold)";
            } else {
                cAct.textContent = "VIEW →";
                cAct.style.color = "var(--ash)";
            }

            tr.append(cName, cId, cStatus, cLinks, cCreated, cAct);
            tr.addEventListener("click", () => openUserDetail(u.id));
            tbody.appendChild(tr);
        }
    }

    async function openUserDetail(id) {
        setError("user-detail-error", null);
        usersState.currentID = id;
        try {
            const u = await apiJSON("/console/api/users/" + encodeURIComponent(id));
            usersState.currentUser = u;
            renderUserDetail(u);
            document.getElementById("user-detail").hidden = false;
            // Reset the enrollment-link surface from a prior open.
            const result = document.getElementById("user-enroll-result");
            if (result) result.hidden = true;
            const sel = document.getElementById("user-enroll-rp");
            if (sel) sel.value = "";
            populateEnrollRPsOnce();
        } catch (e) {
            setError("user-detail-error", e);
        }
    }

    function renderUserDetail(u) {
        document.getElementById("user-detail-name").textContent = u.display_name || u.id;

        const meta = document.getElementById("user-detail-meta");
        meta.innerHTML = "";
        const pairs = [
            ["ID", u.id],
            ["STATUS", (u.status || "—").toUpperCase()],
            ["PASSKEY", u.passkey_count > 0 ? "Enrolled (" + u.passkey_count + ")" : "Not enrolled"],
            ["CREATED", fmtDate(u.created_at)],
            ["APPROVED", u.approved_at ? fmtDate(u.approved_at) : "—"],
            ["APPROVED BY", u.approved_by || "—"],
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

        // Action button visibility by status.
        const isPending  = u.status === "pending";
        const isApproved = u.status === "approved";
        const isDenied   = u.status === "denied";
        const isDisabled = u.status === "disabled";
        document.getElementById("user-approve").hidden  = !(isPending || isDisabled);
        document.getElementById("user-deny").hidden     = !isPending;
        document.getElementById("user-disable").hidden  = !isApproved;
        document.getElementById("user-reinstate").hidden = !isDenied;

        // Linked identities list with per-row unlink buttons.
        const list = document.getElementById("user-detail-links");
        list.innerHTML = "";
        const links = u.identities || [];
        if (links.length === 0) {
            const li = document.createElement("li");
            li.textContent = "NO LINKED IDENTITIES";
            li.style.color = "var(--ash)";
            list.appendChild(li);
        } else {
            for (const l of links) {
                const li = document.createElement("li");
                const p = document.createElement("span");
                p.className = "p";
                p.textContent = (l.platform || "").toUpperCase();
                const pid = document.createElement("span");
                pid.className = "pid";
                pid.textContent = l.platform_id;
                const btn = document.createElement("button");
                btn.type = "button";
                btn.textContent = "UNLINK";
                btn.addEventListener("click", (e) => {
                    e.stopPropagation();
                    unlinkIdentity(l.platform, l.platform_id);
                });
                li.append(p, pid, btn);
                list.appendChild(li);
            }
        }
    }

    function closeUserDetail() {
        document.getElementById("user-detail").hidden = true;
        usersState.currentID = null;
        usersState.currentUser = null;
        setError("user-detail-error", null);
        const result = document.getElementById("user-enroll-result");
        if (result) result.hidden = true;
    }

    // CROSS-DOMAIN-ENROLLMENT.md: the user-detail flyout shows an
    // admin-only "Generate enrollment link" affordance. The RP
    // dropdown is populated lazily from /console/api/auth/passkeys —
    // the available_rps array is the admin's own list, but every
    // configured RP appears there regardless of who owns credentials
    // for it, so it works as a domain catalog.
    // Cached promise so concurrent callers (flyout-open + an immediate
    // "Copy enrollment link" click) share ONE fetch and can await it —
    // generating a link before the dropdown populated was a real race
    // (empty URL). Reset to null on failure so a later open retries.
    let enrollRPsPromise = null;
    function populateEnrollRPsOnce() {
        if (!enrollRPsPromise) enrollRPsPromise = loadEnrollRPs();
        return enrollRPsPromise;
    }
    async function loadEnrollRPs() {
        const sel = document.getElementById("user-enroll-rp");
        if (!sel) return;
        try {
            const data = await apiJSON("/console/api/auth/passkeys");
            const rps = new Map();
            for (const p of (data.passkeys || [])) {
                if (p.rp_id && !rps.has(p.rp_id)) rps.set(p.rp_id, p.rp_id);
            }
            for (const rp of (data.available_rps || [])) {
                if (rp.rp_id) rps.set(rp.rp_id, rp.display_name || rp.rp_id);
            }
            for (const [id, label] of rps.entries()) {
                const opt = document.createElement("option");
                opt.value = id;
                opt.textContent = label + " · " + id;
                sel.appendChild(opt);
            }
            // One-click case: with a single configured domain, pre-select
            // it so the admin just clicks the link button (no picking).
            if (rps.size === 1) {
                sel.value = rps.keys().next().value;
            }
        } catch (e) {
            console.warn("enroll: load RPs for admin user-detail failed", e);
            enrollRPsPromise = null; // allow a retry on the next open
        }
    }

    async function adminGenerateEnrollmentLink() {
        const sel = document.getElementById("user-enroll-rp");
        const result = document.getElementById("user-enroll-result");
        const msg = document.getElementById("user-enroll-msg");
        const urlInput = document.getElementById("user-enroll-url");
        if (!sel || !result || !msg || !urlInput) return;
        if (!usersState.currentID) {
            msg.textContent = "No user selected.";
            result.hidden = false;
            return;
        }
        // Make sure the RP dropdown is populated before we read it — a
        // click right after opening the flyout would otherwise race the
        // (async) RP load and find no domain.
        await populateEnrollRPsOnce();
        // One-click: fall back to the sole configured domain when the
        // admin hasn't explicitly picked one (the common single-RP case).
        let rpid = sel.value;
        if (!rpid) {
            const opts = [...sel.options].filter((o) => o.value);
            if (opts.length === 1) rpid = opts[0].value;
        }
        if (!rpid) {
            msg.textContent = "Pick a target domain first.";
            result.hidden = false;
            return;
        }
        msg.textContent = "Generating link…";
        urlInput.value = "";
        result.hidden = false;
        try {
            const resp = await apiJSON("/console/api/auth/enrollment-token", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({
                    canonical_id: usersState.currentID,
                    target_rp_id: rpid,
                }),
            });
            urlInput.value = resp.url || "";
            // Best-effort auto-copy so the admin can paste straight into
            // Slack/email — the whole point of "one-click re-issue".
            let copied = false;
            try {
                if (resp.url && navigator.clipboard) {
                    await navigator.clipboard.writeText(resp.url);
                    copied = true;
                }
            } catch (_) { /* insecure context / denied — link is still shown */ }
            const hoursLeft = Math.max(0, Math.round((new Date(resp.expires_at) - Date.now()) / 3600000));
            const validFor = hoursLeft >= 1 ? hoursLeft + "h" : "<1h";
            msg.textContent = (copied ? "Copied — " : "") +
                "share with " + usersState.currentID + " (valid " + validFor + "):";
        } catch (e) {
            msg.textContent = "Couldn't generate link: " + (e.message || e);
        }
    }

    async function setUserStatus(status) {
        if (!usersState.currentID) return;
        if (status === "denied" && !confirm("Deny this access request? This tombstones the user — repeat DMs won't re-open it.")) return;
        if (status === "disabled" && !confirm("Disable this user? They'll lose access on all platforms immediately.")) return;
        setError("user-detail-error", null);
        try {
            await apiJSON("/console/api/users/" + encodeURIComponent(usersState.currentID) + "/status", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ status }),
            });
            toast("USER " + status.toUpperCase(), "success");
            await openUserDetail(usersState.currentID);
            loadUsers();
        } catch (e) {
            setError("user-detail-error", e);
            toast("STATUS UPDATE FAILED", "error");
        }
    }

    async function submitLink(e) {
        e.preventDefault();
        if (!usersState.currentID) return;
        const platform = document.getElementById("link-platform").value;
        const platformID = document.getElementById("link-platform-id").value.trim();
        const displayName = document.getElementById("link-display-name").value.trim();
        if (!platformID) return;
        setError("user-detail-error", null);
        try {
            await apiJSON("/console/api/users/" + encodeURIComponent(usersState.currentID) + "/identities", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({
                    platform,
                    platform_id: platformID,
                    display_name: displayName,
                }),
            });
            document.getElementById("link-platform-id").value = "";
            document.getElementById("link-display-name").value = "";
            toast("IDENTITY LINKED", "success");
            await openUserDetail(usersState.currentID);
            loadUsers();
        } catch (err) {
            setError("user-detail-error", err);
            toast("LINK FAILED", "error");
        }
    }

    async function unlinkIdentity(platform, platformID) {
        if (!usersState.currentID) return;
        if (!confirm("Unlink " + platform + ":" + platformID + "?")) return;
        setError("user-detail-error", null);
        try {
            await apiJSON(
                "/console/api/users/" + encodeURIComponent(usersState.currentID) +
                "/identities/" + encodeURIComponent(platform) +
                "/" + encodeURIComponent(platformID),
                { method: "DELETE" }
            );
            await openUserDetail(usersState.currentID);
            loadUsers();
        } catch (e) {
            setError("user-detail-error", e);
        }
    }


    // ── Boot ────────────────────────────────────────────────────

    async function boot() {
        show("loading");
        try {
            const status = await apiJSON("/console/api/auth/status");
            if (status && status.authenticated) {
                renderDashboard(status);
                return;
            }
        } catch (e) {
            // 401 is the expected unauthenticated branch — fall through.
        }

        // Not authenticated. The login + setup views live inline in
        // this same shell — no cross-page navigation, so there's
        // nothing for /login ↔ / to ping-pong against.
        let regStatus;
        try {
            regStatus = await apiJSON("/console/api/auth/register/status");
        } catch (e) {
            setError("login-error", e);
            show("login");
            return;
        }
        if (regStatus && regStatus.credentials_registered === 0) {
            show("setup");
        } else {
            show("login");
        }
    }

    // ── Event wiring ────────────────────────────────────────────

    function wire() {
        document.getElementById("btn-setup-register").addEventListener("click", async () => {
            setError("setup-error", null);
            const email = (document.getElementById("setup-email").value || "").trim();
            const displayName = (document.getElementById("setup-display-name").value || "").trim();
            if (!email) {
                setError("setup-error", new Error("email required"));
                return;
            }
            try {
                await doRegister("setup-error", { email, displayName });
                // After first-time registration, immediately run a login so
                // the user gets a session cookie without a second click.
                await doLogin();
                boot();
            } catch (e) {
                setError("setup-error", e);
            }
        });

        document.getElementById("btn-login").addEventListener("click", async () => {
            setError("login-error", null);
            try {
                await doLogin();
                boot();
            } catch (e) {
                setError("login-error", e);
            }
        });

        // SHARD-AUTH-SPEC Phase 1 — kiosk / cross-device login.
        // Shares the underlying WebAuthn ceremony with btn-login;
        // the visual split is purely so users recognize which kind
        // of credential they're presenting. When the shard's passkey
        // lives on a different device than the kiosk that's logging
        // in, the browser's hybrid transport surfaces a QR code
        // automatically — no extra plumbing needed here.
        const btnShardLogin = document.getElementById("btn-shard-login");
        if (btnShardLogin) {
            btnShardLogin.addEventListener("click", async () => {
                setError("login-error", null);
                try {
                    await doLogin();
                    boot();
                } catch (e) {
                    setError("login-error", e);
                }
            });
        }

        // Suppress accidental form submits on the login form (e.g.
        // pressing Enter while a button is focused). Users always
        // authenticate via one of the two buttons above.
        const lpForm = document.getElementById("lp-form");
        if (lpForm) {
            lpForm.addEventListener("submit", (ev) => ev.preventDefault());
        }

        // (The old login-page "register a key" branch was removed — its
        // DOM (#btn-show-register / #view-login-register) never existed
        // in index.html, so the handlers were dead. The live way to bind
        // a passkey to an admin/created account is the enrollment link
        // (enroll.html); onboarding is being reworked.)

        // Sign out moved to the User panel per DESIGN.md.
        // The legacy nav-logout button is gone; null-guard the
        // listener so we don't NPE on boot. Phase 3c re-wires the
        // sign-out action inside the User panel.
        const legacyLogout = document.getElementById("nav-logout");
        if (legacyLogout) {
            legacyLogout.addEventListener("click", async () => {
                try {
                    await apiJSON("/console/api/auth/logout", { method: "POST" });
                } catch (e) { /* ignore */ }
                location.reload();
            });
        }

        document.getElementById("btn-add-key").addEventListener("click", async () => {
            setError("add-key-error", null);
            // Post-Phase-4 registerBegin requires the email that
            // identifies the target user. For an already-logged-in
            // admin adding another key to their own row, that's the
            // email in the session we got at boot.
            const sess = window.FAMILIAR_SESSION || {};
            if (!sess.email) {
                setError("add-key-error", new Error(
                    "cannot add a key without a known email on your account — ask an admin to set your email first"));
                return;
            }
            try {
                const result = await doRegister("add-key-error", { email: sess.email });
                setError("add-key-error", null);
                const btn = document.getElementById("btn-add-key");
                btn.textContent = "Registered " + (result.credential_id || "").slice(0, 8);
                btn.disabled = true;
            } catch (e) {
                setError("add-key-error", e);
            }
        });

        // Keyboard shortcuts
        document.addEventListener("keydown", (e) => {
            if (e.target.tagName === "INPUT" || e.target.tagName === "TEXTAREA" || e.target.tagName === "SELECT") return;
            if (e.key === "Escape") {
                const memDetail = document.getElementById("memory-detail");
                const entDetail = document.getElementById("entity-detail");
                const userDetail = document.getElementById("user-detail");
                if ((memDetail && !memDetail.hidden) || (entDetail && !entDetail.hidden)) {
                    // memory panel owns its own editing state — let
                    // its module decide cancel-edit vs close, and
                    // which of its two detail panes is on top.
                    const mp = window.familiarPanels && window.familiarPanels.memory;
                    if (mp && mp.closeOrCancelDetail) mp.closeOrCancelDetail();
                    e.preventDefault();
                } else if (userDetail && !userDetail.hidden) {
                    closeUserDetail();
                    e.preventDefault();
                }
            }
            if (e.key === "/" && currentPanel === "memory") {
                const searchField = document.getElementById("m-q");
                if (searchField) { e.preventDefault(); searchField.focus(); }
            }
        });
    }

    // Hot memory browser removed when the RAM tier was retired: the RAM
    // tier no longer exists (writes go straight to pgvector), so the
    // separate /console/api/memory/hot panel was retired here.



    if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", () => { wire(); boot(); });
    } else {
        wire();
        boot();
    }
})();
