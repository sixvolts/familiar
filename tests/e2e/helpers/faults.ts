// faults.ts — deterministic network fault injection via Playwright
// request interception. Lets resilience specs simulate a down model,
// a 5xx gateway, a dropped connection, or an SSE error frame WITHOUT a
// live llama-server. Match by pathname so a fault on /api/chat never
// touches the conversation-create or message-persist calls.

import { Page, Route } from "@playwright/test";

type Matcher = (pathname: string) => boolean;

function pathMatcher(path: string): Matcher {
    return (pathname) => pathname === path;
}

// failPath aborts every request whose pathname matches — simulates a
// dropped connection / unreachable host (the fetch rejects).
export async function failPath(page: Page, path: string): Promise<void> {
    const match = pathMatcher(path);
    await page.route("**/*", async (route: Route) => {
        const url = new URL(route.request().url());
        if (match(url.pathname)) {
            await route.abort("failed");
            return;
        }
        await route.fallback();
    });
}

// status500Path fulfills a matching request with an HTTP error + body,
// simulating a gateway/model 5xx.
export async function status500Path(page: Page, path: string, body = "model unavailable"): Promise<void> {
    const match = pathMatcher(path);
    await page.route("**/*", async (route: Route) => {
        const url = new URL(route.request().url());
        if (match(url.pathname)) {
            await route.fulfill({ status: 503, contentType: "text/plain", body });
            return;
        }
        await route.fallback();
    });
}

// sseErrorPath fulfills a matching request with a well-formed SSE
// stream that carries a single `event: error` frame — exercises the
// in-band error path (vs. a transport failure). The body is delivered
// whole; the client's frame parser handles it the same as a live
// stream.
export async function sseErrorPath(page: Page, path: string, message = "model exploded"): Promise<void> {
    const match = pathMatcher(path);
    const body =
        "event: session\ndata: {}\n\n" +
        `event: error\ndata: ${JSON.stringify({ message })}\n\n`;
    await page.route("**/*", async (route: Route) => {
        const url = new URL(route.request().url());
        if (match(url.pathname)) {
            await route.fulfill({ status: 200, contentType: "text/event-stream", body });
            return;
        }
        await route.fallback();
    });
}
