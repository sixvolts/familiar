#!/usr/bin/env bash
# gen-pwa-splash.sh — generate apple-touch-startup-image PNGs for the
# PWA shell. Each iOS device class needs its own pixel-perfect splash;
# Apple won't scale a single source. Background is #0b0b0f to match
# manifest.json + the in-app theme. The F-mark is sized to ~20% of
# the shorter dimension and centered.
#
# Renders via Chrome headless because sips can't composite over
# alpha. We inline mark.svg directly into the HTML rather than
# loading icon-512.png — that PNG has transparent corners that
# Chrome's screenshot pipeline flattens to white regardless of the
# page background. SVG paint primitives have no alpha leak path,
# so the body background shows cleanly under the shape.
#
# Outputs:
#   familiar-workspace/static/splash/apple-splash-WxH.png  (×10)
#
# Re-run any time the mark changes; bump CACHE in sw.js so already-
# installed PWAs pull the refreshed shell.

set -euo pipefail

WORKSPACE_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="$WORKSPACE_ROOT/static/splash"
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

if [ ! -x "$CHROME" ]; then
    echo "missing Chrome: $CHROME" >&2
    exit 1
fi

mkdir -p "$OUT_DIR"

# Each row: WIDTHxHEIGHT — portrait. Covers iPhone SE through iPhone
# 16 Pro Max (per familiar-pwa-spec.md §4).
SIZES=(
    "750x1334"   # iPhone SE / 8
    "1125x2436"  # iPhone X / XS / 11 Pro / 13 mini
    "828x1792"   # iPhone XR / 11 / 12 / 13 / 14
    "1242x2688"  # iPhone XS Max / 11 Pro Max
    "1170x2532"  # iPhone 12 / 13 / 14 / 12 Pro / 13 Pro
    "1179x2556"  # iPhone 14 Pro / 15 / 15 Pro
    "1284x2778"  # iPhone 14 Plus / 15 Plus
    "1290x2796"  # iPhone 14 Pro Max / 15 Pro Max
    "1206x2622"  # iPhone 16 Pro
    "1320x2868"  # iPhone 16 Pro Max
)

for size in "${SIZES[@]}"; do
    w="${size%x*}"
    h="${size#*x}"
    short_dim=$(( w < h ? w : h ))
    mark_size=$(( short_dim / 5 )) # ~20% of the shorter dimension
    out="$OUT_DIR/apple-splash-${w}x${h}.png"

    # Inline SVG mark — viewBox matches mark.svg exactly. width/height
    # set on the outer <svg> so Chrome lays it out at the right size
    # without any image-rendering pass.
    html=$(cat <<EOF
<!doctype html><html><head><style>
html,body { margin:0; padding:0; width:100%; height:100%;
            background:#0b0b0f; }
body { display:flex; align-items:center; justify-content:center; }
svg { width:${mark_size}px; height:${mark_size}px; }
</style></head><body>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="78 56 96 110" fill="none">
<g transform="translate(-82 0)">
<path d="M 165.41 64.56 L 244.48 64.56 L 233.02 79.97 L 180.48 79.97 L 180.48 100.21 L 218.40 100.21 L 206.94 115.63 L 181.04 115.63 L 181.04 154.56 L 165.41 154.56 Z" fill="#6A4CE0"/>
<circle cx="198.69" cy="146.74" r="7.817" fill="#E8BE55"/>
</g>
</svg>
</body></html>
EOF
)
    data_url="data:text/html;base64,$(echo -n "$html" | base64)"

    "$CHROME" --headless=new --hide-scrollbars --disable-gpu \
        --window-size="$w,$h" \
        --screenshot="$out" \
        "$data_url" 2>/dev/null

    echo "  wrote $out (mark=${mark_size}px on ${w}x${h})"
done

echo "done — $(ls "$OUT_DIR" | wc -l | tr -d ' ') splash files in $OUT_DIR"
