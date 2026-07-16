// weather-location.spec.ts — the Home weather widget must remember the
// coordinates a device shares and reuse them on the next load instead of
// re-triggering browser geolocation. iOS Safari / a home-screen PWA does
// NOT persist the geolocation permission across launches, so without this
// the widget re-prompted on every visit (the iPad-kiosk bug).
//
// The test stack configures no weather backend, so the forecast fetch
// 503s and the widget lands on its prompt — that's fine: what we assert
// is the GEOLOCATION CALL COUNT, which proves the prompt path is avoided
// on the second load.

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

test("the weather widget remembers a shared location and doesn't re-prompt next load", async ({
    stack,
    browser,
}) => {
    const user = await createTestUser();
    const ctx = await browser.newContext({
        permissions: ["geolocation"],
        geolocation: { latitude: 37.7749, longitude: -122.4194 },
    });

    // Count getCurrentPosition invocations across page loads. The
    // counter lives in localStorage so it survives a reload (window
    // state does not). Wrapped once per document.
    await ctx.addInitScript(() => {
        const g = navigator.geolocation as any;
        if (g && !g.__geoWrapped) {
            const orig = g.getCurrentPosition.bind(g);
            g.getCurrentPosition = function (...args: any[]) {
                try {
                    const n = parseInt(localStorage.getItem("__geoCalls") || "0", 10) + 1;
                    localStorage.setItem("__geoCalls", String(n));
                } catch (_) { /* ignore */ }
                return orig(...args);
            };
            g.__geoWrapped = true;
        }
    });

    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        // First load: boot lands on Home, the widget has no remembered
        // location → it asks the browser once and caches the result.
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await expect(page.locator("#home-weather .home-wx-prompt")).toBeVisible({ timeout: 15_000 });

        const calls1 = await page.evaluate(() => localStorage.getItem("__geoCalls"));
        expect(calls1, "geolocation asked exactly once on first load").toBe("1");
        const loc = await page.evaluate(() => localStorage.getItem("familiar.weather.loc"));
        expect(loc, "coordinates remembered on this device").toBeTruthy();
        expect(JSON.parse(loc!)).toMatchObject({ lat: 37.7749, lon: -122.4194 });

        // Second load: the remembered coordinates are reused — geolocation
        // must NOT be called again (no re-prompt).
        await page.reload();
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await expect(page.locator("#home-weather .home-wx-prompt")).toBeVisible({ timeout: 15_000 });

        const calls2 = await page.evaluate(() => localStorage.getItem("__geoCalls"));
        expect(calls2, "geolocation not called again on the second load").toBe("1");
    } finally {
        await ctx.close();
    }
});
