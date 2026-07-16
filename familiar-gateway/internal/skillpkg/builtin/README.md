# Built-in skills

Markdown skill packages embedded in the gateway binary (`go:embed`) and
synced into the skills library at boot. A fresh install gets first-party
skills with no zip import and no admin bootstrap step.

## Trust

A builtin body is versioned gateway source: it ships in the same binary
and is reviewed in the same PRs as `prompts/tiers/*.md` — files that
already enter the trusted system prompt. That is why builtins are
exempt from USER-SKILLS-SPEC's v1 rule that instance skills stay
shard-only: they are reachable in trusted chat (store authz:
owner-or-builtin). This does NOT extend to admin-imported zips, whose
exclusion is unchanged — the trust comes from provenance (first-party
source), not from being an instance-library row.

## Adding one

1. Drop a spec-valid skill directory here: `SKILL.md` with `name` +
   `description` frontmatter, optional `references/*.md`. The directory
   name must equal the frontmatter `name`.
2. Rebuild and deploy the gateway. That's it — the boot sync reconciles
   the embedded files into the library and the DB catalog.

## Sync semantics (boot)

- **Fresh install** — catalog row created with `origin='builtin'`,
  enabled + chat_enabled by default, `imported_by` NULL ("shipped with
  gateway").
- **Digest change on upgrade** — files refreshed in place; admin
  toggles (enabled, chat_enabled) are preserved.
- **Name collision** with an imported/authored skill — the builtin is
  skipped loudly (logged); the existing skill wins.
- **Delete** is refused ("disable it instead; it re-syncs at boot").
  Admins can toggle enabled and chat exposure
  (`POST /console/api/skillpacks/{id}/chat` — builtins only; imported
  instance skills stay shard-only).
- **Serving** — SKILL.md and reference files are read from the binary
  embed, never the disk copy, so a tampered directory cannot serve
  under the BUILT-IN badge. Rescan ignores builtin rows entirely (no
  digest adoption, no missing-dir auto-disable); the boot sync owns
  their disk lifecycle.

## Specs

- `docs/USER-SKILLS-SPEC.md` — trust matrix (§3.2) and the builtin
  amendments.
- `docs/RESEARCH-SKILL-SPEC.md` — the first builtin (`research/`).
