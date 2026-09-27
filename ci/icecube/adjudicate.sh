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
# The adjudicator gets NO write access. It must not be able to edit code
# to make a test pass; fixing is a different role, with different
# permissions, performed by a different invocation. See the claude
# invocation below for how that is enforced, and what it can read.
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
MAX_TURNS=40

while [[ $# -gt 0 ]]; do
    case "$1" in
        --artifacts) ARTIFACTS="$2"; shift 2 ;;
        --enforce)   ENFORCE=true; shift ;;
        --timeout)   TIMEOUT_SECS="$2"; shift 2 ;;
        --max-turns) MAX_TURNS="$2"; shift 2 ;;
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
#
# Only an explicit GREEN goes on to the model. A manifest that cannot be
# read prints UNREADABLE, and a crash prints nothing; both stop here.

DETERMINISTIC="$(/usr/bin/python3 - "$MANIFEST" <<'PY'
import json, sys
try:
    m = json.load(open(sys.argv[1]))
    d = m.get("derived", {})
    codes = [v for v in (m.get("exit_codes") or {}).values() if v is not None]
    red = d.get("any_tier_failed") or d.get("any_tier_ran_nothing") or any(c != 0 for c in codes)
    print("RED" if red else "GREEN")
except Exception:
    print("UNREADABLE")
PY
)"

if [[ "$DETERMINISTIC" != "GREEN" ]]; then
    if [[ "$DETERMINISTIC" == "RED" ]]; then
        echo "==> tier 5: run is already red deterministically — adjudicator not consulted"
        echo "    (it may only veto a green; it can never turn a red run green)"
        write_verdict "FAIL" "deterministic-failure" "One or more tiers failed or ran nothing. Adjudicator skipped by design."
    else
        echo "==> tier 5: manifest unreadable — adjudicator not consulted" >&2
        write_verdict "FAIL" "manifest-unreadable" "manifest.json did not parse as a run manifest; nothing to adjudicate."
    fi
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
after this prompt.

READ manifest.json FIRST, and expect it to be enough. It is ~2KB of
derived summary, and every checklist item below is answerable from its
fields alone. On most runs it is the only artifact you need to open.

DO NOT read tier1.json or tier2.json. Each is a raw "go test -json"
stream of roughly 1.1MB, about a thousand test events apiece. Reading
them is what turned a previous run into 1.3 million tokens and a
max-turns failure that produced no verdict at all. tier3.json and
tier4.json are Playwright reporter output at ~150KB and are also not
worth opening whole.

If, and only if, a specific manifest claim looks wrong and you need to
corroborate it, search that ONE pattern with the Grep tool instead of
opening the file: count '"Action":"fail"' in <artifacts>/tier2.json, or
show the first matches of 'TestMigrateFreshDatabase'. tier3.raw /
tier4.raw are ~15KB human-readable logs, safe to read whole if you need
run detail.

Budget: reach a verdict in under ten tool calls. Ranging over the repo is
not part of the job. You are judging a manifest against a checklist, with
the git diff in item 3 as the single exception.

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
   from "the counts merely moved". The model has the same check:
   derived.model_tests_activated counts the tests tier 3 skipped (no
   model) that tier 4 ran. When tier 4 ran, it must be well above zero.

2. PASSING WHILE VISIBLY BROKEN. DOM assertions pass while a view is
   unusable. If screenshots or pixel baselines are present, look at them
   and judge whether the UI is actually usable — overlapping text, tiled
   backgrounds, controls off-screen. Where no baseline exists yet, that
   absence is itself worth reporting.

3. WEAKENED ASSERTIONS. An exact count assertion was once relaxed to
   ">= 1" to dodge an ordering flake; it passed while testing less. Use
   the git sha in the manifest to read the diff (you are in the repo, so
   `git log` and `git show` work; `git log -p` for a range) and look for
   assertions loosened rather than fixed.

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

6. FLAKY PASSES. CI retries a failing test up to twice, and a test that
   passes on a retry counts as passed. Check tiers["3"].flaky and
   tiers["4"].flaky, and name every entry of flaky_tests in your
   evidence. Tier 3 has no model, so a flake there is a race in the app
   or the suite: object. In tier 4 a flake can be model variance: name
   it, but it is not by itself an objection.

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

# What the adjudicator can do, and what enforces it:
#
#   - Read/Glob/Grep inside the repo (its working directory) and the
#     artifacts directory. A read anywhere else would need a permission
#     prompt, and dontAsk denies anything that would prompt. No bare
#     `Read` rule: that allows reading any path, ~/.claude included.
#   - Bash: `git log` and `git show`, nothing else. Not head/tail/ls,
#     which read any path, and not `git diff`, whose --no-index reads
#     any path too.
#   - No Edit/Write/NotebookEdit.
#
# git's diff options include --output=<file>, which writes. The deny
# rule stops the plain spelling, but a deny rule matches the command
# text, and shell quoting (--"output"=...) gets past text. So Bash also
# runs in Claude Code's sandbox, which confines writes (quoted or not)
# to the working directory and a temp directory, and with unsandboxed
# retries turned off. That is the boundary; the deny rule is courtesy.
#
# autoAllowBashIfSandboxed must be false. It defaults to true, and then
# every sandboxed command is allowed, not just the two git commands: a
# probe with it on read a file outside the repo with `head`.
#
# Probed with Claude Code 2.1.283 (the runner had 2.1.233): plain
# --output denied, --"output"= refused by the sandbox, reads outside the
# repo and artifacts denied, head/ls/touch denied.
RAW="$ARTIFACTS/adjudicator-raw.json"
ARTIFACTS_ABS="$(cd "$ARTIFACTS" && pwd)"
SANDBOX='{"sandbox":{"enabled":true,"autoAllowBashIfSandboxed":false,"allowUnsandboxedCommands":false}}'
# Run from the REPO ROOT, not the artifacts directory. Checklist item 3
# (assertions weakened rather than fixed) requires reading the diff, and
# from inside an artifacts directory there is no git repo to read —
# the adjudicator can only report "unverifiable", which is a guaranteed
# objection and makes the whole verdict useless.
#
# `exec`, so $! is claude itself: bash 3.2 forks the last command of a
# subshell rather than exec'ing it, and the timeout's kill would stop
# only the wrapper, leaving claude running and writing $RAW.
(
    cd "$REPO_ROOT" || exit 1
    exec claude -p "$PROMPT

ARTIFACTS DIRECTORY: $ARTIFACTS_ABS" \
        --output-format json \
        --max-turns "$MAX_TURNS" \
        --model "$MODEL" \
        --permission-mode dontAsk \
        --settings "$SANDBOX" \
        --allowedTools "Read(/$ARTIFACTS_ABS/**)" \
                       "Bash(git log:*)" "Bash(git show:*)" \
        --disallowedTools "Edit" "Write" "NotebookEdit" \
                          "Bash(git *--output*)"
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
# Every path prints a verdict object, and anything but an exact PASS
# below is an objection: an output shape this parser has never seen (a
# CLI update, --verbose's event array) must not read as approval.

PARSED="$(/usr/bin/python3 - "$RAW" "$CLAUDE_RC" <<'PY'
import json, re, sys

def verdict(status, objection, evidence):
    print(json.dumps({"status": status, "objection": objection,
                      "evidence": evidence}))
    sys.exit()

try:
    raw_path, rc = sys.argv[1], int(sys.argv[2])
    try:
        outer = json.load(open(raw_path))
    except Exception as e:
        verdict("FAIL", "adjudicator-unparseable", f"Could not parse adjudicator output: {e}")
    if not isinstance(outer, dict):
        verdict("FAIL", "adjudicator-unparseable",
                f"Expected a JSON object from claude -p, got {type(outer).__name__}.")
    if rc != 0 or outer.get("is_error"):
        verdict("FAIL", "adjudicator-error", f"claude exited {rc}; is_error={outer.get('is_error')}")
    text = outer.get("result")
    if not isinstance(text, str):
        verdict("FAIL", "adjudicator-no-verdict",
                f"Adjudicator result is {type(text).__name__}, not text.")
    m = re.search(r"\{.*\}", text, re.S)
    if not m:
        verdict("FAIL", "adjudicator-no-verdict", "No JSON verdict found in adjudicator reply.")
    try:
        v = json.loads(m.group(0))
    except Exception as e:
        verdict("FAIL", "adjudicator-bad-json", f"Verdict JSON invalid: {e}")
    if not isinstance(v, dict):
        verdict("FAIL", "adjudicator-bad-json", "Verdict is not a JSON object.")
    status = str(v.get("status", "")).upper()
    if status not in ("PASS", "FAIL"):
        verdict("FAIL", "adjudicator-bad-status",
                f"Unrecognised status {status!r}; treating as objection.")
    verdict(status, v.get("objection"), str(v.get("evidence", ""))[:2000])
except Exception as e:
    verdict("FAIL", "adjudicator-parser-error", f"{type(e).__name__}: {e}")
PY
)"

STATUS="$(echo "$PARSED"   | /usr/bin/python3 -c 'import sys,json;print(json.load(sys.stdin)["status"])' 2>/dev/null)"
OBJECTION="$(echo "$PARSED"| /usr/bin/python3 -c 'import sys,json;print(json.load(sys.stdin).get("objection") or "")' 2>/dev/null)"
EVIDENCE="$(echo "$PARSED" | /usr/bin/python3 -c 'import sys,json;print(json.load(sys.stdin).get("evidence") or "")' 2>/dev/null)"
if [[ "$STATUS" != "PASS" && "$STATUS" != "FAIL" ]]; then
    STATUS="FAIL"
    OBJECTION="adjudicator-unparseable"
    EVIDENCE="The verdict parser produced no readable verdict."
fi

write_verdict "$STATUS" "$OBJECTION" "$EVIDENCE"

if [[ "$STATUS" != "PASS" ]]; then
    echo "==> tier 5: OBJECTION — $OBJECTION"
    echo "    $EVIDENCE"
    if $ENFORCE; then exit 1; fi
    echo "    (advisory mode: not failing the gate)"
    exit 0
fi

echo "==> tier 5: pass confirmed real"
exit 0
