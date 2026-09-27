// sanitize.js — the one HTML policy for rendered markdown (chat, notes,
// wiki, mobile) and for the TOAST UI editor's preview.
//
// DOMPurify's defaults keep more than rendered markdown needs, and some
// of it is dangerous without any script: model output (steered by a web
// page it read) or a shared wiki page could carry a <style> block that
// restyles the whole workspace, or a <form>/<button> that looks like the
// app and posts off-site. So on top of DOMPurify:
//
//   - no <style>, no form controls (form, button, textarea, select, …);
//   - <input> only as a disabled checkbox (marked's task lists);
//   - no action/formaction attributes;
//   - a style attribute only when it is a plain "width: N%" (image
//     sizing, see image-resize.js), so nothing can position an overlay.
//
// The editor also stops using the DOMPurify 2.3.3 copy embedded in the
// TOAST UI bundle (known sanitizer bypasses) and uses this one.
(function () {
    "use strict";

    var FORBID_TAGS = [
        "style", "form", "button", "textarea", "select", "option", "optgroup",
        "datalist", "fieldset", "link", "meta", "base", "title", "object",
    ];
    var FORBID_ATTR = ["action", "formaction", "form"];
    var WIDTH_ONLY = /^\s*width\s*:\s*\d{1,3}(\.\d+)?%\s*;?\s*$/i;

    // DOMPurify hooks belong to one instance, and a module may load a
    // second copy; hook whichever instance is current.
    var hooked = null;
    function purifier() {
        var dp = window.DOMPurify;
        if (!dp) return null;
        if (hooked !== dp) {
            dp.addHook("uponSanitizeElement", function (node, data) {
                if (data.tagName !== "input") return;
                var ok = (node.getAttribute("type") || "").toLowerCase() === "checkbox" &&
                    node.hasAttribute("disabled");
                if (!ok && node.parentNode) node.parentNode.removeChild(node);
            });
            dp.addHook("uponSanitizeAttribute", function (_node, data) {
                if (data.attrName === "style" && !WIDTH_ONLY.test(data.attrValue || "")) {
                    data.keepAttr = false;
                }
            });
            hooked = dp;
        }
        return dp;
    }

    // Without DOMPurify nothing is rendered as HTML: the text is shown
    // escaped, rather than blank (or unsanitized).
    function escaped(html) {
        return '<pre class="chat-md-fallback">' + String(html || "")
            .replace(/&/g, "&amp;").replace(/</g, "&lt;")
            .replace(/>/g, "&gt;").replace(/"/g, "&quot;") + "</pre>";
    }

    // markdown sanitizes marked's output for display.
    function markdown(html) {
        var dp = purifier();
        if (!dp) return escaped(html);
        return dp.sanitize(html, {
            ADD_ATTR: ["class"], // code-block classes for highlighting
            FORBID_TAGS: FORBID_TAGS,
            FORBID_ATTR: FORBID_ATTR,
        });
    }

    // editor is TOAST UI's customHTMLSanitizer. It starts from the
    // options the editor's own sanitizer uses (rel/target/hreflang/type
    // on links, no <input>: its task lists aren't inputs) and adds ours.
    function editor(html) {
        var dp = purifier();
        if (!dp) return escaped(html);
        return dp.sanitize(html, {
            ADD_ATTR: ["rel", "target", "hreflang", "type", "class"],
            FORBID_TAGS: FORBID_TAGS.concat(["input", "script"]),
            FORBID_ATTR: FORBID_ATTR,
        });
    }

    window.familiarSanitize = { markdown: markdown, editor: editor };
})();
