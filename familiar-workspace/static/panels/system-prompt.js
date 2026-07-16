// System-prompt panel — admin-only editor for the prompt base
// layer plus the "users may view the system prompt" toggle.
//
// The base layer edited here is the bottom of the tiered-prompt
// stack: the classifier-driven tier overlays and the tool-policy
// layer still append on top automatically. An empty base clears
// the override and the gateway falls back to the file-loaded
// base.md.
//
// Backed by GET/PUT /console/api/system-prompt. The PUT is
// admin-gated server-side; this panel is also only reachable from
// the .admin-only subnav group, but the server check is the real
// boundary. Self-contained IIFE; registered on
// window.familiarPanels as "system-prompt" so app.js's panel
// switcher init()s it lazily on first navigation.

(function () {
    "use strict";

    const helpers = window.familiarAppHelpers;
    if (!helpers) {
        console.error("system-prompt: app helpers not loaded; panel disabled");
        return;
    }
    const { apiJSON, setError } = helpers;

    const state = {
        initialized: false,
        lastSaved: "",   // last-loaded base text, so Revert works
    };

    async function init() {
        if (state.initialized) return;
        state.initialized = true;
        document.getElementById("system-prompt-save").addEventListener("click", save);
        document.getElementById("system-prompt-reset").addEventListener("click", () => {
            document.getElementById("system-prompt-editor").value = state.lastSaved;
            setError("system-prompt-error", null);
        });
        await load();
    }

    async function load() {
        setError("system-prompt-error", null);
        document.getElementById("system-prompt-saved").hidden = true;
        try {
            const data = await apiJSON("/console/api/system-prompt");
            apply(data);
        } catch (e) {
            setError("system-prompt-error", e);
        }
    }

    // apply paints a GET/PUT response into the editor surfaces.
    function apply(data) {
        const base = data.base || "";
        document.getElementById("system-prompt-editor").value = base;
        state.lastSaved = base;
        document.getElementById("system-prompt-user-visible").checked = !!data.user_visible;
        renderOrigin(!!data.has_override);
    }

    // renderOrigin explains whether the gateway is currently
    // serving a saved DB override or the file-loaded default, so
    // the admin can tell that an empty box means "use base.md".
    function renderOrigin(hasOverride) {
        const el = document.getElementById("system-prompt-origin");
        if (!el) return;
        el.textContent = hasOverride
            ? "Currently serving a saved override. Clear the box and save to fall back to the file default (base.md)."
            : "Currently serving the file-loaded default (base.md). Saving text here installs an override.";
    }

    async function save() {
        setError("system-prompt-error", null);
        const base = document.getElementById("system-prompt-editor").value;
        const userVisible = document.getElementById("system-prompt-user-visible").checked;
        const btn = document.getElementById("system-prompt-save");
        btn.disabled = true;
        try {
            const data = await apiJSON("/console/api/system-prompt", {
                method: "PUT",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ base: base, user_visible: userVisible }),
            });
            apply(data);
            const banner = document.getElementById("system-prompt-saved");
            banner.hidden = false;
            setTimeout(() => { banner.hidden = true; }, 2500);
        } catch (e) {
            setError("system-prompt-error", e);
        } finally {
            btn.disabled = false;
        }
    }

    window.familiarPanels = window.familiarPanels || {};
    window.familiarPanels["system-prompt"] = { init: init };
})();
