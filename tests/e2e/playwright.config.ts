import { defineConfig, devices } from "@playwright/test";

// Phase 1: smoke + auth specs. Chromium only — the WebAuthn virtual
// authenticator is driven over the Chrome DevTools Protocol, which is
// Chromium-specific. Mobile / cross-browser projects land in later
// phases. See ../../TESTING-PLAN.md.
export default defineConfig({
    testDir: "./flows",
    // Worker-scoped fixtures boot the gateway + workspace once per
    // worker. Single worker keeps port allocation simple while the
    // suite is tiny; ramp up when we have enough specs to need
    // parallelism.
    workers: 1,
    timeout: 60_000,
    expect: { timeout: 5_000 },
    retries: process.env.CI ? 2 : 0,
    reporter: process.env.CI
        ? [["github"], ["html", { open: "never" }]]
        : "list",
    use: {
        trace: "on-first-retry",
    },
    projects: [
        {
            name: "chromium",
            // Default: Playwright's bundled Chromium. On hosts where
            // `npx playwright install` isn't supported (e.g. Ubuntu
            // 26.04), point PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH at a
            // system Chromium instead. See MAKE_TESTS.md.
            // The desktop project skips the mobile spec — mobile.html is
            // a separate SPA exercised under the Mobile Chrome project.
            testIgnore: /mobile\.spec\.ts/,
            use: {
                ...devices["Desktop Chrome"],
                launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
                    ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
                    : {},
            },
        },
        {
            // Mobile SPA (mobile.html / mobile.js). Phone viewport +
            // touch + isMobile via the Pixel 7 descriptor (still
            // Chromium, so the WebAuthn virtual authenticator + CDP
            // work). Runs ONLY the mobile spec.
            name: "mobile",
            testMatch: /mobile\.spec\.ts/,
            use: {
                ...devices["Pixel 7"],
                launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
                    ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
                    : {},
            },
        },
    ],
});
