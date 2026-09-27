// mermaid-fences.spec.ts — mermaid-blocks.js run in a node:vm context
// with a stub window and mermaid: the fence scanner the diagram tab and
// the share renders use, and the render queue. No browser needed.

import { test, expect } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import * as vm from "node:vm";

const STATIC = path.join(__dirname, "../../../familiar-workspace/static");
const BLOCKS = fs.readFileSync(path.join(STATIC, "mermaid-blocks.js"), "utf8");

function load(mermaid?: any) {
    const removed: string[] = [];
    const elements: Record<string, { remove: () => void }> = {};
    const add = (id: string) => { elements[id] = { remove: () => { removed.push(id); delete elements[id]; } }; };
    const window: any = { mermaid, crypto: (globalThis as any).crypto };
    const document: any = {
        getElementById: (id: string) => elements[id] || null,
        createElement: () => ({ classList: { add() {}, remove() {} }, appendChild() {} }),
    };
    // Enough for syncShareRenders to reach the rasterizer: an empty media
    // listing. Its canvas step then fails (no DOMParser), harmlessly.
    const fetch = async () => ({ ok: true, json: async () => ({ items: [] }) });
    const ctx = vm.createContext({
        window, document, console, Promise, setTimeout, fetch,
        crypto: (globalThis as any).crypto, TextEncoder, DOMParser: undefined,
    });
    vm.runInContext(BLOCKS, ctx);
    return { fm: window.familiarMermaid, elements, removed, add, window };
}

const PAGE = [
    "# How to write a diagram",
    "",
    "````markdown",
    "```mermaid",
    "graph TD; EXAMPLE-->ONLY;",
    "```",
    "````",
    "",
    "The real one:",
    "",
    "```Mermaid",
    "graph TD; REAL-->ONE;",
    "```",
    "",
    "~~~mermaid",
    "graph TD; TILDE-->TWO;",
    "~~~",
    "",
].join("\n");

// The editor counts mermaid code blocks as its parser sees them; the
// diagram tab counted regex matches of "```mermaid", which also matched
// the example inside the ````markdown block, missed ~~~ and ```Mermaid,
// and so Save overwrote the example.
test("mermaid fences are counted as the editor counts them", () => {
    const { fm } = load();
    const fences = fm.fences(PAGE);
    expect(fences.map((f: any) => f.body.trim())).toEqual(["graph TD; REAL-->ONE;", "graph TD; TILDE-->TWO;"]);
});

// Replacing fence 0 changes the real diagram, keeps its own fence lines,
// and leaves the example alone.
test("replacing a fence edits that diagram only", () => {
    const { fm } = load();
    const diagram = fs.readFileSync(path.join(STATIC, "diagram.js"), "utf8");
    const ctx = vm.createContext({
        window: { familiarMermaid: fm, FamiliarWorkspace: null, addEventListener() {}, familiarAppHelpers: {} },
        document: { createElement: () => ({}), addEventListener() {} },
        console,
    });
    // diagram.js keeps replaceFence private; evaluate it from its source.
    const src = diagram.slice(diagram.indexOf("    function fences(content)"), diagram.indexOf("    async function saveShell("));
    vm.runInContext(src + "\nthis.replaceFence = replaceFence; this.fenceBody = fenceBody;", ctx);
    const out = (ctx as any).replaceFence(PAGE, 0, "graph TD; EDITED-->YES;");
    expect(out).toContain("```Mermaid\ngraph TD; EDITED-->YES;\n```");
    expect(out).toContain("graph TD; EXAMPLE-->ONLY;");
    expect(out).toContain("~~~mermaid\ngraph TD; TILDE-->TWO;\n~~~");
    expect((ctx as any).fenceBody(PAGE, 1).trim()).toBe("graph TD; TILDE-->TWO;");
    expect((ctx as any).replaceFence(PAGE, 5, "x")).toBeNull();
});

// Renders go one at a time: a share rasterization sets mermaid's loose
// config around its render, and an interactive render that started
// meanwhile ran under loose (no final sanitizing, raw click hrefs).
test("an interactive render never runs under the share renderer's loose config", async () => {
    let level = "strict";
    const seen: { id: string; level: string }[] = [];
    const mermaid = {
        initialize: (cfg: any) => { level = cfg.securityLevel; },
        render: async (id: string) => {
            seen.push({ id, level });
            await new Promise((r) => setTimeout(r, 30));
            return { svg: "<svg viewBox='0 0 10 10'></svg>" };
        },
    };
    const { fm } = load(mermaid);
    // syncShareRenders needs crypto and fetch; give it enough to reach
    // the rasterizer, whose later canvas step may fail harmlessly.
    const share = fm.syncShareRenders({ bookSlug: "b", pageId: "p" }, "```mermaid\ngraph TD; A-->B;\n```\n").catch(() => {});
    await new Promise((r) => setTimeout(r, 5));
    const el: any = { classList: { add() {}, remove() {} }, getAttribute: () => null, setAttribute() {} };
    const interactive = fm.renderBlock(el, "graph TD; C-->D;");
    await Promise.allSettled([share, interactive]);
    const block = seen.find((s) => s.id.startsWith("familiar-mmd-"));
    expect(block, JSON.stringify(seen)).toBeTruthy();
    expect(block!.level).toBe("strict");
});

// A failed render cleans up after itself only. Its catch removed
// "familiar-mmd-" + the shared counter, by then the id of the next
// block's render in progress: the valid diagram failed too.
test("a failed diagram doesn't take the next one down with it", async () => {
    let add: (id: string) => void = () => {};
    const mermaid = {
        initialize() {},
        render: async (id: string, src: string) => {
            add("d" + id);
            add(id);
            await new Promise((r) => setTimeout(r, 20));
            if (src.includes("{{{")) throw new Error("Parse error");
            return { svg: "<svg></svg>" };
        },
    };
    const env = load(mermaid);
    add = env.add;
    const el = () => ({ classList: { add() {}, remove() {} }, innerHTML: "", textContent: "", title: "" });
    const bad = el(), good = el();
    await Promise.all([env.fm.renderBlock(bad, "not mermaid {{{"), env.fm.renderBlock(good, "graph TD; A-->B;")]);
    expect(env.removed.sort()).toEqual(["dfamiliar-mmd-1", "familiar-mmd-1"]);
    expect(good.innerHTML).toBe("<svg></svg>");
});

// Rich Text writes a code block back with a fence no line inside it
// can close: Toast UI always wrote ```, which broke any block showing
// fenced code.
test("code blocks are written back with a fence their content can't close", () => {
    const { fm } = load();
    expect(fm.fenceFor("plain code")).toBe("```");
    expect(fm.fenceFor("```mermaid\ngraph TD; A-->B;\n```")).toBe("````");
    expect(fm.fenceFor("  ````\nx")).toBe("`````");
    expect(fm.fenceFor("a ``` in the middle")).toBe("```");
    expect(fm.fenceFor("    ```` indented four is code, not a fence")).toBe("```");
});
