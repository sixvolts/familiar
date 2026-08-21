#!/usr/bin/env python3
"""manifest.py — collapse the tier artifacts into one run manifest.

The manifest is what makes the tier-5 checks possible. Each field below
exists because some false green needs it:

  dsn_set + go tier skip counts  -> hollow skips (§5.1)
  playwright counts + "did not run" -> nothing ran (§5.4)
  git sha/dirty                  -> lets the adjudicator read the diff (§5.3)
  mlx health + model id          -> proves tier 4 hit a real model, not a
                                    silent repeat of tier 3

Reads only files already on disk; never runs a test itself. Stdlib only —
it must work under /usr/bin/python3 with no venv.
"""
import json
import os
import sys

ART = os.environ["ARTIFACTS"]


def _migration_failures():
    """Count of distinct 'migrations failed' lines the runner scraped."""
    f = os.path.join(ART, "migration-failures.count")
    if not os.path.exists(f):
        return None          # not scanned — distinct from "scanned, found none"
    try:
        return int(open(f).read().strip() or 0)
    except ValueError:
        return None


migration_failures = _migration_failures()


# A test that CANNOT pass without a live database. Counting skips proves
# the DSN changed something; this proves a specific DB-dependent test
# actually executed, which is what §5.1 asks for. If it is absent or
# skipped while the DSN is set, the run is hollow no matter how green.
DB_CANARY = "TestMigrateFreshDatabase"


def go_summary(path):
    """Per-test outcomes and skip reasons from `go test -json`."""
    if not os.path.exists(path):
        return None
    counts = {"pass": 0, "fail": 0, "skip": 0}
    failed, pkg_failed = [], set()
    dsn_skips = 0
    canary = "absent"
    # go test -json interleaves output lines with result lines; a skip's
    # reason arrives as output on the same test before its "skip" action.
    pending = {}
    for line in open(path, errors="replace"):
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            e = json.loads(line)
        except json.JSONDecodeError:
            continue
        act, test, pkg = e.get("Action"), e.get("Test"), e.get("Package", "")
        if act == "output" and test:
            out = e.get("Output", "")
            if "FAMILIAR_TEST_DSN" in out:
                pending[(pkg, test)] = "dsn"
        if test and act in counts:
            counts[act] += 1
            if test == DB_CANARY:
                canary = act
            if act == "fail":
                failed.append(f"{pkg}::{test}")
            if act == "skip" and pending.get((pkg, test)) == "dsn":
                dsn_skips += 1
        if act == "fail" and not test:
            pkg_failed.add(pkg)
    return {
        "passed": counts["pass"],
        "failed": counts["fail"],
        "skipped": counts["skip"],
        "dsn_gated_skips": dsn_skips,
        "failed_tests": failed[:50],
        "failed_packages": sorted(pkg_failed)[:50],
        "ran_any": sum(counts.values()) > 0,
        "db_canary": {"test": DB_CANARY, "outcome": canary},
    }


def pw_summary(path):
    """Counts from Playwright's JSON reporter.

    `did not run` is the interesting one: Playwright reports those tests
    with no results array, and a run that abandons half its specs can
    still look mostly-green in a line reporter.
    """
    if not os.path.exists(path):
        return None
    try:
        d = json.load(open(path, errors="replace"))
    except (json.JSONDecodeError, OSError):
        return {"parse_error": True, "ran_any": False}

    counts = {"passed": 0, "failed": 0, "skipped": 0, "timedOut": 0,
              "interrupted": 0, "did_not_run": 0}
    failed, by_project = [], {}

    def walk(suite, project=None):
        project = suite.get("title") if suite.get("suites") is None else project
        for spec in suite.get("specs", []) or []:
            for test in spec.get("tests", []) or []:
                proj = test.get("projectName") or project or "?"
                results = test.get("results") or []
                if not results:
                    counts["did_not_run"] += 1
                    by_project.setdefault(proj, {}).setdefault("did_not_run", 0)
                    by_project[proj]["did_not_run"] += 1
                    continue
                status = test.get("status") or results[-1].get("status")
                key = {"expected": "passed", "unexpected": "failed",
                       "flaky": "passed"}.get(status, status)
                if key not in counts:
                    key = "failed"
                counts[key] += 1
                by_project.setdefault(proj, {}).setdefault(key, 0)
                by_project[proj][key] += 1
                if key in ("failed", "timedOut"):
                    failed.append(f"{spec.get('file','?')}::{spec.get('title','?')}")
        for child in suite.get("suites", []) or []:
            walk(child, project)

    for s in d.get("suites", []) or []:
        walk(s)

    total = sum(counts.values())
    return {
        **counts,
        "failed_tests": failed[:50],
        "by_project": by_project,
        "ran_any": total > 0 and (total - counts["did_not_run"]) > 0,
    }


tiers = {}
for t in ("1", "2"):
    s = go_summary(os.path.join(ART, f"tier{t}.json"))
    if s:
        tiers[t] = s
for t in ("3", "4"):
    s = pw_summary(os.path.join(ART, f"tier{t}.json"))
    if s:
        # The URL THIS tier used. Tier 3 deliberately points at a closed
        # port so the model-backed specs skip by their own /health gate;
        # recording only the runner's configured URL makes that
        # intentional skip look like a hollow one.
        url_file = os.path.join(ART, f"tier{t}.modelurl")
        url = None
        if os.path.exists(url_file):
            url = open(url_file).read().strip()
        s["model_url_used"] = url
        s["model_expected"] = bool(url) and not url.endswith(":1")
        tiers[t] = s

exit_codes = json.loads(os.environ.get("EXIT_JSON") or "{}")
for t, s in tiers.items():
    s["exit_code"] = exit_codes.get(t)

# The delta that proves the DB tests actually executed. Positive means
# the DSN activated tests that tier 1 skipped; ~0 with dsn_set=true is
# the hollow-skip signature.
activated = None
if "1" in tiers and "2" in tiers:
    activated = tiers["2"]["passed"] - tiers["1"]["passed"]

manifest = {
    "git": {
        "sha": os.environ.get("GIT_SHA"),
        "branch": os.environ.get("GIT_BRANCH"),
        "dirty": os.environ.get("GIT_DIRTY") == "true",
    },
    "config": {
        # The FACT, never the value — the DSN carries a password.
        "familiar_test_dsn_set": os.environ.get("DSN_SET") == "true",
        "mlx_healthy": os.environ.get("MLX_HEALTHY") == "true",
        "mlx_model_id": os.environ.get("MLX_MODEL_ID"),
        "mlx_url": os.environ.get("MLX_URL"),
        "tiers_requested": os.environ.get("TIERS"),
    },
    "tiers": tiers,
    "derived": {
        "db_tests_activated_by_dsn": activated,
        # db.Migrate failing at boot is logged, not fatal (main.go:355), so
        # a gateway on a table-less database still answers /api/health ok.
        # Non-zero here means some booted gateway was running blind.
        "migrations_failed": migration_failures,
        # True only if the canary actually PASSED in the DSN tier. It
        # cannot pass without a live database, so this is the one field
        # that distinguishes "the DB tests ran" from "the counts moved".
        "db_canary_ran": (
            tiers.get("2", {}).get("db_canary", {}).get("outcome") == "pass"
        ),
        "any_tier_failed": any(
            (s.get("exit_code") or 0) != 0 for s in tiers.values()
        ),
        "any_tier_ran_nothing": any(not s.get("ran_any") for s in tiers.values()),
    },
    "exit_codes": exit_codes,
}

out = os.path.join(ART, "manifest.json")
with open(out, "w") as f:
    json.dump(manifest, f, indent=2, sort_keys=True)

print(f"    manifest -> {out}")
for t in sorted(tiers):
    s = tiers[t]
    if "did_not_run" in s:
        print(f"    tier {t}: {s['passed']}p/{s['failed']}f/"
              f"{s['skipped']}s dnr={s['did_not_run']} exit={s['exit_code']}")
    else:
        print(f"    tier {t}: {s['passed']}p/{s['failed']}f/"
              f"{s['skipped']}s (dsn-gated skips={s['dsn_gated_skips']}) "
              f"exit={s['exit_code']}")
if activated is not None:
    print(f"    db tests activated by DSN: {activated}")
sys.exit(0)
