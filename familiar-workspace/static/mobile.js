/* Familiar Mobile UI — runtime.
   Owns: tab switching (5 screens, only one visible), hash routing
   with optional sub-path (#chat/<id> opens a thread), shard toggle
   click handling, Wiki filter pills, and the chat data flow that
   talks to /console/api/conversations + /api/chat (native protocol). */

(function () {
    'use strict';

    /* -----------------------------------------------------------
       base64url <-> ArrayBuffer — required by the WebAuthn API.
       Same shape as the desktop helpers in app.js so a shared
       refactor is trivial later.
       -----------------------------------------------------------*/

    function b64urlToBuf(s) {
        var padded = s.replace(/-/g, '+').replace(/_/g, '/');
        var pad = (4 - (padded.length % 4)) % 4;
        var raw = atob(padded + '='.repeat(pad));
        var buf = new Uint8Array(raw.length);
        for (var i = 0; i < raw.length; i++) buf[i] = raw.charCodeAt(i);
        return buf.buffer;
    }
    function bufToB64url(buf) {
        var bytes = new Uint8Array(buf);
        var s = '';
        for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
        return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=/g, '');
    }

    var TABS = ['home', 'chat', 'notes', 'wiki', 'scheduled'];

    /* -----------------------------------------------------------
       Markdown deps — same shape as desktop chat.js. Lazily loads
       marked + DOMPurify + highlight.js from CDN once on first
       chat render. Resolves to a renderer (md) → safeHTML, or a
       <pre>-wrapped fallback if any CDN fails.
       -----------------------------------------------------------*/

    var MD_CDN = {
        marked:    '/vendor/marked/marked.min.js',
        dompurify: '/vendor/dompurify/purify.min.js',
        hljsJS:    '/vendor/highlight/core.min.js',
        hljsCSS:   '/vendor/highlight/atom-one-dark.min.css',
    };
    var mdDepsPromise = null;
    function loadScript(src) {
        return new Promise(function (resolve, reject) {
            var s = document.createElement('script');
            s.src = src;
            s.async = true;
            s.onload = function () { resolve(); };
            s.onerror = function () { reject(new Error('script load failed: ' + src)); };
            document.head.appendChild(s);
        });
    }
    function ensureMarkdownDeps() {
        if (mdDepsPromise) return mdDepsPromise;
        mdDepsPromise = (async function () {
            try {
                var css = document.createElement('link');
                css.rel = 'stylesheet';
                css.href = MD_CDN.hljsCSS;
                document.head.appendChild(css);
                await Promise.all([loadScript(MD_CDN.marked), loadScript(MD_CDN.dompurify)]);
                await loadScript(MD_CDN.hljsJS);
                if (window.marked && window.marked.use && window.hljs) {
                    window.marked.use({
                        renderer: {
                            code: function (code, infostring) {
                                var lang = (infostring || '').match(/\S*/)[0];
                                // Inline completed-research card (research-blocks.js).
                                if (lang === 'research-card' && window.familiarResearchCard) {
                                    return window.familiarResearchCard.html(code);
                                }
                                if (lang && window.hljs.getLanguage && window.hljs.getLanguage(lang)) {
                                    try {
                                        return '<pre><code class="hljs language-' + lang + '">'
                                            + window.hljs.highlight(code, { language: lang }).value
                                            + '</code></pre>';
                                    } catch (_) { /* fall through */ }
                                }
                                return '<pre><code class="hljs">' + escapeHTML(code) + '</code></pre>';
                            },
                        },
                    });
                }
                return function (md) {
                    var html = window.marked.parse(md || '');
                    return window.DOMPurify.sanitize(html, { ADD_ATTR: ['class'] });
                };
            } catch (e) {
                console.warn('mobile chat: markdown deps failed, falling back to plain text', e);
                return function (md) {
                    return '<pre class="mob-md-fallback">' + escapeHTML(md || '') + '</pre>';
                };
            }
        })();
        return mdDepsPromise;
    }

    function escapeHTML(s) {
        return String(s == null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

    function relativeTime(iso) {
        if (!iso) return '';
        var t = new Date(iso).getTime();
        if (isNaN(t)) return '';
        var diff = (Date.now() - t) / 1000;
        if (diff < 60) return 'now';
        if (diff < 3600) return Math.floor(diff / 60) + 'm';
        if (diff < 86400) return Math.floor(diff / 3600) + 'h';
        if (diff < 86400 * 7) return Math.floor(diff / 86400) + 'd';
        var d = new Date(t);
        var mons = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'];
        return mons[d.getMonth()] + ' ' + String(d.getDate()).padStart(2, '0');
    }

    // handleUnauthorized routes back to the login view when a session
    // goes away mid-use. Mobile sessions expire while the app is
    // backgrounded far more than desktop, and the old behaviour was to
    // dump a raw "HTTP 401" string into a content slot and wedge —
    // there was no watchdog and no redirect. Idempotent: showView is a
    // no-op when already on login. Wired by Auth at boot.
    var onUnauthorized = null;
    function handleUnauthorized() {
        window.FAMILIAR_SESSION = null;
        if (typeof onUnauthorized === 'function') onUnauthorized();
    }

    async function apiJSON(url, opts) {
        opts = opts || {};
        var controller, timer;
        var signal = opts.signal;
        if (!signal && typeof AbortController !== 'undefined') {
            controller = new AbortController();
            signal = controller.signal;
            timer = setTimeout(function () { controller.abort(); }, opts.timeoutMs || 30000);
        }
        var r;
        try {
            r = await fetch(url, Object.assign({ credentials: 'include' }, opts, { signal: signal }));
        } catch (e) {
            if (e && e.name === 'AbortError') throw new Error('Request timed out — check your connection.');
            throw new Error('Network error — check your connection.');
        } finally {
            if (timer) clearTimeout(timer);
        }
        if (r.status === 401) {
            handleUnauthorized();
            throw new Error('Your session expired. Please sign in again.');
        }
        if (!r.ok) {
            var text = await r.text().catch(function () { return ''; });
            throw new Error('HTTP ' + r.status + (text ? ': ' + text.slice(0, 160) : ''));
        }
        return r.json();
    }

    // startSessionWatchdog probes auth/status when the app regains
    // focus / visibility and on a slow interval while visible, so a
    // session that expired while the phone was backgrounded lands the
    // user on the login view instead of a screen of failing requests.
    // Transient network errors are ignored — only a definitive 401 /
    // unauthenticated answer logs the user out. Started once by startApp.
    var watchdogStarted = false;
    // applyMaintenanceBanner mirrors the desktop banner: a warning strip
    // when the big model is offline and a slower fallback is answering.
    // Fed by /auth/status (boot + 90s watchdog) so it shows and auto-clears
    // live. Rendered as a flex item BELOW each screen's header (before the
    // .mob-scroll body), NOT at the top of the shell — the header owns the
    // iOS safe-area top inset, so a strip beneath it never collides with the
    // status bar / Dynamic Island. One slot per screen; only the active
    // screen's (display:flex) is visible.
    function applyMaintenanceBanner(m) {
        var active = !!(m && m.active);
        // Compact single-line label on mobile — screen space is precious,
        // so we drop the fallback-model detail the desktop banner carries
        // and just show the state with the warning triangle.
        var msg = active ? 'Maintenance Mode' : '';
        var screens = document.querySelectorAll('.mob-screen');
        for (var i = 0; i < screens.length; i++) {
            var screen = screens[i];
            var slot = screen.querySelector('.mob-maint-slot');
            var homeHeader = screen.querySelector(':scope > .mob-home-header');
            if (!active) {
                if (slot) slot.remove();
                if (homeHeader) homeHeader.classList.remove('has-maint');
                continue;
            }
            if (!slot) {
                slot = document.createElement('div');
                slot.className = 'mob-maint-slot';
                var searchPill = homeHeader && homeHeader.querySelector(':scope > .mob-search-pill');
                if (homeHeader && searchPill) {
                    // Home: the search pill lives inside the header, so wedge
                    // the strip between the title row and search (gap split
                    // evenly above/below via CSS), full-bleed across the
                    // header padding.
                    homeHeader.insertBefore(slot, searchPill);
                    homeHeader.classList.add('has-maint');
                } else {
                    // Other screens: strip sits directly below the header.
                    var header = screen.firstElementChild;
                    screen.insertBefore(slot, header ? header.nextElementSibling : screen.firstChild);
                }
            }
            if (slot.getAttribute('data-msg') === msg) continue;
            slot.setAttribute('data-msg', msg);
            slot.innerHTML = '';
            var banner = document.createElement('div');
            banner.className = 'mob-maint-banner';
            var icon = document.createElement('span');
            icon.className = 'mob-maint-icon';
            icon.textContent = '⚠';
            var text = document.createElement('span');
            text.textContent = msg;
            banner.appendChild(icon);
            banner.appendChild(text);
            slot.appendChild(banner);
        }
    }

    function startSessionWatchdog() {
        if (watchdogStarted) return;
        watchdogStarted = true;
        async function probe() {
            if (document.hidden) return;
            try {
                var r = await fetch('/console/api/auth/status', { credentials: 'include' });
                if (r.status === 401) { handleUnauthorized(); return; }
                if (r.ok) {
                    var s = await r.json().catch(function () { return null; });
                    if (s && s.authenticated === false) handleUnauthorized();
                    else if (s) applyMaintenanceBanner(s.maintenance);
                }
            } catch (_) { /* transient — don't log out on a network blip */ }
        }
        document.addEventListener('visibilitychange', function () {
            if (document.hidden) return;
            probe();
            // PWA resume: SSE dropped while backgrounded and missed
            // events aren't replayed, so an open wiki page may be stale.
            // Reconcile it (clean -> pull latest; dirty -> save).
            if (typeof Wiki !== 'undefined' && Wiki.onResume) Wiki.onResume();
        });
        window.addEventListener('focus', probe);
        setInterval(function () { if (!document.hidden) probe(); }, 90000);
    }

    /* -----------------------------------------------------------
       Routing — `route` is what we pass to activate(). Forms:
         "home" | "chat" | "notes" | "wiki" | "shards"
         "chat/<id>"  → chat-thread sub-screen for that id
       -----------------------------------------------------------*/

    function parseRoute(route) {
        var parts = (route || '').split('/');
        return { tab: parts[0] || 'home', detail: parts.slice(1).join('/') };
    }

    // Routes that are valid screen targets even though they
    // aren't bottom-tab entries — overflow sub-screens.
    var SUB_ROUTES = ['account', 'memory'];

    function activate(route) {
        var p = parseRoute(route);
        if (TABS.indexOf(p.tab) < 0 && SUB_ROUTES.indexOf(p.tab) < 0) {
            p = { tab: 'home', detail: '' };
        }

        // chat/<id> and notes/<id> open detail sub-screens; the
        // parent tab in the bottom bar still shows as active so
        // the user knows where they are in the hierarchy.
        // 'account' and 'memory' are home-overflow sub-screens
        // that don't pin a tab — Home stays highlighted in the
        // tab bar so the user maps "back" to Home.
        var screenName = p.tab;
        var activeTab = p.tab;
        if (p.tab === 'chat'  && p.detail) screenName = 'chat-thread';
        if (p.tab === 'notes' && p.detail) screenName = 'notes-detail';
        if (p.tab === 'wiki'  && p.detail) screenName = 'wiki-page';
        if (p.tab === 'scheduled' && p.detail === 'new') screenName = 'scheduled-new';
        else if (p.tab === 'scheduled' && p.detail)      screenName = 'scheduled-detail';
        if (p.tab === 'memory' && p.detail) screenName = 'memory-entity';
        if (p.tab === 'account' || p.tab === 'memory') {
            activeTab = 'home';
        }

        document.querySelectorAll('.mob-screen').forEach(function (el) {
            el.classList.toggle('is-active', el.dataset.screen === screenName);
        });
        document.querySelectorAll('.mob-tab').forEach(function (el) {
            el.classList.toggle('is-active', el.dataset.tab === activeTab);
        });

        // Reset scroll position on top-level tab change. For sub-
        // screens (chat-thread / notes-detail / wiki-page) we leave
        // scrollTop alone so the module owns positioning.
        if (screenName !== 'chat-thread' && screenName !== 'notes-detail' && screenName !== 'wiki-page' && screenName !== 'scheduled-detail') {
            var scroll = document.querySelector('.mob-screen.is-active .mob-scroll');
            if (scroll) scroll.scrollTop = 0;
        }

        // Update hash to match without re-firing hashchange.
        var want = '#' + p.tab + (p.detail ? '/' + p.detail : '');
        if (location.hash !== want) history.replaceState(null, '', want);

        // Wake up screen-specific data loaders.
        if      (p.tab === 'home')                { HomePins.refresh(); HomeRecent.refresh(); }
        else if (p.tab === 'chat'  && !p.detail)  { Chat.refreshList(); }
        else if (screenName === 'chat-thread')    { Chat.openThread(p.detail); }
        else if (p.tab === 'notes' && !p.detail)  { Notes.refreshList(); }
        else if (screenName === 'notes-detail')   { Notes.openNote(p.detail); }
        else if (p.tab === 'wiki'  && !p.detail)  { Wiki.refresh(); }
        else if (screenName === 'wiki-page')      { Wiki.openPage(p.detail); }
        else if (screenName === 'scheduled-new')  { Scheduled.openCreate(); }
        else if (screenName === 'scheduled-detail') { Scheduled.openDetail(p.detail); }
        else if (p.tab === 'scheduled')           { Scheduled.refreshList(); }
        else if (p.tab === 'account')             { Account.refresh(); }
        else if (screenName === 'memory-entity')  { Memory.openEntity(decodeURIComponent(p.detail)); }
        else if (p.tab === 'memory')              { Memory.refresh(); }

        // Match the iOS status-bar background to the active screen's
        // header so the safe-area top inset reads as part of the chrome
        // instead of an unstyled strip. Pulled from the CSS var the
        // header gradients are derived from so the two stay in sync.
        updateThemeColor(screenName);
    }

    // Computed from the header gradient colors in mobile.css so the
    // iOS status-bar background matches the header within ~1 RGB
    // unit. Update both sides together if the gradients change.
    var SCREEN_THEME_COLOR = {
        chat:          '#13221E', // moss
        'chat-thread': '#13221E',
        notes:         '#18142C', // iris
        'notes-detail':'#18142C',
        wiki:          '#171C2B', // slate
        'wiki-page':   '#171C2B',
        scheduled:        '#102420', // teal
        'scheduled-detail':'#102420',
        'scheduled-new':  '#102420',
    };
    function updateThemeColor(screenName) {
        var meta = document.querySelector('meta[name="theme-color"]');
        if (!meta) return;
        meta.setAttribute('content', SCREEN_THEME_COLOR[screenName] || '#0B0B0F');
    }

    function readHashRoute() {
        var h = (location.hash || '').replace(/^#/, '');
        return h || 'home';
    }

    /* -----------------------------------------------------------
       Chat — list, thread, send + SSE streaming. Mirrors the
       desktop chat.js patterns; mobile renders plain text bubbles
       (no markdown for now — can layer that on later).
       -----------------------------------------------------------*/

    var Chat = (function () {
        var state = {
            conversations: [],
            currentId: null,
            currentSessionId: null, // authoritative turn key for server-side stop
            messages: [],
            streaming: false,
            loadedListOnce: false,
            // Research-in-progress poll (RESEARCH-SKILL-SPEC §6.7).
            researchTimer: null,
            researchPollId: null,
            researchCardShown: false,
        };

        function chatGlyphSVG() {
            return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round"><path d="M4 5.5 a2 2 0 0 1 2 -2 h12 a2 2 0 0 1 2 2 v9 a2 2 0 0 1 -2 2 H9 l-4 3.5 V5.5 Z"/></svg>';
        }

        // Stop button on the research strip → cancel the run (delegated
        // once; the strip's innerHTML is rebuilt each poll tick).
        document.addEventListener('click', function (ev) {
            var s = ev.target.closest && ev.target.closest('.mob-research-strip .rc-stop');
            if (!s) return;
            ev.preventDefault();
            if (!state.researchRunId) return;
            apiJSON('/console/api/research/runs/' + encodeURIComponent(state.researchRunId) + '/cancel',
                { method: 'POST' }).catch(function () { /* poll reflects terminal state */ });
        });

        // In-chat note links (research write-ups render `#note/<book>/<page>`)
        // resolve the note by slug and open the note screen, instead of
        // letting the raw hash navigate to an unrecognized route. Delegated
        // once so it covers live-appended and reloaded message links.
        document.addEventListener('click', async function (ev) {
            var a = ev.target.closest && ev.target.closest('a[href^="#note/"]');
            if (!a) return;
            ev.preventDefault();
            var rest = a.getAttribute('href').slice('#note/'.length);
            var slash = rest.indexOf('/');
            if (slash < 0) return;
            var book, page;
            try {
                book = decodeURIComponent(rest.slice(0, slash));
                page = decodeURIComponent(rest.slice(slash + 1));
            } catch (err) {
                return; // malformed percent-escape → inert
            }
            try {
                var np = await apiJSON('/console/api/books/' + encodeURIComponent(book) +
                    '/pages/' + encodeURIComponent(page));
                if (np && np.id) location.hash = 'notes/' + encodeURIComponent(np.id);
            } catch (e) { /* note not resolvable → leave the user in chat */ }
        });

        async function refreshList() {
            var listEl = document.getElementById('mob-chat-list');
            if (!listEl) return;
            // Show loading only on first load to avoid flashes when
            // we re-pull after sending a message.
            if (!state.loadedListOnce) {
                listEl.innerHTML = '<div class="mob-empty">Loading…</div>';
            }
            try {
                var resp = await apiJSON('/console/api/conversations?limit=50');
                state.conversations = (resp && resp.items) || [];
                state.loadedListOnce = true;
                renderList();
            } catch (e) {
                listEl.innerHTML = '<div class="mob-empty">Couldn\'t load chats:<br>' + escapeHTML(e.message) + '</div>';
            }
        }

        function renderList() {
            var listEl = document.getElementById('mob-chat-list');
            if (!listEl) return;
            listEl.innerHTML = '';
            if (state.conversations.length === 0) {
                listEl.innerHTML = '<div class="mob-empty">No conversations yet — tap + to start.</div>';
                return;
            }
            // Group: pinned first, then recent. Pinned flag isn't
            // supported by the API yet; use updated_at order only.
            for (var i = 0; i < state.conversations.length; i++) {
                listEl.appendChild(rowEl(state.conversations[i]));
            }
        }

        function rowEl(c) {
            var row = document.createElement('button');
            row.type = 'button';
            row.className = 'mob-chat-row';
            var title = c.title || 'Untitled';
            var when = relativeTime(c.updated_at || c.created_at);
            var preview = c.last_message ? c.last_message : (c.model || 'familiar');
            row.innerHTML =
                '<div class="avatar"><span>' + chatGlyphSVG() + '</span></div>' +
                '<div class="body">' +
                    '<div class="head"><span class="name">' + escapeHTML(title) + '</span>' +
                        '<span class="when">' + escapeHTML(when) + '</span></div>' +
                    '<div class="preview">' + escapeHTML(preview) + '</div>' +
                '</div>';
            row.addEventListener('click', function () {
                location.hash = 'chat/' + encodeURIComponent(c.id);
            });
            return row;
        }

        async function openThread(id) {
            // "new" is the sentinel for a chat that doesn't exist yet
            // (opened via startNew / the + button). There's nothing to
            // fetch — leave currentId null so the first send creates
            // the conversation, and just show the empty thread. Without
            // this, openThread("new") would GET /conversations/new and
            // the gateway 500s on the non-UUID id.
            if (id === 'new') {
                state.currentId = null;
                state.messages = [];
                // No conversation yet → nothing to poll.
                stopResearchPoll();
                clearResearchStrip();
                var newTitleEl = document.getElementById('mob-thread-title');
                if (newTitleEl) newTitleEl.textContent = 'New chat';
                var newScrollEl = document.getElementById('mob-thread-scroll');
                if (newScrollEl) newScrollEl.innerHTML = '<div class="mob-empty">Send a message to start.</div>';
                return;
            }

            // Scope the research poll to this conversation. Started
            // before the early-return below so re-entering the same
            // thread (after navigating away stopped the poll) resumes it.
            startResearchPoll(id);

            // Same id already loaded? leave the DOM alone so messages
            // stay visible (e.g. when the hash gets re-set after send).
            if (state.currentId === id && state.messages.length > 0) return;

            state.currentId = id;
            state.messages = [];
            var titleEl = document.getElementById('mob-thread-title');
            var scrollEl = document.getElementById('mob-thread-scroll');
            if (titleEl) titleEl.textContent = '…';
            if (scrollEl) scrollEl.innerHTML = '<div class="mob-empty">Loading…</div>';
            try {
                var resp = await apiJSON('/console/api/conversations/' + encodeURIComponent(id));
                var conv = resp && resp.conversation;
                state.messages = (resp && resp.messages) || [];
                if (titleEl) titleEl.textContent = (conv && conv.title) || 'Conversation';
                renderThread();
            } catch (e) {
                if (scrollEl) scrollEl.innerHTML = '<div class="mob-empty">Couldn\'t load:<br>' + escapeHTML(e.message) + '</div>';
            }
        }

        // A persisted message is worth painting only when it's actual
        // conversation, not agentic plumbing. The gateway persists the
        // whole tool loop so a restart can replay it to the model:
        //   • role="tool" — tool-result rows.
        //   • assistant turns that carried ONLY tool calls — these persist
        //     with empty content (the spawn_research_workers kickoff, etc.).
        // Neither is a reply; rendering them leaves bare "Familiar" bubbles
        // with nothing under them. Skip both.
        function isDisplayableMsg(m) {
            if (!m || m.role === 'tool') return false;
            if (m.role === 'assistant' && !String(m.content || '').trim()) return false;
            return true;
        }

        function renderThread() {
            var scroll = document.getElementById('mob-thread-scroll');
            if (!scroll) return;
            scroll.innerHTML = '';
            if (state.messages.length === 0) {
                scroll.innerHTML = '<div class="mob-empty">Send a message to start.</div>';
                return;
            }
            // Kick off markdown deps async; render plain text first
            // for fast paint, then upgrade in place once the renderer
            // is ready. Same pattern as desktop chat.js.
            var visible = state.messages.filter(isDisplayableMsg);
            for (var i = 0; i < visible.length; i++) {
                scroll.appendChild(messageEl(visible[i]));
            }
            scroll.scrollTop = scroll.scrollHeight;
            ensureMarkdownDeps().then(function (renderMD) {
                scroll.innerHTML = '';
                var vis = state.messages.filter(isDisplayableMsg);
                for (var i = 0; i < vis.length; i++) {
                    scroll.appendChild(messageEl(vis[i], renderMD));
                }
                scroll.scrollTop = scroll.scrollHeight;
            });
        }

        // ── Research-in-progress strip (RESEARCH-SKILL-SPEC §6.7) ──
        //
        // Mirrors desktop chat.js. Autonomous deep-research runs
        // execute in the background and append an assistant summary
        // when done. While a run is live we show a compact status
        // strip above the composer, driven ONLY by polling the
        // active-run endpoint — so a reopened thread (or reload)
        // restores it from server state. The endpoint returns a
        // non-terminal run (researching / synthesizing) or, once the
        // run goes terminal, {"run": null}. A null response *after* a
        // strip was showing means the run finished — we refetch the
        // thread (surfacing the delivered summary) and clear the strip.
        var RESEARCH_POLL_MS = 5000;

        function startResearchPoll(id) {
            stopResearchPoll();
            clearResearchStrip(); // drop any strip from a previous thread
            if (!id) return;
            state.researchPollId = id;
            pollResearch(); // immediate tick so a live run shows at once
            state.researchTimer = setInterval(pollResearch, RESEARCH_POLL_MS);
        }

        function stopResearchPoll() {
            if (state.researchTimer) {
                clearInterval(state.researchTimer);
                state.researchTimer = null;
            }
            state.researchPollId = null;
        }

        async function pollResearch() {
            var id = state.researchPollId;
            if (!id) return;
            var run = null;
            try {
                var resp = await apiJSON(
                    '/console/api/research/runs/active?conversation_id=' + encodeURIComponent(id));
                run = resp && resp.run;
            } catch (e) {
                return; // best-effort — retry next tick
            }
            // Thread changed while the request was in flight — bail.
            if (state.researchPollId !== id) return;

            if (run && (run.status === 'researching' || run.status === 'synthesizing')) {
                renderResearchStrip(run);
                state.researchCardShown = true;
                return;
            }
            // run is null / terminal.
            if (state.researchCardShown) {
                // Don't clobber an in-flight stream mid-send; retry next tick.
                if (state.streaming) return;
                state.researchCardShown = false;
                clearResearchStrip();
                if (state.currentId === id) reloadThread(id);
                // Note was written server-side — no note_changed reached
                // the client; refresh the notes list explicitly.
                window.dispatchEvent(new CustomEvent('familiar:notesChanged'));
            } else {
                clearResearchStrip();
            }
        }

        // reloadThread force-refetches the conversation, bypassing
        // openThread's "same id already loaded" short-circuit, so the
        // just-delivered research summary appears.
        async function reloadThread(id) {
            try {
                var resp = await apiJSON('/console/api/conversations/' + encodeURIComponent(id));
                if (state.currentId !== id) return;
                state.messages = (resp && resp.messages) || [];
                renderThread();
            } catch (e) { /* best-effort */ }
        }

        function rcPixels(state) {
            var cls = state === 'done' ? 'rc-px-moss rc-px-fill'
                : state === 'active' ? 'rc-px-anim'
                : state === 'failed' ? 'rc-px-amber'
                : 'rc-px-dim';
            return '<span class="rc-px ' + cls + '" aria-hidden="true">' +
                '<span class="rc-c rc-c1"><i></i></span>' +
                '<span class="rc-c rc-c2"><i></i><i></i></span>' +
                '<span class="rc-c rc-c3"><i></i><i></i><i></i></span></span>';
        }
        function rcWorkerText(state) {
            return state === 'done' ? 'done' : state === 'active' ? 'searching'
                : state === 'failed' ? 'no result' : 'queued';
        }
        function rcDisplayState(w, synth) {
            var s = (w && w.state) || 'queued';
            return synth && s !== 'failed' ? 'done' : s;
        }
        function compactTok(n) { return n >= 1000 ? Math.round(n / 1000) + 'k' : '' + n; }

        function renderResearchStrip(run) {
            var strip = document.getElementById('mob-research-strip');
            if (!strip) return;
            var topic = run.topic || 'your topic';
            var workers = Array.isArray(run.workers) ? run.workers : [];
            var synth = run.status === 'synthesizing';
            var total = workers.length || run.workers_total || 0;
            var dstates = workers.map(function (w) { return rcDisplayState(w, synth); });
            var doneN = dstates.filter(function (s) { return s === 'done'; }).length;
            var failedN = dstates.filter(function (s) { return s === 'failed'; }).length;

            var title = synth ? 'Writing up' : 'Researching';
            var sub = synth
                ? (failedN > 0 ? doneN + ' of ' + total + ' areas gathered · composing the note'
                    : 'composing the note from the evidence')
                : (total ? doneN + ' of ' + total + ' areas in' : 'starting…');

            // Roster model. While researching, one row per fan-out worker.
            // During synthesis, collapse the finished fan-out into a single
            // "Research workers" row (done, green) and add a live "Writing the
            // note" row so the strip keeps an animated indicator through the
            // write-up phase.
            var roster = synth
                ? [
                    // Amber "failed" only when the fan-out came back empty
                    // (doneN 0); green otherwise, with an honest "N/total"
                    // count for partial failures. Never green success on 0/N.
                    { state: (doneN > 0 || total === 0) ? 'done' : 'failed', label: 'Research workers', wtext: failedN > 0 ? doneN + '/' + total : 'complete' },
                    { state: 'active', label: 'Writing the note', wtext: 'composing' }
                ]
                : workers.map(function (w, i) {
                    return {
                        state: dstates[i],
                        label: (w && w.question) || ('Area ' + (i + 1)),
                        wtext: rcWorkerText(dstates[i])
                    };
                });

            // Rebuild only on a structural change; a per-area state change
            // patches just that row, a steady tick patches only counters —
            // so the other rows' pixel pulses never reset.
            var struct = (run.status || '') + '|' + (run.round || 1) + '|' + topic + '|' + total;
            var setText = function (sel, v) { var el = strip.querySelector(sel); if (el) el.textContent = v; };
            if (!strip.hidden && strip.dataset.struct === struct) {
                var rowEls = strip.querySelectorAll('.rc-row');
                for (var r = 0; r < rowEls.length && r < roster.length; r++) {
                    if (rowEls[r].dataset.st !== roster[r].state) {
                        rowEls[r].dataset.st = roster[r].state;
                        rowEls[r].className = 'rc-row rc-' + roster[r].state;
                        rowEls[r].querySelector('.rc-slot').innerHTML = rcPixels(roster[r].state);
                        rowEls[r].querySelector('.rc-wstate').textContent = roster[r].wtext;
                    }
                }
                setText('[data-rc="areas"]', doneN + '/' + (total || '?'));
                setText('[data-rc="tokens"]', compactTok(run.tokens || 0));
                setText('.rc-s', sub);
                return;
            }
            strip.dataset.struct = struct;

            var rows = '';
            for (var i = 0; i < roster.length; i++) {
                var rr = roster[i];
                rows += '<div class="rc-row rc-' + rr.state + '" data-st="' + rr.state + '"><span class="rc-slot">' + rcPixels(rr.state) + '</span>' +
                    '<span class="rc-label">' + escapeHTML(rr.label) + '</span>' +
                    '<span class="rc-wstate">' + escapeHTML(rr.wtext) + '</span></div>';
            }

            var meta = '<span class="rc-m"><span class="rc-k">round</span> <b>' + (run.round || 1) + '</b></span>' +
                '<span class="rc-m rc-m-moss"><span class="rc-k">areas</span> <b data-rc="areas">' + doneN + '/' + (total || '?') + '</b></span>' +
                '<span class="rc-m"><span class="rc-k">tokens</span> <b data-rc="tokens">' + compactTok(run.tokens || 0) + '</b></span>';

            state.researchRunId = run.id || null;
            var stop = '<button class="rc-stop" type="button" title="Stop research" aria-label="Stop research"><span class="mob-stop-oct" aria-hidden="true"></span></button>';
            strip.className = 'mob-research-strip' + (synth ? ' rc-synth' : '');
            strip.innerHTML =
                '<div class="rc-head">' +
                '<div class="rc-title"><div class="rc-t">' + title +
                ' <span class="rc-topic">“' + escapeHTML(topic) + '”</span></div>' +
                '<div class="rc-s">' + escapeHTML(sub) + '</div></div>' + stop + '</div>' +
                (rows ? '<div class="rc-roster">' + rows + '</div>' : '') +
                '<div class="rc-meta">' + meta + '</div>';
            strip.hidden = false;
        }

        function clearResearchStrip() {
            var strip = document.getElementById('mob-research-strip');
            if (strip) { strip.hidden = true; strip.className = 'mob-research-strip'; strip.dataset.struct = ''; strip.innerHTML = ''; }
            state.researchCardShown = false;
        }

        // Resolve the human-readable name for a user-role message.
        // Mirrors desktop chat.js: display_name → email → "User",
        // with the first letter upper-cased. Pulled at render time
        // (rather than at module init) so post-login session updates
        // are reflected without reload.
        function userDisplayName() {
            var sess = window.FAMILIAR_SESSION || {};
            var name = sess.display_name || sess.email || 'User';
            return name.charAt(0).toUpperCase() + name.slice(1);
        }

        function messageEl(m, renderMD) {
            var wrap = document.createElement('div');
            wrap.className = 'mob-msg ' + (m.role === 'user' ? 'is-user' : 'is-assistant');
            var who = document.createElement('div');
            who.className = 'who';
            who.textContent = m.role === 'user' ? userDisplayName() : 'Familiar';
            wrap.appendChild(who);

            if (m.role === 'assistant' && m.reasoning_content) {
                wrap.appendChild(reasoningEl(m.reasoning_content, false));
            }

            var bubble = document.createElement('div');
            bubble.className = 'bubble';
            // renderMD safely sanitizes via DOMPurify; both user and
            // assistant content is markdown — pasted code blocks,
            // checklists, headings — and rendering it as plain text
            // would be hostile.
            if (renderMD) {
                bubble.innerHTML = renderMD(m.content || '');
            } else {
                bubble.textContent = m.content || '';
            }
            wrap.appendChild(bubble);
            return wrap;
        }

        function reasoningEl(initialText, live) {
            var r = document.createElement('div');
            r.className = 'reasoning';
            var head = document.createElement('div');
            head.className = 'head';
            // A dedicated label span so the working indicator (appended by
            // the caller) survives updateReasoning's text updates.
            var label = document.createElement('span');
            label.className = 'mob-think-label';
            head.appendChild(label);
            var body = document.createElement('div');
            body.className = 'body';
            r.append(head, body);
            r.addEventListener('click', function () { r.classList.toggle('is-open'); });
            updateReasoning(r, initialText || '', live);
            return r;
        }

        function updateReasoning(r, text, live) {
            r.querySelector('.body').textContent = text;
            var wc = text ? text.split(/\s+/).filter(Boolean).length : 0;
            var el = r.querySelector('.mob-think-label');
            if (!el) return;
            // Omit the word count while there's no reasoning yet, so a
            // no-reasoning turn reads "Thinking…" rather than "0 words".
            var base = live ? 'Thinking…' : 'Thinking ▸';
            el.textContent = wc > 0 ? base + ' ' + wc + ' word' + (wc === 1 ? '' : 's') : base;
        }

        // While a turn generates, the Send button becomes a small red
        // octagon Stop (tapping it aborts the stream, keeping partial text).
        function setMobStop(btn, on) {
            if (!btn) return;
            if (on) {
                btn.classList.add('is-stop');
                btn.innerHTML = '<span class="mob-stop-oct" aria-hidden="true"></span>';
                btn.setAttribute('aria-label', 'Stop generating');
            } else {
                btn.classList.remove('is-stop');
                btn.textContent = 'Send';
                btn.removeAttribute('aria-label');
            }
            btn.disabled = false;
        }

        function titleFromPrompt(prompt) {
            var t = prompt.trim().split(/\s+/).slice(0, 8).join(' ');
            if (t.length > 60) t = t.slice(0, 60) + '…';
            return t;
        }

        async function send(text) {
            if (state.streaming) return;
            text = (text || '').trim();
            if (!text) return;

            // First send creates the conversation.
            if (!state.currentId) {
                try {
                    var c = await apiJSON('/console/api/conversations', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ title: titleFromPrompt(text), model: 'familiar' }),
                    });
                    state.currentId = c.id;
                    state.conversations.unshift(c);
                    // A first-send-created conversation may host a
                    // research kickoff — poll it like any other.
                    startResearchPoll(c.id);
                    var titleEl = document.getElementById('mob-thread-title');
                    if (titleEl) titleEl.textContent = c.title || 'Conversation';
                    history.replaceState(null, '', '#chat/' + encodeURIComponent(c.id));
                } catch (e) {
                    alert('Couldn\'t create conversation: ' + e.message);
                    return;
                }
            }

            var scroll = document.getElementById('mob-thread-scroll');
            // Wipe any "Send a message to start." placeholder.
            var empty = scroll.querySelector('.mob-empty');
            if (empty) empty.remove();

            // Markdown deps are loaded once and cached; await here
            // so user + assistant bubbles render formatted from the
            // first paint (rather than plain-text-then-upgrade).
            var renderMD = await ensureMarkdownDeps();

            var userMsg = { role: 'user', content: text };
            state.messages.push(userMsg);
            scroll.appendChild(messageEl(userMsg, renderMD));
            scroll.scrollTop = scroll.scrollHeight;

            // Persist the user message before calling the LLM so a
            // refresh during streaming preserves what was sent.
            try {
                await apiJSON('/console/api/conversations/' + encodeURIComponent(state.currentId) + '/messages', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(userMsg),
                });
            } catch (e) {
                console.warn('mobile chat: persist user msg failed', e);
            }

            // Build assistant bubble and stream into it.
            // is-streaming class drives the iris blinking caret
            // tail on .bubble — same affordance as desktop.
            var aWrap = document.createElement('div');
            aWrap.className = 'mob-msg is-assistant is-streaming';
            var aWho = document.createElement('div'); aWho.className = 'who'; aWho.textContent = 'Familiar';
            var aReasoning = reasoningEl('', true);
            // Working indicator on the reasoning head, shown from the start;
            // dropped when answer tokens stream (replaces the blinking caret).
            var thinkPx = document.createElement('span');
            thinkPx.className = 'rc-px rc-px-anim mob-think-px';
            thinkPx.setAttribute('aria-hidden', 'true');
            thinkPx.innerHTML = '<span class="rc-c rc-c1"><i></i></span><span class="rc-c rc-c2"><i></i><i></i></span><span class="rc-c rc-c3"><i></i><i></i><i></i></span>';
            aReasoning.querySelector('.head').appendChild(thinkPx);
            var thinkDropped = false;
            var dropThinkPx = function () { if (thinkDropped) return; thinkDropped = true; if (thinkPx.parentNode) thinkPx.remove(); };
            // Relight the indicator when the model re-enters a working
            // phase (tool call / search / resumed reasoning) so it stays
            // lit through tool execution instead of dropping on the first
            // token — which is often a short tool preamble, not the answer.
            var showThinkPx = function () {
                if (!state.streaming) return;
                thinkDropped = false;
                if (!thinkPx.parentNode) { var h = aReasoning.querySelector('.head'); if (h) h.appendChild(thinkPx); }
            };
            var aBubble = document.createElement('div'); aBubble.className = 'bubble';
            aWrap.append(aWho, aReasoning, aBubble);
            scroll.appendChild(aWrap);
            scroll.scrollTop = scroll.scrollHeight;

            var sendBtn = document.querySelector('.mob-thread-send');
            state.streaming = true;
            var abort = new AbortController();
            state.currentAbort = abort;
            setMobStop(sendBtn, true);

            var assistantText = '';
            var reasoningText = '';
            // Timing + usage instrumentation; same shape as desktop
            // chat.js. Used after the stream ends to compute prefill
            // / decode tok/s and append a debug line at the bottom
            // of the thinking panel.
            var tStart = performance.now();
            var tFirstToken = null;
            var resolvedModel = null;
            var memHits = null;
            var inputTokens = null;
            var outputTokens = null;
            var decodeMs = null;
            // Research note the backend reports this turn wrote (inline
            // path) — appended to the message as a tappable link below.
            var researchNote = null;
            var aborted = false; // user tapped stop
            // CHAT-REARCH §"Phase 0" — native /api/chat protocol.
            // Just send the new user message; gateway has the history.
            try {
                var resp = await fetch('/api/chat', {
                    method: 'POST',
                    credentials: 'include',
                    signal: abort.signal,
                    headers: {
                        'Content-Type': 'application/json',
                        'Accept': 'text/event-stream',
                    },
                    body: JSON.stringify({ message: text }),
                });
                if (!resp.ok || !resp.body) {
                    var errText = await resp.text().catch(function () { return ''; });
                    throw new Error('HTTP ' + resp.status + ': ' + errText.slice(0, 200));
                }
var reader = resp.body.getReader();
                var decoder = new TextDecoder();
                var buf = '';
                var pendingEvent = 'message';
                var pendingData = '';
                var handleEvent = function (kind, payload) {
                    if (kind === 'session') {
                        // Authoritative turn key for a server-side stop — the
                        // mobile session id is derived server-side (the chat
                        // POST sends no conversation_id), so this event is the
                        // only place the client learns it.
                        if (payload && payload.session_id) state.currentSessionId = payload.session_id;
                    } else if (kind === 'token') {
                        var c = (payload && payload.chunk) || '';
                        if (!c) return;
                        if (tFirstToken == null) tFirstToken = performance.now();
                        dropThinkPx(); // real output began — hand motion to the text
                        assistantText += c;
                        aBubble.innerHTML = renderMD(assistantText);
                        scroll.scrollTop = scroll.scrollHeight;
                    } else if (kind === 'reasoning') {
                        var c2 = (payload && payload.chunk) || '';
                        if (!c2) return;
                        // Tool-effect signals ride the reasoning channel
                        // but aren't visible thinking text — they trigger
                        // UI side effects. Strip before display, parity
                        // with desktop chat.js.
                        if (c2.indexOf('__TOOL_EFFECT__:note_changed:') !== -1) {
                            window.dispatchEvent(new CustomEvent('familiar:notesChanged'));
                            c2 = c2.replace(/__TOOL_EFFECT__:note_changed:[^\n]*\n?/g, '');
                            if (!c2) return;
                        }
                        if (tFirstToken == null) tFirstToken = performance.now();
                        reasoningText += c2;
                        aReasoning.style.display = '';
                        updateReasoning(aReasoning, reasoningText, true);
                        showThinkPx(); // reasoning resumed → working
                        scroll.scrollTop = scroll.scrollHeight;
                    } else if (kind === 'status') {
                        var s = (payload && payload.message) || '';
                        if (!s) return;
                        reasoningText += s + (s.charAt(s.length - 1) === '\n' ? '' : '\n');
                        aReasoning.style.display = '';
                        updateReasoning(aReasoning, reasoningText, true);
                        showThinkPx(); // tool/search status → working, relight
                    } else if (kind === 'done') {
                        if (payload && payload.model_id) resolvedModel = payload.model_id;
                        if (payload && payload.research_note && payload.research_note.page_slug) {
                            researchNote = payload.research_note;
                        }
                        if (payload && typeof payload.mem_hits === 'number') memHits = payload.mem_hits;
                        if (payload && typeof payload.input_tokens === 'number') inputTokens = payload.input_tokens;
                        if (payload && typeof payload.output_tokens === 'number') outputTokens = payload.output_tokens;
                        if (payload && typeof payload.decode_ms === 'number') decodeMs = payload.decode_ms;
                    } else if (kind === 'error') {
                        throw new Error((payload && payload.message) || 'stream error');
                    }
                };
                var flushEvent = function () {
                    if (!pendingData) { pendingEvent = 'message'; return; }
                    var p = {};
                    try { p = JSON.parse(pendingData); } catch (_) { /* ignore */ }
                    handleEvent(pendingEvent, p);
                    pendingEvent = 'message';
                    pendingData = '';
                };
                while (true) {
                    var chunk = await reader.read();
                    if (chunk.done) { flushEvent(); break; }
                    buf += decoder.decode(chunk.value, { stream: true });
                    var lines = buf.split('\n');
                    buf = lines.pop();
                    for (var i = 0; i < lines.length; i++) {
                        var line = lines[i];
                        if (line === '') { flushEvent(); continue; }
                        if (line.indexOf('event: ') === 0) {
                            pendingEvent = line.slice(7).trim();
                        } else if (line.indexOf('data: ') === 0) {
                            pendingData += (pendingData ? '\n' : '') + line.slice(6);
                        }
                    }
                }
            } catch (e) {
                // User hit stop → keep the partial answer; other errors show.
                if (e.name === 'AbortError') { aborted = true; }
                else { aBubble.textContent = '⚠ ' + (e.message || e); }
            } finally {
                state.streaming = false;
                state.currentAbort = null;
                state.currentSessionId = null;
                dropThinkPx();
                setMobStop(sendBtn, false);
                aWrap.classList.remove('is-streaming');
            }

            // Stopped before any output → drop the empty bubble instead of
            // persisting a blank assistant message.
            if (aborted && !assistantText.trim()) {
                if (aWrap.parentNode) aWrap.remove();
                return;
            }

            // Compute timing + tok/s metrics and append a single
            // dim line at the END of the thinking body. Always keep
            // the reasoning panel around now (even with empty
            // reasoning text) so the metrics are reachable when the
            // user expands.
            //
            // Model name is omitted post-rearch — it's the single
            // configured chat model on every turn. Prefill / decode
            // tok/s are wall-clock derived; prefill includes
            // gateway-side overhead (classifier, context build) so
            // it under-reports raw model rate.
            var totalSec = (performance.now() - tStart) / 1000;
            var ttftSec = tFirstToken != null ? (tFirstToken - tStart) / 1000 : null;
            var decodeSec = (ttftSec != null) ? Math.max(totalSec - ttftSec, 0.001) : null;
            var parts = [];
            if (ttftSec != null) parts.push(ttftSec.toFixed(2) + 's ttft');
            parts.push(totalSec.toFixed(2) + 's total');
            if (inputTokens != null && ttftSec != null && ttftSec > 0) {
                parts.push(Math.round(inputTokens / ttftSec) + ' tok/s prefill');
            }
            if (outputTokens != null && decodeSec != null) {
                // Use server decode_ms when it's shorter than wall-clock
                // (strips tool overhead). Fall back to wall-clock when
                // decode_ms exceeds it (TTFT overlap on first iteration).
                var effectiveDecodeSec = decodeSec;
                if (decodeMs != null && decodeMs > 0) {
                    var serverSec = decodeMs / 1000;
                    if (serverSec < decodeSec) effectiveDecodeSec = serverSec;
                }
                parts.push(Math.round(outputTokens / effectiveDecodeSec) + ' tok/s decode');
            }
            if (memHits != null) parts.push(memHits + ' mem hit' + (memHits === 1 ? '' : 's'));
            var metricsLine = parts.join(' · ');
            var metricsEl = document.createElement('div');
            metricsEl.className = 'metrics';
            metricsEl.textContent = metricsLine;
            aReasoning.style.display = '';
            aReasoning.querySelector('.body').appendChild(metricsEl);

            if (reasoningText) {
                updateReasoning(aReasoning, reasoningText, false);
                // updateReasoning replaced .body's textContent, which
                // dropped the metrics div — re-attach it now.
                aReasoning.querySelector('.body').appendChild(metricsEl);
            } else {
                // No reasoning text — show "Debug stats" in the head
                // so the user has a clue what the panel holds.
                aReasoning.querySelector('.head').textContent = 'Debug stats';
            }

            // Research note delivered this turn → append a durable,
            // tappable link (mobile is single-pane, so no auto-open —
            // the tap opens the note screen). Appended before persist so
            // it survives reload and re-renders as an anchor that the
            // thread's #note/ tap-delegation opens.
            if (researchNote && researchNote.page_slug && assistantText.indexOf('#note/') === -1) {
                var noteHref = '#note/' +
                    encodeURIComponent(researchNote.book_slug) + '/' +
                    encodeURIComponent(researchNote.page_slug);
                var noteLabel = (researchNote.title || 'the note').replace(/[[\]]/g, '');
                assistantText += '\n\n**[📄 Open ' + noteLabel + ' →](' + noteHref + ')**';
                aBubble.innerHTML = renderMD(assistantText);
            }

            var assistantMsg = {
                role: 'assistant',
                content: assistantText,
                model: resolvedModel || 'familiar',
                reasoning_content: reasoningText || undefined,
            };
            state.messages.push({
                role: 'assistant',
                content: assistantText,
                reasoning_content: reasoningText || null,
            });
            try {
                await apiJSON('/console/api/conversations/' + encodeURIComponent(state.currentId) + '/messages', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(assistantMsg),
                });
            } catch (e) {
                console.warn('mobile chat: persist assistant msg failed', e);
            }

            // Refresh list ordering — the just-touched conversation
            // should bubble to the top next time the list is shown.
            refreshList();
        }

        async function startNew() {
            // Open an empty thread; the first send will create the
            // conversation server-side and patch the hash.
            state.currentId = null;
            state.messages = [];
            var titleEl = document.getElementById('mob-thread-title');
            if (titleEl) titleEl.textContent = 'New chat';
            var scroll = document.getElementById('mob-thread-scroll');
            if (scroll) scroll.innerHTML = '<div class="mob-empty">Send a message to start.</div>';
            location.hash = 'chat/new';
        }

        async function deleteCurrent() {
            if (!state.currentId) return;
            var titleEl = document.getElementById('mob-thread-title');
            var title = (titleEl && titleEl.textContent) || 'this chat';
            if (!confirm('Delete "' + title + '"? This cannot be undone.')) return;
            var id = state.currentId;
            try {
                await apiJSON('/console/api/conversations/' + encodeURIComponent(id), { method: 'DELETE' });
                // Stop polling the now-deleted conversation.
                stopResearchPoll();
                clearResearchStrip();
                state.conversations = state.conversations.filter(function (c) { return c.id !== id; });
                state.currentId = null;
                state.messages = [];
                location.hash = 'chat';
            } catch (e) {
                alert('Couldn\'t delete: ' + (e.message || e));
            }
        }

        function currentPinned() {
            if (!state.currentId) return false;
            var c = state.conversations.find(function (x) { return x.id === state.currentId; });
            return !!(c && c.pinned);
        }

        async function togglePin() {
            if (!state.currentId) return;
            var nextPinned = !currentPinned();
            var id = state.currentId;
            try {
                var c = await apiJSON('/console/api/conversations/' + encodeURIComponent(id), {
                    method: 'PATCH',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ pinned: nextPinned }),
                });
                var idx = state.conversations.findIndex(function (x) { return x.id === id; });
                if (idx >= 0) state.conversations[idx] = c;
                if (window.HomePins && window.HomePins.refresh) window.HomePins.refresh();
            } catch (e) {
                alert('Couldn\'t pin: ' + (e.message || e));
            }
        }

        // Stop cuts generation server-side (freeing the model and
        // committing the partial produced so far) and aborts the local
        // fetch for an instant response / as a fallback when there's no
        // live server turn. Fire-and-forget: a failed stop just leaves the
        // detached turn to finish on its own, same as before.
        function requestServerStop(sessionId) {
            if (!sessionId) return;
            try {
                fetch('/api/chat/stop', {
                    method: 'POST',
                    credentials: 'include',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ session_id: sessionId }),
                }).catch(function () {});
            } catch (_) { /* fetch threw synchronously — ignore */ }
        }
        function stopStream() {
            requestServerStop(state.currentSessionId);
            if (state.currentAbort) state.currentAbort.abort();
        }

        return {
            refreshList: refreshList, openThread: openThread, send: send,
            startNew: startNew, deleteCurrent: deleteCurrent,
            togglePin: togglePin, currentPinned: currentPinned, stop: stopStream,
        };
    })();

    /* -----------------------------------------------------------
       Notes — list, detail, autosave. Mirrors the Chat module
       shape: list view fetches /console/api/books/personal/pages;
       tapping a row routes to #notes/<id> which loads
       /console/api/books/personal/page-by-id/{id}
       and shows the detail sub-screen with title input + textarea
       + 500ms-debounced PATCH on every edit.
       -----------------------------------------------------------*/

    var Notes = (function () {
        var state = {
            list: [],
            currentId: null,
            note: null,        // {id, title, content, ...}
            saveTimer: null,
            saving: false,
            loadedListOnce: false,
            suppressInput: false,  // gate to prevent autosave on programmatic value sets
            tuiEditor: null,       // lazy Toast UI Editor instance
        };

        // Lazy-init the Toast UI editor against #mob-note-body the
        // first time a note is opened. Mirrors the desktop notes
        // surface: WYSIWYG by default, "Markdown / Rich Text" mode
        // switch at the bottom, no toolbar (icon font doesn't load
        // in our embedded context).
        async function mobileNotesNavigate(parsed) {
            var isPersonal = !parsed.bookSlug || parsed.bookSlug.indexOf('personal:') === 0;
            if (isPersonal) {
                try {
                    var p = await apiJSON(
                        '/console/api/books/personal/pages/' +
                        encodeURIComponent(parsed.pageSlug));
                    if (p && p.id) location.hash = 'notes/' + encodeURIComponent(p.id);
                } catch (e) {
                    console.warn('mobile notes: wiki-link target not found', parsed, e);
                }
                return;
            }
            location.hash = 'wiki/' + encodeURIComponent(parsed.pageSlug);
        }

        function ensureEditor() {
            if (state.tuiEditor) return state.tuiEditor;
            if (!window.toastui || !window.toastui.Editor) return null;
            var host = document.getElementById('mob-note-body');
            if (!host) return null;
            state.tuiEditor = new window.toastui.Editor({
                el: host,
                height: '100%',
                initialEditType: 'wysiwyg',
                previewStyle: 'tab',
                theme: 'dark',
                placeholder: 'Start writing…',
                usageStatistics: false,
                hideModeSwitch: false,
                toolbarItems: [],
                widgetRules: window.familiarWikiLink
                    ? [window.familiarWikiLink.makeWidgetRule(mobileNotesNavigate)]
                    : [],
                customHTMLRenderer: window.familiarWikiLink
                    ? window.familiarWikiLink.makeCustomHTMLRenderer()
                    : undefined,
                plugins: window.familiarMermaid && window.familiarMermaid.editorPlugin
                    ? [window.familiarMermaid.editorPlugin]
                    : [],
            });

            if (window.familiarWikiLink) {
                window.familiarWikiLink.wireClickHandler(host, mobileNotesNavigate);
            }
            if (window.familiarMermaid) {
                window.familiarMermaid.observe(host);
            }
            if (window.familiarWikiLink && window.familiarWikiLink.wireImageUpload) {
                window.familiarWikiLink.wireImageUpload(state.tuiEditor, function () {
                    return state.note
                        ? { bookSlug: 'personal', pageId: state.note.id }
                        : null;
                });
            }

            state.tuiEditor.on('change', function () {
                if (state.suppressInput) return;
                scheduleSave();
            });
            // Rename "WYSIWYG" → "Rich Text" in the bottom mode switch.
            var modeTabs = host.querySelectorAll('.toastui-editor-mode-switch .tab-item');
            modeTabs.forEach(function (t) {
                if (t.textContent.trim() === 'WYSIWYG') t.textContent = 'Rich Text';
            });
            return state.tuiEditor;
        }
        function getBodyContent() {
            return state.tuiEditor ? state.tuiEditor.getMarkdown() : '';
        }
        function setBodyContent(md) {
            var ed = ensureEditor();
            if (!ed) return;
            state.suppressInput = true;
            ed.setMarkdown(md || '');
            state.suppressInput = false;
        }

        function noteGlyphSVG() {
            return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round"><path d="M6 3 h8 l5 5 v12 a1.5 1.5 0 0 1 -1.5 1.5 H6 a1.5 1.5 0 0 1 -1.5 -1.5 V4.5 A1.5 1.5 0 0 1 6 3 Z"/><path d="M14 3 v3.5 A1.5 1.5 0 0 0 15.5 8 H19"/></svg>';
        }
        function chevSVG() {
            return '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>';
        }

        async function refreshList() {
            var listEl = document.getElementById('mob-notes-list');
            if (!listEl) return;
            if (!state.loadedListOnce) {
                listEl.innerHTML = '<div class="mob-empty">Loading…</div>';
            }
            try {
                var resp = await apiJSON('/console/api/books/personal/pages');
                state.list = (resp && resp.items) || [];
                state.loadedListOnce = true;
                renderList();
            } catch (e) {
                listEl.innerHTML = '<div class="mob-empty">Couldn\'t load notes:<br>' + escapeHTML(e.message) + '</div>';
            }
        }

        function renderList() {
            var listEl = document.getElementById('mob-notes-list');
            var pinnedEl = document.getElementById('mob-notes-pinned');
            var pinnedHead = document.getElementById('mob-notes-pinned-head');
            var allHead = document.getElementById('mob-notes-all-head');
            if (!listEl) return;
            listEl.innerHTML = '';
            if (pinnedEl) pinnedEl.innerHTML = '';
            if (state.list.length === 0) {
                if (pinnedHead) pinnedHead.hidden = true;
                if (allHead) allHead.hidden = true;
                listEl.innerHTML = '<div class="mob-empty">No notes yet — tap + to start.</div>';
                return;
            }
            var pinned = state.list.filter(function (n) { return n.pinned; });
            var rest = state.list.filter(function (n) { return !n.pinned; });
            // Pinned section above (matches the home Pinned section);
            // the unpinned list keeps the "All" eyebrow only when both
            // sections have content so a pinned-only library doesn't
            // show an empty "All" header.
            if (pinnedHead) pinnedHead.hidden = pinned.length === 0;
            if (allHead) allHead.hidden = !(pinned.length > 0 && rest.length > 0);
            pinned.forEach(function (n) {
                if (pinnedEl) pinnedEl.appendChild(rowEl(n));
            });
            rest.forEach(function (n) {
                listEl.appendChild(rowEl(n));
            });
        }

        function rowEl(n) {
            var row = document.createElement('button');
            row.type = 'button';
            row.className = 'mob-note-row' + (n.pinned ? ' is-pinned' : '');
            var meta = (n.folder || '') + (n.folder ? ' · ' : '') + relativeTime(n.updated_at || n.created_at);
            row.innerHTML =
                '<span class="glyph">' + noteGlyphSVG() + '</span>' +
                '<div class="body">' +
                    '<div class="title">' + escapeHTML(n.title || 'Untitled') + '</div>' +
                    '<div class="meta">' + escapeHTML(meta) + '</div>' +
                '</div>' +
                '<div class="chev">' + chevSVG() + '</div>';
            row.addEventListener('click', function () {
                location.hash = 'notes/' + encodeURIComponent(n.id);
            });
            return row;
        }

        async function openNote(id) {
            // Same id already loaded? Don't re-fetch — preserves cursor
            // position when the hash gets re-set after a save.
            if (state.currentId === id && state.note && state.note.id === id) return;

            // Flush any pending save from the previous note before
            // switching so we don't lose edits to it.
            if (state.saveTimer) {
                clearTimeout(state.saveTimer);
                state.saveTimer = null;
                await flushSave();
            }

            state.currentId = id;
            state.note = null;
            state.saveBlocked = false; // reloading clears any conflict
            var titleInp = document.getElementById('mob-note-title');
            var statusEl = document.getElementById('mob-note-status');
            if (titleInp) { state.suppressInput = true; titleInp.value = ''; titleInp.placeholder = 'Loading…'; state.suppressInput = false; }
            setBodyContent('');
            if (statusEl) statusEl.textContent = '';

            try {
                var n = await apiJSON('/console/api/books/personal/page-by-id/' + encodeURIComponent(id));
                state.note = n;
                if (titleInp) { state.suppressInput = true; titleInp.value = n.title || ''; titleInp.placeholder = 'Untitled'; state.suppressInput = false; }
                setBodyContent(n.content || '');
            } catch (e) {
                if (statusEl) statusEl.textContent = 'load failed';
            }
        }

        function scheduleSave() {
            if (state.suppressInput) return;
            if (!state.note) return;
            var statusEl = document.getElementById('mob-note-status');
            if (statusEl) statusEl.textContent = 'saving…';
            if (state.saveTimer) clearTimeout(state.saveTimer);
            state.saveTimer = setTimeout(function () {
                state.saveTimer = null;
                flushSave();
            }, 500);
        }

        async function flushSave() {
            if (!state.note) return;
            if (state.saving) return;
            if (state.saveBlocked) return; // conflict — wait for reload
            state.saving = true;
            var titleInp = document.getElementById('mob-note-title');
            var statusEl = document.getElementById('mob-note-status');
            var patch = {
                title: (titleInp && titleInp.value) || 'Untitled',
                content: getBodyContent(),
            };
            // If-Match: refuses the write if the server's row has
            // moved since we loaded it. Without this, two devices
            // editing the same note silently overwrite each other.
            var headers = { 'Content-Type': 'application/json' };
            if (state.note.updated_at) headers['If-Match'] = state.note.updated_at;
            try {
                var resp = await fetch('/console/api/books/personal/page-by-id/' +
                    encodeURIComponent(state.note.id), {
                    method: 'PATCH',
                    credentials: 'include',
                    headers: headers,
                    body: JSON.stringify(patch),
                });
                var text = await resp.text();
                var body = null;
                try { body = text ? JSON.parse(text) : null; } catch (e) { /* fall through */ }
                if (resp.status === 409 && body && body.error === 'stale') {
                    // Server has a newer version. If our local content
                    // already matches it, just adopt the timestamp and
                    // continue. Otherwise pause saves so we don't
                    // clobber the upstream edit.
                    var server = body.current || {};
                    if ((server.content || '') === (patch.content || '') &&
                        (server.title || '') === (patch.title || '')) {
                        state.note = server;
                    } else {
                        state.saveBlocked = true;
                        if (statusEl) statusEl.textContent = 'conflict — reload to continue';
                    }
                    return;
                }
                if (!resp.ok) {
                    throw new Error((body && body.error) || ('HTTP ' + resp.status));
                }
                state.note = body;
                if (statusEl) statusEl.textContent = 'saved';
                setTimeout(function () { if (statusEl && statusEl.textContent === 'saved') statusEl.textContent = ''; }, 1200);
                // Bump in list ordering.
                var idx = state.list.findIndex(function (x) { return x.id === body.id; });
                if (idx >= 0) {
                    state.list[idx] = Object.assign({}, state.list[idx], {
                        title: body.title, updated_at: body.updated_at,
                    });
                }
            } catch (e) {
                if (statusEl) statusEl.textContent = 'save failed';
                console.warn('mobile notes: save failed', e);
            } finally {
                state.saving = false;
            }
        }

        async function startNew() {
            // Flush any pending save from the currently-open note.
            if (state.saveTimer) {
                clearTimeout(state.saveTimer);
                state.saveTimer = null;
                await flushSave();
            }
            try {
                var n = await apiJSON('/console/api/books/personal/pages', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ title: '', content: '' }),
                });
                state.list.unshift({
                    id: n.id, title: n.title, folder: n.folder || '',
                    pinned: false, snippet: '', updated_at: n.updated_at,
                });
                location.hash = 'notes/' + encodeURIComponent(n.id);
            } catch (e) {
                alert('Couldn\'t create note: ' + (e.message || e));
            }
        }

        function currentPinned() {
            return !!(state.note && state.note.pinned);
        }

        async function togglePin() {
            if (!state.note) return;
            var nextPinned = !state.note.pinned;
            var id = state.note.id;
            try {
                var n = await apiJSON('/console/api/books/personal/page-by-id/' + encodeURIComponent(id), {
                    method: 'PATCH',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ pinned: nextPinned }),
                });
                state.note = n;
                var idx = state.list.findIndex(function (x) { return x.id === id; });
                if (idx >= 0) state.list[idx] = Object.assign({}, state.list[idx], { pinned: n.pinned });
                if (window.HomePins && window.HomePins.refresh) window.HomePins.refresh();
            } catch (e) {
                alert('Couldn\'t pin: ' + (e.message || e));
            }
        }

        async function deleteCurrent() {
            if (!state.note) return;
            var title = state.note.title || 'this note';
            if (!confirm('Delete "' + title + '"? This cannot be undone.')) return;
            // Suppress any pending autosave so the in-flight PATCH
            // doesn't recreate state after the delete lands.
            if (state.saveTimer) {
                clearTimeout(state.saveTimer);
                state.saveTimer = null;
            }
            var id = state.note.id;
            try {
                await apiJSON('/console/api/books/personal/page-by-id/' + encodeURIComponent(id), { method: 'DELETE' });
                state.list = state.list.filter(function (n) { return n.id !== id; });
                state.note = null;
                state.currentId = null;
                // A deleted note that was pinned would otherwise echo
                // on the Home Pinned section until it next re-fetches.
                if (window.HomePins && window.HomePins.refresh) window.HomePins.refresh();
                location.hash = 'notes';
            } catch (e) {
                alert('Couldn\'t delete: ' + (e.message || e));
            }
        }

        // onPageEvent handles a server-pushed page event. Refreshes
        // the list always (so a remote rename / new note shows up
        // immediately) and reloads the open note's body only when:
        //   • the event is for our open note,
        //   • the event's updated_at is newer than ours (so we
        //     don't echo our own write back), and
        //   • the editor is clean (no pending autosave, no
        //     in-flight save) — if the user is mid-typing the next
        //     save will hit 409 and surface the conflict.
        async function onPageEvent(detail) {
            if (!detail) return;
            refreshList();
            if (!state.note || detail.page_id !== state.note.id) return;
            if (detail.kind === 'page-deleted') {
                state.note = null;
                state.currentId = null;
                location.hash = 'notes';
                return;
            }
            if (detail.kind !== 'page-saved') return;
            var payload = detail.payload || {};
            if (payload.updated_at && state.note.updated_at === payload.updated_at) return;
            if (state.saveTimer || state.saving) return;
            try {
                var fresh = await apiJSON('/console/api/books/personal/page-by-id/' +
                    encodeURIComponent(state.note.id));
                state.note = fresh;
                state.saveBlocked = false;
                var titleInp = document.getElementById('mob-note-title');
                if (titleInp) {
                    state.suppressInput = true;
                    titleInp.value = fresh.title || '';
                    state.suppressInput = false;
                }
                setBodyContent(fresh.content || '');
                if (payload.updated_by) {
                    var statusEl = document.getElementById('mob-note-status');
                    if (statusEl) {
                        statusEl.textContent = 'Synced — ' + payload.updated_by;
                        setTimeout(function () {
                            if (statusEl.textContent.indexOf('Synced') === 0) statusEl.textContent = '';
                        }, 4000);
                    }
                }
            } catch (e) {
                console.debug('mobile notes: refresh failed', e);
            }
        }

        return {
            refreshList: refreshList,
            openNote: openNote,
            startNew: startNew,
            scheduleSave: scheduleSave,
            flushSave: flushSave,
            deleteCurrent: deleteCurrent,
            togglePin: togglePin,
            currentPinned: currentPinned,
            onPageEvent: onPageEvent,
        };
    })();

    /* -----------------------------------------------------------
       Wiki — books + pages (BOOKS-WIKI-ARCHITECTURE Phase 1c).
       Tab Wiki shows a horizontal pill row of books on top + a
       page list below scoped to the selected book. Tap a row →
       #wiki/<pageId> opens the page detail with title input +
       full-bleed textarea + 500ms-debounced autosave. New book
       lives in the list overflow menu; new page in the title-
       header "+" once a book is picked.
       -----------------------------------------------------------*/

    var Wiki = (function () {
        var state = {
            books: [],           // BookSummary[]
            currentBookSlug: null,
            pages: [],           // WikiPageSummary[]
            currentPageSlug: null,
            page: null,          // full WikiPage when a page is open
            saveTimer: null,
            saving: false,
            suppressInput: false,
            tuiEditor: null,     // lazy Toast UI Editor instance
            // Content/title baselines for a REAL dirty check. Seeded
            // (normalized, post-setBodyContent) on load/save/swap so a
            // remote update can tell "user has unsaved edits" from "user
            // is just viewing" — the mobile editor had no such check and
            // silently overwrote unsaved edits on the next SSE event.
            baseContent: '',
            baseTitle: '',
        };

        // seedBase snapshots the editor's normalized content + title as
        // the authoritative baseline. Call AFTER setBodyContent so
        // getBodyContent() reflects Toast UI's round-tripped form (else
        // reformatting reads as a phantom edit).
        function seedBase() {
            state.baseContent = getBodyContent();
            var t = document.getElementById('mob-wiki-page-title');
            state.baseTitle = (t && t.value) || '';
        }
        function isDirty() {
            if (!state.page) return false;
            var t = document.getElementById('mob-wiki-page-title');
            return getBodyContent() !== state.baseContent ||
                   ((t && t.value) || '') !== state.baseTitle;
        }

        async function mobileWikiNavigate(parsed) {
            var bookSlug = parsed.bookSlug || state.currentBookSlug;
            if (bookSlug !== state.currentBookSlug) {
                console.warn('mobile wiki: cross-book wiki-link nav not yet wired', parsed);
                return;
            }
            try {
                var p = await apiJSON(
                    '/console/api/books/' + encodeURIComponent(bookSlug) +
                    '/pages/' + encodeURIComponent(parsed.pageSlug));
                if (p && p.id) location.hash = 'wiki/' + encodeURIComponent(p.id);
            } catch (e) {
                console.warn('mobile wiki: link target not found', parsed, e);
            }
        }

        // Mirrors the Notes module's editor lifecycle.
        function ensureEditor() {
            if (state.tuiEditor) return state.tuiEditor;
            if (!window.toastui || !window.toastui.Editor) return null;
            var host = document.getElementById('mob-wiki-page-body');
            if (!host) return null;
            state.tuiEditor = new window.toastui.Editor({
                el: host,
                height: '100%',
                initialEditType: 'wysiwyg',
                previewStyle: 'tab',
                theme: 'dark',
                placeholder: 'Start writing markdown…',
                usageStatistics: false,
                hideModeSwitch: false,
                toolbarItems: [],
                widgetRules: window.familiarWikiLink
                    ? [window.familiarWikiLink.makeWidgetRule(mobileWikiNavigate)]
                    : [],
                customHTMLRenderer: window.familiarWikiLink
                    ? window.familiarWikiLink.makeCustomHTMLRenderer()
                    : undefined,
                plugins: window.familiarMermaid && window.familiarMermaid.editorPlugin
                    ? [window.familiarMermaid.editorPlugin]
                    : [],
            });
            if (window.familiarWikiLink) {
                window.familiarWikiLink.wireClickHandler(host, mobileWikiNavigate);
            }
            if (window.familiarMermaid) {
                window.familiarMermaid.observe(host);
            }
            if (window.familiarWikiLink && window.familiarWikiLink.wireImageUpload) {
                window.familiarWikiLink.wireImageUpload(state.tuiEditor, function () {
                    return state.page && state.currentBookSlug
                        ? { bookSlug: state.currentBookSlug, pageId: state.page.id }
                        : null;
                });
            }
            state.tuiEditor.on('change', function () {
                if (state.suppressInput) return;
                scheduleSave();
            });
            var modeTabs = host.querySelectorAll('.toastui-editor-mode-switch .tab-item');
            modeTabs.forEach(function (t) {
                if (t.textContent.trim() === 'WYSIWYG') t.textContent = 'Rich Text';
            });
            return state.tuiEditor;
        }
        function getBodyContent() {
            return state.tuiEditor ? state.tuiEditor.getMarkdown() : '';
        }
        function setBodyContent(md) {
            var ed = ensureEditor();
            if (!ed) return;
            state.suppressInput = true;
            ed.setMarkdown(md || '');
            state.suppressInput = false;
        }

        // Caller's role on the active book — "owner" / "writer" /
        // "reader" / "" (no book selected). Resolved off the cached
        // BookSummary list since /books returns role per row.
        function currentRole() {
            if (!state.currentBookSlug) return "";
            var b = state.books.find(function (x) { return x.slug === state.currentBookSlug; });
            return (b && b.role) || "";
        }
        function canWritePages() {
            var r = currentRole();
            return r === "owner" || r === "writer";
        }

        function chevSVG() {
            return '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>';
        }

        async function refresh() {
            // Top-level tab activation. Pull the books list, paint
            // the pill row, then load pages for the active book (or
            // pick the most recent one if there's none yet).
            var listEl = document.getElementById('mob-wiki-pages');
            var newBtn = document.getElementById('mob-wiki-new-page-btn');
            var archiveItem = document.getElementById('mob-wiki-archive-book');
            try {
                var resp = await apiJSON('/console/api/books');
                state.books = (resp && resp.items) || [];
            } catch (e) {
                state.books = [];
            }
            renderBooksRow();
            if (state.books.length === 0) {
                if (listEl) listEl.innerHTML = '<div class="mob-empty">No books yet — open the ⋯ menu to create one.</div>';
                if (newBtn) newBtn.disabled = true;
                if (archiveItem) archiveItem.style.display = 'none';
                state.currentBookSlug = null;
                return;
            }
            // Default to the persisted slug if it's still in the
            // list; otherwise pick the most recently-updated.
            if (!state.currentBookSlug || !state.books.find(function (b) { return b.slug === state.currentBookSlug; })) {
                state.currentBookSlug = state.books[0].slug;
            }
            // Role-gated affordances: only writers + owners can
            // create pages; only owners can archive.
            if (newBtn) newBtn.disabled = !canWritePages();
            if (archiveItem) archiveItem.style.display = currentRole() === 'owner' ? '' : 'none';
            await loadPagesForCurrentBook();
        }

        function bookFolderGlyph() {
            return '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round"><path d="M4 6.5a2 2 0 0 1 2-2h3.4a2 2 0 0 1 1.5.68l1.0 1.14a2 2 0 0 0 1.5.68H18a2 2 0 0 1 2 2v8.6a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2Z"/></svg>';
        }

        // Renders the 2-col book grid above the page list. Each card
        // shows the book name + (live) page count; the active book
        // takes the slate gradient so the selection state is obvious.
        // Mirrors the Notes folder mock in the original design doc.
        function renderBooksRow() {
            var grid = document.getElementById('mob-wiki-books-grid');
            if (!grid) return;
            grid.innerHTML = '';
            if (state.books.length === 0) {
                grid.style.display = 'none';
                return;
            }
            grid.style.display = '';
            state.books.forEach(function (b) {
                var card = document.createElement('button');
                card.type = 'button';
                card.className = 'mob-book-card' + (b.slug === state.currentBookSlug ? ' is-active' : '');
                card.dataset.bookSlug = b.slug;
                var count = typeof b.page_count === 'number' ? b.page_count : 0;
                card.innerHTML =
                    '<div class="name">' + bookFolderGlyph() +
                        '<span>' + escapeHTML(b.name || b.slug) + '</span>' +
                    '</div>' +
                    '<span class="count">' + count + '</span>';
                card.addEventListener('click', function () {
                    if (state.currentBookSlug === b.slug) return;
                    state.currentBookSlug = b.slug;
                    renderBooksRow();
                    var newBtn = document.getElementById('mob-wiki-new-page-btn');
                    var archiveItem = document.getElementById('mob-wiki-archive-book');
                    if (newBtn) newBtn.disabled = !canWritePages();
                    if (archiveItem) archiveItem.style.display = currentRole() === 'owner' ? '' : 'none';
                    loadPagesForCurrentBook();
                });
                grid.appendChild(card);
            });
        }

        function currentPinned() {
            return !!(state.page && state.page.pinned);
        }

        async function togglePin() {
            if (!state.page || !state.currentBookSlug) return;
            var nextPinned = !state.page.pinned;
            try {
                var p = await apiJSON(
                    '/console/api/books/' + encodeURIComponent(state.currentBookSlug) +
                    '/page-by-id/' + encodeURIComponent(state.page.id) + '/pin', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ pinned: nextPinned }),
                });
                state.page = p;
                var idx = state.pages.findIndex(function (x) { return x.id === p.id; });
                if (idx >= 0) state.pages[idx] = Object.assign({}, state.pages[idx], { pinned: p.pinned });
                if (window.HomePins && window.HomePins.refresh) window.HomePins.refresh();
            } catch (e) {
                alert('Couldn\'t pin: ' + (e.message || e));
            }
        }

        async function loadPagesForCurrentBook() {
            var listEl = document.getElementById('mob-wiki-pages');
            if (!listEl || !state.currentBookSlug) return;
            listEl.innerHTML = '<div class="mob-empty">Loading…</div>';
            try {
                var resp = await apiJSON('/console/api/books/' + encodeURIComponent(state.currentBookSlug) + '/pages');
                state.pages = (resp && resp.items) || [];
                renderPages();
            } catch (e) {
                listEl.innerHTML = '<div class="mob-empty">Couldn\'t load pages:<br>' + escapeHTML(e.message) + '</div>';
            }
        }

        function renderPages() {
            var listEl = document.getElementById('mob-wiki-pages');
            if (!listEl) return;
            listEl.innerHTML = '';
            if (state.pages.length === 0) {
                listEl.innerHTML = '<div class="mob-empty">No pages yet — tap + to create one.</div>';
                return;
            }
            // Build a flat list using the same wiki-row layout the
            // mockup sets up; first letter of the title becomes the
            // tile letter (no per-topic color since we don't have
            // categories yet).
            state.pages.forEach(function (p) {
                var row = document.createElement('button');
                row.type = 'button';
                row.className = 'mob-wiki-row';
                var letter = (p.title || '?').trim().charAt(0).toUpperCase() || '?';
                var meta = relativeTime(p.updated_at) + (p.updated_by ? ' · ' + p.updated_by : '');
                row.innerHTML =
                    '<div class="letter">' + escapeHTML(letter) + '</div>' +
                    '<div class="body">' +
                        '<div class="title">' + escapeHTML(p.title || 'Untitled') + '</div>' +
                        '<div class="meta"><span>' + escapeHTML(meta) + '</span></div>' +
                    '</div>' +
                    '<div class="chev">' + chevSVG() + '</div>';
                row.addEventListener('click', function () {
                    location.hash = 'wiki/' + encodeURIComponent(p.id);
                });
                listEl.appendChild(row);
            });
        }

        async function openPage(pageId) {
            // Same id already loaded? skip the re-fetch.
            if (state.page && state.page.id === pageId) return;

            // Flush a pending save from the previously-open page.
            if (state.saveTimer) {
                clearTimeout(state.saveTimer);
                state.saveTimer = null;
                await flushSave();
            }
            state.page = null;
            state.currentPageSlug = null;
            state.saveBlocked = false; // reloading clears any conflict

            // We need bookSlug + pageSlug for the API. The pages list
            // is the source — find the row with this id. If we don't
            // have one cached (e.g. deep-link from a notification),
            // refresh books then re-attempt.
            var rec = state.pages.find(function (p) { return p.id === pageId; });
            if (!rec) {
                await refresh();
                rec = state.pages.find(function (p) { return p.id === pageId; });
                if (!rec) {
                    var titleEl = document.getElementById('mob-wiki-page-title');
                    if (titleEl) titleEl.placeholder = 'Page not found';
                    setBodyContent('');
                    return;
                }
            }
            state.currentPageSlug = rec.slug;

            var titleInp = document.getElementById('mob-wiki-page-title');
            var statusEl = document.getElementById('mob-wiki-page-status');
            if (titleInp) { state.suppressInput = true; titleInp.value = ''; titleInp.placeholder = 'Loading…'; state.suppressInput = false; }
            setBodyContent('');
            if (statusEl) statusEl.textContent = '';

            try {
                var p = await apiJSON('/console/api/books/' + encodeURIComponent(state.currentBookSlug) +
                                      '/pages/' + encodeURIComponent(rec.slug));
                state.page = p;
                if (titleInp) { state.suppressInput = true; titleInp.value = p.title || ''; titleInp.placeholder = 'Untitled'; state.suppressInput = false; }
                setBodyContent(p.content || '');
                seedBase();
                // Readers get a read-only editor — saves would be
                // 403'd by the backend anyway, but the UI shouldn't
                // pretend the field is editable.
                var ro = !canWritePages();
                if (titleInp) titleInp.readOnly = ro;
                if (state.tuiEditor && state.tuiEditor.setMarkdown) {
                    // Toast UI doesn't have a direct read-only toggle,
                    // but we can disable user input by toggling the
                    // contenteditable on the wysiwyg pane.
                    var ww = document.querySelector('#mob-wiki-page-body .toastui-editor-contents');
                    if (ww) ww.setAttribute('contenteditable', ro ? 'false' : 'true');
                }
            } catch (e) {
                if (statusEl) statusEl.textContent = 'load failed';
            }
        }

        function scheduleSave() {
            if (state.suppressInput || !state.page) return;
            var statusEl = document.getElementById('mob-wiki-page-status');
            if (statusEl) statusEl.textContent = 'saving…';
            if (state.saveTimer) clearTimeout(state.saveTimer);
            state.saveTimer = setTimeout(function () { state.saveTimer = null; flushSave(); }, 500);
        }

        async function flushSave(keepalive) {
            if (!state.page || state.saving) return;
            if (state.saveBlocked) return; // conflict — wait for reload
            state.saving = true;
            var titleInp = document.getElementById('mob-wiki-page-title');
            var statusEl = document.getElementById('mob-wiki-page-status');
            var patch = {
                title: (titleInp && titleInp.value) || 'Untitled',
                content: getBodyContent(),
            };
            // If-Match: refuses the write if the server's row has
            // moved since we loaded it. Without this, two devices
            // editing the same page silently overwrite each other.
            var headers = { 'Content-Type': 'application/json' };
            if (state.page.updated_at) headers['If-Match'] = state.page.updated_at;
            try {
                var resp = await fetch('/console/api/books/' +
                    encodeURIComponent(state.currentBookSlug) +
                    '/pages/' + encodeURIComponent(state.currentPageSlug), {
                    method: 'PATCH',
                    credentials: 'include',
                    headers: headers,
                    body: JSON.stringify(patch),
                    keepalive: !!keepalive,
                });
                var text = await resp.text();
                var body = null;
                try { body = text ? JSON.parse(text) : null; } catch (e) { /* fall through */ }
                if (resp.status === 409 && body && body.error === 'stale') {
                    var server = body.current || {};
                    if ((server.content || '') === (patch.content || '') &&
                        (server.title || '') === (patch.title || '')) {
                        state.page = server;
                        state.currentPageSlug = server.slug;
                    } else {
                        state.saveBlocked = true;
                        if (statusEl) statusEl.textContent = 'conflict — reload to continue';
                    }
                    return;
                }
                if (!resp.ok) {
                    throw new Error((body && body.error) || ('HTTP ' + resp.status));
                }
                state.page = body;
                state.currentPageSlug = body.slug;
                // P2-3: the server auto-merged this save against another
                // device's disjoint edit — the response IS the merged
                // document. Reflect it in the editor so both changes are
                // visible, but only if the user hasn't typed further since
                // we sent the save (else we'd clobber in-flight keystrokes;
                // their newer edit merges again on the next flush).
                if (body.merged && getBodyContent() === patch.content) {
                    setBodyContent(body.content || '');
                }
                seedBase(); // editor now matches the saved version
                if (statusEl) {
                    if (body.merged) {
                        statusEl.textContent = 'Merged — ' + (body.updated_by || 'another editor');
                        setTimeout(function () { if (statusEl && statusEl.textContent.indexOf('Merged') === 0) statusEl.textContent = ''; }, 5000);
                    } else {
                        statusEl.textContent = 'saved';
                        setTimeout(function () { if (statusEl && statusEl.textContent === 'saved') statusEl.textContent = ''; }, 1200);
                    }
                }
                // Refresh the list-cache row so back-nav lands on
                // an updated entry without a re-fetch.
                var idx = state.pages.findIndex(function (x) { return x.id === body.id; });
                if (idx >= 0) {
                    state.pages[idx] = Object.assign({}, state.pages[idx], {
                        title: body.title, slug: body.slug, updated_at: body.updated_at,
                    });
                }
            } catch (e) {
                if (statusEl) statusEl.textContent = 'save failed';
                console.warn('mobile wiki: save failed', e);
            } finally {
                state.saving = false;
            }
        }

        async function newBook() {
            var name = prompt('Book name:');
            if (!name || !name.trim()) return;
            try {
                var b = await apiJSON('/console/api/books', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ name: name.trim() }),
                });
                state.books.unshift({
                    id: b.id, slug: b.slug, name: b.name,
                    description: b.description || '', role: 'owner',
                    updated_at: b.updated_at,
                });
                state.currentBookSlug = b.slug;
                state.pages = [];
                renderBooksRow();
                renderPages();
                var newBtn = document.getElementById('mob-wiki-new-page-btn');
                if (newBtn) newBtn.disabled = false;
            } catch (e) {
                alert('Couldn\'t create book: ' + (e.message || e));
            }
        }

        async function newPage() {
            if (!state.currentBookSlug) return;
            try {
                var p = await apiJSON('/console/api/books/' + encodeURIComponent(state.currentBookSlug) + '/pages', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ title: 'Untitled', content: '' }),
                });
                state.pages.unshift({
                    id: p.id, slug: p.slug, title: p.title,
                    snippet: '', updated_at: p.updated_at, updated_by: p.updated_by,
                });
                location.hash = 'wiki/' + encodeURIComponent(p.id);
            } catch (e) {
                alert('Couldn\'t create page: ' + (e.message || e));
            }
        }

        async function archiveBook() {
            if (!state.currentBookSlug) return;
            var book = state.books.find(function (b) { return b.slug === state.currentBookSlug; });
            var name = (book && book.name) || 'this book';
            if (!confirm('Archive "' + name + '"? It hides from your list (recoverable).')) return;
            try {
                await apiJSON('/console/api/books/' + encodeURIComponent(state.currentBookSlug), {
                    method: 'PATCH',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ archive: true }),
                });
                state.books = state.books.filter(function (b) { return b.slug !== state.currentBookSlug; });
                state.currentBookSlug = state.books.length > 0 ? state.books[0].slug : null;
                state.pages = [];
                renderBooksRow();
                if (state.currentBookSlug) {
                    await loadPagesForCurrentBook();
                } else {
                    var listEl = document.getElementById('mob-wiki-pages');
                    if (listEl) listEl.innerHTML = '<div class="mob-empty">No books yet — open the ⋯ menu to create one.</div>';
                    var newBtn = document.getElementById('mob-wiki-new-page-btn');
                    if (newBtn) newBtn.disabled = true;
                }
            } catch (e) {
                alert('Couldn\'t archive: ' + (e.message || e));
            }
        }

        async function deletePage() {
            if (!state.page) return;
            var title = state.page.title || 'this page';
            if (!confirm('Delete "' + title + '"?')) return;
            // Suppress any pending autosave so a stale PATCH doesn't
            // race the DELETE.
            if (state.saveTimer) { clearTimeout(state.saveTimer); state.saveTimer = null; }
            var pageId = state.page.id;
            try {
                await apiJSON('/console/api/books/' + encodeURIComponent(state.currentBookSlug) +
                              '/pages/' + encodeURIComponent(state.currentPageSlug), { method: 'DELETE' });
                state.pages = state.pages.filter(function (x) { return x.id !== pageId; });
                state.page = null;
                state.currentPageSlug = null;
                location.hash = 'wiki';
            } catch (e) {
                alert('Couldn\'t delete: ' + (e.message || e));
            }
        }

        // onPageEvent handles a server-pushed page event for the
        // wiki module — same shape as the Notes module, scoped to
        // state.page / state.currentPageSlug / state.currentBookSlug.
        async function onPageEvent(detail) {
            if (!detail) return;
            if (!state.page || detail.page_id !== state.page.id) return;
            if (detail.kind === 'page-deleted') {
                state.page = null;
                state.currentPageSlug = null;
                location.hash = 'wiki';
                return;
            }
            if (detail.kind !== 'page-saved') return;
            var payload = detail.payload || {};
            if (payload.updated_at && state.page.updated_at === payload.updated_at) return;
            // Never overwrite unsaved edits. The old guard
            // (saveTimer || saving) was false the instant the 500ms
            // debounce fired, so a remote save would silently replace
            // the user's local edits. A real dirty check keeps them.
            if (isDirty()) {
                showRemoteAvailable(payload.updated_by);
                return;
            }
            try {
                var fresh = await apiJSON('/console/api/books/' +
                    encodeURIComponent(state.currentBookSlug) + '/pages/' +
                    encodeURIComponent(state.currentPageSlug));
                // Re-check after the await: the user may have started
                // typing while the fetch was in flight.
                if (isDirty()) { showRemoteAvailable(payload.updated_by); return; }
                state.page = fresh;
                state.currentPageSlug = fresh.slug;
                state.saveBlocked = false;
                var titleInp = document.getElementById('mob-wiki-page-title');
                if (titleInp) {
                    state.suppressInput = true;
                    titleInp.value = fresh.title || '';
                    state.suppressInput = false;
                }
                setBodyContent(fresh.content || '');
                seedBase();
                if (payload.updated_by) {
                    var statusEl = document.getElementById('mob-wiki-page-status');
                    if (statusEl) {
                        statusEl.textContent = 'Synced — ' + payload.updated_by;
                        setTimeout(function () {
                            if (statusEl.textContent.indexOf('Synced') === 0) statusEl.textContent = '';
                        }, 4000);
                    }
                }
            } catch (e) {
                console.debug('mobile wiki: refresh failed', e);
            }
        }

        // showRemoteAvailable tells the user a newer version exists
        // without clobbering their unsaved edits. Saving now auto-merges
        // server-side when the edits are disjoint (the save returns the
        // merged document, flagged Merged); only a same-region conflict
        // falls back to the 409 flow, where a manual reload adopts theirs.
        function showRemoteAvailable(who) {
            var statusEl = document.getElementById('mob-wiki-page-status');
            if (statusEl) {
                statusEl.textContent = who ? ('Updated by ' + who + ' — save or reload') : 'Updated elsewhere — save or reload';
            }
        }

        // onResume reconciles the open page after the PWA comes back to
        // the foreground: the SSE stream drops while backgrounded and
        // missed events aren't replayed, so the editor may be stale.
        // Clean editor -> pull the latest; dirty editor -> try to save
        // (the CAS/conflict flow catches a divergence).
        async function onResume() {
            if (!state.page || !state.currentBookSlug || !state.currentPageSlug) return;
            if (isDirty()) { await flushSave(); return; }
            try {
                var fresh = await apiJSON('/console/api/books/' +
                    encodeURIComponent(state.currentBookSlug) + '/pages/' +
                    encodeURIComponent(state.currentPageSlug));
                if (isDirty()) return; // user started typing during fetch
                if (fresh.updated_at === state.page.updated_at) return;
                state.page = fresh;
                state.currentPageSlug = fresh.slug;
                state.saveBlocked = false;
                var titleInp = document.getElementById('mob-wiki-page-title');
                if (titleInp) { state.suppressInput = true; titleInp.value = fresh.title || ''; state.suppressInput = false; }
                setBodyContent(fresh.content || '');
                seedBase();
            } catch (e) {
                console.debug('mobile wiki: resume refresh failed', e);
            }
        }

        // flushOnUnload fires one last save when the PWA is hidden/
        // closed — but ONLY when there are unsaved edits. The old
        // unconditional flush pushed the (Toast-UI-normalized) body on
        // every app-switch, which the server treated as a real edit and
        // broadcast, 409'ing the OTHER device. keepalive lets the
        // request outlive the page.
        function flushOnUnload() {
            if (!isDirty()) return;
            if (state.saveTimer) { clearTimeout(state.saveTimer); state.saveTimer = null; }
            flushSave(/*keepalive=*/true);
        }

        return {
            refresh: refresh,
            openPage: openPage,
            scheduleSave: scheduleSave,
            flushSave: flushSave,
            newBook: newBook,
            newPage: newPage,
            archiveBook: archiveBook,
            deletePage: deletePage,
            canWritePages: canWritePages,
            currentRole: currentRole,
            togglePin: togglePin,
            currentPinned: currentPinned,
            onPageEvent: onPageEvent,
            onResume: onResume,
            flushOnUnload: flushOnUnload,
        };
    })();

    /* -----------------------------------------------------------
       HomePins — fetches /console/api/home/pins and renders the
       Pinned section on Home. Each kind ("chat" | "note" | "wiki")
       gets its own glyph + tap target that routes to #chat/<id>,
       #notes/<id>, or #wiki/<id>. Hides the section header entirely
       when the list is empty.
       -----------------------------------------------------------*/

    var HomePins = (function () {
        function chatGlyph() {
            return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round"><path d="M4 5.5 a2 2 0 0 1 2 -2 h12 a2 2 0 0 1 2 2 v9 a2 2 0 0 1 -2 2 H9 l-4 3.5 V5.5 Z"/></svg>';
        }
        function noteGlyph() {
            return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round"><path d="M6 3 h8 l5 5 v12 a1.5 1.5 0 0 1 -1.5 1.5 H6 a1.5 1.5 0 0 1 -1.5 -1.5 V4.5 A1.5 1.5 0 0 1 6 3 Z"/><path d="M14 3 v3.5 A1.5 1.5 0 0 0 15.5 8 H19"/></svg>';
        }
        function wikiGlyph() {
            return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round"><path d="M4 4h6v16H4z"/><path d="M10 4h4v16h-4z"/><path d="M16 4h4v16h-4z"/></svg>';
        }
        function chev() {
            return '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>';
        }

        async function refresh() {
            var listEl = document.getElementById('mob-home-pins');
            var headEl = document.getElementById('mob-home-pins-head');
            var countEl = document.getElementById('mob-home-pins-count');
            if (!listEl || !headEl) return;
            try {
                var resp = await apiJSON('/console/api/home/pins');
                var items = (resp && resp.items) || [];
                listEl.innerHTML = '';
                if (items.length === 0) {
                    headEl.hidden = true;
                    return;
                }
                headEl.hidden = false;
                if (countEl) countEl.textContent = String(items.length);
                items.forEach(function (it) { listEl.appendChild(rowEl(it)); });
            } catch (e) {
                // Silent fail — Home should still render the rest.
                console.warn('mobile home: pins fetch failed', e);
                headEl.hidden = true;
            }
        }

        function rowEl(it) {
            var row = document.createElement('button');
            row.type = 'button';
            row.className = 'mob-row';
            var kind = it.kind;
            var tileClass, glyph, route, meta;
            if (kind === 'chat') {
                tileClass = 'is-chat';
                glyph = chatGlyph();
                route = 'chat/';
                meta = 'Chat · ' + relativeTime(it.updated_at);
            } else if (kind === 'wiki') {
                tileClass = 'is-wiki';
                glyph = wikiGlyph();
                route = 'wiki/';
                meta = 'Wiki' + (it.book_name ? ' · ' + it.book_name : '') + ' · ' + relativeTime(it.updated_at);
            } else {
                tileClass = 'is-notes';
                glyph = noteGlyph();
                route = 'notes/';
                meta = 'Note' + (it.folder ? ' · ' + it.folder : '') + ' · ' + relativeTime(it.updated_at);
            }
            row.innerHTML =
                '<div class="icon-tile ' + tileClass + '">' +
                    '<span>' + glyph + '</span>' +
                '</div>' +
                '<div class="body">' +
                    '<div class="title">' + escapeHTML(it.title || 'Untitled') + '</div>' +
                    '<div class="meta">' + escapeHTML(meta) + '</div>' +
                '</div>' +
                '<div class="chev">' + chev() + '</div>';
            row.addEventListener('click', function () {
                location.hash = route + encodeURIComponent(it.id);
            });
            return row;
        }

        return { refresh: refresh };
    })();
    // Expose so Chat/Notes can ping this after pin toggles.
    window.HomePins = HomePins;

    /* -----------------------------------------------------------
       HomeRecent — merges the most recent conversations + notes
       into the "Recent" section on Home. Sorted by updated_at,
       capped at 8 rows. Hides the section header when empty (the
       Pinned section already covers the common case for a fresh
       install, so a blank "Recent" eyebrow would just be visual
       noise).
       -----------------------------------------------------------*/

    var HomeRecent = (function () {
        var RECENT_LIMIT = 8;

        function chatGlyph() {
            return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round"><path d="M4 5.5 a2 2 0 0 1 2 -2 h12 a2 2 0 0 1 2 2 v9 a2 2 0 0 1 -2 2 H9 l-4 3.5 V5.5 Z"/></svg>';
        }
        function noteGlyph() {
            return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round"><path d="M6 3 h8 l5 5 v12 a1.5 1.5 0 0 1 -1.5 1.5 H6 a1.5 1.5 0 0 1 -1.5 -1.5 V4.5 A1.5 1.5 0 0 1 6 3 Z"/><path d="M14 3 v3.5 A1.5 1.5 0 0 0 15.5 8 H19"/></svg>';
        }
        function chev() {
            return '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>';
        }

        function timeKey(it) {
            return Date.parse(it.updated_at || it.created_at || '') || 0;
        }

        async function refresh() {
            var listEl = document.getElementById('mob-home-recent');
            var headEl = document.getElementById('mob-home-recent-head');
            if (!listEl) return;
            var convs = [];
            var notes = [];
            try {
                var c = await apiJSON('/console/api/conversations?limit=' + RECENT_LIMIT);
                convs = ((c && c.items) || []).map(function (x) {
                    return { kind: 'chat', id: x.id, title: x.title || 'Untitled', updated_at: x.updated_at || x.created_at };
                });
            } catch (e) {
                console.warn('mobile home: conversations fetch failed', e);
            }
            try {
                var n = await apiJSON('/console/api/books/personal/pages');
                notes = ((n && n.items) || []).map(function (x) {
                    return { kind: 'note', id: x.id, title: x.title || 'Untitled', updated_at: x.updated_at || x.created_at };
                });
            } catch (e) {
                console.warn('mobile home: notes fetch failed', e);
            }

            var merged = convs.concat(notes).sort(function (a, b) { return timeKey(b) - timeKey(a); }).slice(0, RECENT_LIMIT);
            listEl.innerHTML = '';
            if (merged.length === 0) {
                if (headEl) headEl.hidden = true;
                return;
            }
            if (headEl) headEl.hidden = false;
            merged.forEach(function (it) { listEl.appendChild(rowEl(it)); });
        }

        function rowEl(it) {
            var row = document.createElement('button');
            row.type = 'button';
            row.className = 'mob-row';
            var isChat = it.kind === 'chat';
            var meta = (isChat ? 'Chat' : 'Note') + ' · ' + relativeTime(it.updated_at);
            row.innerHTML =
                '<div class="icon-tile ' + (isChat ? 'is-chat' : 'is-notes') + '">' +
                    '<span>' + (isChat ? chatGlyph() : noteGlyph()) + '</span>' +
                '</div>' +
                '<div class="body">' +
                    '<div class="title">' + escapeHTML(it.title) + '</div>' +
                    '<div class="meta">' + escapeHTML(meta) + '</div>' +
                '</div>' +
                '<div class="chev">' + chev() + '</div>';
            row.addEventListener('click', function () {
                location.hash = (isChat ? 'chat/' : 'notes/') + encodeURIComponent(it.id);
            });
            return row;
        }

        return { refresh: refresh };
    })();
    window.HomeRecent = HomeRecent;

    /* -----------------------------------------------------------
       Shards — list of agent shards from /console/api/shards.
       Read-only for now (tap a row toggles the on-pill optimistically
       and POSTs activate/deactivate, mirroring the desktop pattern;
       the detail screen lands in a follow-up).
       -----------------------------------------------------------*/

    // Shards on mobile are view-and-toggle only; they live in the
    // Account screen as on/off switches. Creating shards and editing
    // their prompt / tools / access is desktop-console work.
    var Shards = (function () {
        var state = { items: [] };

        function shardGlyphSVG() {
            return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2.2 L20.5 9.6 L17.2 21.6 L7.4 19.0 L4.2 12.4 Z"/><path d="M14 2.2 L10.4 11.6 L4.2 12.4"/><path d="M10.4 11.6 L17.2 21.6"/><path d="M10.4 11.6 L20.5 9.6"/></svg>';
        }

        async function refresh() {
            var listEl = document.getElementById('mob-account-shards');
            var countEl = document.getElementById('mob-account-shards-count');
            if (!listEl) return;
            try {
                var resp = await apiJSON('/console/api/shards');
                state.items = (resp && resp.items) || [];
                if (countEl) countEl.textContent = String(state.items.length);
                render();
            } catch (e) {
                if (countEl) countEl.textContent = '';
                listEl.innerHTML = '<div class="mob-empty">Couldn\'t load shards.</div>';
            }
        }

        function render() {
            var listEl = document.getElementById('mob-account-shards');
            if (!listEl) return;
            listEl.innerHTML = '';
            if (state.items.length === 0) {
                listEl.innerHTML = '<div class="mob-empty">No shards yet.</div>';
                return;
            }
            state.items.forEach(function (s) { listEl.appendChild(rowEl(s)); });
        }

        function rowEl(s) {
            var row = document.createElement('div');
            row.className = 'mob-row mob-row-static mob-shard-row';
            var tools = s.tool_allowlist ? s.tool_allowlist.length : 0;
            var desc = (s.persistence ? String(s.persistence).toLowerCase() + ' · ' : '') +
                tools + ' tool' + (tools === 1 ? '' : 's');
            row.innerHTML =
                '<span class="mob-shard-glyph">' + shardGlyphSVG() + '</span>' +
                '<div class="body">' +
                    '<div class="title">' + escapeHTML(s.name || s.id || 'Shard') + '</div>' +
                    '<div class="meta">' + escapeHTML(desc) + '</div>' +
                '</div>' +
                '<button type="button" class="mob-switch' + (s.active ? ' is-on' : '') + '" role="switch"' +
                    ' aria-checked="' + (s.active ? 'true' : 'false') + '"' +
                    ' data-action="toggle-shard" data-id="' + escapeHTML(s.id) + '"></button>';
            return row;
        }

        async function toggle(id, el) {
            if (!id || !el) return;
            var turningOn = !el.classList.contains('is-on');
            // Optimistic flip; revert if the server rejects.
            el.classList.toggle('is-on', turningOn);
            el.setAttribute('aria-checked', turningOn ? 'true' : 'false');
            el.disabled = true;
            try {
                await apiJSON('/console/api/shards/' + encodeURIComponent(id) + '/' +
                    (turningOn ? 'enable' : 'disable'), { method: 'POST' });
                for (var i = 0; i < state.items.length; i++) {
                    if (state.items[i].id === id) { state.items[i].active = turningOn; break; }
                }
            } catch (e) {
                el.classList.toggle('is-on', !turningOn);
                el.setAttribute('aria-checked', !turningOn ? 'true' : 'false');
            } finally {
                el.disabled = false;
            }
        }

        return { refresh: refresh, toggle: toggle };
    })();
    window.Shards = Shards;

    /* -----------------------------------------------------------
       Scheduled — list of scheduled actions, a detail screen
       (run now, edit prompt, past runs, enable/disable/delete),
       and a minimal create form. Mirrors the desktop
       /console/api/actions contract; advanced trigger/target
       options stay on desktop.
       -----------------------------------------------------------*/
    var Scheduled = (function () {
        var TZ_ABBR = {
            'UTC': 'UTC', 'America/New_York': 'ET', 'America/Chicago': 'CT',
            'America/Denver': 'MT', 'America/Los_Angeles': 'PT', 'Europe/Paris': 'CET',
        };
        var state = { items: [], loadedOnce: false, current: null, pollTimer: null };

        function clockSVG() {
            return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 7 v5 l3.5 2"/></svg>';
        }
        function enc(id) { return encodeURIComponent(id); }

        // humanizeCron turns a 5-field crontab into plain English for the
        // common shapes (daily / weekday / weekend / specific day / hourly
        // / monthly). Anything it doesn't recognize falls back to the raw
        // expression so nothing is ever misrepresented.
        var DOW_NAME = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
        function ordinal(n) {
            var s = ['th', 'st', 'nd', 'rd'], v = n % 100;
            return n + (s[(v - 20) % 10] || s[v] || s[0]);
        }
        function clockTime(hh, mm) {
            var H = parseInt(hh, 10), M = parseInt(mm, 10);
            var ap = H < 12 ? 'AM' : 'PM';
            var h12 = H % 12; if (h12 === 0) h12 = 12;
            return h12 + ':' + String(M).padStart(2, '0') + ' ' + ap;
        }
        function humanizeCron(cron) {
            var p = String(cron || '').trim().split(/\s+/);
            if (p.length !== 5) return cron;
            var min = p[0], hr = p[1], dom = p[2], mon = p[3], dow = p[4];
            var isNum = function (x) { return /^\d+$/.test(x); };
            // Hourly (minute fixed, every hour).
            if (isNum(min) && hr === '*' && dom === '*' && mon === '*' && dow === '*') {
                return parseInt(min, 10) === 0 ? 'Hourly' : 'Hourly at :' + String(parseInt(min, 10)).padStart(2, '0');
            }
            // Everything below needs a concrete time of day.
            if (!(isNum(min) && isNum(hr))) return cron;
            var t = clockTime(hr, min);
            if (mon === '*' && dom === '*') {
                if (dow === '*') return 'Daily at ' + t;
                if (dow === '1-5') return 'Weekdays at ' + t;
                if (dow === '0,6' || dow === '6,0' || dow === '0,7') return 'Weekends at ' + t;
                if (isNum(dow)) return DOW_NAME[parseInt(dow, 10) % 7] + 's at ' + t;
            }
            if (dow === '*' && mon === '*' && isNum(dom)) {
                return 'Monthly on the ' + ordinal(parseInt(dom, 10)) + ' at ' + t;
            }
            return cron;
        }

        function scheduleSummary(a) {
            if (a.trigger_kind === 'page_saved') return 'on page save';
            if (a.trigger_kind === 'webhook') return 'webhook';
            if (a.run_at) return 'one-shot';
            if (a.cron) {
                var tz = (a.timezone && a.timezone !== 'UTC')
                    ? ' · ' + (TZ_ABBR[a.timezone] || a.timezone) : '';
                return humanizeCron(a.cron) + tz;
            }
            return '';
        }
        function statusPill(status) {
            var s = (status || '').toLowerCase();
            var cls = 'skip', label = status || '—';
            if (s === 'ok') { cls = 'ok'; label = 'ok'; }
            else if (s === 'error' || s === 'timeout') { cls = 'err'; label = s; }
            else if (s === 'running') { cls = 'run'; label = 'running'; }
            else if (!s) { cls = 'off'; label = '—'; }
            return '<span class="mob-sa-status ' + cls + '">' + escapeHTML(label) + '</span>';
        }
        function stopPoll() { if (state.pollTimer) { clearTimeout(state.pollTimer); state.pollTimer = null; } }

        // ---- list ----
        async function refreshList() {
            stopPoll();
            var listEl = document.getElementById('mob-sa-list');
            if (!listEl) return;
            if (!state.loadedOnce) listEl.innerHTML = '<div class="mob-empty">Loading…</div>';
            try {
                var resp = await apiJSON('/console/api/actions');
                state.items = (resp && resp.items) || [];
                state.loadedOnce = true;
                renderList();
            } catch (e) {
                listEl.innerHTML = '<div class="mob-empty">Couldn\'t load actions:<br>' + escapeHTML(e.message) + '</div>';
            }
        }
        function renderList() {
            var listEl = document.getElementById('mob-sa-list');
            if (!listEl) return;
            listEl.innerHTML = '';
            if (state.items.length === 0) {
                listEl.innerHTML = '<div class="mob-empty">No scheduled actions yet.<br>Tap + to create one.</div>';
                return;
            }
            state.items.forEach(function (a) { listEl.appendChild(listCard(a)); });
        }
        function listCard(a) {
            var card = document.createElement('button');
            card.type = 'button';
            card.className = 'mob-sa-card' + (a.enabled === false ? ' is-off' : '');
            card.setAttribute('data-action', 'open-action');
            card.setAttribute('data-id', a.id);
            var tail = (a.enabled === false) ? '<span class="mob-sa-status off">off</span>' : statusPill(a.last_status);
            card.innerHTML =
                '<span class="mob-sa-glyph">' + clockSVG() + '</span>' +
                '<div class="body">' +
                    '<div class="t">' + escapeHTML(a.name || 'Action') + '</div>' +
                    '<div class="m">' + escapeHTML(scheduleSummary(a)) + '</div>' +
                '</div>' + tail;
            return card;
        }

        // ---- detail ----
        async function openDetail(id) {
            stopPoll();
            var body = document.getElementById('mob-sa-detail-body');
            var titleEl = document.getElementById('mob-sa-detail-title');
            if (!body) return;
            body.innerHTML = '<div class="mob-empty">Loading…</div>';
            if (titleEl) titleEl.textContent = '…';
            try {
                var a = await apiJSON('/console/api/actions/' + enc(id));
                state.current = a;
                if (titleEl) titleEl.textContent = a.name || 'Action';
                renderDetail(a);
                loadRuns(id);
            } catch (e) {
                body.innerHTML = '<div class="mob-empty">Couldn\'t load action.</div>';
            }
        }
        function renderDetail(a) {
            var body = document.getElementById('mob-sa-detail-body');
            if (!body) return;
            var last = a.last_status
                ? (a.last_status + (a.last_run_at ? ' · ' + relativeTime(a.last_run_at) : ''))
                : 'never run';
            body.innerHTML =
                '<button type="button" class="mob-sa-run" data-action="run-action">' +
                    '<span class="mob-sa-run-ico"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round"><path d="M7 5 L19 12 L7 19 Z"/></svg></span>' +
                    '<span id="mob-sa-run-label">Run now</span>' +
                '</button>' +
                '<div class="mob-sa-meta">' +
                    '<div class="mob-sa-chip"><div class="k">Last run</div><div class="v">' + escapeHTML(last) + '</div></div>' +
                    '<div class="mob-sa-chip"><div class="k">Schedule</div><div class="v">' + escapeHTML(scheduleSummary(a) || '—') + '</div></div>' +
                '</div>' +
                '<div class="mob-sa-flabel">Prompt</div>' +
                '<textarea class="mob-sa-textarea" id="mob-sa-edit-prompt" rows="6"></textarea>' +
                '<div class="mob-sa-save-row"><button type="button" class="mob-sa-savebtn" data-action="save-action-prompt">Save prompt</button><span class="mob-sa-save-hint" id="mob-sa-save-hint"></span></div>' +
                '<div class="mob-sa-sechead">Past runs</div>' +
                '<div id="mob-sa-runs"><div class="mob-empty">Loading runs…</div></div>';
            // Set textarea value via property to avoid HTML-escaping pitfalls.
            var ta = document.getElementById('mob-sa-edit-prompt');
            if (ta) ta.value = a.prompt || '';
        }
        async function loadRuns(id) {
            var host = document.getElementById('mob-sa-runs');
            if (!host) return;
            try {
                var resp = await apiJSON('/console/api/actions/' + enc(id) + '/runs');
                renderRuns(host, (resp && resp.items) || []);
            } catch (e) {
                host.innerHTML = '<div class="mob-empty">Couldn\'t load runs.</div>';
            }
        }
        function renderRuns(host, runs) {
            if (!host) return;
            host.innerHTML = '';
            if (runs.length === 0) { host.innerHTML = '<div class="mob-empty">No runs yet.</div>'; return; }
            runs.forEach(function (r) {
                var row = document.createElement('div');
                row.className = 'mob-sa-run-row';
                var when = relativeTime(r.started_at || r.created_at) || '—';
                var dur = (r.duration_ms != null)
                    ? (r.duration_ms < 1000 ? r.duration_ms + 'ms' : (r.duration_ms / 1000).toFixed(1) + 's')
                    : '';
                var sub = [r.trigger || '', dur, r.model_id || ''].filter(Boolean).join(' · ');
                var out = r.error || r.output || '';
                row.innerHTML =
                    '<div class="col">' +
                        '<div class="when">' + escapeHTML(when) + '</div>' +
                        (sub ? '<div class="sub">' + escapeHTML(sub) + '</div>' : '') +
                        (out ? '<div class="out">' + escapeHTML(String(out).slice(0, 160)) + '</div>' : '') +
                    '</div>' + statusPill(r.status);
                host.appendChild(row);
            });
        }

        function resetRunBtn() {
            var label = document.getElementById('mob-sa-run-label');
            if (label) label.textContent = 'Run now';
            var btn = label ? label.closest('.mob-sa-run') : null;
            if (btn) btn.classList.remove('is-busy');
        }
        async function runNow() {
            if (!state.current) return;
            var id = state.current.id;
            var label = document.getElementById('mob-sa-run-label');
            var btn = label ? label.closest('.mob-sa-run') : null;
            if (btn) btn.classList.add('is-busy');
            if (label) label.textContent = 'Running…';
            try {
                await apiJSON('/console/api/actions/' + enc(id) + '/run', { method: 'POST' });
                loadRuns(id);
                pollRuns(id, 0);
            } catch (e) {
                if (label) label.textContent = 'Run failed';
                if (btn) btn.classList.remove('is-busy');
            }
        }
        function pollRuns(id, n) {
            stopPoll();
            state.pollTimer = setTimeout(function () {
                apiJSON('/console/api/actions/' + enc(id) + '/runs').then(function (resp) {
                    var items = (resp && resp.items) || [];
                    renderRuns(document.getElementById('mob-sa-runs'), items);
                    var anyRunning = false;
                    for (var i = 0; i < items.length; i++) {
                        if ((items[i].status || '') === 'running') { anyRunning = true; break; }
                    }
                    if (!anyRunning || n > 80) { resetRunBtn(); refreshListSilently(); }
                    else pollRuns(id, n + 1);
                }).catch(function () { resetRunBtn(); });
            }, 1500);
        }
        // Re-pull the list in the background so its status pills update,
        // without clobbering a list the user may be scrolled into.
        function refreshListSilently() {
            apiJSON('/console/api/actions').then(function (resp) {
                state.items = (resp && resp.items) || [];
                if (document.querySelector('.mob-screen[data-screen="scheduled"].is-active')) renderList();
            }).catch(function () {});
        }

        async function savePrompt() {
            if (!state.current) return;
            var ta = document.getElementById('mob-sa-edit-prompt');
            var hint = document.getElementById('mob-sa-save-hint');
            if (!ta) return;
            var val = ta.value.trim();
            if (!val) { if (hint) hint.textContent = 'Prompt can’t be empty'; return; }
            if (hint) hint.textContent = 'Saving…';
            try {
                var updated = await apiJSON('/console/api/actions/' + enc(state.current.id), {
                    method: 'PATCH', headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ prompt: val }),
                });
                state.current = updated;
                if (hint) hint.textContent = 'Saved';
                setTimeout(function () { var h = document.getElementById('mob-sa-save-hint'); if (h) h.textContent = ''; }, 1500);
            } catch (e) {
                if (hint) hint.textContent = 'Save failed';
            }
        }

        function enableLabel() {
            var item = document.getElementById('mob-sa-enable-item');
            if (item && state.current) item.textContent = (state.current.enabled === false) ? 'Enable' : 'Disable';
        }
        async function toggleEnable() {
            if (!state.current) return;
            var id = state.current.id;
            var turnOff = state.current.enabled !== false;
            try {
                var updated = await apiJSON('/console/api/actions/' + enc(id) + '/' +
                    (turnOff ? 'disable' : 'enable'), { method: 'POST' });
                state.current = updated;
                enableLabel();
                refreshListSilently();
            } catch (e) { /* ignore */ }
        }
        async function deleteCurrent() {
            if (!state.current) return;
            if (!window.confirm('Delete this scheduled action?')) return;
            try {
                await apiJSON('/console/api/actions/' + enc(state.current.id), { method: 'DELETE' });
                state.current = null;
                state.loadedOnce = false;
                location.hash = 'scheduled';
            } catch (e) { /* ignore */ }
        }

        // ---- create ----
        function openCreate() {
            stopPoll();
            var f = document.getElementById('mob-sa-form');
            if (f) f.reset();
            var cron = document.getElementById('mob-sa-cron');
            if (cron) cron.value = '0 7 * * *';
            var err = document.getElementById('mob-sa-form-error');
            if (err) { err.hidden = true; err.textContent = ''; }
        }
        async function submitCreate() {
            var err = document.getElementById('mob-sa-form-error');
            var btn = document.getElementById('mob-sa-submit');
            function fail(m) { if (err) { err.hidden = false; err.textContent = m; } }
            var name = (document.getElementById('mob-sa-name') || {}).value || '';
            var prompt = (document.getElementById('mob-sa-prompt') || {}).value || '';
            var cron = (document.getElementById('mob-sa-cron') || {}).value || '';
            var tz = (document.getElementById('mob-sa-timezone') || {}).value || 'UTC';
            name = name.trim(); prompt = prompt.trim(); cron = cron.trim();
            if (!name) return fail('Name is required.');
            if (!prompt) return fail('Prompt is required.');
            if (!cron) return fail('Schedule (cron) is required.');
            if (err) err.hidden = true;
            if (btn) btn.disabled = true;
            try {
                var created = await apiJSON('/console/api/actions', {
                    method: 'POST', headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        name: name, prompt: prompt, cron: cron, timezone: tz,
                        report_targets: [{ kind: 'conversation' }],
                    }),
                });
                state.loadedOnce = false;
                location.hash = 'scheduled/' + enc(created.id);
            } catch (e) {
                fail((e && e.message) || 'Could not create action.');
            } finally {
                if (btn) btn.disabled = false;
            }
        }

        return {
            refreshList: refreshList, openDetail: openDetail, openCreate: openCreate,
            runNow: runNow, savePrompt: savePrompt, toggleEnable: toggleEnable,
            deleteCurrent: deleteCurrent, submitCreate: submitCreate, enableLabel: enableLabel,
        };
    })();
    window.Scheduled = Scheduled;

    /* -----------------------------------------------------------
       Account — sub-screen reachable from the home overflow menu.
       Renders the active session's display name + email + role
       (already cached on window.FAMILIAR_SESSION by Auth.boot)
       plus a count of recent chat sessions from /console/api/
       sessions. Sign-out below routes through Auth.logout().
       -----------------------------------------------------------*/

    /* -----------------------------------------------------------
       Push — Web Push subscription for PWA notifications. iOS only
       fires push for a home-screen-installed PWA (standalone), so the
       toggle gates on display-mode and tells the user to install first
       otherwise. The subscription is tied to the service-worker
       registration; the SW's `push` handler shows the notification.
       -----------------------------------------------------------*/
    var Push = (function () {
        function supported() {
            return 'serviceWorker' in navigator &&
                'PushManager' in window &&
                typeof Notification !== 'undefined';
        }
        function standalone() {
            return (window.matchMedia && window.matchMedia('(display-mode: standalone)').matches) ||
                window.navigator.standalone === true;
        }
        function urlB64ToUint8Array(b64) {
            var pad = '='.repeat((4 - (b64.length % 4)) % 4);
            var base = (b64 + pad).replace(/-/g, '+').replace(/_/g, '/');
            var raw = atob(base);
            var out = new Uint8Array(raw.length);
            for (var i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
            return out;
        }
        async function currentSubscription() {
            if (!supported()) return null;
            var reg = await navigator.serviceWorker.ready;
            return reg.pushManager.getSubscription();
        }
        async function enable() {
            if (!supported()) throw new Error('Notifications aren’t supported on this browser.');
            if (!standalone()) {
                throw new Error('Add Familiar to your home screen first, then enable notifications from the installed app.');
            }
            var perm = await Notification.requestPermission();
            if (perm !== 'granted') throw new Error('Notification permission was denied.');
            var key = (await apiJSON('/console/api/push/key')).public_key;
            var reg = await navigator.serviceWorker.ready;
            var sub = await reg.pushManager.subscribe({
                userVisibleOnly: true,
                applicationServerKey: urlB64ToUint8Array(key),
            });
            var j = sub.toJSON();
            await apiJSON('/console/api/push/subscribe', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ endpoint: j.endpoint, keys: j.keys }),
            });
        }
        async function disable() {
            var sub = await currentSubscription();
            if (!sub) return;
            try {
                await apiJSON('/console/api/push/subscribe', {
                    method: 'DELETE',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ endpoint: sub.endpoint }),
                });
            } catch (_) { /* drop the local sub regardless */ }
            await sub.unsubscribe();
        }
        // render updates the Account screen's toggle + meta line.
        async function render() {
            var meta = document.getElementById('mob-push-meta');
            var btn = document.getElementById('mob-push-toggle');
            if (!meta || !btn) return;
            if (!supported()) {
                meta.textContent = 'Not supported on this browser';
                btn.hidden = true;
                return;
            }
            // Server-side push must be configured (VAPID keys).
            try {
                await apiJSON('/console/api/push/key');
            } catch (e) {
                meta.textContent = 'Not available on this server';
                btn.hidden = true;
                return;
            }
            if (!standalone()) {
                meta.textContent = 'Add Familiar to your home screen to enable';
                btn.hidden = true;
                return;
            }
            var sub = await currentSubscription();
            if (sub) {
                meta.textContent = 'On — scheduled actions can notify this device';
                btn.textContent = 'Disable';
                btn.dataset.on = '1';
            } else {
                meta.textContent = 'Off';
                btn.textContent = 'Enable';
                btn.dataset.on = '';
            }
            btn.hidden = false;
        }
        async function toggle() {
            var btn = document.getElementById('mob-push-toggle');
            var meta = document.getElementById('mob-push-meta');
            if (!btn) return;
            btn.disabled = true;
            try {
                if (btn.dataset.on === '1') { await disable(); }
                else { await enable(); }
            } catch (e) {
                if (meta) meta.textContent = e.message || String(e);
            } finally {
                btn.disabled = false;
                render();
            }
        }
        return { render: render, toggle: toggle };
    })();

    var Account = (function () {
        function initials(name) {
            var trimmed = String(name || '').trim();
            if (!trimmed) return '?';
            var parts = trimmed.split(/\s+/).filter(Boolean).slice(0, 2);
            var out = parts.map(function (p) { return p.charAt(0).toUpperCase(); }).join('');
            return out || '?';
        }

        async function refresh() {
            var sess = window.FAMILIAR_SESSION || {};
            var name = sess.display_name || sess.user || sess.email || 'Signed in';
            var email = sess.email || '';
            var role = sess.role ? String(sess.role).toUpperCase() : '';

            var av = document.getElementById('mob-account-avatar');
            var nm = document.getElementById('mob-account-name');
            var em = document.getElementById('mob-account-email');
            var rl = document.getElementById('mob-account-role');
            if (av) av.textContent = initials(name);
            if (nm) nm.textContent = name;
            if (em) em.textContent = email;
            if (rl) rl.textContent = role;

            Push.render();
            Shards.refresh();
        }

        return { refresh: refresh };
    })();

    /* -----------------------------------------------------------
       Memory — sub-screen reachable from the home overflow menu.
       Real numbers only: store totals from the dashboard overview
       plus the top entities by relationship degree. Tapping an
       entity opens memory/<name> — its linked facts via the
       entity-facts endpoint (MEMORY-UI-SPEC Phase B).
       -----------------------------------------------------------*/

    var Memory = (function () {
        function linkSVG() {
            return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg>';
        }
        function chevSVG() {
            return '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>';
        }

        function setStat(id, v) {
            var el = document.getElementById(id);
            if (el) el.textContent = (v == null ? '—' : String(v));
        }

        async function refresh() {
            // Totals — best-effort, cells keep their "—" if the
            // overview endpoint isn't reachable.
            apiJSON('/console/api/dashboard/overview').then(function (resp) {
                setStat('mob-memstat-facts', resp && resp.fact_count);
                setStat('mob-memstat-entities', resp && resp.entity_count);
                setStat('mob-memstat-links', resp && resp.relationship_count);
            }).catch(function () {});

            var host = document.getElementById('mob-memory-entities');
            if (!host) return;
            host.innerHTML = '<div class="mob-empty">Loading…</div>';
            try {
                var resp = await apiJSON('/console/api/memory/entities?limit=30');
                renderEntities(host, (resp && resp.items) || []);
            } catch (e) {
                host.innerHTML = '<div class="mob-empty">Couldn\'t load memory.</div>';
            }
        }

        function renderEntities(host, items) {
            var count = document.getElementById('mob-memory-count');
            if (count) count.textContent = items.length ? String(items.length) : '';
            host.innerHTML = '';
            if (!items.length) {
                host.innerHTML = '<div class="mob-empty">No entities yet — talk to Familiar about things you care about and they\'ll show up here.</div>';
                return;
            }
            items.forEach(function (it) {
                var row = document.createElement('button');
                row.type = 'button';
                row.className = 'mob-memory-row';
                var d = it.degree || 0;
                row.innerHTML =
                    '<span class="glyph">' + linkSVG() + '</span>' +
                    '<div class="body">' +
                        '<div class="title">' + escapeHTML(it.name || '—') + '</div>' +
                        '<div class="meta">' + d + ' connection' + (d === 1 ? '' : 's') + '</div>' +
                    '</div>' +
                    '<div class="chev">' + chevSVG() + '</div>';
                row.addEventListener('click', function () {
                    location.hash = 'memory/' + encodeURIComponent(it.name);
                });
                host.appendChild(row);
            });
        }

        async function openEntity(name) {
            var title = document.getElementById('mob-mement-title');
            if (title) title.textContent = name;
            var count = document.getElementById('mob-mement-count');
            if (count) count.textContent = '';
            var host = document.getElementById('mob-mement-facts');
            if (!host) return;
            host.innerHTML = '<div class="mob-empty">Loading…</div>';
            try {
                var resp = await apiJSON('/console/api/memory/entity/' + encodeURIComponent(name) + '/facts?limit=50');
                var items = (resp && resp.items) || [];
                if (count) count.textContent = items.length ? String(items.length) : '';
                if (!items.length) {
                    host.innerHTML = '<div class="mob-empty">No linked facts for this entity.</div>';
                    return;
                }
                host.innerHTML = '';
                items.forEach(function (f) {
                    var row = document.createElement('div');
                    row.className = 'mob-memory-fact';
                    var meta = (f.source_type || '—') +
                        (f.created_at ? ' · ' + String(f.created_at).slice(0, 10) : '');
                    row.innerHTML =
                        '<div class="title">' + escapeHTML(f.content || '') + '</div>' +
                        '<div class="meta">' + escapeHTML(meta) + '</div>';
                    host.appendChild(row);
                });
            } catch (e) {
                host.innerHTML = '<div class="mob-empty">Couldn\'t load facts.</div>';
            }
        }

        return { refresh: refresh, openEntity: openEntity };
    })();

    /* -----------------------------------------------------------
       Wire global event handlers — only after auth succeeds.
       -----------------------------------------------------------*/

    var appStarted = false;
    function startApp() {
        if (appStarted) return;
        appStarted = true;

        // Watch for a session that lapses while the app is open /
        // backgrounded so the user lands on login, not a wall of 401s.
        startSessionWatchdog();

        document.getElementById('mob-app').hidden = false;

        document.getElementById('mob-tabbar').addEventListener('click', function (ev) {
            var btn = ev.target.closest('.mob-tab');
            if (!btn) return;
            ev.preventDefault();
            location.hash = btn.dataset.tab;
        });

        document.addEventListener('click', function (ev) {
            var t = ev.target.closest('.mob-toggle');
            if (!t) return;
            ev.preventDefault();
            var on = t.classList.toggle('is-on');
            t.setAttribute('aria-pressed', on ? 'true' : 'false');
        });

        document.addEventListener('click', function (ev) {
            var btn = ev.target.closest('[data-action]');
            if (!btn) {
                // Click outside any overflow menu closes them all.
                document.querySelectorAll('.mob-overflow.is-open').forEach(function (el) {
                    el.classList.remove('is-open');
                });
                return;
            }
            var action = btn.dataset.action;
            if      (action === 'new-note')           { Notes.startNew(); }
            else if (action === 'new-chat')           { Chat.startNew(); }
            else if (action === 'back-to-chat-list')  { location.hash = 'chat'; }
            else if (action === 'back-to-notes-list') { Notes.flushSave(); location.hash = 'notes'; }
            else if (action === 'back-to-wiki-list')  { Wiki.flushSave(); location.hash = 'wiki'; }
            else if (action === 'back-to-home')       { location.hash = 'home'; }
            else if (action === 'back-to-memory')     { location.hash = 'memory'; }
            else if (action === 'new-wiki-book')      {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Wiki.newBook();
            }
            else if (action === 'new-wiki-page')      { Wiki.newPage(); }
            else if (action === 'archive-wiki-book')  {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Wiki.archiveBook();
            }
            else if (action === 'delete-wiki-page')   {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Wiki.deletePage();
            }
            else if (action === 'open-account')       {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                location.hash = 'account';
            }
            else if (action === 'open-memory')        {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                location.hash = 'memory';
            }
            else if (action === 'toggle-push')        { Push.toggle(); }
            else if (action === 'logout')             {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Auth.logout();
            }
            else if (action === 'toggle-overflow')    {
                ev.stopPropagation();
                var host = btn.closest('.mob-overflow');
                // Close any other open menus first (single-open pattern).
                document.querySelectorAll('.mob-overflow.is-open').forEach(function (el) {
                    if (el !== host) el.classList.remove('is-open');
                });
                if (host) {
                    host.classList.toggle('is-open');
                    // Refresh dynamic items every time the menu opens
                    // — Pin/Unpin reflects current state, and the
                    // wiki-page Delete item is hidden when the
                    // caller's book role is reader (read-only).
                    if (host.classList.contains('is-open')) {
                        var pinChat = document.getElementById('mob-toggle-pin-chat');
                        if (pinChat) pinChat.textContent = Chat.currentPinned() ? 'Unpin chat' : 'Pin chat';
                        var pinNote = document.getElementById('mob-toggle-pin-note');
                        if (pinNote) pinNote.textContent = Notes.currentPinned() ? 'Unpin note' : 'Pin note';
                        var pinWiki = document.getElementById('mob-toggle-pin-wiki');
                        if (pinWiki) pinWiki.textContent = Wiki.currentPinned() ? 'Unpin page' : 'Pin page';
                        var delWiki = document.getElementById('mob-delete-wiki-page');
                        var roWiki = document.getElementById('mob-wiki-page-readonly');
                        if (delWiki) {
                            var canWrite = Wiki.canWritePages();
                            delWiki.style.display = canWrite ? '' : 'none';
                            if (roWiki) roWiki.hidden = canWrite;
                        }
                        var saEnable = document.getElementById('mob-sa-enable-item');
                        if (saEnable && window.Scheduled) Scheduled.enableLabel();
                    }
                }
            }
            else if (action === 'toggle-pin-chat') {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Chat.togglePin();
            }
            else if (action === 'toggle-pin-note') {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Notes.togglePin();
            }
            else if (action === 'toggle-pin-wiki') {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Wiki.togglePin();
            }
            else if (action === 'delete-chat') {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Chat.deleteCurrent();
            }
            else if (action === 'delete-note') {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Notes.deleteCurrent();
            }
            else if (action === 'new-scheduled')      { location.hash = 'scheduled/new'; }
            else if (action === 'back-to-scheduled')  { location.hash = 'scheduled'; }
            else if (action === 'open-action')        { location.hash = 'scheduled/' + encodeURIComponent(btn.dataset.id); }
            else if (action === 'run-action')         { Scheduled.runNow(); }
            else if (action === 'save-action-prompt') { Scheduled.savePrompt(); }
            else if (action === 'toggle-enable-action') {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Scheduled.toggleEnable();
            }
            else if (action === 'delete-action') {
                var host = btn.closest('.mob-overflow');
                if (host) host.classList.remove('is-open');
                Scheduled.deleteCurrent();
            }
            else if (action === 'toggle-shard')       { Shards.toggle(btn.dataset.id, btn); }
        });

        // Notes detail: title input autosave on each keystroke
        // (body autosave is wired inside Notes.ensureEditor via
        // Toast UI's "change" callback now that the body is a
        // contenteditable, not a textarea).
        var noteTitleInp = document.getElementById('mob-note-title');
        if (noteTitleInp) noteTitleInp.addEventListener('input', function () { Notes.scheduleSave(); });

        // Wiki page editor — same shape; body autosave lives inside
        // Wiki.ensureEditor.
        var wikiTitleInp = document.getElementById('mob-wiki-page-title');
        if (wikiTitleInp) wikiTitleInp.addEventListener('input', function () { Wiki.scheduleSave(); });

        // Scheduled-action create form: submit + cron presets.
        var saForm = document.getElementById('mob-sa-form');
        if (saForm) saForm.addEventListener('submit', function (ev) {
            ev.preventDefault();
            Scheduled.submitCreate();
        });
        document.addEventListener('click', function (ev) {
            var preset = ev.target.closest('.mob-sa-preset');
            if (!preset) return;
            ev.preventDefault();
            var cron = document.getElementById('mob-sa-cron');
            if (cron && preset.dataset.cron) cron.value = preset.dataset.cron;
        });

        // Flush pending saves on tab/page switch so a backgrounded
        // tab doesn't lose the last few keystrokes.
        window.addEventListener('pagehide', function () { Notes.flushSave(); Wiki.flushOnUnload(); });
        window.addEventListener('beforeunload', function () { Notes.flushSave(); Wiki.flushOnUnload(); });

        // Notes-list refresh on tool-effect signal. Chat dispatches
        // familiar:notesChanged when an assistant turn writes a note
        // (gateway emits __TOOL_EFFECT__:note_changed on the reasoning
        // channel; Chat parses + dispatches). Re-fetch the notes list
        // so a swap to the Notes tab shows the new row.
        window.addEventListener('familiar:notesChanged', function () {
            Notes.refreshList();
        });

        // Server-Sent Events: page-saved / page-deleted push so an
        // idle phone picks up edits made on another device or by the
        // AI without polling. Skipped for shard sessions (kiosk-style
        // accounts have no editor to refresh and shouldn't hold a
        // long-lived auth channel). EventSource auto-reconnects on
        // close; we don't manage retries beyond that.
        (function wirePageEventsSSE() {
            var sess = window.FAMILIAR_SESSION || {};
            if (sess.principal_type === 'shard') return;
            if (typeof EventSource !== 'function') return;
            var es;
            try { es = new EventSource('/console/api/events/pages', { withCredentials: true }); }
            catch (e) { return; }
            var dispatch = function (raw, kind) {
                try {
                    var data = JSON.parse(raw);
                    data.kind = kind;
                    if (Notes && Notes.onPageEvent) Notes.onPageEvent(data);
                    if (Wiki && Wiki.onPageEvent) Wiki.onPageEvent(data);
                } catch (e) { /* malformed — ignore */ }
            };
            es.addEventListener('page-saved', function (ev) { dispatch(ev.data, 'page-saved'); });
            es.addEventListener('page-deleted', function (ev) { dispatch(ev.data, 'page-deleted'); });
            es.addEventListener('error', function () { /* let browser reconnect */ });
            window.familiarPageEvents = es;
        })();

        var threadForm = document.getElementById('mob-thread-form');
        if (threadForm) {
            threadForm.addEventListener('submit', function (ev) {
                ev.preventDefault();
                // While generating the Send button is a Stop — tap aborts.
                var btn = threadForm.querySelector('.mob-thread-send');
                if (btn && btn.classList.contains('is-stop')) { Chat.stop(); return; }
                var inp = document.getElementById('mob-thread-input');
                var v = inp.value;
                inp.value = '';
                Chat.send(v);
            });
        }

        window.addEventListener('hashchange', function () { activate(readHashRoute()); });
        activate(readHashRoute());

        wirePullToRefresh();
    }

    /* -----------------------------------------------------------
       Pull-to-refresh.

       Touch-drag down from the top of any .mob-scroll container
       (when it's already at scrollTop=0) to fire a hard refresh:
       unregister all service workers, delete every CacheStorage
       entry, then location.reload(). This is the manual escape
       hatch for "the SW is serving stale assets" — useful during
       PWA development and after a deploy.

       Implementation notes:
       - touchstart only arms when the active scroll container is
         at the top. If the user starts a drag mid-scroll, native
         scrolling handles it and we never engage.
       - preventDefault on touchmove only when we're driving the
         indicator. Otherwise the browser's own rubber-band stays.
       - Threshold = 70px. Anything shorter releases without firing.
       ----------------------------------------------------------- */
    function wirePullToRefresh() {
        var indicator = document.getElementById('mob-ptr');
        if (!indicator) return;
        var label = indicator.querySelector('.mob-ptr-label');
        var THRESHOLD = 70;
        var MAX_PULL = 120;

        var startY = 0;
        var armed = false;
        var dragging = false;
        var pull = 0;
        var refreshing = false;

        // The PTR gesture is restricted to touches that START inside
        // a screen header. Anywhere below the header is fair game for
        // normal scrolling — pulling down from mid-conversation must
        // not engage. Header markup varies by screen so we recognise
        // the three classes that show up across mobile.html.
        var HEADER_CLASSES = ['mob-home-header', 'mob-title-header', 'mob-thread-head'];
        function insideHeader(node) {
            while (node && node !== document.body) {
                if (node.classList) {
                    for (var i = 0; i < HEADER_CLASSES.length; i++) {
                        if (node.classList.contains(HEADER_CLASSES[i])) return true;
                    }
                }
                node = node.parentNode;
            }
            return false;
        }

        function setIndicator(y, releaseReady) {
            // y is how far the pull has progressed in CSS pixels.
            // Map it to a translateY (with diminishing returns past
            // the threshold so it feels rubber-bandy) and an opacity.
            var capped = Math.min(y, MAX_PULL);
            var slide = capped * 0.85;
            indicator.style.transform = 'translateY(' + (slide - 56) + 'px)';
            indicator.style.opacity = Math.min(1, capped / THRESHOLD).toFixed(2);
            label.textContent = releaseReady ? 'Release to refresh' : 'Pull to refresh';
        }

        function resetIndicator() {
            indicator.classList.remove('is-dragging');
            indicator.classList.remove('is-spinning');
            indicator.style.transform = '';
            indicator.style.opacity = '';
            label.textContent = 'Pull to refresh';
        }

        async function hardRefresh() {
            refreshing = true;
            indicator.classList.add('is-spinning');
            indicator.style.transform = 'translateY(0)';
            indicator.style.opacity = '1';
            label.textContent = 'Refreshing…';
            try {
                if ('serviceWorker' in navigator) {
                    var regs = await navigator.serviceWorker.getRegistrations();
                    await Promise.all(regs.map(function (r) { return r.unregister(); }));
                }
                if (window.caches && caches.keys) {
                    var keys = await caches.keys();
                    await Promise.all(keys.map(function (k) { return caches.delete(k); }));
                }
            } catch (e) {
                console.warn('mobile ptr: cache purge failed', e);
            }
            // Append a cache-buster on the URL so the document fetch
            // itself bypasses any intermediary (browser disk cache,
            // CDN edge) — the SW + caches.delete handle the SW path
            // but the HTML doc itself can still be disk-cached.
            var u = new URL(location.href);
            u.searchParams.set('_r', Date.now().toString());
            location.replace(u.toString());
        }

        document.addEventListener('touchstart', function (e) {
            if (refreshing) return;
            if (!e.touches || e.touches.length !== 1) return;
            // Restrict the gesture to touches that land inside a
            // screen header. Anywhere else — message lists, note
            // bodies, the tab bar — passes through to normal touch
            // handling, so users can scroll the conversation up and
            // down without triggering refresh.
            if (!insideHeader(e.target)) return;
            startY = e.touches[0].clientY;
            armed = true;
            dragging = false;
            pull = 0;
        }, { passive: true });

        document.addEventListener('touchmove', function (e) {
            if (!armed || refreshing) return;
            var y = e.touches[0].clientY;
            var dy = y - startY;
            if (dy <= 0) {
                if (dragging) {
                    dragging = false;
                    resetIndicator();
                }
                return;
            }
            if (!dragging) {
                dragging = true;
                indicator.classList.add('is-dragging');
            }
            pull = dy;
            setIndicator(pull, pull >= THRESHOLD);
            // Suppress the browser's native pull-to-refresh / rubber
            // band so the indicator owns the gesture cleanly.
            if (e.cancelable) e.preventDefault();
        }, { passive: false });

        document.addEventListener('touchend', function () {
            if (!armed) return;
            armed = false;
            if (!dragging) return;
            dragging = false;
            if (pull >= THRESHOLD) {
                hardRefresh();
            } else {
                resetIndicator();
            }
        });

        document.addEventListener('touchcancel', function () {
            armed = false;
            if (dragging) {
                dragging = false;
                resetIndicator();
            }
        });
    }

    /* -----------------------------------------------------------
       Auth — WebAuthn (passkey) ceremonies, ported from app.js so
       mobile can boot without the desktop SPA. Three views:
       loading (spinner), setup (first-time, register first user),
       login (existing user signs in). After /console/api/auth/
       status returns authenticated, we hide the auth views and
       call startApp() to wire the rest of the UI.
       -----------------------------------------------------------*/

    var Auth = (function () {
        function showView(name) {
            ['loading', 'setup', 'login'].forEach(function (v) {
                var el = document.getElementById('mob-auth-' + v);
                if (el) el.hidden = (v !== name);
            });
        }
        // Let the module-level 401 handler re-reveal the login overlay
        // (.mob-auth is fixed/inset:0, so it covers the app). Assigned
        // here so apiJSON / the watchdog can route to login without
        // reaching into this closure.
        onUnauthorized = function () { showView('login'); };
        function hideAll() {
            ['loading', 'setup', 'login'].forEach(function (v) {
                var el = document.getElementById('mob-auth-' + v);
                if (el) el.hidden = true;
            });
        }
        function setError(id, err) {
            var el = document.getElementById(id);
            if (!el) return;
            if (!err) { el.hidden = true; el.textContent = ''; return; }
            el.hidden = false;
            // Friendly copy for account-status gates — parity with the
            // desktop login (app.js setError). A pending/disabled/denied
            // user should see a sentence, not a raw server string.
            var raw = err.message || String(err);
            var lower = raw.toLowerCase();
            if (lower.indexOf('pending approval') !== -1) {
                el.textContent = "Your account is awaiting admin approval. You'll be able to sign in once it's approved.";
            } else if (lower.indexOf('account disabled') !== -1) {
                el.textContent = "This account has been disabled. Contact the admin if you think that's a mistake.";
            } else if (lower.indexOf('access denied') !== -1) {
                el.textContent = "Your access request was denied. Contact the admin if you think that's a mistake.";
            } else {
                el.textContent = raw;
            }
        }

        // requireWebAuthn throws a readable error when the
        // navigator.credentials API isn't available — typically
        // because the page is loaded over plain HTTP from a non-
        // localhost origin (browsers gate WebAuthn behind a secure
        // context).
        function requireWebAuthn() {
            if (typeof navigator === 'undefined' || !navigator.credentials) {
                throw new Error(
                    "Passkeys aren't available on this page. Open the workspace over HTTPS, or via http://localhost — " +
                    "browsers gate the WebAuthn API behind a secure context."
                );
            }
        }

        async function doRegister(params) {
            var body = {};
            if (params && params.email) body.email = params.email;
            if (params && params.displayName) body.display_name = params.displayName;
            var creation = await apiJSON('/console/api/auth/register/begin', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body),
            });
            var pk = creation.publicKey;
            pk.challenge = b64urlToBuf(pk.challenge);
            // user.id is a plain string (EncodeUserIDAsString=true).
            // Always UTF-8 encode — base64url-decoding silently
            // produces wrong bytes for IDs like "canyon_r".
            pk.user.id = new TextEncoder().encode(pk.user.id).buffer;
            if (Array.isArray(pk.excludeCredentials)) {
                pk.excludeCredentials.forEach(function (c) { c.id = b64urlToBuf(c.id); });
            }
            requireWebAuthn();
            var credential = await navigator.credentials.create({ publicKey: pk });
            if (!credential) throw new Error('authenticator returned no credential');
            var finishBody = {
                id: credential.id,
                rawId: bufToB64url(credential.rawId),
                type: credential.type,
                response: {
                    attestationObject: bufToB64url(credential.response.attestationObject),
                    clientDataJSON:    bufToB64url(credential.response.clientDataJSON),
                },
                clientExtensionResults: credential.getClientExtensionResults
                    ? credential.getClientExtensionResults() : {},
            };
            return apiJSON('/console/api/auth/register/finish', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(finishBody),
            });
        }

        async function doLogin() {
            var assertion = await apiJSON('/console/api/auth/login/begin', { method: 'POST' });
            var pk = assertion.publicKey;
            pk.challenge = b64urlToBuf(pk.challenge);
            if (Array.isArray(pk.allowCredentials)) {
                pk.allowCredentials.forEach(function (c) { c.id = b64urlToBuf(c.id); });
            }
            requireWebAuthn();
            var credential = await navigator.credentials.get({ publicKey: pk });
            if (!credential) throw new Error('authenticator returned no credential');
            var body = {
                id: credential.id,
                rawId: bufToB64url(credential.rawId),
                type: credential.type,
                response: {
                    authenticatorData: bufToB64url(credential.response.authenticatorData),
                    clientDataJSON:    bufToB64url(credential.response.clientDataJSON),
                    signature:         bufToB64url(credential.response.signature),
                    userHandle: credential.response.userHandle
                        ? bufToB64url(credential.response.userHandle) : null,
                },
                clientExtensionResults: credential.getClientExtensionResults
                    ? credential.getClientExtensionResults() : {},
            };
            return apiJSON('/console/api/auth/login/finish', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body),
            });
        }

        async function logout() {
            try {
                await apiJSON('/console/api/auth/logout', { method: 'POST' });
            } catch (_) { /* even if the call fails, kick the user back to login */ }
            location.reload();
        }

        async function boot() {
            showView('loading');
            try {
                var status = await apiJSON('/console/api/auth/status');
                if (status && status.authenticated) {
                    window.FAMILIAR_SESSION = status;
                    // SHARD-AUTH-SPEC: stamp body so CSS hide rules
                    // mirror desktop. Mobile is read-mostly so we
                    // don't sweep through subnav rows the way app.js
                    // does — just the headline flags.
                    document.body.dataset.role = status.role || '';
                    document.body.dataset.principalType = status.principal_type || 'user';
                    if (status.permissions && status.permissions.can_chat === false) {
                        document.body.dataset.canChat = 'false';
                    }
                    hideAll();
                    startApp();
                    applyMaintenanceBanner(status.maintenance);
                    return;
                }
            } catch (_) { /* 401 → fall through */ }

            // Not authenticated. Login + setup views live inline in
            // this same shell, no cross-page navigation, so there's
            // nothing for /login ↔ / to ping-pong against.
            try {
                var reg = await apiJSON('/console/api/auth/register/status');
                if (reg && reg.credentials_registered === 0) {
                    showView('setup');
                    return;
                }
            } catch (_) { /* fall through to login view */ }
            showView('login');
        }

        function wireAuthButtons() {
            var setupBtn = document.getElementById('mob-setup-btn');
            if (setupBtn) {
                setupBtn.addEventListener('click', async function () {
                    setError('mob-setup-error', null);
                    var email = (document.getElementById('mob-setup-email').value || '').trim();
                    var name = (document.getElementById('mob-setup-name').value || '').trim();
                    if (!email) {
                        setError('mob-setup-error', new Error('email required'));
                        return;
                    }
                    setupBtn.disabled = true;
                    try {
                        await doRegister({ email: email, displayName: name });
                        // First-time: immediately do a login so the
                        // user gets a session cookie without a second
                        // tap on the passkey prompt.
                        await doLogin();
                        await boot();
                    } catch (e) {
                        setError('mob-setup-error', e);
                    } finally {
                        setupBtn.disabled = false;
                    }
                });
            }

            var loginBtn = document.getElementById('mob-login-btn');
            if (loginBtn) {
                loginBtn.addEventListener('click', async function () {
                    setError('mob-login-error', null);
                    loginBtn.disabled = true;
                    try {
                        await doLogin();
                        await boot();
                    } catch (e) {
                        setError('mob-login-error', e);
                    } finally {
                        loginBtn.disabled = false;
                    }
                });
            }

            // SHARD-AUTH-SPEC Phase 1 — same WebAuthn ceremony as
            // the purple button; the visual split is so a phone
            // owner recognizes the shard-credential path. The
            // browser handles cross-device QR if the credential
            // lives on a different device.
            var shardLoginBtn = document.getElementById('mob-shard-login-btn');
            if (shardLoginBtn) {
                shardLoginBtn.addEventListener('click', async function () {
                    setError('mob-login-error', null);
                    shardLoginBtn.disabled = true;
                    try {
                        await doLogin();
                        await boot();
                    } catch (e) {
                        setError('mob-login-error', e);
                    } finally {
                        shardLoginBtn.disabled = false;
                    }
                });
            }

            // Suppress accidental form submits — both buttons drive
            // the WebAuthn ceremony directly.
            var lpmForm = document.getElementById('lpm-form');
            if (lpmForm) {
                lpmForm.addEventListener('submit', function (ev) { ev.preventDefault(); });
            }

            // (The "register a new device" email-form branch was removed
            // — its DOM (#mob-login-register-form etc.) never existed in
            // mobile.html, so the handlers were dead. Binding a passkey to
            // an admin-created account goes through the enrollment link
            // (enroll.html); onboarding is being reworked.)
        }

        return { boot: boot, logout: logout, wireAuthButtons: wireAuthButtons };
    })();

    Auth.wireAuthButtons();
    Auth.boot();
})();
