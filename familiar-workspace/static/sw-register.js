// Registers the service worker (PWA install + offline shell + push).
// Externalized from an inline <script> so the page can ship under a
// strict Content-Security-Policy (no 'unsafe-inline' for scripts).
if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js')
        .then((reg) => console.log('[sw] registered:', reg.scope))
        .catch((err) => console.warn('[sw] registration failed:', err));
}
