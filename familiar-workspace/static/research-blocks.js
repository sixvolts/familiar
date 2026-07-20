// research-blocks.js — RESEARCH-CARD: a ```research-card fence in an
// assistant message renders as a compact "completed research" card,
// inline in the chat transcript, with the note as a prominent CTA.
//
// The gateway's synthesizer (research.go) emits the fence when a deep
// run finishes; chat.js + mobile.js call familiarResearchCard.html()
// from their marked `code` renderer, so it renders identically live
// and on reload. Self-contained: the card's CSS is injected once here
// (scoped under .research-note-card, token-based) so it works in both
// the desktop (app.css) and mobile (mobile.css) contexts without
// depending on either. Degrades to a plain code block if this script
// didn't load.
(function () {
    "use strict";

    var STYLE_ID = "familiar-research-card-css";
    function injectStyle() {
        if (document.getElementById(STYLE_ID)) return;
        var s = document.createElement("style");
        s.id = STYLE_ID;
        s.textContent = [
            ".research-note-card{margin:12px 0 6px;border:1px solid var(--iris-600,#5138B0);border-radius:var(--radius-lg,14px);",
            "background:linear-gradient(180deg,rgba(106,76,224,0.10),rgba(106,76,224,0.03)),var(--bg-card,#1B1B23);",
            "box-shadow:0 0 0 1px rgba(106,76,224,0.10),0 10px 30px rgba(8,4,22,0.45);color:var(--fg-2,#C9C9D1);",
            "font-size:13px;line-height:1.4;overflow:hidden;max-width:520px;--pc:92,181,133;}",
            ".research-note-card .rc-head{display:flex;align-items:center;gap:11px;padding:12px 14px;}",
            ".research-note-card .rc-title{min-width:0;flex:1 1 auto;}",
            ".research-note-card .rc-t{color:var(--fg-1,#F4F4F7);font-weight:600;display:flex;gap:6px;align-items:baseline;}",
            ".research-note-card .rc-topic{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}",
            ".research-note-card .rc-s{color:var(--fg-3,#7A7A86);font-size:12px;margin-top:2px;}",
            ".research-note-card .rc-done-badge{flex:0 0 auto;color:var(--moss-300,#82cfa4);font-family:var(--font-mono,ui-monospace,monospace);font-size:11px;letter-spacing:0.04em;}",
            ".research-note-card .rc-roster{padding:2px 6px 4px;}",
            ".research-note-card .rc-row{display:flex;align-items:center;gap:12px;padding:8px;}",
            ".research-note-card .rc-row+.rc-row{border-top:1px solid var(--border-subtle,rgba(255,255,255,0.06));}",
            ".research-note-card .rc-slot{width:15px;display:flex;justify-content:center;flex:0 0 auto;}",
            ".research-note-card .rc-label{flex:1 1 auto;min-width:0;color:var(--fg-3,#7A7A86);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}",
            ".research-note-card .rc-wstate{font-size:12px;color:var(--moss-300,#82cfa4);font-family:var(--font-mono,ui-monospace,monospace);flex:0 0 auto;}",
            ".research-note-card .rc-meta{display:flex;align-items:center;gap:15px;flex-wrap:wrap;padding:10px 14px;border-top:1px solid var(--border-subtle,rgba(255,255,255,0.06));font-family:var(--font-mono,ui-monospace,monospace);font-size:12px;color:var(--fg-3,#7A7A86);font-variant-numeric:tabular-nums;}",
            ".research-note-card .rc-m{display:flex;align-items:baseline;gap:5px;}",
            ".research-note-card .rc-k{color:var(--fg-4,#52525F);font-size:10px;letter-spacing:0.08em;text-transform:uppercase;}",
            ".research-note-card .rc-m b{color:var(--accent-soft-fg,#AC98F3);font-weight:600;}",
            ".research-note-card .rc-m-moss b{color:var(--moss-300,#82cfa4);}",
            ".research-note-card .rc-px{display:inline-flex;align-items:flex-end;gap:3px;}",
            ".research-note-card .rc-c{display:flex;flex-direction:column;gap:3px;}",
            ".research-note-card .rc-c i{display:block;width:3px;height:3px;border-radius:1px;background:rgba(var(--pc),0.9);}",
            ".research-note-card .rc-note{display:flex;align-items:center;gap:11px;padding:14px;border-top:1px solid var(--border-subtle,rgba(255,255,255,0.06));background:var(--accent-soft-bg,rgba(106,76,224,0.14));color:var(--iris-100,#E8E1FB);font-weight:600;text-decoration:none;transition:background .12s ease;}",
            ".research-note-card .rc-note:hover{background:rgba(106,76,224,0.24);}",
            ".research-note-card .rc-note-doc{flex:0 0 auto;font-size:15px;}",
            ".research-note-card .rc-note-lab{flex:1 1 auto;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}",
            ".research-note-card .rc-note-open{flex:0 0 auto;margin-left:auto;display:inline-flex;align-items:center;gap:5px;padding:3px 9px;border-radius:999px;background:rgba(106,76,224,0.22);color:var(--iris-200,#CEC2F8);font-size:12px;font-weight:600;}",
            ".research-note-card .rc-note-arrow{color:var(--iris-300,#AC98F3);}",
        ].join("");
        (document.head || document.documentElement).appendChild(s);
    }

    // Parse the fence body — one `key: value` per line, split on the
    // FIRST colon so a value with colons (book slug personal:<id>, a
    // title with a subtitle) survives intact.
    function parse(body) {
        var d = {};
        var lines = (body || "").split("\n");
        for (var i = 0; i < lines.length; i++) {
            var idx = lines[i].indexOf(":");
            if (idx < 0) continue;
            var k = lines[i].slice(0, idx).trim().toLowerCase();
            if (k) d[k] = lines[i].slice(idx + 1).trim();
        }
        return d;
    }

    function esc(s) {
        return String(s == null ? "" : s)
            .replace(/&/g, "&amp;").replace(/</g, "&lt;")
            .replace(/>/g, "&gt;").replace(/"/g, "&quot;");
    }

    // A solid moss pixel indicator (the done state of the live card's .rc-px).
    function pixels() {
        return '<span class="rc-px" aria-hidden="true">' +
            '<span class="rc-c rc-c1"><i></i></span>' +
            '<span class="rc-c rc-c2"><i></i><i></i></span>' +
            '<span class="rc-c rc-c3"><i></i><i></i><i></i></span></span>';
    }

    // html renders one research-card fence body into the completed card.
    // The return value flows through the caller's marked renderer +
    // DOMPurify, so it uses only sanitiser-safe tags (div/span/a + class
    // + a #note/ href) — no inline SVG or style.
    function html(body) {
        injectStyle();
        var d = parse(body);
        var topic = d.topic || "your topic";
        var title = d.title || "the note";
        var href = "";
        if (d.book && d.page) {
            href = "#note/" + encodeURIComponent(d.book) + "/" + encodeURIComponent(d.page);
        } else if (d.note) {
            href = d.note;
        }

        // Per-item token tail, appended to the grey row label. New blocks
        // carry worker_in/out + note_in/out; older blocks only a combined
        // `tokens` (workers) — fall back to that so rehydrated history still
        // reads sensibly.
        function io(inTok, outTok) {
            return (inTok || outTok)
                ? " · " + esc(inTok || "0") + " in · " + esc(outTok || "0") + " out"
                : "";
        }
        var workerTail = io(d.worker_in, d.worker_out) || (d.tokens ? " · " + esc(d.tokens) : "");
        var noteTail = io(d.note_in, d.note_out);

        var note = "";
        if (href) {
            note = '<a class="rc-note" href="' + esc(href) + '">' +
                '<span class="rc-note-doc">📄</span>' +
                '<span class="rc-note-lab">' + esc(title) + '</span>' +
                '<span class="rc-note-open">Open<span class="rc-note-arrow">→</span></span></a>';
        }

        return '<div class="research-note-card">' +
            '<div class="rc-head"><div class="rc-title">' +
                '<div class="rc-t">Researched <span class="rc-topic">“' + esc(topic) + '”</span></div>' +
                '</div></div>' +
            '<div class="rc-roster">' +
                '<div class="rc-row rc-done"><span class="rc-slot">' + pixels() + '</span><span class="rc-label">Research workers' + workerTail + '</span><span class="rc-wstate">complete</span></div>' +
                '<div class="rc-row rc-done"><span class="rc-slot">' + pixels() + '</span><span class="rc-label">Note written' + noteTail + '</span><span class="rc-wstate">complete</span></div>' +
            "</div>" +
            note +
        "</div>";
    }

    window.familiarResearchCard = { html: html };
})();
