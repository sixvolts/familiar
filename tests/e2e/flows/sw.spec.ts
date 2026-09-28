// sw.spec.ts — the mobile service worker (familiar-workspace/static/sw.js)
// run in a node:vm context with a stubbed `self`, CacheStorage and
// fetch: no stack, no browser, no push service. Covers what a headless
// browser can't reach: push display, subscription rotation, and the
// caching policy.

import { test, expect } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import * as vm from "node:vm";

const SW = fs.readFileSync(path.join(__dirname, "../../../familiar-workspace/static/sw.js"), "utf8");
const ORIGIN = "https://app.example";

type Handler = (e: any) => void;

function loadSW(opts: { fetch?: (req: any, init?: any) => Promise<Response> } = {}) {
    const handlers: Record<string, Handler> = {};
    const shown: any[] = [];
    const fetches: { url: string; init?: any; method: string; body?: string }[] = [];
    const store = new Map<string, Map<string, Response>>();
    const cacheFor = (name: string) => {
        if (!store.has(name)) store.set(name, new Map());
        const m = store.get(name)!;
        const key = (r: any) => (typeof r === "string" ? new URL(r, ORIGIN).href : r.url);
        return {
            put: async (r: any, resp: Response) => { m.set(key(r), resp); },
            match: async (r: any) => m.get(key(r)),
            delete: async (r: any) => m.delete(key(r)),
            keys: async () => [...m.keys()].map((u) => new Request(u)),
            addAll: async (reqs: any[]) => { for (const r of reqs) m.set(key(r), new Response("precached")); },
        };
    };
    const caches = {
        open: async (name: string) => cacheFor(name),
        keys: async () => [...store.keys()],
        delete: async (name: string) => store.delete(name),
        match: async (r: any) => {
            for (const name of store.keys()) {
                const hit = await cacheFor(name).match(r);
                if (hit) return hit;
            }
            return undefined;
        },
    };
    const fetchImpl = async (input: any, init?: any) => {
        const req = typeof input === "string" ? new Request(new URL(input, ORIGIN).href, init) : input;
        const body = init && init.body;
        fetches.push({ url: req.url, init, method: (init && init.method) || req.method, body });
        return opts.fetch ? opts.fetch(req, init) : new Response("net:" + req.url, { status: 200 });
    };
    const self: any = {
        location: new URL(ORIGIN + "/sw.js"),
        addEventListener: (type: string, fn: Handler) => { handlers[type] = fn; },
        skipWaiting: async () => {},
        clients: { claim: async () => {} },
        registration: {
            showNotification: async (title: string, options: any) => { shown.push({ title, options }); },
            pushManager: { subscribe: async (o: any) => ({ toJSON: () => ({ endpoint: "https://push.example/new", keys: { p256dh: "k", auth: "a" } }), options: o }) },
        },
    };
    const ctx = vm.createContext({ self, caches, fetch: fetchImpl, Request, Response, URL, console, Promise, setTimeout });
    vm.runInContext(SW, ctx);
    return { handlers, shown, fetches, store, self };
}

// Dispatch an event with waitUntil/respondWith, and settle everything.
async function dispatch(h: Handler, extra: any) {
    const waits: Promise<any>[] = [];
    let responded: Promise<Response> | undefined;
    const e = { ...extra, waitUntil: (p: Promise<any>) => waits.push(p), respondWith: (p: Promise<Response>) => { responded = p; } };
    h(e);
    const resp = responded ? await responded : undefined;
    await Promise.all(waits);
    return resp;
}

function fetchEvent(url: string, mode = "no-cors") {
    const request = new Request(new URL(url, ORIGIN).href);
    Object.defineProperty(request, "mode", { value: mode });
    return { request };
}

// A tagged notification that replaces an unread one alerts again
// (renotify); without it a recurring action's second alert was silent.
test("a push with a tag renotifies", async () => {
    const sw = loadSW();
    await dispatch(sw.handlers.push, { data: { json: () => ({ title: "Digest", body: "b", tag: "action:1", url: "/#chat/x" }) } });
    expect(sw.shown).toHaveLength(1);
    expect(sw.shown[0].options.tag).toBe("action:1");
    expect(sw.shown[0].options.renotify).toBe(true);
});

// A replaced subscription is re-registered with the gateway and the old
// endpoint dropped (the handler batch 10 added, which nothing ran).
test("a rotated push subscription is re-registered", async () => {
    const sw = loadSW();
    await dispatch(sw.handlers.pushsubscriptionchange, {
        oldSubscription: { endpoint: "https://push.example/old", options: { userVisibleOnly: true } },
        newSubscription: null,
    });
    const post = sw.fetches.find((f) => f.method === "POST");
    const del = sw.fetches.find((f) => f.method === "DELETE");
    expect(post && JSON.parse(post.body!).endpoint).toBe("https://push.example/new");
    expect(del && JSON.parse(del.body!).endpoint).toBe("https://push.example/old");
});

// A new stamped copy of an asset evicts the old ones: every deploy that
// changed mobile.js used to add another full copy to CacheStorage.
test("a new stamped asset evicts its older copies", async () => {
    const sw = loadSW();
    await dispatch(sw.handlers.fetch, fetchEvent("/mobile.js?v=aaa"));
    await dispatch(sw.handlers.fetch, fetchEvent("/mobile.js?v=bbb"));
    const keys = [...[...sw.store.values()][0].keys()];
    expect(keys.filter((k) => k.includes("/mobile.js"))).toEqual([ORIGIN + "/mobile.js?v=bbb"]);
});

// A stamped asset in the cache is served from it without a request.
test("a cached stamped asset makes no request", async () => {
    const sw = loadSW();
    await dispatch(sw.handlers.fetch, fetchEvent("/mobile.css?v=abc"));
    const before = sw.fetches.length;
    const resp = await dispatch(sw.handlers.fetch, fetchEvent("/mobile.css?v=abc"));
    expect(await resp!.text()).toBe("net:" + ORIGIN + "/mobile.css?v=abc");
    expect(sw.fetches.length).toBe(before);
});

// An unversioned file (a vendor script) is served from the cache but
// revalidated, so a replaced file reaches the app on its next load. It
// was cache-first forever: a DOMPurify security fix never arrived.
test("an unversioned asset is revalidated in the background", async () => {
    let version = 1;
    const sw = loadSW({ fetch: async () => new Response("purify v" + version) });
    let resp = await dispatch(sw.handlers.fetch, fetchEvent("/vendor/dompurify/purify.min.js"));
    expect(await resp!.text()).toBe("purify v1");
    version = 2;
    resp = await dispatch(sw.handlers.fetch, fetchEvent("/vendor/dompurify/purify.min.js"));
    expect(await resp!.text()).toBe("purify v1"); // served from cache…
    const revalidate = sw.fetches[sw.fetches.length - 1];
    expect(revalidate.init && revalidate.init.cache).toBe("no-cache");
    resp = await dispatch(sw.handlers.fetch, fetchEvent("/vendor/dompurify/purify.min.js"));
    expect(await resp!.text()).toBe("purify v2"); // …and the next load has the new file
});

// Navigations are cached by path: /?_r=<time> (pull-to-refresh) added a
// copy of the shell each time.
test("navigations are cached without their query string", async () => {
    const sw = loadSW();
    await dispatch(sw.handlers.fetch, fetchEvent("/?_r=1", "navigate"));
    await dispatch(sw.handlers.fetch, fetchEvent("/?_r=2", "navigate"));
    const keys = [...[...sw.store.values()][0].keys()];
    expect(keys).toEqual([ORIGIN + "/"]);
});
