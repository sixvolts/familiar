#!/bin/bash
# adjudicate.sh — tier 5. Reads the artifacts a tier run produced and
# answers one question: is this pass real?
#
# TWO PROPERTIES THIS SCRIPT ENFORCES IN CODE, NOT BY ASKING NICELY:
#
#   1. VETO ONLY. The adjudicator may turn a green run red. It may never
#      turn a red run green. That is not a prompt instruction the model
#      could talk itself out of — the deterministic exit codes are read
#      from the manifest FIRST, and a red manifest short-circuits before
#      the model is even consulted.
#
#   2. FAIL SAFE. A confused, timed-out, or unparseable judge does not
#      produce a pass. The enemy is false green, so ambiguity resolves
#      to objection, never to approval.
#
# The adjudicator gets NO write access to the repo. It must not be able
# to edit code to make a test pass; fixing is a different role, with
# different permissions, performed by a different invocation.
#
# Usage: ci/icecube/adjudicate.sh --artifacts DIR [--enforce] [--timeout SECS]
#
# Default is advisory: the verdict is recorded and printed, and the exit
# code still reflects the deterministic run. --enforce makes a veto
# actually fail the gate. Keep it advisory until its verdicts have a
# track record (build spec §7).

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ARTIFACTS=""
ENFORCE=false
TIMEOUT_SECS=600
# Pin the model explicitly. Left unset, `claude -p` inherits whatever the CLI
# default happens to be that week (it resolved to claude-sonnet-5 when this was
# written) — so a CLI update could silently change both the cost and the
# judgement of a step whose entire job is deciding whether a green run can be
# trusted. Nothing in the repo would record that it had changed.
#
# Sonnet rather than Haiku: the §5.3 check is spotting an assertion that was
# weakened rather than fixed, which means reading a diff for intent, and that is
# where a stronger model earns its cost. Sonnet rather than Opus: this is
# checklist-shaped reading, not deep reasoning.
#
# Note this pins WHICH model, not determinism — `claude -p` exposes no
# temperature control, so two runs over identical artifacts can still differ.
# The raw response is written to the artifacts dir, and its modelUsage block
# records what actually answered, so a verdict can always be traced to a model.
MODEL="${FAMILIAR_ADJUDICATOR_MODEL:-claude-sonnet-5}"
MAX_TURNS=30

while [[ $# -gt 0 ]]; do
    case "$1" in
        --artifacts) ARTIFACTS="$2"; shift 2 ;;
        --enforce)   ENFORCE=true; shift ;;
        --timeout)   TIMEOUT_SECS="$2"; shift 2 ;;
        *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
done

[[ -z "$ARTIFACTS" ]] && { echo "--artifacts is required" >&2; exit 2; }
MANIFEST="$ARTIFACTS/manifest.json"
VERDICT="$ARTIFACTS/verdict.json"

write_verdict() {   # status, objection, evidence
    /usr/bin/python3 - "$VERDICT" "$1" "$2" "$3" <<'PY'
import json, sys
path, status, objection, evidence = sys.argv[1:5]
json.dump({"status": status, "objection": objection or None,
           "evidence": evidence, "adjudicator": "tier5"},
          open(path, "w"), indent=2)
print(f"    verdict: {status} ({objection or 'no objection'})")
PY
}

# ── Gate 0: the manifest itself ─────────────────────────────────────
# No manifest means the run did not complete far enough to be judged.
# That is an objection, not a pass.

if [[ ! -f "$MANIFEST" ]]; then
    echo "==> tier 5: no manifest at $MANIFEST" >&2
    write_verdict "FAIL" "no-manifest" "The tier run produced no manifest.json; nothing to adjudicate."
    exit 1
fi

# ── Gate 1: deterministic red short-circuits ────────────────────────
# Read the hard facts before consulting any model. If the run is already
# red, the adjudicator has no say — this is what makes "veto only"
# structural rather than aspirational.

DETERMINISTIC="$(/usr/bin/python3 - "$MANIFEST" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
d = m.get("derived", {})
codes = [v for v in (m.get("exit_codes") or {}).values() if v is not None]
red = d.get("any_tier_failed") or d.get("any_tier_ran_nothing") or any(c != 0 for c in codes)
print("RED" if red else "GREEN")
PY
)"

if [[ "$DETERMINISTIC" == "RED" ]]; then
    echo "==> tier 5: run is already red deterministically — adjudicator not consulted"
    echo "    (it may only veto a green; it can never turn a red run green)"
    write_verdict "FAIL" "deterministic-failure" "One or more tiers failed or ran nothing. Adjudicator skipped by design."
    exit 1
fi

# ── Gate 2: consult the adjudicator ─────────────────────────────────

command -v claude >/dev/null 2>&1 || {
    echo "==> tier 5: claude CLI not found" >&2
    write_verdict "FAIL" "adjudicator-unavailable" "claude CLI missing; cannot verify a green run. Failing safe."
    $ENFORCE && exit 1 || exit 0
}

# `read -r -d ''` rather than `PROMPT=$(cat <<EOF)`: bash's $() parser
# tracks quotes naively, so a single apostrophe anywhere in the prompt
# ("every tier's counts") silently breaks the whole script. This form
# takes the heredoc verbatim. It returns non-zero at EOF, hence `|| true`.
read -r -d '' PROMPT <<'EOF' || true
You are adjudicating a CI run for the Familiar project. The tiers all
reported success. Your ONLY job is to decide whether that pass is REAL.

You have read-only access. You cannot edit code, and you must not try.

You are in the repo root. The artifacts are at the absolute path given
after this prompt; read them from there. Read these files:
  manifest.json  — what was actually configured and what each tier counted
  tier1.json / tier2.json — "go test -json" output
  tier3.json / tier4.json — Playwright JSON reporter output
  tier3.raw / tier4.raw   — human-readable run logs

Work through this checklist. Each item is a false green this project has
actually produced, so treat each as a live hypothesis, not a formality:

1. HOLLOW SKIPS. The DB-backed tests skip silently without
   FAMILIAR_TEST_DSN, and every package still prints "ok". Check
   config.familiar_test_dsn_set is true. Check
   derived.db_tests_activated_by_dsn is a large positive number (~160 is
   the healthy value; near zero with the DSN set means the tests skipped
   anyway). Check tier2.dsn_gated_skips is ~0, not ~160. Finally check
   derived.db_canary_ran is true: that canary cannot pass without a live
   database, so it is the field separating "the DB tests actually ran"
   from "the counts merely moved".

2. PASSING WHILE VISIBLY BROKEN. DOM assertions pass while a view is
   unusable. If screenshots or pixel baselines are present, look at them
   and judge whether the UI is actually usable — overlapping text, tiled
   backgrounds, controls off-screen. Where no baseline exists yet, that
   absence is itself worth reporting.

3. WEAKENED ASSERTIONS. An exact count assertion was once relaxed to
   ">= 1" to dodge an ordering flake; it passed while testing less. Use
   the git sha in the manifest to read the diff (you are in the repo, so
   git log/show/diff work) and look for assertions loosened rather than
   fixed.

   Note: pixel baselines live in the repo at
   tests/e2e/flows/visual.spec.ts-snapshots/, not in the artifacts
   directory. Playwright only attaches screenshots on FAILURE, so their
   absence from the artifacts is expected on a green run and is not
   itself an objection.

4. SILENTLY BROKEN DATABASE. db.Migrate failing at gateway boot is
   logged, never fatal, so the gateway answers /api/health with "ok" on a
   database that has no tables. Specs that do not touch the missing tables
   then pass. Check derived.migrations_failed is 0 — null means the scan
   did not run, which is itself unverified, not clean.

5. NOTHING RAN. Zero-test runs, collection failures, or specs reported
   as "did not run" that still exit 0. Check every tier's counts are
   plausible and that did_not_run is 0.

Then reply with ONLY a JSON object, no prose around it:

{"status":"PASS"|"FAIL",
 "objection":"<checklist item that objected, or null>",
 "evidence":"<specific numbers/files that justify the verdict>"}

Rules for your verdict:
- Object if ANY checklist item fails.
- If you are uncertain, or you could not read what you needed, return
  FAIL. A pass you cannot justify is the exact failure mode you exist to
  prevent. Do not be agreeable.
EOF

echo "==> tier 5: consulting adjudicator (timeout ${TIMEOUT_SECS}s, max-turns $MAX_TURNS)"

# Read-only tools plus a narrow Bash allowlist. No Edit/Write/NotebookEdit,
# so the adjudicator structurally cannot patch a test into passing.
RAW="$ARTIFACTS/adjudicator-raw.json"
# Run from the REPO ROOT, not the artifacts directory. Checklist item 3
# (assertions weakened rather than fixed) requires reading the diff, and
# from inside an artifacts directory there is no git repo to read —
# the adjudicator can only report "unverifiable", which is a guaranteed
# objection and makes the whole verdict useless. Read-only tools mean
# repo access still cannot modify anything.
(
    cd "$REPO_ROOT" || exit 1
    claude -p "$PROMPT

ARTIFACTS DIRECTORY: $ARTIFACTS" \
        --output-format json \
        --max-turns "$MAX_TURNS" \
        --model "$MODEL" \
        --allowedTools "Read" "Glob" "Grep" \
                       "Bash(cat:*)" "Bash(ls:*)" "Bash(head:*)" \
                       "Bash(tail:*)" "Bash(wc:*)" "Bash(git log:*)" \
                       "Bash(git show:*)" "Bash(git diff:*)"
) > "$RAW" 2>"$ARTIFACTS/adjudicator.err" &
CLAUDE_PID=$!

# macOS ships no timeout(1); poll instead of adding a coreutils dep.
elapsed=0
while kill -0 "$CLAUDE_PID" 2>/dev/null; do
    sleep 5
    elapsed=$((elapsed + 5))
    if (( elapsed >= TIMEOUT_SECS )); then
        kill -9 "$CLAUDE_PID" 2>/dev/null
        echo "==> tier 5: adjudicator exceeded ${TIMEOUT_SECS}s — failing safe" >&2
        write_verdict "FAIL" "adjudicator-timeout" "Adjudicator exceeded ${TIMEOUT_SECS}s wall clock."
        $ENFORCE && exit 1 || exit 0
    fi
done
wait "$CLAUDE_PID"; CLAUDE_RC=$?

# ── Gate 3: parse, failing safe on anything unexpected ──────────────

PARSED="$(/usr/bin/python3 - "$RAW" "$CLAUDE_RC" <<'PY'
import json, re, sys
raw_path, rc = sys.argv[1], int(sys.argv[2])
try:
    outer = json.load(open(raw_path))
except Exception as e:
    print(json.dumps({"status": "FAIL", "objection": "adjudicator-unparseable",
                      "evidence": f"Could not parse adjudicator output: {e}"}))
    sys.exit()
if rc != 0 or outer.get("is_error"):
    print(json.dumps({"status": "FAIL", "objection": "adjudicator-error",
                      "evidence": f"claude exited {rc}; is_error={outer.get('is_error')}"}))
    sys.exit()
text = outer.get("result") or ""
m = re.search(r"\{.*\}", text, re.S)
if not m:
    print(json.dumps({"status": "FAIL", "objection": "adjudicator-no-verdict",
                      "evidence": "No JSON verdict found in adjudicator reply."}))
    sys.exit()
try:
    v = json.loads(m.group(0))
except Exception as e:
    print(json.dumps({"status": "FAIL", "objection": "adjudicator-bad-json",
                      "evidence": f"Verdict JSON invalid: {e}"}))
    sys.exit()
status = str(v.get("status", "")).upper()
if status not in ("PASS", "FAIL"):
    print(json.dumps({"status": "FAIL", "objection": "adjudicator-bad-status",
                      "evidence": f"Unrecognised status {status!r}; treating as objection."}))
    sys.exit()
print(json.dumps({"status": status, "objection": v.get("objection"),
                  "evidence": str(v.get("evidence", ""))[:2000]}))
PY
)"

STATUS="$(echo "$PARSED"   | /usr/bin/python3 -c 'import sys,json;print(json.load(sys.stdin)["status"])')"
OBJECTION="$(echo "$PARSED"| /usr/bin/python3 -c 'import sys,json;print(json.load(sys.stdin).get("objection") or "")')"
EVIDENCE="$(echo "$PARSED" | /usr/bin/python3 -c 'import sys,json;print(json.load(sys.stdin).get("evidence") or "")')"

write_verdict "$STATUS" "$OBJECTION" "$EVIDENCE"

if [[ "$STATUS" == "FAIL" ]]; then
    echo "==> tier 5: OBJECTION — $OBJECTION"
    echo "    $EVIDENCE"
    if $ENFORCE; then exit 1; fi
    echo "    (advisory mode: not failing the gate)"
    exit 0
fi

echo "==> tier 5: pass confirmed real"
exit 0
