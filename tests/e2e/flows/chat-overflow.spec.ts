// chat-overflow.spec.ts — the chat panel must never scroll horizontally.
//
// Three fixes for this shipped on reasoning rather than measurement, and two
// made it worse (one clipped the text with overflow-x:hidden, removing any way
// to reach what was cut off). This test measures instead: it seeds a
// deliberately wide assistant message — a table with long cells, an unbroken
// 300-char URL, and a 400-char code line — then asserts nothing in the chat
// panel has scrollWidth > clientWidth.
//
// A TABLE or PRE scrolling inside ITSELF is the intended design and is
// allowed; anything else overflowing is the bug. On failure the assertion
// message names every offender with its widths, so the culprit is identified
// rather than guessed.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession } from "../fixtures/user";

const test = base.extend<{}, { stack: GatewayStack }>({
    stack: [
        async ({}, use) => {
            const stack = await start({ admin: true });
            await use(stack);
            await stack.stop();
        },
        { scope: "worker" },
    ],
});

const WIDE = [
    "Here is a wide table:",
    "",
    "| Part | Ship | Issue / execution | L1 | Notes |",
    "|---|---|---|---|---|",
    "| Pentium P5 | Mar 1993 | Dual-issue in-order, U/V pipes, retires in order, no renaming | 4K I + 4K D | first superscalar x86 |",
    "| PowerPC 603 / 603e | 1993 | In-order, limited dual-issue, 5-stage pipeline with folding | 32K I + 32K D | low power target |",
    "| MIPS R4600 / RM5200 | 1997 | In-order dual-issue with a deep write buffer | on-chip unified | embedded focus |",
    "",
    "Unbroken token: https://example.com/" + "a".repeat(300),
    "",
    "```",
    "x".repeat(400),
    "```",
].join("\n");

test("the chat panel never scrolls horizontally", async ({ stack, browser, request }) => {
    const user = await createTestUser({ role: "admin" });
    const authed = { Cookie: user.cookieHeader, "Content-Type": "application/json" };

    const conv = await (
        await request.post(`${stack.workspaceURL}/console/api/conversations`, {
            headers: authed,
            data: { title: "Overflow probe", model: "familiar" },
        })
    ).json();
    await request.post(`${stack.workspaceURL}/console/api/conversations/${conv.id}/messages`, {
        headers: authed,
        data: { role: "user", content: "show me a wide table" },
    });
    await request.post(`${stack.workspaceURL}/console/api/conversations/${conv.id}/messages`, {
        headers: authed,
        data: { role: "assistant", content: WIDE },
    });

    const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-chat").click();
        await page.evaluate(
            (id) => window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat", id } })),
            conv.id,
        );

        const shell = page.locator(".chat-shell", { hasText: "Unbroken token" });
        await expect(shell).toBeVisible({ timeout: 15_000 });
        await page.waitForTimeout(600); // let layout settle

        // Diagnostic: name the widest descendants of the bubble outright,
        // so the offending element is identified rather than inferred.
        const widest = await page.evaluate(() => {
            const bubble = document.querySelector(".chat-msg-assistant");
            if (!bubble) return ["no bubble"];
            return Array.from(bubble.querySelectorAll("*"))
                .map((el) => {
                    const cls = typeof el.className === "string" ? el.className : "";
                    const w = Math.round(el.getBoundingClientRect().width);
                    const txt = (el.textContent || "").slice(0, 32).replace(/\s+/g, " ");
                    return el.tagName + "." + cls + " scrollW=" + el.scrollWidth +
                           " boxW=" + w + " :: " + txt;
                })
                .sort((a, b) => {
                    const na = Number(a.match(/scrollW=(\d+)/)[1]);
                    const nb = Number(b.match(/scrollW=(\d+)/)[1]);
                    return nb - na;
                })
                .slice(0, 10);
        });
        console.log("WIDEST DESCENDANTS:\n  " + widest.join("\n  "));

        const offenders = await page.evaluate(() => {
            const out = [];
            const root = document.querySelector(".chat-messages");
            if (!root) return ["NO .chat-messages FOUND"];
            const nodes = Array.from(root.querySelectorAll("*"));
            for (let el = root; el; el = el.parentElement) nodes.push(el);
            for (const el of nodes) {
                if (el.scrollWidth > el.clientWidth + 1) {
                    const cls = typeof el.className === "string" ? el.className : "";
                    out.push(el.tagName + "." + cls +
                        " scroll=" + el.scrollWidth + " client=" + el.clientWidth);
                }
            }
            return out;
        });

        // TABLE / PRE / the thinking trace scrolling inside themselves is by design.
        const bad = offenders.filter(
            (o) => !/^(TABLE|PRE|CODE)\./.test(o) && !/chat-msg-thinking-body/.test(o),
        );
        expect(bad, "unexpected horizontal overflow:\n" + offenders.join("\n")).toEqual([]);
    } finally {
        await ctx.close();
    }
});
