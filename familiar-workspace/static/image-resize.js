// image-resize.js — graphical image sizing (MEDIA-DIAGRAMS Phase 1b).
// Click an image in Rich Text → selection outline + a corner drag
// handle + Small/Medium/Full preset chips (the chips are the iPad
// gesture; the handle is the desktop one). The chosen width persists
// as a URL FRAGMENT on the image: ![alt](url#w=45) where 45 means
// 45% of the content width. Fragments are part of the URL, so the
// size round-trips through ProseMirror, markdown, and the database
// untouched, never reaches the server, and the model can set sizes
// when it authors pages — it's just text.
//
// The WYSIWYG node view is registered through mermaid-blocks.js's
// editor plugin (one plugin, one wysiwygNodeViews map — merge
// semantics across multiple plugins are undocumented in tui).
(function () {
    "use strict";

    var FRAG_RE = /#w=(\d{1,3})\s*$/;
    var MIN_PCT = 10;

    function getWidthPct(url) {
        var m = FRAG_RE.exec(url || "");
        if (!m) return null;
        var pct = parseInt(m[1], 10);
        if (!(pct >= MIN_PCT && pct <= 100)) return null;
        return pct;
    }

    function setWidthPct(url, pct) {
        var base = String(url || "").replace(FRAG_RE, "");
        if (pct == null || pct >= 100) return base; // full width = no fragment
        return base + "#w=" + pct;
    }

    // ── WYSIWYG node view ───────────────────────────────────────
    function ImageView(node, view, getPos) {
        var self = this;
        this.node = node;
        this.view = view;
        this.getPos = getPos;

        this.dom = document.createElement("span");
        this.dom.className = "familiar-img-wrap";
        this.dom.contentEditable = "false";

        this.img = document.createElement("img");
        this.dom.appendChild(this.img);

        // Selection overlay: preset chips + the corner handle.
        this.chips = document.createElement("span");
        this.chips.className = "familiar-img-chips";
        var presets = [["S", 25], ["M", 50], ["Full", 100]];
        presets.forEach(function (p) {
            var chip = document.createElement("button");
            chip.type = "button";
            chip.className = "familiar-img-chip";
            chip.textContent = p[0];
            chip.title = p[1] + "% width";
            chip.addEventListener("click", function (e) {
                e.preventDefault();
                e.stopPropagation();
                self.commitWidth(p[1]);
            });
            self.chips.appendChild(chip);
        });
        this.dom.appendChild(this.chips);

        this.handle = document.createElement("span");
        this.handle.className = "familiar-img-handle";
        this.dom.appendChild(this.handle);
        this.wireHandle();

        // NOTE: the click must BUBBLE — document-level click-away
        // handlers (the ⋯ menus) depend on it. Our own deselect
        // listener checks containment, so bubbling doesn't fight
        // selection.
        this.dom.addEventListener("click", function (e) {
            e.preventDefault();
            self.select(true);
        });
        this._docClick = function (e) {
            if (!self.dom.contains(e.target)) self.select(false);
        };
        document.addEventListener("click", this._docClick, true);

        this.sync();
    }

    ImageView.prototype.sync = function () {
        var attrs = this.node.attrs || {};
        var url = attrs.imageUrl || "";
        this.img.src = url.replace(FRAG_RE, "");
        this.img.alt = attrs.altText || "";
        var pct = getWidthPct(url);
        this.dom.style.width = pct != null ? pct + "%" : "";
    };

    ImageView.prototype.select = function (on) {
        this.dom.classList.toggle("is-selected", !!on);
    };

    ImageView.prototype.commitWidth = function (pct) {
        var pos = typeof this.getPos === "function" ? this.getPos() : null;
        if (pos == null) return;
        var attrs = Object.assign({}, this.node.attrs, {
            imageUrl: setWidthPct(this.node.attrs.imageUrl, pct),
        });
        var tr = this.view.state.tr.setNodeMarkup(pos, null, attrs);
        this.view.dispatch(tr);
    };

    ImageView.prototype.wireHandle = function () {
        var self = this;
        var drag = null;
        this.handle.addEventListener("pointerdown", function (e) {
            e.preventDefault();
            e.stopPropagation();
            var container = self.dom.closest(".toastui-editor-contents") || self.dom.parentElement;
            drag = {
                startX: e.clientX,
                startW: self.dom.getBoundingClientRect().width,
                max: container ? container.getBoundingClientRect().width : 800,
            };
            self.select(true);
            self.handle.setPointerCapture(e.pointerId);
        });
        this.handle.addEventListener("pointermove", function (e) {
            if (!drag) return;
            var px = drag.startW + (e.clientX - drag.startX);
            var pct = Math.round((px / drag.max) * 100);
            pct = Math.max(MIN_PCT, Math.min(100, pct));
            self.dom.style.width = pct + "%";
            self.dom.dataset.livePct = String(pct);
        });
        var finish = function () {
            if (!drag) return;
            drag = null;
            var pct = parseInt(self.dom.dataset.livePct || "", 10);
            delete self.dom.dataset.livePct;
            if (pct >= MIN_PCT && pct <= 100) self.commitWidth(pct);
        };
        this.handle.addEventListener("pointerup", finish);
        this.handle.addEventListener("pointercancel", finish);
    };

    ImageView.prototype.update = function (node) {
        if (!node.type || node.type.name !== "image") return false;
        this.node = node;
        this.sync();
        return true;
    };
    ImageView.prototype.ignoreMutation = function () { return true; };
    ImageView.prototype.stopEvent = function () { return true; };
    ImageView.prototype.destroy = function () {
        document.removeEventListener("click", this._docClick, true);
    };

    function nodeView(node, view, getPos) {
        return new ImageView(node, view, getPos);
    }

    // applyWidthAttrs decorates Toast UI's markdown-preview <img>
    // rendering with the fragment width (wikilink.js merges this
    // into the customHTMLRenderer).
    function imageRenderer(node, context) {
        var result = context.origin();
        var pct = getWidthPct(node.destination || "");
        if (pct != null && result && result.attributes) {
            result.attributes.style = "width:" + pct + "%;" + (result.attributes.style || "");
        }
        return result;
    }

    window.familiarImageResize = {
        nodeView: nodeView,
        imageRenderer: imageRenderer,
        getWidthPct: getWidthPct,
        setWidthPct: setWidthPct,
    };
})();
