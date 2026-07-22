// Chat surface (FAMILIAR-WORKSPACE-SPEC Phase 1c).
//
// Two-pane layout inside a workspace tab:
//
//   ┌─────────────────────────────┬────────────────────────────────┐
//   │ Conversations list          │ Message stream                 │
//   │ + New                       │                                │
//   │ • current conversation      │  user / assistant turns        │
//   │ • another                   │  (markdown rendered, code with │
//   │ • archived... [hidden]      │   syntax highlighting)         │
//   │                             │                                │
//   │                             │ ┌────────────────────────────┐ │
//   │                             │ │ message input              │ │
//   │                             │ │                       Send │ │
//   │                             │ └────────────────────────────┘ │
//   └─────────────────────────────┴────────────────────────────────┘
//
// The chat endpoint is /api/chat (native protocol — CHAT-REARCH
// §"Phase 0"). Body is just {message: string}; gateway holds the
// conversation history and the classifier picks the model. SSE
// stream uses named events (token / reasoning / status / done /
// error) instead of the old OpenAI delta envelope.
//
// Conversation persistence lives at /console/api/conversations/* —
// added in Phase 1a. Each user prompt is:
//
//   1. Persisted via POST /console/api/conversations/{id}/messages
//      so the conversation has the prompt before the LLM call —
//      makes resume-after-page-refresh trivial.
//   2. Sent to /api/chat with Accept: text/event-stream.
//   3. Tokens streamed into the assistant's pending bubble.
//   4. On stream end, the assembled assistant message is persisted
//      via POST /console/api/conversations/{id}/messages.
//
// Tool effects (notes changed, etc.) ride the reasoning channel as
// __TOOL_EFFECT__ markers; the UI listens and dispatches custom
// events. A richer tool-call card UI lands as a polish pass.
//
// CDN deps loaded on first chat render: marked + DOMPurify +
// highlight.js. Failure to load any one is degraded-but-functional —
// markdown falls back to <pre> rendering. The CDN URLs are pinned
// to specific versions.
//
// Multiple chat tabs are independent — each tab carries its own
// conversation_id and message stream. State is held in a per-tab
// closure so two side-by-side chats don't cross-contaminate.

(function () {
    "use strict";

    const helpers = window.familiarAppHelpers;
    if (!helpers) {
        console.error("chat: app helpers not loaded; chat surface disabled");
        return;
    }
    const { apiJSON } = helpers;

    // Surface an action failure through the toast UI instead of a
    // blocking native alert (EXTERNAL-READINESS-REVIEW.md P2).
    function notifyErr(msg) {
        if (helpers && helpers.toast) helpers.toast(msg, "error");
        else window.alert(msg);
    }

    // ── CDN deps ──────────────────────────────────────────────

    // Pinned versions — bumped manually after a release smoke test.
    // Failure here downgrades markdown to plain <pre>; the chat
    // still works.
    const CDN = {
        marked:   "/vendor/marked/marked.min.js",
        dompurify:"/vendor/dompurify/purify.min.js",
        hljsJS:   "/vendor/highlight/core.min.js",
        hljsCSS:  "/vendor/highlight/atom-one-dark.min.css",
    };

    // ensureMarkdownDeps loads marked + DOMPurify + highlight.js
    // exactly once. Returns a promise that resolves with a renderer
    // function `(md) => safeHTML`. On any load failure the renderer
    // falls back to <pre>-wrapped escaped text — the chat does not
    // hard-fail because a CDN went down.
    let depsPromise = null;
    function ensureMarkdownDeps() {
        if (depsPromise) return depsPromise;
        depsPromise = (async () => {
            try {
                const css = document.createElement("link");
                css.rel = "stylesheet";
                css.href = CDN.hljsCSS;
                document.head.appendChild(css);

                await Promise.all([loadScript(CDN.marked), loadScript(CDN.dompurify)]);
                await loadScript(CDN.hljsJS);
                // Register a few common languages so codeblocks
                // syntax-highlight without bloating the bundle.
                // hljs.core ships no languages; each is loaded
                // separately. We'll lazy-load on first use too.
                if (window.marked && window.marked.use && window.hljs) {
                    // marked extension hook for code blocks — runs
                    // hljs against any language tag it recognizes,
                    // falls back to plain text otherwise.
                    window.marked.use({
                        renderer: {
                            code(code, infostring) {
                                const lang = (infostring || "").match(/\S*/)[0];
                                // A research-card fence renders as the inline
                                // completed-research card (research-blocks.js);
                                // falls through to a code block if unloaded.
                                if (lang === "research-card" && window.familiarResearchCard) {
                                    return window.familiarResearchCard.html(code);
                                }
                                if (lang && window.hljs.getLanguage && window.hljs.getLanguage(lang)) {
                                    try {
                                        return '<pre><code class="hljs language-' + lang + '">'
                                            + window.hljs.highlight(code, { language: lang }).value
                                            + '</code></pre>';
                                    } catch (e) { /* fall through */ }
                                }
                                return '<pre><code class="hljs">' + escapeHTML(code) + '</code></pre>';
                            },
                        },
                    });
                }
                return renderMarkdownReal;
            } catch (e) {
                console.warn("chat: markdown deps failed, falling back to plain text", e);
                return renderMarkdownFallback;
            }
        })();
        return depsPromise;
    }

    function loadScript(src) {
        return new Promise((resolve, reject) => {
            const s = document.createElement("script");
            s.src = src;
            s.async = true;
            s.onload = () => resolve();
            s.onerror = () => reject(new Error("script load failed: " + src));
            document.head.appendChild(s);
        });
    }

    function renderMarkdownReal(md) {
        const html = window.marked.parse(md || "");
        return window.DOMPurify.sanitize(html, {
            // Allow class on code blocks for hljs styling.
            ADD_ATTR: ["class"],
        });
    }

    function renderMarkdownFallback(md) {
        return '<pre class="chat-md-fallback">' + escapeHTML(md || "") + '</pre>';
    }

    function relTime(iso) {
        if (!iso) return "";
        const t = new Date(iso).getTime();
        if (isNaN(t)) return "";
        const d = (Date.now() - t) / 1000;
        if (d < 60) return "now";
        if (d < 3600) return Math.floor(d / 60) + "m";
        if (d < 86400) return Math.floor(d / 3600) + "h";
        if (d < 86400 * 7) return Math.floor(d / 86400) + "d";
        const dt = new Date(t);
        const mon = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
        return mon[dt.getMonth()] + " " + String(dt.getDate()).padStart(2, "0");
    }
    function chatGlyphSVG() {
        return (
            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" width="32" height="32">' +
                '<path d="M4 5 h16 a1.5 1.5 0 0 1 1.5 1.5 v9 a1.5 1.5 0 0 1 -1.5 1.5 H10 l-4 4 v-4 H4 a1.5 1.5 0 0 1 -1.5 -1.5 v-9 A1.5 1.5 0 0 1 4 5 Z"/>' +
            '</svg>'
        );
    }
    function escapeHTML(s) {
        return String(s)
            .replace(/&/g, "&amp;").replace(/</g, "&lt;")
            .replace(/>/g, "&gt;").replace(/"/g, "&quot;")
            .replace(/'/g, "&#39;");
    }

    // ── Surface renderer ──────────────────────────────────────

    // render is the function the workspace calls when a tab whose
    // surface=chat needs to display. host is the panel's
    // .ws-panel-content div. tab carries id + surface + state.
    //
    // Each tab keeps its conversation id + cached message list in
    // tab.state. We do NOT re-render the entire shell on every
    // workspace re-render — DOM diffing for a streaming chat is
    // expensive. Instead we cache the chat shell in a Map keyed
    // by tab.id, and on subsequent renderTabContent calls we
    // reattach the same shell into the host. This preserves
    // scroll position + pending stream state across panel
    // re-renders (resize, layout switch, sidebar refresh).
    const shells = new Map(); // tab.id -> { root, model }

    // updateStatusBar pushes the chat-tab's context-sensitive
    // status text (DESIGN.md: "model · ctx N/N"). Called
    // whenever a chat tab renders or its active conversation
    // changes. Best-effort — if window.familiarStatusBar isn't
    // wired (e.g. older app.js), the call no-ops.
    function updateStatusBar(model, msgCount) {
        const sb = window.familiarStatusBar;
        if (!sb || !sb.setContext) return;
        const m = model || "familiar";
        const n = msgCount || 0;
        sb.setContext(m + " · " + n + " message" + (n === 1 ? "" : "s"));
    }

    function render(host, tab) {
        // Push status bar context immediately on render so the
        // model + count appears the moment a chat tab is focused.
        // The model gets refined inside the model object once a
        // conversation loads.
        updateStatusBar("familiar", 0);
        const cached = shells.get(tab.id);
        if (cached) {
            host.innerHTML = "";
            host.appendChild(cached.root);
            // Detach + re-attach during workspace re-render resets
            // messagesEl.scrollTop. Restore the user's last position
            // (or anchor at the bottom on a fresh shell) so they
            // don't lose their place when another tab is touched.
            if (cached.model.restoreScroll) cached.model.restoreScroll();
            // Refresh the conversation list — cheap enough that
            // doing it on every render keeps it current as new
            // conversations are created in other tabs.
            cached.model.refreshConversations();
            return;
        }
        const model = newChatModel(tab);
        host.innerHTML = "";
        host.appendChild(model.root);
        shells.set(tab.id, { root: model.root, model });
        model.init();
    }

    // newChatModel constructs the per-tab chat shell + state. Returns
    // { root, init, refreshConversations }.
    function newChatModel(tab) {
        // Per-tab state. tab.state may carry a conversation_id from
        // a previous render — restore if present.
        const persisted = (tab.state && tab.state.conversationId) || null;
        const localState = {
            conversationId: persisted,
            conversations: [],
            messages: [],
            streaming: false,
            // Live deep-research (RESEARCH-SKILL-SPEC §6.7).
            // researchLastRun: the most recent non-null poll payload,
            //   so the card click can reopen the evidence page and the
            //   done-transition can find the delivered note slugs.
            // researchOpenedRunId: the run id we've already auto-opened
            //   the evidence pane for — the once-per-run guard.
            // researchEvTabId: the wiki tab we opened in the right pane.
            researchLastRun: null,
            researchOpenedRunId: null,
            researchEvTabId: null,
        };

        // ── Shell DOM ─────────────────────────────────────────
        const root = document.createElement("div");
        root.className = "chat-shell";

        const left = document.createElement("aside");
        left.className = "chat-left";
        const leftHead = document.createElement("div");
        leftHead.className = "chat-left-head";
        const leftTitle = document.createElement("div");
        leftTitle.className = "label";
        leftTitle.textContent = "Conversations";
        const newBtn = document.createElement("button");
        newBtn.type = "button";
        newBtn.className = "chat-new-btn";
        newBtn.textContent = "+ New";
        newBtn.title = "Start a new conversation (Cmd+N coming soon)";
        leftHead.append(leftTitle, newBtn);
        left.appendChild(leftHead);

        const convList = document.createElement("div");
        convList.className = "chat-conv-list";
        left.appendChild(convList);
        root.appendChild(left);

        const right = document.createElement("section");
        right.className = "chat-right";

        const header = document.createElement("header");
        header.className = "chat-header";
        const titleEl = document.createElement("input");
        titleEl.className = "chat-conv-title";
        titleEl.value = "New conversation";
        titleEl.placeholder = "Untitled";
        titleEl.spellcheck = false;

        // Overflow menu — "⋯" with dropdown for conversation actions.
        const overflow = document.createElement("div");
        overflow.className = "notes-overflow"; // reuse notes overflow styles
        const overflowBtn = document.createElement("button");
        overflowBtn.type = "button";
        overflowBtn.className = "notes-overflow-btn";
        overflowBtn.textContent = "⋯";
        overflowBtn.title = "More actions";
        overflowBtn.addEventListener("click", (e) => {
            e.stopPropagation();
            overflow.classList.toggle("is-open");
        });
        const overflowMenu = document.createElement("div");
        overflowMenu.className = "notes-overflow-menu";

        function currentConv() {
            if (!localState.conversationId) return null;
            return localState.conversations.find((c) => c.id === localState.conversationId) || null;
        }

        const pinItem = document.createElement("button");
        pinItem.type = "button";
        pinItem.className = "notes-overflow-item";
        pinItem.textContent = "Pin chat";
        pinItem.addEventListener("click", async (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            if (!localState.conversationId) return;
            const conv = currentConv();
            const nextPinned = !(conv && conv.pinned);
            try {
                const c = await apiJSON("/console/api/conversations/" + encodeURIComponent(localState.conversationId), {
                    method: "PATCH",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ pinned: nextPinned }),
                });
                const idx = localState.conversations.findIndex((x) => x.id === c.id);
                if (idx >= 0) localState.conversations[idx] = c;
                renderConvList();
                window.dispatchEvent(new CustomEvent("familiar:pinsChanged"));
            } catch (err) {
                notifyErr("Couldn't pin: " + (err.message || String(err)));
            }
        });
        overflowMenu.appendChild(pinItem);

        const deleteItem = document.createElement("button");
        deleteItem.type = "button";
        deleteItem.className = "notes-overflow-item danger";
        deleteItem.textContent = "Delete chat";
        deleteItem.addEventListener("click", async (e) => {
            e.stopPropagation();
            overflow.classList.remove("is-open");
            if (!localState.conversationId) return;
            const title = titleEl.value || "this chat";
            if (!confirm('Delete "' + title + '"? This cannot be undone.')) return;
            const deletedId = localState.conversationId;
            try {
                await apiJSON("/console/api/conversations/" + encodeURIComponent(deletedId), {
                    method: "DELETE",
                });
                // Broadcast the deletion so every open chat shell drops
                // the row from its conversation list synchronously — a
                // stale row stays clickable and 404s on open otherwise.
                window.dispatchEvent(new CustomEvent("familiar:conversationDeleted", {
                    detail: { id: deletedId },
                }));
                window.dispatchEvent(new CustomEvent("familiar:pinsChanged"));
                // The conversation this tab showed is gone — close the
                // tab now that the user has confirmed.
                if (window.FamiliarWorkspace && window.FamiliarWorkspace.closeTab) {
                    window.FamiliarWorkspace.closeTab(tab.id);
                }
            } catch (err) {
                notifyErr("Couldn't delete: " + (err.message || String(err)));
            }
        });
        overflowMenu.appendChild(deleteItem);
        overflow.append(overflowBtn, overflowMenu);
        // Refresh Pin/Unpin label every time the menu opens.
        overflowBtn.addEventListener("click", () => {
            const conv = currentConv();
            pinItem.textContent = (conv && conv.pinned) ? "Unpin chat" : "Pin chat";
        });

        // Close overflow menu when clicking elsewhere.
        document.addEventListener("click", () => {
            overflow.classList.remove("is-open");
        });

        // Shard chip — visible only on shard-bound conversations so
        // it's never ambiguous which brain is answering
        // (SKILL-PACKAGES-SPEC Phase 1).
        const shardChip = document.createElement("span");
        shardChip.className = "chat-shard-chip";
        shardChip.hidden = true;
        header.append(titleEl, shardChip, overflow);

        // updateShardChip reflects a conversation's binding. Shard
        // names come from the lazily-cached shards catalog when
        // available; the raw id is an acceptable fallback.
        function updateShardChip(model) {
            const bound = typeof model === "string" && model.indexOf("shard:") === 0;
            shardChip.hidden = !bound;
            localState.shardBound = bound;
            if (!bound) return;
            const id = model.slice(6);
            let label = id;
            for (const sh of (localState.shardsCatalog || [])) {
                if (sh.id === id) { label = sh.name || id; break; }
            }
            shardChip.textContent = "\u2b21 " + label;
            shardChip.title = "This conversation runs inside the \"" + label + "\" shard envelope";
            // Catalog miss (e.g. arrived via sidebar before the
            // models menu ever opened) — resolve the display name
            // lazily; the id keeps the chip honest meanwhile.
            if (label === id) {
                apiJSON("/console/api/shards/" + encodeURIComponent(id)).then((sh) => {
                    if (sh && sh.name && localState.shardBound) {
                        shardChip.textContent = "\u2b21 " + sh.name;
                        shardChip.title = "This conversation runs inside the \"" + sh.name + "\" shard envelope";
                    }
                }).catch(() => {});
            }
        }
        right.appendChild(header);

        const messagesEl = document.createElement("div");
        messagesEl.className = "chat-messages";
        // Track the user's scroll position continuously so we can
        // restore it after a re-render (workspace.js's renderGrid
        // detaches + re-attaches our cached shell on every layout
        // change, and the reflow during re-attach resets scrollTop
        // unless we explicitly put it back). 0 = top; we initialize
        // to -1 to signal "no user scroll yet, fall back to bottom"
        // since fresh chats should sit at the latest message.
        let lastScrollTop = -1;
        // Sticky autoscroll. Streaming used to slam scrollTop to the
        // bottom on every token (and every thinking chunk), so a user
        // who scrolled up to re-read earlier messages got yanked back
        // down mid-generation. Instead we only follow new content while
        // "stuck" to the bottom.
        //
        // "Stuck to bottom" follows the scroll position: any time the
        // viewport sits at (or within NEAR_BOTTOM_PX of) the bottom we
        // keep following new content; once it's higher, we stop. The
        // wheel/key handlers below detach SYNCHRONOUSLY on an upward
        // gesture so a streamed token arriving in the same frame can't
        // re-yank before the (async) scroll event recomputes — without
        // that, our own scroll-to-bottom would fight the user.
        let stickToBottom = true;
        const NEAR_BOTTOM_PX = 64;
        function isNearBottom() {
            return messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight <= NEAR_BOTTOM_PX;
        }
        function autoscrollIfStuck() {
            if (stickToBottom) messagesEl.scrollTop = messagesEl.scrollHeight;
        }
        messagesEl.addEventListener("scroll", () => {
            lastScrollTop = messagesEl.scrollTop;
            stickToBottom = isNearBottom();
        });
        messagesEl.addEventListener("wheel", (e) => {
            if (e.deltaY < 0) stickToBottom = false; // scrolling up → let go now
        }, { passive: true });
        messagesEl.addEventListener("keydown", (e) => {
            if (e.key === "ArrowUp" || e.key === "PageUp" || e.key === "Home") {
                stickToBottom = false;
            }
        });
        // In-chat note links (research write-ups render `#note/<book>/<page>`
        // links) open the note in a side pane instead of navigating the
        // page. Delegated so it covers both live-appended links and links
        // re-rendered from persisted message markdown on reload.
        messagesEl.addEventListener("click", (e) => {
            const a = e.target.closest && e.target.closest('a[href^="#note/"]');
            if (!a) return;
            e.preventDefault();
            const rest = a.getAttribute("href").slice("#note/".length);
            const slash = rest.indexOf("/");
            if (slash < 0) return;
            let book, page;
            try {
                book = decodeURIComponent(rest.slice(0, slash));
                page = decodeURIComponent(rest.slice(slash + 1));
            } catch (err) {
                return; // malformed percent-escape → inert, not a thrown handler
            }
            openResearchNoteInPane({ book_slug: book, page_slug: page, title: a.textContent || "" });
        });
        function restoreScroll() {
            // Two RAFs — first lets the browser commit the new
            // children + measure scrollHeight, second lets us set
            // scrollTop against the now-stable layout.
            requestAnimationFrame(() => {
                requestAnimationFrame(() => {
                    if (lastScrollTop < 0) {
                        messagesEl.scrollTop = messagesEl.scrollHeight;
                    } else {
                        messagesEl.scrollTop = lastScrollTop;
                    }
                });
            });
        }
        const empty = document.createElement("div");
        empty.className = "chat-empty";
        empty.textContent = "Send a message to start.";
        messagesEl.appendChild(empty);
        right.appendChild(messagesEl);

        // Persistent "research in progress" card (RESEARCH-SKILL-SPEC
        // §6.7). Pinned just above the composer, rebuilt purely from
        // the polled server state each tick so a reopened tab restores
        // it with no client memory. Hidden until a run is active.
        const researchCard = document.createElement("div");
        researchCard.className = "chat-research-card";
        researchCard.hidden = true;
        // Clickable so a user who closed the right pane can reopen the
        // live evidence view. Re-opens for the run the card is showing.
        researchCard.setAttribute("role", "button");
        researchCard.tabIndex = 0;
        researchCard.title = "Open the live research view";
        function reopenResearchPane() {
            const run = localState.researchLastRun;
            if (run && run.evidence_book_slug && run.evidence_page_slug) {
                openResearchEvidencePane(run);
            }
        }
        // Stop button on the card cancels the run (workers cut server-side,
        // run marked failed); the next poll tick clears the card.
        async function cancelResearchRun() {
            const run = localState.researchLastRun;
            if (!run || !run.id) return;
            try {
                await apiJSON("/console/api/research/runs/" + encodeURIComponent(run.id) + "/cancel", { method: "POST" });
            } catch (e) { /* best-effort; the poll reflects the terminal state */ }
        }
        researchCard.addEventListener("click", (e) => {
            if (e.target.closest(".rc-stop")) { e.stopPropagation(); cancelResearchRun(); return; }
            reopenResearchPane();
        });
        researchCard.addEventListener("keydown", (e) => {
            if (e.target.closest(".rc-stop")) return; // the button handles its own Enter/Space
            if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                reopenResearchPane();
            }
        });
        right.appendChild(researchCard);

        const inputWrap = document.createElement("form");
        inputWrap.className = "chat-input-wrap";
        const input = document.createElement("textarea");
        input.className = "chat-input";
        input.rows = 2;
        input.placeholder = "Message Familiar…";
        const sendBtn = document.createElement("button");
        sendBtn.type = "submit";
        sendBtn.className = "btn-accent btn-small chat-send-btn";
        sendBtn.textContent = "Send";
        inputWrap.append(input, sendBtn);
        right.appendChild(inputWrap);

        // While a turn is generating, the Send button becomes a small red
        // octagon Stop (clicking it aborts the stream, keeping the partial
        // text). Idle → "Send" again.
        function setComposerStreaming(on) {
            if (on) {
                sendBtn.classList.add("chat-send-stop");
                sendBtn.innerHTML = '<span class="chat-stop-oct" aria-hidden="true"></span>';
                sendBtn.setAttribute("aria-label", "Stop generating");
                sendBtn.title = "Stop generating";
            } else {
                sendBtn.classList.remove("chat-send-stop");
                sendBtn.textContent = "Send";
                sendBtn.removeAttribute("aria-label");
                sendBtn.removeAttribute("title");
            }
            sendBtn.disabled = false;
        }
        // Clicking the button while streaming stops generation. We fire a
        // server-side stop (cuts the model's generation and commits the
        // partial the turn produced, so persisted history matches what was
        // shown) AND abort the local fetch for an instant UI response and
        // as a fallback when there is no live server turn to cut.
        sendBtn.addEventListener("click", (e) => {
            if (localState.streaming && localState.currentAbort) {
                e.preventDefault();
                requestServerStop(localState.conversationId);
                localState.currentAbort.abort();
            }
        });

        // Best-effort POST to cut the server-side turn. Fire-and-forget:
        // the local abort already stopped the UI, and the server commits
        // whatever partial it produced. Errors are non-fatal — a failed
        // stop just means the detached turn finishes on its own (the prior
        // behavior), so there's nothing to surface to the user.
        function requestServerStop(convID) {
            if (!convID) return;
            try {
                fetch("/api/chat/stop", {
                    method: "POST",
                    credentials: "include",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ conversation_id: convID }),
                }).catch(() => {});
            } catch (_) { /* fetch threw synchronously — ignore */ }
        }

        // Splash host — full-width landing page rendered when the
        // user clicks "Chat" in the sidebar nav, or when a fresh tab
        // opens with no conversation selected. Mirrors the wiki +
        // notes splash: big "+ New chat" tile + pinned chats list.
        const splashHost = document.createElement("div");
        splashHost.className = "chat-splash-host wiki-splash-host";
        right.appendChild(splashHost);

        root.appendChild(right);

        // ── Behavior ──────────────────────────────────────────

        function autosizeInput() {
            input.style.height = "auto";
            const max = 160;
            input.style.height = Math.min(input.scrollHeight, max) + "px";
        }

        async function refreshConversations() {
            try {
                const resp = await apiJSON("/console/api/conversations?limit=50");
                localState.conversations = (resp && resp.items) || [];
                renderConvList();
            } catch (e) {
                // Network or backend down — show a tiny error
                // chip without blocking the surface.
                convList.innerHTML = '<div class="chat-conv-error">' + escapeHTML(e.message || String(e)) + '</div>';
            }
        }

        function renderConvList() {
            convList.innerHTML = "";
            if (localState.conversations.length === 0) {
                const stub = document.createElement("div");
                stub.className = "chat-conv-empty";
                stub.textContent = "No conversations yet — click New.";
                convList.appendChild(stub);
                return;
            }
            for (const c of localState.conversations) {
                const row = document.createElement("button");
                row.type = "button";
                row.className = "chat-conv-row";
                if (c.id === localState.conversationId) row.classList.add("is-active");
                const t = document.createElement("div");
                t.className = "chat-conv-row-title";
                t.textContent = c.title || "Untitled";
                const m = document.createElement("div");
                m.className = "chat-conv-row-meta";
                m.textContent = (c.model || "familiar") + " · " + agoOrDate(c.updated_at);
                row.append(t, m);
                row.addEventListener("click", () => loadConversation(c.id));
                convList.appendChild(row);
            }
        }

        // ── Splash view ────────────────────────────────────────
        // Pulls pinned conversations from /console/api/home/pins
        // (filtered to kind === "chat") and renders a wiki-style
        // landing page. Toggled by enterSplash / exitSplash; the
        // header / messages / input chrome is hidden via the root's
        // .is-splash class.
        async function renderSplash() {
            splashHost.innerHTML = "";

            const head = document.createElement("div");
            head.className = "wiki-splash-head";
            head.innerHTML = '<h1 class="wiki-splash-title">Your chats</h1>';
            splashHost.appendChild(head);

            const grid = document.createElement("div");
            grid.className = "wiki-splash-grid";

            // Left column: compact "+ New chat" button with this
            // section's pinned items below it (splash rework
            // 2026-06-12). Recents take the right column.
            const tileCol = document.createElement("div");
            tileCol.className = "wiki-splash-tile-col";
            const tile = document.createElement("button");
            tile.type = "button";
            tile.className = "wiki-empty-tile is-moss is-compact";
            tile.innerHTML =
                '<div class="wiki-empty-tile-glyph">' + chatGlyphSVG() + '</div>' +
                '<div class="wiki-empty-tile-title">New chat</div>';
            tile.addEventListener("click", () => { newConversation(); });
            const pinLabel = document.createElement("div");
            pinLabel.className = "wiki-splash-list-label";
            pinLabel.textContent = "Pinned";
            const pinList = document.createElement("div");
            pinList.className = "wiki-splash-list";
            pinList.innerHTML = '<div class="wiki-splash-empty">Loading…</div>';
            tileCol.append(tile, pinLabel, pinList);
            grid.appendChild(tileCol);

            const listCol = document.createElement("div");
            listCol.className = "wiki-splash-list-col";
            const recLabel = document.createElement("div");
            recLabel.className = "wiki-splash-list-label";
            recLabel.textContent = "Recent";
            const list = document.createElement("div");
            list.className = "wiki-splash-list is-dense";
            list.innerHTML = '<div class="wiki-splash-empty">Loading recent chats…</div>';
            listCol.append(recLabel, list);
            grid.appendChild(listCol);

            splashHost.appendChild(grid);

            const chatRowGlyph =
                '<div class="wiki-splash-row-glyph">' +
                    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round" width="18" height="18">' +
                        '<path d="M4 5 h16 a1.5 1.5 0 0 1 1.5 1.5 v9 a1.5 1.5 0 0 1 -1.5 1.5 H10 l-4 4 v-4 H4 a1.5 1.5 0 0 1 -1.5 -1.5 v-9 A1.5 1.5 0 0 1 4 5 Z"/>' +
                    '</svg>' +
                '</div>';

            // Pinned (left, compact rows: glyph + title only).
            apiJSON("/console/api/home/pins").then((resp) => {
                const items = ((resp && resp.items) || []).filter((it) => it.kind === "chat");
                pinList.innerHTML = "";
                if (items.length === 0) {
                    pinList.innerHTML = '<div class="wiki-splash-empty">Nothing pinned — pin a chat from its ⋯ menu.</div>';
                    return;
                }
                items.forEach((it) => {
                    const row = document.createElement("button");
                    row.type = "button";
                    row.className = "wiki-splash-row";
                    row.innerHTML =
                        chatRowGlyph +
                        '<div class="wiki-splash-row-body">' +
                            '<div class="wiki-splash-row-title">' + escapeHTML(it.title || "Untitled") + '</div>' +
                        '</div>';
                    row.addEventListener("click", () => loadConversation(it.id));
                    pinList.appendChild(row);
                });
            }).catch(() => {
                pinList.innerHTML = '<div class="wiki-splash-empty">Couldn’t load pins.</div>';
            });

            // Recents (right, dense).
            apiJSON("/console/api/conversations?limit=50").then((resp) => {
                const items = (resp && resp.items) || [];
                list.innerHTML = "";
                if (items.length === 0) {
                    list.innerHTML = '<div class="wiki-splash-empty">No conversations yet — click New chat.</div>';
                    return;
                }
                items.forEach((c) => {
                    const row = document.createElement("button");
                    row.type = "button";
                    row.className = "wiki-splash-row";
                    row.innerHTML =
                        chatRowGlyph +
                        '<div class="wiki-splash-row-body">' +
                            '<div class="wiki-splash-row-title">' + escapeHTML(c.title || "Untitled") + '</div>' +
                        '</div>' +
                        '<div class="wiki-splash-row-meta">' + escapeHTML(relTime(c.updated_at)) + '</div>';
                    row.addEventListener("click", () => loadConversation(c.id));
                    list.appendChild(row);
                });
            }).catch((e) => {
                list.innerHTML = '<div class="wiki-splash-empty">' + escapeHTML(e.message || String(e)) + '</div>';
            });
        }
        function enterSplash() {
            // Clear conversation state so the workspace's isTabEmpty
            // sees this tab as "available" — the next sidebar nav
            // click will reuse it instead of stacking another splash.
            localState.conversationId = null;
            localState.messages = [];
            tab.state = { conversationId: null };
            // No conversation open → nothing to poll; drop any card.
            stopResearchPoll();
            clearResearchCard();
            titleEl.value = "New conversation";
            if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                window.FamiliarWorkspace.updateTabTitle(tab.id, "Chat");
            }
            root.classList.add("is-splash");
            renderSplash();
        }
        function exitSplash() {
            root.classList.remove("is-splash");
        }

        async function loadConversation(id) {
            exitSplash();
            localState.conversationId = id;
            tab.state = { conversationId: id };
            // Scope the research poll to the conversation now open —
            // replaces any poll left running for a previous one.
            startResearchPoll(id);
            renderConvList();
            try {
                const resp = await apiJSON("/console/api/conversations/" + encodeURIComponent(id));
                const conv = resp && resp.conversation;
                const msgs = (resp && resp.messages) || [];
                titleEl.value = (conv && conv.title) || "Conversation";
                updateShardChip(conv && conv.model);
                localState.messages = msgs;
                renderMessages();
                // Update workspace tab label.
                if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                    window.FamiliarWorkspace.updateTabTitle(tab.id, titleEl.value);
                }
            } catch (e) {
                // Deleted on another device (or a stale localStorage
                // tab restore) — land on the splash instead of a
                // dead-end error tab, and drop the ghost row.
                if (/not found|HTTP 404/i.test(e.message || "")) {
                    localState.conversations = localState.conversations.filter((c) => c.id !== id);
                    renderConvList();
                    enterSplash();
                    return;
                }
                renderError("Couldn't load conversation: " + e.message);
            }
        }

        // ── Research-in-progress card (RESEARCH-SKILL-SPEC §6.7) ──
        //
        // Autonomous deep-research runs execute in the background
        // (workers → synthesis) and, when done, append an assistant
        // summary to the conversation. While a run is live we show a
        // persistent status card above the composer. The card is
        // driven ONLY by polling the active-run endpoint — never from
        // in-memory kickoff state — so a reopened tab (or a full page
        // reload) restores it straight from the server.
        //
        // The endpoint only ever returns a non-terminal run
        // (researching / synthesizing); once a run goes terminal it
        // returns {"run": null}. So a null response *after* a card was
        // showing means the run just finished — we refetch the
        // conversation (to surface the delivered summary) and clear the
        // card. There is no separate "done" payload.
        const RESEARCH_POLL_MS = 5000;

        function startResearchPoll(id) {
            stopResearchPoll();
            clearResearchCard(); // drop any card from a previous conversation
            // New conversation scope → forget the run we auto-opened
            // the evidence pane for, so this conversation's own run can
            // open fresh. We keep researchEvTabId around: if the pane is
            // still open it gets reused instead of stacking a new tab.
            localState.researchOpenedRunId = null;
            localState.researchLastRun = null;
            if (!id) return;
            localState.researchPollId = id;
            pollResearch(); // immediate tick so a live run shows at once
            localState.researchTimer = setInterval(pollResearch, RESEARCH_POLL_MS);
        }

        function stopResearchPoll() {
            if (localState.researchTimer) {
                clearInterval(localState.researchTimer);
                localState.researchTimer = null;
            }
            localState.researchPollId = null;
        }

        async function pollResearch() {
            const id = localState.researchPollId;
            if (!id) return;
            let run = null;
            try {
                const resp = await apiJSON(
                    "/console/api/research/runs/active?conversation_id=" + encodeURIComponent(id));
                run = resp && resp.run;
            } catch (e) {
                return; // best-effort — try again next tick
            }
            // Conversation changed while the request was in flight —
            // don't paint a stale run onto the now-current card.
            if (localState.researchPollId !== id) return;

            if (run && (run.status === "researching" || run.status === "synthesizing")) {
                renderResearchCard(run);
                localState.researchCardShown = true;
                // Capture the live payload — the card click reopens from
                // it and the done-transition reads the note slugs off it.
                localState.researchLastRun = run;
                if (localState.researchOpenedRunId !== run.id) {
                    // First sighting of this active run for the open
                    // conversation → auto-open the evidence page
                    // read-only in the right pane (enter side-by-side).
                    // Only when THIS chat shell is the visible tab —
                    // a background conversation's run must not hijack
                    // the layout out from under the user. Retries when
                    // the tab is brought forward (guard stays unset).
                    if (document.body.contains(root)) {
                        localState.researchOpenedRunId = run.id;
                        openResearchEvidencePane(run);
                    }
                } else if (localState.researchEvTabId) {
                    // Fallback live refresh in case a page-saved SSE
                    // event didn't reach the evidence shell (every 5s).
                    window.dispatchEvent(new CustomEvent("familiar:researchEvidenceRefresh", {
                        detail: { tabId: localState.researchEvTabId },
                    }));
                }
                return;
            }
            // run is null / terminal.
            if (localState.researchCardShown) {
                // Don't clobber an in-flight stream mid-send; the run
                // finishes long after the kickoff reply, so retrying
                // next tick is safe.
                if (localState.streaming) return;
                localState.researchCardShown = false;
                clearResearchCard();
                // Run finished — if we auto-opened the evidence view for
                // it, swap the right pane to the delivered note (editable).
                if (localState.researchOpenedRunId && localState.researchLastRun) {
                    swapResearchPaneToNote(localState.researchLastRun);
                }
                localState.researchOpenedRunId = null;
                localState.researchLastRun = null;
                // Refetch so the just-delivered summary (or failure
                // note) appears, then the card is gone.
                if (localState.conversationId === id) loadConversation(id);
                // The note was written server-side (deep synthesis) or by
                // the async writer, so no __TOOL_EFFECT__:note_changed
                // reached the client — refresh the notes rail explicitly
                // so the new note shows without a manual reload.
                window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
            } else {
                clearResearchCard();
            }
        }

        // ── Right-pane orchestration for live research ────────────
        //
        // The evidence page and the delivered note both live in the
        // OTHER pane from this chat. We force side-by-side, then reuse
        // (or mint) a tab in that pane: a wiki tab for the read-only
        // evidence page, a notes tab for the editable delivered note.

        // rightPaneSlot returns the layout slot that isn't this chat's
        // — the one that should host the research view. Falls back to
        // "B" when state can't be read.
        function rightPaneSlot() {
            const ws = window.FamiliarWorkspace;
            try {
                const st = ws.getState();
                const chatSlot = st.tabs[tab.id] && st.tabs[tab.id].panelSlot;
                const slots = Object.keys(st.panels);
                return slots.find((s) => s !== chatSlot) || slots[0] || "B";
            } catch (e) {
                return "B";
            }
        }

        // openResearchEvidencePane forces side-by-side and drives a
        // wiki tab in the right pane to show the evidence page read-only.
        function openResearchEvidencePane(run) {
            const ws = window.FamiliarWorkspace;
            if (!ws || !run || !run.evidence_book_slug || !run.evidence_page_slug) return;
            if (ws.getState().layout !== "split-2x1") ws.setLayout("split-2x1");
            const slot = rightPaneSlot();
            const st = ws.getState();
            let tabId = localState.researchEvTabId;
            const existing = tabId && st.tabs[tabId];
            if (existing && existing.surface === "wiki") {
                // Reuse the pane we already opened — focus it if the user
                // had switched it into the background.
                ws.switchTab(existing.panelSlot, tabId);
            } else {
                tabId = ws.openTab("wiki", slot, { title: researchTabTitle(run) });
                localState.researchEvTabId = tabId;
            }
            const detail = {
                surface: "wiki",
                research: true,
                tabId: tabId,
                bookSlug: run.evidence_book_slug,
                pageSlug: run.evidence_page_slug,
                title: researchTabTitle(run),
            };
            // Let the openTab render pass mount the wiki shell first.
            setTimeout(() => {
                window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail }));
            }, 0);
        }

        // swapResearchPaneToNote replaces the read-only evidence view
        // with the delivered note, opened editable via the standard
        // notes surface, and retires the evidence tab.
        async function swapResearchPaneToNote(run) {
            const ws = window.FamiliarWorkspace;
            if (!ws) return;
            const evTabId = localState.researchEvTabId;
            const nb = run && run.note_book_slug;
            const np = run && run.note_page_slug;
            if (!nb || !np) return; // note slugs unknown → leave the pane as-is
            let notePage;
            try {
                notePage = await apiJSON(
                    "/console/api/books/" + encodeURIComponent(nb) +
                    "/pages/" + encodeURIComponent(np));
            } catch (e) {
                return; // couldn't resolve the note → leave the evidence view
            }
            if (ws.getState().layout !== "split-2x1") ws.setLayout("split-2x1");
            const slot = rightPaneSlot();
            const noteTabId = ws.openTab("notes", slot, { title: notePage.title || "Note" });
            setTimeout(() => {
                window.dispatchEvent(new CustomEvent("familiar:openDoc", {
                    detail: { surface: "notes", id: notePage.id, tabId: noteTabId, title: notePage.title },
                }));
            }, 0);
            // Retire the transient evidence tab now the note has the pane.
            if (evTabId && ws.getState().tabs[evTabId] && ws.closeTab) {
                ws.closeTab(evTabId);
            }
            localState.researchEvTabId = null;
        }

        function researchTabTitle(run) {
            const topic = (run && run.topic) || "Research";
            return "Research: " + topic;
        }

        // openResearchNoteInPane opens a just-delivered personal research
        // note (inline path) editable in the side pane — the same
        // machinery swapResearchPaneToNote uses for the deep path, but
        // driven off a {book_slug,page_slug,title} ref instead of a run.
        // Best-effort: any failure (no workspace, note not resolvable)
        // leaves the layout untouched — the chat link still works.
        async function openResearchNoteInPane(note) {
            const ws = window.FamiliarWorkspace;
            if (!ws || !note || !note.book_slug || !note.page_slug) return;
            // The notes surface renders personal notes only.
            if (note.book_slug.indexOf("personal") !== 0) return;
            let notePage;
            try {
                notePage = await apiJSON(
                    "/console/api/books/" + encodeURIComponent(note.book_slug) +
                    "/pages/" + encodeURIComponent(note.page_slug));
            } catch (e) {
                return;
            }
            // Already open somewhere? Focus it instead of minting a
            // duplicate tab / re-forcing the split — the durable in-chat
            // link is clickable repeatedly (and after reload), so opening
            // must be idempotent.
            if (ws.focusExistingDoc && ws.focusExistingDoc("notes", notePage.id)) return;
            if (ws.getState().layout !== "split-2x1") ws.setLayout("split-2x1");
            const slot = rightPaneSlot();
            const noteTabId = ws.openTab("notes", slot, { title: notePage.title || note.title || "Note" });
            setTimeout(() => {
                window.dispatchEvent(new CustomEvent("familiar:openDoc", {
                    detail: { surface: "notes", id: notePage.id, tabId: noteTabId, title: notePage.title },
                }));
            }, 0);
        }

        // The pixel indicator (three columns 1·2·3 tall). State drives
        // the color/motion: active = purple pulse wave, done = solid
        // green, failed = amber, queued = faint. Purely presentational.
        function rcPixels(state) {
            const cls = state === "done" ? "rc-px-moss rc-px-fill"
                : state === "active" ? "rc-px-anim"
                : state === "failed" ? "rc-px-amber"
                : "rc-px-dim";
            return '<span class="rc-px ' + cls + '" aria-hidden="true">' +
                '<span class="rc-c rc-c1"><i></i></span>' +
                '<span class="rc-c rc-c2"><i></i><i></i></span>' +
                '<span class="rc-c rc-c3"><i></i><i></i><i></i></span></span>';
        }
        function rcWorkerText(state) {
            return state === "done" ? "done"
                : state === "active" ? "searching"
                : state === "failed" ? "no result"
                : "queued";
        }
        // Display state per area: during synthesis every gathered area
        // reads done (only a genuinely-failed, un-retried one stays amber),
        // so the roster, subtitle, and footer all agree.
        function rcDisplayState(w, synth) {
            const s = (w && w.state) || "queued";
            return synth && s !== "failed" ? "done" : s;
        }

        function renderResearchCard(run) {
            const topic = run.topic || "your topic";
            const workers = Array.isArray(run.workers) ? run.workers : [];
            const synth = run.status === "synthesizing";
            const total = workers.length || run.workers_total || 0;
            const dstates = workers.map((w) => rcDisplayState(w, synth));
            const doneN = dstates.filter((s) => s === "done").length;
            const failedN = dstates.filter((s) => s === "failed").length;
            const tokens = run.tokens || 0;

            const title = synth ? "Writing up" : "Researching";
            const sub = synth
                ? (failedN > 0 ? doneN + " of " + total + " areas gathered · composing the note"
                    : "all areas gathered · composing the note")
                : (total ? doneN + " of " + total + " areas in" : "starting…");

            // Roster model (one entry per row). While researching, one row
            // per fan-out worker. During synthesis we collapse the finished
            // fan-out into a single "Research workers" row (done, green) and
            // add a live "Writing the note" row so the card keeps an animated
            // progress indicator through the write-up phase.
            const roster = synth
                ? [
                    // Green "done" once the fan-out gathered evidence; amber
                    // "failed" only when it came back empty (doneN 0), so an
                    // all-failed run can't show green success pixels. Partial
                    // failures stay green with an honest "N/total" count.
                    { state: (doneN > 0 || total === 0) ? "done" : "failed", label: "Research workers", wtext: failedN > 0 ? doneN + "/" + total : "complete" },
                    { state: "active", label: "Writing the note", wtext: "composing" },
                ]
                : workers.map((w, i) => ({
                    state: dstates[i],
                    label: (w && w.question) || ("Area " + (i + 1)),
                    wtext: rcWorkerText(dstates[i]),
                }));

            // A full innerHTML swap restarts every pixel animation. So we
            // only rebuild when the STRUCTURE changes (status, round, topic,
            // roster size); a per-area state change patches just that row,
            // and a steady tick patches only the counters — the other rows'
            // pulses keep marching without a reset.
            const struct = (run.status || "") + "|" + (run.round || 1) + "|" + topic + "|" + total;
            const setText = (sel, v) => { const el = researchCard.querySelector(sel); if (el) el.textContent = v; };
            if (!researchCard.hidden && researchCard.dataset.struct === struct) {
                const rowEls = researchCard.querySelectorAll(".rc-row");
                for (let i = 0; i < rowEls.length && i < roster.length; i++) {
                    if (rowEls[i].dataset.st !== roster[i].state) {
                        rowEls[i].dataset.st = roster[i].state;
                        rowEls[i].className = "rc-row rc-" + roster[i].state;
                        rowEls[i].querySelector(".rc-slot").innerHTML = rcPixels(roster[i].state);
                        rowEls[i].querySelector(".rc-wstate").textContent = roster[i].wtext;
                    }
                }
                setText('[data-rc="areas"]', doneN + "/" + (total || "?"));
                setText('[data-rc="tokens"]', compactTokens(tokens));
                setText(".rc-s", sub);
                return;
            }
            researchCard.dataset.struct = struct;

            let rows = "";
            for (let i = 0; i < roster.length; i++) {
                const r = roster[i];
                rows += '<div class="rc-row rc-' + r.state + '" data-st="' + r.state + '">' +
                    '<span class="rc-slot">' + rcPixels(r.state) + "</span>" +
                    '<span class="rc-label">' + escapeHTML(r.label) + "</span>" +
                    '<span class="rc-wstate">' + escapeHTML(r.wtext) + "</span></div>";
            }

            // Counters carry data-rc/data-st so the incremental path can
            // update them in place; pages/tokens always render (even 0) so
            // they exist to update without forcing a structural rebuild.
            const meta = '<span class="rc-m"><span class="rc-k">round</span> <b>' + (run.round || 1) + "</b></span>" +
                '<span class="rc-m rc-m-moss"><span class="rc-k">areas</span> <b data-rc="areas">' + doneN + "/" + (total || "?") + "</b></span>" +
                '<span class="rc-m"><span class="rc-k">tokens</span> <b data-rc="tokens">' + compactTokens(tokens) + "</b></span>";

            const stop = '<button class="rc-stop" type="button" title="Stop research" aria-label="Stop research"><span class="chat-stop-oct" aria-hidden="true"></span></button>';
            researchCard.className = "chat-research-card" + (synth ? " rc-synth" : "");
            researchCard.innerHTML =
                '<div class="rc-head">' +
                '<div class="rc-title"><div class="rc-t">' + title +
                ' <span class="rc-topic">“' + escapeHTML(topic) + '”</span></div>' +
                '<div class="rc-s">' + escapeHTML(sub) + "</div></div>" + stop + "</div>" +
                (rows ? '<div class="rc-roster">' + rows + "</div>" : "") +
                '<div class="rc-meta">' + meta + "</div>";
            researchCard.hidden = false;
        }

        function clearResearchCard() {
            researchCard.hidden = true;
            researchCard.className = "chat-research-card";
            researchCard.dataset.struct = "";
            researchCard.innerHTML = "";
            localState.researchCardShown = false;
        }

        async function newConversation(shardID) {
            try {
                exitSplash();
                const c = await apiJSON("/console/api/conversations", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ title: "", model: shardID ? "shard:" + shardID : "familiar" }),
                });
                localState.conversations.unshift(c);
                renderConvList();
                loadConversation(c.id);
            } catch (e) {
                renderError("Couldn't create conversation: " + e.message);
            }
        }

        // openShardChat focuses the most recent conversation bound to
        // the shard, or starts one. The sidebar Shards children and
        // the models menu both land here.
        async function openShardChat(shardID) {
            const want = "shard:" + shardID;
            const existing = (localState.conversations || []).find((c) => c.model === want);
            if (existing) {
                loadConversation(existing.id);
                return;
            }
            await newConversation(shardID);
        }

        // A persisted message is worth painting only when it's actual
        // conversation, not agentic plumbing. The gateway persists the
        // whole tool loop so a restart can replay it to the model:
        //   • role="tool" — tool-result rows.
        //   • assistant turns that carried ONLY tool calls — these persist
        //     with empty content (the spawn_research_workers kickoff, etc.).
        // Neither is a reply. We no longer paint a "tools: X" chip, so
        // rendering them just leaves bare "Familiar" bubbles with nothing
        // under them. Skip both; m.tool_calls stays persisted regardless.
        function isDisplayableMessage(m) {
            if (!m || m.role === "tool") return false;
            if (m.role === "assistant" && !String(m.content || "").trim()) return false;
            return true;
        }

        async function renderMessages() {
            messagesEl.innerHTML = "";
            if (localState.messages.length === 0) {
                const e = document.createElement("div");
                e.className = "chat-empty";
                e.textContent = "Send a message to start.";
                messagesEl.appendChild(e);
                return;
            }
            const renderMD = await ensureMarkdownDeps();
            for (const m of localState.messages) {
                if (!isDisplayableMessage(m)) continue;
                messagesEl.appendChild(renderMessage(m, renderMD));
            }
            // If the last real turn is an unanswered user message, the
            // previous reply was interrupted — the send path persists the
            // prompt BEFORE streaming and the answer only AFTER, so a
            // reload (or crash) mid-stream leaves the prompt with no
            // reply. Surface that instead of a conversation that looks
            // silently broken. (Full regenerate needs a gateway affordance
            // — tracked separately; this at least explains the state.)
            const lastReal = [...localState.messages].reverse().find((m) => m.role !== "tool");
            if (lastReal && lastReal.role === "user" && !localState.streaming) {
                const note = document.createElement("div");
                note.className = "chat-interrupted-note";
                note.textContent = "The previous reply was interrupted. Send a message to continue.";
                messagesEl.appendChild(note);
            }
            messagesEl.scrollTop = messagesEl.scrollHeight;
        }

        function renderMessage(m, renderMD) {
            const wrap = document.createElement("div");
            wrap.className = "chat-msg chat-msg-" + m.role;
            const head = document.createElement("div");
            head.className = "chat-msg-head";
            if (m.role === "assistant") {
                head.textContent = "Familiar";
            } else if (m.role === "user") {
                const sess = window.FAMILIAR_SESSION || {};
                const name = sess.display_name || sess.email || "User";
                head.textContent = name.charAt(0).toUpperCase() + name.slice(1);
            } else {
                head.textContent = m.role;
            }

            // Historical thinking trace, if persisted with the
            // message. Renders as a collapsed <details> above the
            // body so the bubble looks identical to a freshly-
            // streamed assistant message after the auto-collapse.
            // Empty reasoning → no element.
            const reasoning = m.reasoning_content;
            let thinkingEl = null;
            if (m.role === "assistant" && reasoning && String(reasoning).trim() !== "") {
                thinkingEl = document.createElement("details");
                thinkingEl.className = "chat-msg-thinking";
                const sum = document.createElement("summary");
                sum.className = "chat-msg-thinking-summary";
                sum.textContent = "Thinking";
                const pre = document.createElement("pre");
                pre.className = "chat-msg-thinking-body";
                pre.textContent = String(reasoning);
                thinkingEl.append(sum, pre);
            }

            const body = document.createElement("div");
            body.className = "chat-msg-body";
            // User content goes through the markdown pipeline too —
            // users paste code + lists + checklists, treating their
            // input as plain text would be hostile. DOMPurify keeps
            // it safe.
            body.innerHTML = renderMD(m.content || "");
            wrap.append(head);
            if (thinkingEl) wrap.append(thinkingEl);
            wrap.append(body);

            // Tool calls are intentionally NOT surfaced on rehydrate: the
            // "tools: X" summary was low-signal noise (and mobile never
            // showed it, so desktop was the odd one out). What a tool
            // produced is already in the content/result — and a research run
            // renders its own inline card. m.tool_calls stays persisted;
            // we just don't paint the chip.
            return wrap;
        }

        function renderError(msg) {
            const e = document.createElement("div");
            e.className = "chat-error";
            e.textContent = msg;
            messagesEl.appendChild(e);
            messagesEl.scrollTop = messagesEl.scrollHeight;
        }

        async function send() {
            if (localState.streaming) return;
            const prompt = input.value.trim();
            if (!prompt) return;

            // First turn? Track it so we can auto-title once the
            // assistant has replied. Keyed on message count, NOT on
            // conversationId — the "New chat" button calls
            // newConversation() which creates the conversation row
            // up front, so by the time the user sends, conversationId
            // already exists. An empty message list is the real
            // "this is the opening turn" signal.
            const isFirstTurn = localState.messages.length === 0;

            // Ensure a conversation exists. First send creates one
            // when the user typed straight into a fresh shell without
            // going through the New-chat button.
            if (!localState.conversationId) {
                try {
                    const c = await apiJSON("/console/api/conversations", {
                        method: "POST",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({ title: titleFromPrompt(prompt), model: "familiar" }),
                    });
                    localState.conversationId = c.id;
                    tab.state = { conversationId: c.id };
                    // A first-send-created conversation may host a
                    // research kickoff — poll it like any other.
                    startResearchPoll(c.id);
                    titleEl.value = c.title || "Conversation";
                    localState.conversations.unshift(c);
                    renderConvList();
                    // Update workspace tab label.
                    if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                        window.FamiliarWorkspace.updateTabTitle(tab.id, titleEl.value);
                    }
                } catch (e) {
                    renderError("Couldn't create conversation: " + e.message);
                    return;
                }
            }

            const userMsg = { role: "user", content: prompt };
            localState.messages.push(userMsg);
            input.value = "";
            autosizeInput();

            const renderMD = await ensureMarkdownDeps();
            // First time? Wipe the empty placeholder.
            const oldEmpty = messagesEl.querySelector(".chat-empty");
            if (oldEmpty) oldEmpty.remove();
            messagesEl.appendChild(renderMessage(userMsg, renderMD));
            // The user just sent — they want to watch the reply, so
            // re-engage sticky autoscroll even if they'd scrolled up
            // during the previous turn.
            stickToBottom = true;
            messagesEl.scrollTop = messagesEl.scrollHeight;

            // Persist the user message before calling the LLM —
            // makes resume-after-refresh work even mid-stream.
            try {
                await apiJSON("/console/api/conversations/" + encodeURIComponent(localState.conversationId) + "/messages", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(userMsg),
                });
            } catch (e) {
                renderError("Couldn't persist user message: " + e.message);
                return;
            }

            // Build assistant bubble; stream tokens into it.
            // Structure (top to bottom):
            //   <div class="chat-msg chat-msg-assistant ...">
            //     <div class="chat-msg-head">assistant</div>
            //     <details class="chat-msg-thinking" hidden>
            //       <summary>Thinking…</summary>
            //       <pre class="chat-msg-thinking-body"></pre>
            //     </details>
            //     <div class="chat-msg-body"></div>
            //   </div>
            //
            // The thinking <details> stays hidden until the first
            // reasoning_content chunk arrives; on the first chunk
            // we unhide it but leave it COLLAPSED — the user
            // expands when they want to inspect. The summary
            // text updates live ("Thinking… N words") so progress
            // is visible without forcing the panel open. Critical
            // for debugging Familiar's pipeline because the
            // gateway also pumps status updates ("Searching
            // memories…", "Generating response…") through the
            // reasoning_content channel.
            const assistantBubble = document.createElement("div");
            assistantBubble.className = "chat-msg chat-msg-assistant chat-msg-streaming";
            const head = document.createElement("div");
            head.className = "chat-msg-head";
            head.textContent = "Familiar";

            const thinking = document.createElement("details");
            thinking.className = "chat-msg-thinking";
            thinking.hidden = true;
            // Collapsed by default — user clicks to inspect. We
            // never set thinking.open during streaming.
            const thinkingSummary = document.createElement("summary");
            thinkingSummary.className = "chat-msg-thinking-summary";
            const thinkLabel = document.createElement("span");
            thinkLabel.className = "chat-think-label";
            thinkLabel.textContent = "Thinking";
            // The working indicator: the pixel staircase (same signal as
            // the research card). It rides the thinking line while the
            // model reasons and is dropped the moment answer tokens start
            // streaming — the streaming text carries the motion from there.
            // This replaces the old blinking body caret.
            const thinkPx = document.createElement("span");
            thinkPx.className = "rc-px rc-px-anim chat-think-px";
            thinkPx.setAttribute("aria-hidden", "true");
            thinkPx.innerHTML =
                '<span class="rc-c rc-c1"><i></i></span>' +
                '<span class="rc-c rc-c2"><i></i><i></i></span>' +
                '<span class="rc-c rc-c3"><i></i><i></i><i></i></span>';
            thinkingSummary.append(thinkLabel, thinkPx);
            const thinkingBody = document.createElement("pre");
            thinkingBody.className = "chat-msg-thinking-body";
            thinking.append(thinkingSummary, thinkingBody);
            // Show the working line immediately — before the first token —
            // so there is always a live "working" signal.
            thinking.hidden = false;

            // setStatus updates only the label span, preserving the
            // indicator ("Thinking" ⟶ "Searching the web" ⟶ …).
            const setStatus = (label) => {
                if (label) thinkLabel.textContent = label;
            };
            // Drop the indicator once real output begins.
            let indicatorDropped = false;
            const dropIndicator = () => {
                if (indicatorDropped) return;
                indicatorDropped = true;
                if (thinkPx.parentNode) thinkPx.remove();
            };
            // Bring the working indicator BACK when the model re-enters a
            // working phase — calling tools, searching, or resuming its
            // reasoning after a tool returns. Without this the indicator
            // drops on the first token (often a short tool preamble) and
            // stays gone through the whole tool-execution wait, leaving no
            // live "working" signal. Real answer text (token) re-drops it.
            const showIndicator = () => {
                if (!localState.streaming) return;
                indicatorDropped = false;
                if (!thinkPx.parentNode) thinkingSummary.appendChild(thinkPx);
            };

            const body = document.createElement("div");
            body.className = "chat-msg-body";
            assistantBubble.append(head, thinking, body);
            messagesEl.appendChild(assistantBubble);
            messagesEl.scrollTop = messagesEl.scrollHeight;

            localState.streaming = true;
            const abort = new AbortController();
            localState.currentAbort = abort;
            setComposerStreaming(true);

            // CHAT-REARCH §"Phase 0" — native /api/chat protocol.
            // The gateway holds conversation history, so we only send
            // the new user message. No messages[], no model field.
            let assistantText = "";
            let reasoningText = "";
            const tStart = performance.now();
            let tFirstToken = null;
            let resolvedModel = null;
            let memHits = null;
            let inputTokens = null;
            let outputTokens = null;
            let decodeMs = null; // server-measured decode time (authoritative)
            // A research note the backend reports this turn wrote (inline
            // quick/standard path) — {book_slug, page_slug, title}. Set in
            // the "done" handler, consumed after the stream to auto-open
            // the note + append a durable link to the message.
            let researchNote = null;
            let aborted = false; // user hit stop

            try {
                // conversation_id doubles as the in-memory session
                // id: the gateway keys its session by this UUID so
                // a restart rehydrates this chat's verbatim turns
                // from the messages table (SESSION-HYDRATION.md).
                // We always have one here — sendChat only fires
                // after a conversation row has been created.
                const resp = await fetch("/api/chat", {
                    method: "POST",
                    credentials: "include",
                    signal: abort.signal,
                    headers: {
                        "Content-Type": "application/json",
                        "Accept": "text/event-stream",
                    },
                    body: JSON.stringify({
                        message: prompt,
                        conversation_id: localState.conversationId || undefined,
                    }),
                });
                if (!resp.ok || !resp.body) {
                    const text = await resp.text();
                    throw new Error("HTTP " + resp.status + ": " + text.slice(0, 200));
                }
// Native SSE: events come as `event: <kind>\ndata: <json>\n\n`.
                // Kinds emitted by the gateway: session, token, reasoning,
                // status, done, error.
                const reader = resp.body.getReader();
                const decoder = new TextDecoder();
                let buf = "";
                let pendingEvent = "message";
                let pendingData = "";
                const flushEvent = () => {
                    if (!pendingData) {
                        pendingEvent = "message";
                        return;
                    }
                    let payload = {};
                    try { payload = JSON.parse(pendingData); } catch (_) { /* skip */ }
                    handleNativeEvent(pendingEvent, payload);
                    pendingEvent = "message";
                    pendingData = "";
                };
                const handleNativeEvent = (kind, p) => {
                    if (kind === "session") {
                        return; // reserved — gateway-side session id ack
                    }
                    if (kind === "status") {
                        const s = (p && p.message) || "";
                        if (!s) return;
                        // Full status text rides the (collapsed)
                        // thinking panel body as before.
                        if (thinking.hidden) thinking.hidden = false;
                        reasoningText += s + (s.endsWith("\n") ? "" : "\n");
                        thinkingBody.textContent = reasoningText;
                        thinkingBody.scrollTop = thinkingBody.scrollHeight;
                        // Engagement-worthy statuses (tool use, web /
                        // memory search) take over the one-line summary
                        // — "Searching the web", etc. Debug-only
                        // statuses map to null and leave it unchanged.
                        const activity = friendlyActivity(s);
                        setStatus(activity);
                        // A surfaced activity means the model is working
                        // again (not answering) — restore the indicator so
                        // it stays lit through tool execution.
                        if (activity) showIndicator();
                        return;
                    }
                    if (kind === "reasoning") {
                        const chunk = (p && p.chunk) || "";
                        if (!chunk) return;
                        // Tool-effect signals ride the reasoning channel
                        // but aren't visible thinking text — they trigger
                        // UI side effects. Strip them before display.
                        let cleaned = chunk;
                        if (cleaned.includes("__TOOL_EFFECT__:note_changed:")) {
                            window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
                            cleaned = cleaned.replace(/__TOOL_EFFECT__:note_changed:[^\n]*\n?/g, "");
                        }
                        if (!cleaned) return;
                        if (tFirstToken == null) tFirstToken = performance.now();
                        reasoningText += cleaned;
                        if (thinking.hidden) thinking.hidden = false;
                        thinkingBody.textContent = reasoningText;
                        // Reasoning is flowing — the one-line summary
                        // reads "Thinking" (a tool label, if one was
                        // set, gets superseded here once the model
                        // resumes reasoning).
                        thinkLabel.textContent = "Thinking";
                        // Model resumed reasoning (e.g. after a tool
                        // returned) → it's working, so relight the
                        // indicator until real answer text streams.
                        showIndicator();
                        thinkingBody.scrollTop = thinkingBody.scrollHeight;
                        autoscrollIfStuck();
                        return;
                    }
                    if (kind === "token") {
                        const chunk = (p && p.chunk) || "";
                        if (!chunk) return;
                        if (tFirstToken == null) tFirstToken = performance.now();
                        dropIndicator(); // real output began — hand motion to the text
                        assistantText += chunk;
                        body.innerHTML = renderMD(assistantText);
                        autoscrollIfStuck();
                        return;
                    }
                    if (kind === "done") {
                        if (p && p.model_id) resolvedModel = p.model_id;
                        if (p && p.research_note && p.research_note.page_slug) {
                            researchNote = p.research_note;
                        }
                        if (p && typeof p.mem_hits === "number") memHits = p.mem_hits;
                        if (p && typeof p.input_tokens === "number") inputTokens = p.input_tokens;
                        if (p && typeof p.output_tokens === "number") outputTokens = p.output_tokens;
                        if (p && typeof p.decode_ms === "number") decodeMs = p.decode_ms;
                        // The gateway sends authoritative parsed content
                        // that may differ from the streamed tokens (e.g.
                        // untagged reasoning stripped). Replace what was
                        // streamed with the clean version.
                        if (p && typeof p.content === "string" && p.content !== assistantText) {
                            assistantText = p.content;
                            body.innerHTML = renderMD(assistantText);
                        }
                        // Post-hoc reasoning (from formatters that split
                        // untagged chain-of-thought). Populate the
                        // thinking bubble if reasoning wasn't streamed.
                        if (p && typeof p.reasoning_content === "string" && p.reasoning_content) {
                            if (!reasoningText || reasoningText.trim() === "") {
                                reasoningText = p.reasoning_content;
                            } else {
                                reasoningText += "\n" + p.reasoning_content;
                            }
                            if (thinking.hidden) thinking.hidden = false;
                            thinkingBody.textContent = reasoningText;
                            thinkLabel.textContent = "Thinking";
                        }
                        return;
                    }
                    if (kind === "error") {
                        const msg = (p && p.message) || "stream error";
                        throw new Error(msg);
                    }
                };
                while (true) {
                    const { done, value } = await reader.read();
                    if (done) {
                        flushEvent();
                        break;
                    }
                    buf += decoder.decode(value, { stream: true });
                    const lines = buf.split("\n");
                    buf = lines.pop(); // last partial line stays
                    for (const line of lines) {
                        if (line === "") {
                            flushEvent();
                            continue;
                        }
                        if (line.startsWith("event: ")) {
                            pendingEvent = line.slice(7).trim();
                        } else if (line.startsWith("data: ")) {
                            pendingData += (pendingData ? "\n" : "") + line.slice(6);
                        }
                        // Other SSE fields (id:, retry:) ignored.
                    }
                }
            } catch (e) {
                // User hit stop → keep the partial answer and finalize
                // normally (fall through). Any other error is surfaced.
                if (e.name !== "AbortError") {
                    renderError("Stream error: " + e.message);
                    dropIndicator();
                    thinkLabel.textContent = "Thinking";
                    assistantBubble.classList.remove("chat-msg-streaming");
                    localState.streaming = false;
                    localState.currentAbort = null;
                    setComposerStreaming(false);
                    return;
                }
                aborted = true;
                dropIndicator(); // aborted — keep partial text, fall through
            }

            assistantBubble.classList.remove("chat-msg-streaming");

            // Research note delivered this turn (inline quick/standard
            // path) → append a durable, clickable link to the message and
            // auto-open it in a side pane. The link is appended to
            // assistantText BEFORE the message is persisted below, so it
            // survives reload and re-renders as an anchor that click-
            // delegation opens in-pane (no navigation). The link append is
            // always safe; only the auto-open is skipped when a deep run
            // owns the right pane (the poll loop handles that swap) so the
            // two don't fight over the layout.
            if (researchNote && researchNote.page_slug) {
                // Only append if the model didn't already leave one.
                if (assistantText.indexOf("#note/") === -1) {
                    const href = "#note/" +
                        encodeURIComponent(researchNote.book_slug) + "/" +
                        encodeURIComponent(researchNote.page_slug);
                    // Strip [] from the title so it can't break the link markdown.
                    const label = (researchNote.title || "the note").replace(/[[\]]/g, "");
                    assistantText += "\n\n**[📄 Open " + label + " →](" + href + ")**";
                    body.innerHTML = renderMD(assistantText);
                }
                if (!localState.researchCardShown) openResearchNoteInPane(researchNote);
            }

            // Stopped before any output landed → drop the empty bubble
            // rather than persisting a blank assistant message.
            if (aborted && !assistantText.trim()) {
                if (assistantBubble.parentNode) assistantBubble.remove();
                localState.streaming = false;
                localState.currentAbort = null;
                setComposerStreaming(false);
                input.focus();
                return;
            }

            // Compute timing + tok/s metrics now that the stream is
            // done. Append a single dim line at the END of the
            // thinking body — debug info, only visible when the
            // panel is expanded. Always keep the thinking element
            // around (even if reasoning was empty) so the metrics
            // line is reachable.
            //
            // Model name is omitted post-rearch — it's the single
            // configured chat model on every turn, so showing it
            // every line is just noise.
            //
            // Decode rate: prefer the server's own decode time
            // (decode_ms) over browser wall-clock. For reasoning models
            // the wall-clock measure is badly deflated — the client
            // re-renders the whole markdown on every token, so the
            // browser's first-to-last-token span runs ~2× the real
            // decode time, and the hidden think phase counts against it.
            // decode_ms is the backend's measured generation time over
            // the same (total, incl. reasoning) token count, so
            // output_tokens / decode_ms is the honest rate. Wall-clock
            // is the fallback when the backend didn't report timings.
            // Prefill (input_tokens / ttft) is wall-clock by nature and
            // includes gateway overhead, so it under-reports a bit.
            const totalSec = (performance.now() - tStart) / 1000;
            const ttftSec = tFirstToken != null ? (tFirstToken - tStart) / 1000 : null;
            const decodeSec = (ttftSec != null) ? Math.max(totalSec - ttftSec, 0.001) : null;
            const metricsParts = [];
            if (ttftSec != null) metricsParts.push(ttftSec.toFixed(2) + "s ttft");
            metricsParts.push(totalSec.toFixed(2) + "s total");
            if (inputTokens != null && ttftSec != null && ttftSec > 0) {
                metricsParts.push(Math.round(inputTokens / ttftSec) + " tok/s prefill");
            }
            if (outputTokens != null && decodeMs != null && decodeMs > 0) {
                metricsParts.push(Math.round(outputTokens / (decodeMs / 1000)) + " tok/s decode");
            } else if (outputTokens != null && decodeSec != null) {
                metricsParts.push(Math.round(outputTokens / decodeSec) + " tok/s decode");
            }
            if (memHits != null) metricsParts.push(memHits + " mem hit" + (memHits === 1 ? "" : "s"));
            const metricsLine = metricsParts.join(" · ");
            const metricsEl = document.createElement("div");
            metricsEl.className = "chat-msg-thinking-metrics";
            metricsEl.textContent = metricsLine;
            thinkingBody.appendChild(metricsEl);
            thinking.hidden = false;

            // Turn's done — the answer is on screen and is what the
            // user watches now. Reset the one-line summary to a plain
            // "Thinking" affordance for expanding the trace.
            thinkingSummary.textContent = "Thinking";
            const assistantMsg = {
                role: "assistant",
                content: assistantText,
                model: resolvedModel || "familiar",
                // Persist the reasoning trace as part of the
                // assistant message so reloading the conversation
                // restores it.
                reasoning_content: reasoningText || undefined,
            };
            localState.messages.push({
                ...assistantMsg,
                reasoning_content: reasoningText || null,
            });

            try {
                await apiJSON("/console/api/conversations/" + encodeURIComponent(localState.conversationId) + "/messages", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(assistantMsg),
                });
            } catch (e) {
                // Persistence failed but the user already saw the
                // response — surface a non-fatal warning rather than
                // re-rendering / wiping the bubble.
                console.warn("chat: couldn't persist assistant message", e);
            }

            // Bump the conversation list ordering — the conversation
            // we just appended to is the most recent.
            refreshConversations();

            // First turn done — auto-title the conversation off the
            // opening exchange. Asymmetric: only fires once, on the
            // first turn. Fire-and-forget so it doesn't hold up the
            // input becoming usable again. NOT gated on assistantText:
            // a research kickoff often ends the turn on the spawn tool
            // call with little/no prose, but the user's prompt alone is
            // plenty to title from (and autoTitle falls back to a
            // prompt-derived title if the sidecar returns nothing).
            if (isFirstTurn) {
                // Pass the title as it stands right now as the
                // baseline — autoTitle re-checks it after generating
                // and bails if the user renamed in the meantime.
                autoTitle(localState.conversationId, prompt, assistantText, titleEl.value);
            }

            // Note refresh is now driven by the gateway's
            // __TOOL_EFFECT__:note_changed signal on the reasoning
            // channel (handled inline in the SSE loop above), so this
            // block no longer needs the surface-level tool_calls
            // inspection that the OpenAI shape carried.

            localState.streaming = false;
            localState.currentAbort = null;
            setComposerStreaming(false);
            input.focus();
        }

        // autoTitle generates a 1-3 word title for a freshly-created
        // conversation from its opening exchange, persists it, and
        // updates the UI. Best-effort and non-blocking: any failure
        // leaves the prompt-derived title in place.
        //
        // It will not clobber a title the user renamed in the
        // meantime — the UI update is gated on titleEl still showing
        // the original derived title, and the PATCH is skipped too.
        async function autoTitle(convId, userMsg, assistantMsg, derived) {
            try {
                const resp = await fetch("/api/chat/title", {
                    method: "POST",
                    credentials: "include",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({
                        user_message: userMsg,
                        assistant_message: assistantMsg,
                    }),
                });
                let title = "";
                if (resp.ok) {
                    const data = await resp.json().catch(() => null);
                    title = ((data && data.title) || "").trim();
                } else {
                    console.warn("chat: autotitle endpoint returned HTTP " + resp.status);
                }
                if (!title) {
                    // Sidecar soft-failed (title task down/unconfigured) or
                    // returned nothing — derive from the prompt so the
                    // conversation never stays "Conversation" (a research
                    // kickoff with no AI title is the case that exposed this).
                    title = titleFromPrompt(userMsg);
                }
                if (!title) return;

                // Bail if the user renamed the conversation while the
                // title was generating — their choice wins.
                const stillOnConv = localState.conversationId === convId;
                if (stillOnConv && titleEl.value !== derived) return;

                await apiJSON("/console/api/conversations/" + encodeURIComponent(convId), {
                    method: "PATCH",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ title: title }),
                });
                if (stillOnConv) {
                    titleEl.value = title;
                    if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                        window.FamiliarWorkspace.updateTabTitle(tab.id, title);
                    }
                }
                refreshConversations();
                // Refresh the sidebar rail + home pins so the new title
                // shows everywhere the conversation is listed, not just
                // in this tab.
                window.dispatchEvent(new Event("familiar:sidebarRefresh"));
                window.dispatchEvent(new CustomEvent("familiar:pinsChanged"));
            } catch (e) {
                console.warn("chat: autotitle failed", e);
            }
        }

        function init() {
            input.addEventListener("input", autosizeInput);
            input.addEventListener("keydown", (e) => {
                // Enter sends; Shift+Enter inserts a newline.
                // Cmd/Ctrl+Enter still sends too — muscle memory
                // from the old binding stays intact. IME
                // composition (e.isComposing) suppresses the
                // send so dictation / Asian input methods can
                // commit a candidate without firing the message.
                if (e.key !== "Enter" || e.isComposing) return;
                if (e.shiftKey) return; // Shift+Enter → newline
                e.preventDefault();
                send();
            });
            inputWrap.addEventListener("submit", (e) => {
                e.preventDefault();
                send();
            });
            newBtn.addEventListener("click", newConversation);

            // Rename conversation on title input change (debounced).
            let renameTimer = null;
            titleEl.addEventListener("input", () => {
                if (!localState.conversationId) return;
                // Update tab label live as user types.
                if (window.FamiliarWorkspace && window.FamiliarWorkspace.updateTabTitle) {
                    window.FamiliarWorkspace.updateTabTitle(tab.id, titleEl.value || "Conversation");
                }
                if (renameTimer) clearTimeout(renameTimer);
                renameTimer = setTimeout(async () => {
                    renameTimer = null;
                    const newTitle = titleEl.value.trim();
                    if (!newTitle) return;
                    try {
                        await apiJSON("/console/api/conversations/" + encodeURIComponent(localState.conversationId), {
                            method: "PATCH",
                            headers: { "Content-Type": "application/json" },
                            body: JSON.stringify({ title: newTitle }),
                        });
                        refreshConversations();
                    } catch (e) {
                        console.warn("chat: rename failed", e);
                    }
                }, 500);
            });

            // Refresh sidebar list on blur so renamed titles appear
            // without a page refresh.
            titleEl.addEventListener("blur", () => {
                if (localState.conversationId) {
                    refreshConversations();
                    window.dispatchEvent(new Event("familiar:sidebarRefresh"));
                }
            });

            refreshConversations().then(() => {
                if (localState.conversationId) {
                    loadConversation(localState.conversationId);
                } else {
                    // Fresh tab with no conversation → splash.
                    enterSplash();
                }
            });
        }

        // dropConversation removes a deleted conversation from this
        // shell's list without a network round-trip. Driven by the
        // module-level familiar:conversationDeleted listener so every
        // open chat tab — not just the one that issued the delete —
        // drops the now-dead row immediately. A stale row stays
        // clickable and 404s otherwise. If this shell happens to have
        // the deleted conversation open, it falls back to the splash.
        function dropConversation(id) {
            if (!id) return;
            const before = localState.conversations.length;
            localState.conversations = localState.conversations.filter((c) => c.id !== id);
            if (localState.conversationId === id) {
                enterSplash(); // also clears state + re-renders the list
                return;
            }
            if (localState.conversations.length !== before) renderConvList();
        }

        // Expose loadConversation + newConversation so sidebar
        // child-clicks can drive the shell from outside the
        // closure (Phase 3e openDoc events).
        return { root, init, refreshConversations, loadConversation, newConversation, openShardChat, restoreScroll, enterSplash, dropConversation, stopResearchPoll };
    }

    // ── Helpers ───────────────────────────────────────────────

    // friendlyActivity maps a gateway status line to a short label
    // for the one-line thinking-summary status. Engagement-worthy
    // statuses (tool use, web / memory search) get a friendly label;
    // debug-only statuses (token counts, complexity, rerank stats)
    // and "generating response" — the answer is about to stream and
    // is its own signal — return null and leave the summary alone.
    function friendlyActivity(msg) {
        const m = msg.toLowerCase().trim();
        if (m.startsWith("calling tools:")) {
            const tools = msg.slice(msg.indexOf(":") + 1)
                .split(",").map((s) => s.trim()).filter(Boolean);
            if (tools.includes("web_search")) return "Searching the web";
            if (tools.includes("fetch_page")) return "Reading a page";
            if (tools.some((t) => t.startsWith("search_memory"))) return "Searching memory";
            if (tools.some((t) => t.includes("weather"))) return "Checking the weather";
            if (tools.some((t) => t.includes("news"))) return "Pulling the news";
            // Research kickoff tools — otherwise the generic path below reads
            // "Using spawn research workers". Surface the phase cleanly so the
            // silent kickoff loop shows a live label until the roster card
            // takes over.
            if (tools.some((t) => t.includes("spawn_research"))) return "Spawning research workers";
            if (tools.some((t) => t.includes("research_note"))) return "Writing the note";
            // Skill tools (use_skill / read_skill_file) name the skill only
            // in their arguments, which the status line doesn't carry — so
            // the generic path below would render "Using use skill". Label
            // them cleanly instead.
            if (tools.some((t) => t.includes("skill"))) return "Using a skill";
            if (tools.length === 1) return "Using " + tools[0].replace(/_/g, " ");
            return "Running tools";
        }
        if (m.startsWith("searching memories")) return "Searching memory";
        if (m.startsWith("searched:")) return "Searching the web";
        return null; // debug-only / generating — leave the summary as-is
    }

    // compactTokens renders a running token total compactly: 88000 →
    // "88k", small counts stay exact so the card doesn't read "0k".
    function compactTokens(n) {
        if (n >= 1000) return Math.round(n / 1000) + "k";
        return String(n);
    }

    function titleFromPrompt(prompt) {
        // First line, clamped to 60 chars. Used as the conversation
        // title until the user renames or the model offers a
        // summary.
        const line = (prompt.split("\n", 1)[0] || "").trim();
        if (line.length > 60) return line.slice(0, 57) + "…";
        return line || "New conversation";
    }

    function agoOrDate(iso) {
        if (!iso) return "";
        const d = new Date(iso);
        const diff = Date.now() - d.getTime();
        if (diff < 60_000) return "just now";
        if (diff < 3_600_000) return Math.round(diff / 60_000) + "m ago";
        if (diff < 86_400_000) return Math.round(diff / 3_600_000) + "h ago";
        if (diff < 30 * 86_400_000) return Math.round(diff / 86_400_000) + "d ago";
        return d.toLocaleDateString();
    }

    // ── Register ──────────────────────────────────────────────

    // Wait for workspace.js's IIFE to install its API. Both files
    // are loaded with default async ordering (regular <script>
    // tags), so by the time DOMContentLoaded fires the registry is
    // present. If chat.js loads first for any reason, we retry on
    // a microtask.
    function register() {
        if (window.FamiliarWorkspace && window.FamiliarWorkspace.registerSurfaceRenderer) {
            window.FamiliarWorkspace.registerSurfaceRenderer("chat", render);
        } else {
            setTimeout(register, 0);
        }
    }
    register();

    // Sidebar children dispatch `familiar:openDoc` when a chat
    // child is clicked (Phase 3e). We pluck the most recently-
    // rendered chat shell + load that conversation into it. The
    // sidebar already focused a chat tab via focusSurface() so by
    // the time this event fires there's a live shell to drive.
    window.addEventListener("familiar:openDoc", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "chat") return;
        const drive = (entry) => {
            if (d.shard_id) {
                entry.model.openShardChat(d.shard_id);
            } else if (d.id) {
                entry.model.loadConversation(d.id);
            } else {
                entry.model.newConversation();
            }
        };
        // The workspace names the tab it prepared — drive exactly
        // that shell. The mounted-shell scan below is only a
        // fallback for tabId-less dispatchers; with two panels both
        // showing chat it picks whichever shell registered first,
        // which used to overwrite a visible conversation.
        if (d.tabId) {
            const entry = shells.get(d.tabId);
            if (entry && document.body.contains(entry.root)) {
                drive(entry);
                return;
            }
        }
        for (const [, entry] of shells) {
            if (document.body.contains(entry.root)) {
                drive(entry);
                return;
            }
        }
    });

    // Tab closed (or its splash morphed to another surface) — drop
    // the shell entry so the map doesn't accumulate dead roots.
    // Mirrors the wiki surface's listener.
    window.addEventListener("familiar:tabClosed", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "chat") return;
        const entry = shells.get(d.tabId);
        // Stop the research poller so a closed tab leaves no timer
        // firing against a conversation nobody's looking at.
        if (entry && entry.model.stopResearchPoll) entry.model.stopResearchPoll();
        shells.delete(d.tabId);
    });

    // A chat was deleted (from this or another tab). Drop the row
    // from every open chat shell's conversation list so no stale,
    // 404-on-click entry survives. Iterates ALL shells, including
    // background (unmounted) tabs — their list must be correct when
    // they next render.
    window.addEventListener("familiar:conversationDeleted", (ev) => {
        const id = ev.detail && ev.detail.id;
        if (!id) return;
        for (const [, entry] of shells) {
            if (entry.model.dropConversation) entry.model.dropConversation(id);
        }
    });

    // Sidebar primary nav click → fall back to splash. Mirrors the
    // wiki + notes behavior: clicking "Chat" in the rail returns
    // the user to the landing page regardless of what was open.
    window.addEventListener("familiar:surfaceNavRoot", (ev) => {
        const d = ev.detail || {};
        if (d.surface !== "chat") return;
        if (d.tabId) {
            const entry = shells.get(d.tabId);
            if (entry && entry.model.enterSplash) entry.model.enterSplash();
            return;
        }
        for (const [tabId, entry] of shells) {
            if (document.body.contains(entry.root) && entry.model.enterSplash) {
                entry.model.enterSplash();
                return;
            }
        }
    });
})();
