# Note template

Title: `Research: <topic>`

A finished briefing someone will read cold — coherent and explanatory,
not reading notes. Each fact appears once, in its most specific section;
Details deepens Key findings rather than repeating it. Every claim is
cited inline. Sources lists only what you actually used. End on Sources —
no "Open questions" / "Further research" / "Next steps" section.

```
## Summary
2–4 sentences: the answer, up front, with the load-bearing specifics.

## Key findings
- <complete claim with a figure/named entity> [Title](URL)

## Details
### <angle>
Prose or bullets that explain a mechanism/cause/narrative — not a
restated finding. A citation at each claim; ≥2 claims per angle from a
fetched source.

## Sources
- [Title](URL) — what it supported
```

## Miniature example

```
# Research: Matter smart-home adoption

## Summary
Matter 1.3 shipped in May 2024 and all four major hub vendors now
bridge it over Thread, but device coverage lags — there is still no
camera device type [CSA](https://csa-iot.org/newsroom/matter-1-3/).

## Key findings
- Matter 1.3 added water- and energy-management device types
  [CSA](https://csa-iot.org/newsroom/matter-1-3/).
- Echo (4th gen) acts as a Thread border router
  [The Verge](https://www.theverge.com/23164952/matter-hubs).

## Sources
- [CSA — Matter 1.3](https://csa-iot.org/newsroom/matter-1-3/) — release scope
- [The Verge — Matter hubs](https://www.theverge.com/23164952/matter-hubs) — hub support
```

## Example save_fact contents

- "Matter 1.3 (released May 2024) added water- and energy-management device types (source: csa-iot.org/newsroom/matter-1-3)."
- "As of July 2026 the Matter standard has no camera device type; camera vendors bridge via proprietary hubs."
- "Amazon Echo 4th gen works as a Matter-over-Thread border router (source: theverge.com/23164952, as of Jul 2026)."
