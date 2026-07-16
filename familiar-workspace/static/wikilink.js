// Toast UI Editor wiki-link integration.
//
// Dual-mode approach:
//   Rich Text — Toast UI WYSIWYG with widgetRules. [[links]]
//     render as "French Toast" widget chips. Click to navigate.
//   Markdown — Toast UI Markdown WITHOUT widgetRules. [[links]]
//     stay as raw editable text in the editor pane; the preview
//     pane renders them as styled links via customHTMLRenderer.
//
// Each surface manages its own mode toggle. On switch, the editor
// is destroyed and recreated with the appropriate config. Content
// is synced via getMarkdown() / setMarkdown().

(function () {
    "use strict";

    var WIKI_LINK_RE_SRC = "\\[\\[([^\\]]+)\\]\\]";

    function slugify(s) {
        s = s.toLowerCase().trim()
             .replace(/[^a-z0-9]+/g, "-")
             .replace(/^-+|-+$/g, "");
        if (s === "") return "untitled";
        if (s.length > 60) {
            s = s.slice(0, 60);
            var i = s.lastIndexOf("-");
            if (i > 30) s = s.slice(0, i);
        }
        return s;
    }

    function parseTarget(inner) {
        var trimmed = String(inner || "").trim();
        if (trimmed === "") return null;
        // Detect URLs inside [[...]] — they're external links, not page refs.
        if (/^https?:\/\//i.test(trimmed)) {
            return { isExternal: true, href: trimmed, displayText: trimmed };
        }
        var target = trimmed, displayText = "";
        var pipe = target.indexOf("|");
        if (pipe >= 0) {
            displayText = target.slice(pipe + 1).trim();
            target = target.slice(0, pipe).trim();
        }
        var bookPart = "", pagePart = target;
        var originalBookName = "";
        var originalPageName = target;
        // Accept both / and \ as book/page separators.
        var slash = target.indexOf("/");
        if (slash < 0) slash = target.indexOf("\\");
        if (slash >= 0) {
            bookPart = target.slice(0, slash).trim();
            pagePart = target.slice(slash + 1).trim();
            originalBookName = bookPart;
            originalPageName = pagePart;
        }
        var bookSlug = bookPart ? slugify(bookPart) : "";
        var pageSlug = slugify(pagePart);
        if (pageSlug === "") return null;
        if (!displayText) displayText = originalPageName;
        return {
            bookSlug: bookSlug,
            pageSlug: pageSlug,
            displayText: displayText,
            originalBookName: originalBookName,
            isCrossBook: bookSlug !== "",
        };
    }

    // ── Widget rule (Rich Text mode only) ─────────────────────

    function makeWidgetRule(navigateFn, getBookContext) {
        return {
            rule: /\[\[([^\]]+)\]\]/,
            toDOM: function (text) {
                var m = /\[\[([^\]]+)\]\]/.exec(text);
                if (!m) return document.createTextNode(text);
                var parsed = parseTarget(m[1]);
                if (!parsed) return document.createTextNode(text);

                // [[https://...]] — render as an external link, not a wiki chip.
                if (parsed.isExternal) {
                    var extEl = document.createElement("a");
                    extEl.className = "ext-link";
                    extEl.href = parsed.href;
                    extEl.target = "_blank";
                    extEl.rel = "noopener noreferrer";
                    extEl.textContent = parsed.displayText || parsed.href;
                    extEl.addEventListener("click", function (e) {
                        e.preventDefault();
                        e.stopPropagation();
                        window.open(parsed.href, "_blank", "noopener,noreferrer");
                    });
                    return extEl;
                }

                var ctx = typeof getBookContext === "function"
                    ? getBookContext() : { slug: "", name: "" };
                if (typeof ctx === "string") ctx = { slug: ctx, name: ctx };
                var showDot = parsed.bookSlug && parsed.bookSlug !== ctx.slug;

                // Tooltip: "Book Name: Page Name"
                var bookLabel = showDot
                    ? (parsed.originalBookName || parsed.bookSlug)
                    : (ctx.name || ctx.slug);

                var el = document.createElement("a");
                el.className = "wiki-link";
                el.href = "#";
                el.dataset.bookSlug = parsed.bookSlug;
                el.dataset.pageSlug = parsed.pageSlug;
                el.title = bookLabel + ": " + parsed.displayText;

                if (showDot) {
                    var dot = document.createElement("span");
                    dot.className = "wiki-link-dot";
                    dot.textContent = "•";
                    el.appendChild(dot);
                }

                var label = document.createTextNode(parsed.displayText);
                el.appendChild(label);

                el.addEventListener("click", function (e) {
                    e.preventDefault();
                    e.stopPropagation();
                    if (typeof navigateFn === "function") {
                        navigateFn(parsed);
                    }
                });
                return el;
            },
        };
    }

    // ── Custom HTML renderer (Markdown preview) ───────────────

    function makeCustomHTMLRenderer(getBookContext) {
        return {
            // ```mermaid fences render as diagrams (mermaid-blocks.js,
            // MEDIA-DIAGRAMS Phase 0). Everything else falls through
            // to Toast UI's default code block.
            codeBlock: function (node, context) {
                if (window.familiarMermaid) {
                    return window.familiarMermaid.codeBlock(node, context);
                }
                return context.origin();
            },
            // #w=NN fragment widths apply in markdown previews too
            // (image-resize.js owns the fragment convention).
            image: function (node, context) {
                if (window.familiarImageResize) {
                    return window.familiarImageResize.imageRenderer(node, context);
                }
                return context.origin();
            },
            text: function (node) {
                var content = node.literal || "";
                if (content.indexOf("[[") < 0) {
                    return { type: "text", content: content };
                }
                var ctx = typeof getBookContext === "function"
                    ? getBookContext() : { slug: "", name: "" };
                if (typeof ctx === "string") ctx = { slug: ctx, name: ctx };
                var parts = [];
                var lastIndex = 0;
                var re = new RegExp(WIKI_LINK_RE_SRC, "g");
                var m;
                while ((m = re.exec(content)) !== null) {
                    if (m.index > lastIndex) {
                        parts.push({ type: "text", content: content.slice(lastIndex, m.index) });
                    }
                    var parsed = parseTarget(m[1]);
                    if (parsed && parsed.isExternal) {
                        parts.push({
                            type: "openTag",
                            tagName: "a",
                            attributes: {
                                class: "ext-link",
                                href: parsed.href,
                                target: "_blank",
                                rel: "noopener noreferrer",
                            },
                        });
                        parts.push({ type: "text", content: parsed.displayText || parsed.href });
                        parts.push({ type: "closeTag", tagName: "a" });
                    } else if (parsed) {
                        var showDot = parsed.bookSlug && parsed.bookSlug !== ctx.slug;
                        parts.push({
                            type: "openTag",
                            tagName: "a",
                            attributes: {
                                class: "wiki-link",
                                href: "#",
                                "data-book-slug": parsed.bookSlug,
                                "data-page-slug": parsed.pageSlug,
                            },
                        });
                        if (showDot) {
                            parts.push({ type: "openTag", tagName: "span", attributes: { class: "wiki-link-dot" } });
                            parts.push({ type: "text", content: "•" });
                            parts.push({ type: "closeTag", tagName: "span" });
                        }
                        parts.push({ type: "text", content: parsed.displayText });
                        parts.push({ type: "closeTag", tagName: "a" });
                    } else {
                        parts.push({ type: "text", content: m[0] });
                    }
                    lastIndex = re.lastIndex;
                }
                if (lastIndex < content.length) {
                    parts.push({ type: "text", content: content.slice(lastIndex) });
                }
                return parts;
            },
            // Regular markdown links — [text](url) — need to open in
            // a new window so the user doesn't clobber their Familiar
            // session by navigating away. Toast UI's default link
            // renderer emits a naked <a href="url"> with no target,
            // which means a click would either (a) replace the whole
            // SPA in markdown-preview mode or (b) be swallowed by
            // contenteditable in WYSIWYG. Both are wrong.
            link: function (node, context) {
                if (context.entering) {
                    var href = node.destination || "";
                    var attrs = { href: href };
                    if (node.title) attrs.title = node.title;
                    // External http(s) links get the new-tab treatment.
                    // Same-document anchors (#section) stay in-page.
                    if (/^https?:/i.test(href)) {
                        attrs.target = "_blank";
                        attrs.rel = "noopener noreferrer";
                        attrs["class"] = "ext-link";
                    }
                    return { type: "openTag", tagName: "a", attributes: attrs };
                }
                return { type: "closeTag", tagName: "a" };
            },
        };
    }

    // ── Editor config builder ─────────────────────────────────
    //
    // Returns Toast UI constructor options for the given mode.
    // Rich Text: widgetRules (links as chips) + customHTMLRenderer.
    // Markdown: customHTMLRenderer only (raw [[]] is editable,
    //           preview shows styled links).
    // Both modes hide Toast UI's built-in mode switch — the
    // surface provides its own toggle.

    function editorOptions(mode, navigateFn, getBookContext) {
        var base = {
            height: "100%",
            previewStyle: "tab",
            theme: "dark",
            placeholder: "Start writing…",
            usageStatistics: false,
            hideModeSwitch: true,
            toolbarItems: [],
            customHTMLRenderer: makeCustomHTMLRenderer(getBookContext),
            // Mermaid blocks render inline in Rich Text (read-only;
            // click opens a diagram tab). mermaid-blocks.js.
            plugins: window.familiarMermaid && window.familiarMermaid.editorPlugin
                ? [window.familiarMermaid.editorPlugin]
                : [],
        };
        if (mode === "wysiwyg") {
            base.initialEditType = "wysiwyg";
            base.widgetRules = [makeWidgetRule(navigateFn, getBookContext)];
        } else {
            base.initialEditType = "markdown";
            // Preview is behind a tab, not side-by-side. (A vertical
            // split was briefly tried for MEDIA-DIAGRAMS Phase 0 and
            // reverted — the surfaces flip whole-mode via the bottom
            // tabs; this isn't a typesetting UI. Diagram exposure on
            // desktop is an open design question — likely its own
            // workspace tab.)
            base.previewStyle = "tab";
        }
        return base;
    }

    // ── Click handler (Markdown preview <a> tags) ─────────────
    //
    // Two link classes to intercept:
    //   1. a.wiki-link — internal [[Page]] navigation; preventDefault
    //      and hand off to the per-surface navigateFn.
    //   2. external http(s) anchors — open in a new tab via
    //      window.open. In WYSIWYG mode the editor is contenteditable,
    //      which swallows the default <a> click and leaves the link
    //      "dead"; in markdown-preview mode the default would replace
    //      the SPA. Forcing window.open with noopener gives the user
    //      a stable Familiar session AND opens the destination.

    function wireClickHandler(containerEl, navigateFn) {
        if (!containerEl) return;
        containerEl.addEventListener("click", function (e) {
            var wiki = e.target.closest("a.wiki-link");
            if (wiki) {
                e.preventDefault();
                e.stopPropagation();
                if (typeof navigateFn === "function") {
                    navigateFn({
                        bookSlug: wiki.dataset.bookSlug || "",
                        pageSlug: wiki.dataset.pageSlug || "",
                        displayText: wiki.textContent || "",
                    });
                }
                return;
            }
            var ext = e.target.closest("a");
            if (!ext) return;
            var href = ext.getAttribute("href") || "";
            if (!/^https?:/i.test(href)) return;
            e.preventDefault();
            e.stopPropagation();
            window.open(href, "_blank", "noopener,noreferrer");
        });
    }

    // ── Image uploads (MEDIA-DIAGRAMS Phase 1) ─────────────────
    // Pasting / dropping an image into any editor uploads it to the
    // page's media endpoint instead of Toast UI's default — which is
    // base64 straight into the markdown, bloating page content and
    // every backup. getPageContext returns {bookSlug, pageId} for
    // the open doc, or null when nothing is open to attach to.
    function mediaNotify(msg) {
        var h = window.familiarAppHelpers;
        if (h && h.toast) h.toast(msg, "error");
        else console.warn("[media] " + msg);
    }

    // uploadImage POSTs one blob to a page's media endpoint and
    // resolves {url, thumb_url, alt_text, ...}. Shared by the paste
    // hook and the ⋯-menu "Add image" flow.
    function uploadImage(ctx, blob) {
        var form = new FormData();
        form.append("file", blob, (blob && blob.name) || "image");
        return fetch("/console/api/books/" + encodeURIComponent(ctx.bookSlug) +
              "/page-by-id/" + encodeURIComponent(ctx.pageId) + "/media", {
            method: "POST",
            credentials: "include",
            body: form,
        }).then(function (r) {
            if (!r.ok) {
                return r.text().then(function (t) {
                    var msg = t;
                    try { msg = JSON.parse(t).error || t; } catch (_) { /* raw */ }
                    throw new Error(msg || ("upload failed (" + r.status + ")"));
                });
            }
            return r.json();
        });
    }

    function wireImageUpload(editorInstance, getPageContext) {
        if (!editorInstance || !editorInstance.addHook) return;
        editorInstance.addHook("addImageBlobHook", function (blob, callback) {
            var ctx = typeof getPageContext === "function" ? getPageContext() : null;
            if (!ctx || !ctx.pageId) {
                mediaNotify("Open a page before adding images.");
                return false;
            }
            uploadImage(ctx, blob).then(function (d) {
                callback(d.url, d.alt_text || (blob && blob.name) || "image");
            }).catch(function (e) {
                mediaNotify("Image upload failed: " + (e.message || e));
            });
            return false; // never fall through to base64 insertion
        });
    }

    window.familiarWikiLink = {
        parseTarget: parseTarget,
        slugify: slugify,
        makeWidgetRule: makeWidgetRule,
        makeCustomHTMLRenderer: makeCustomHTMLRenderer,
        editorOptions: editorOptions,
        wireClickHandler: wireClickHandler,
        wireImageUpload: wireImageUpload,
        uploadImage: uploadImage,
    };
})();
