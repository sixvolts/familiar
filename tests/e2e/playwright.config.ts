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
    // A committed test.only would silently shrink the CI run to one test and
    // still report green. Fail the run instead; locally .only stays usable.
    forbidOnly: !!process.env.CI,
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
            // work). Runs the mobile spec, plus the pixel baselines —
            // visual.spec.ts is deliberately in BOTH projects so a
            // regression that only shows at phone width (the tap-target
            // and checkbox class of bug) can't hide behind a green
            // desktop baseline. Playwright keys snapshots by project,
            // so the two viewports keep independent baselines.
            name: "mobile",
            testMatch: /(mobile|visual)\.spec\.ts/,
            use: {
                ...devices["Pixel 7"],
                launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
                    ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
                    : {},
            },
        },
    ],
});
