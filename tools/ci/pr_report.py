#!/usr/bin/env python3
"""Report Jenkins CI results back to GitHub for good failure visibility.

Two outputs, driven entirely by files the Jenkinsfile writes into CI_LOGS_DIR:
  1. Per-stage commit statuses (context ``ci/<stage>``) on STATUS_SHA, so the PR
     shows at a glance WHICH stage failed.
  2. A single sticky PR comment (marker below) with a per-stage table and, on
     failure, the tail of each failing stage's log — so the actual error is
     visible directly in the PR without opening Jenkins.

Stdlib only. Inputs come from environment variables (see main()).
For each stage ``<name>`` the Jenkinsfile writes:
  <CI_LOGS_DIR>/<name>.status   -> "pass" or "fail"
  <CI_LOGS_DIR>/<name>.log      -> combined stdout/stderr of that stage
"""
import glob
import json
import os
import re
import urllib.error
import urllib.request

API = "https://api.github.com"
MARKER = "<!-- sub2api-ci-report -->"

# A plain tail() of a stage's log is not reliable evidence of *why* it
# failed: tools like `go test` on a heavily-logging package (e.g.
# internal/service's structured INFO output from gemini_oauth_service.go)
# can print thousands of chars of noise *after* the "--- FAIL: TestX" line
# that actually names the failing test, pushing it clean out of the tail.
# SIGNAL_PATTERNS lists line shapes that identify a failure regardless of
# where they sit in the log; extract_signals() below pulls those out first,
# and the tail is kept only as secondary context. Extend this list as new
# failure shapes turn up in CI.
SIGNAL_PATTERNS = [re.compile(p) for p in [
    r"^\s*--- FAIL: ",   # go test: (sub)test failure header, incl. "TestX/sub"
    r"^panic: ",         # go: unrecovered panic
    r"^FAIL\t",          # go test: per-package summary line "FAIL\t<pkg>\t<time>"
    r"DATA RACE",        # go test -race
    r"^\s*Error Trace:", # testify/assert: failing assertion's call site
    r"^\s*Error:\s",     # testify/assert: failure message
    r"^\s*Test:\s",      # testify/assert: names the test the assertion ran in
    r"^FAIL ",           # vitest: "FAIL  path/to/test.ts"
    r"AssertionError",   # vitest/node assert
    r"✕",           # vitest: "✕" marks a failing test
    r"Unhandled Error",  # vitest
]]

MAX_SIGNAL_LINES = 40    # cap on distinct signal lines kept per stage
MAX_SIGNAL_CHARS = 2000  # cap on chars of the extracted-signal section per stage
MAX_LOG_CHARS = 3000     # cap on chars of the plain tail (context) section per stage
# Budget: a comment can carry at most one row per known stage (currently 4:
# backend-unit, golangci-lint, govulncheck, frontend). Worst case all 4 fail
# and each embeds a signal section (<= MAX_SIGNAL_CHARS) plus a tail section
# (<= MAX_LOG_CHARS): 4 * (2000 + 3000) = 20000 chars, plus the summary table
# and <details>/``` wrapping (a few hundred chars) — well within GitHub's
# 65536-char issue-comment limit.

STAGE_TITLES = {
    "backend-unit": "后端单元测试",
    "golangci-lint": "golangci-lint",
    "govulncheck": "govulncheck",
    "frontend": "前端 lint/typecheck/单测/audit",
}


def gh(method, path, token, data=None):
    url = path if path.startswith("http") else API + path
    body = json.dumps(data).encode() if data is not None else None
    req = urllib.request.Request(url, data=body, method=method)
    req.add_header("Authorization", "token " + token)
    req.add_header("Accept", "application/vnd.github+json")
    if body:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req) as r:
            text = r.read().decode()
            return r.status, (json.loads(text) if text else None)
    except urllib.error.HTTPError as e:
        return e.code, {"error": e.read().decode()[:300]}
    except urllib.error.URLError as e:
        return 0, {"error": str(e)}


def tail_text(data, max_chars=MAX_LOG_CHARS):
    data = data.strip()
    if len(data) > max_chars:
        data = "…(truncated)…\n" + data[-max_chars:]
    return data


def tail(path, max_chars=MAX_LOG_CHARS):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            data = f.read()
    except OSError:
        return ""
    return tail_text(data, max_chars)


def extract_signals(data, max_lines=MAX_SIGNAL_LINES, max_chars=MAX_SIGNAL_CHARS):
    """Pull out the lines that identify *why* a stage failed, in original
    order, no matter where in the log they sit. Returns (excerpt, omitted)
    where omitted is how many *additional* matching lines (beyond max_lines)
    were dropped. Returns ("", 0) if nothing matched, so callers can fall
    back to the plain tail.

    Identical lines are deduplicated (a flaky retry can print the same
    "--- FAIL: TestX" many times) before the line cap is applied.
    """
    seen = set()
    matched = []
    for line in data.splitlines():
        if not any(p.search(line) for p in SIGNAL_PATTERNS):
            continue
        key = line.strip()
        if key in seen:
            continue
        seen.add(key)
        matched.append(line)

    if not matched:
        return "", 0

    omitted = 0
    if len(matched) > max_lines:
        omitted = len(matched) - max_lines
        matched = matched[:max_lines]

    excerpt = "\n".join(matched)
    if len(excerpt) > max_chars:
        excerpt = excerpt[:max_chars].rstrip() + "\n…(截断)…"
    return excerpt, omitted


def collect_stages(logs_dir):
    stages = []
    for sf in sorted(glob.glob(os.path.join(logs_dir, "*.status"))):
        name = os.path.basename(sf)[: -len(".status")]
        try:
            state = open(sf).read().strip()
        except OSError:
            state = "fail"
        stages.append((name, state))
    return stages


def post_statuses(repo, sha, token, build_url, stages):
    for name, state in stages:
        code, _ = gh("POST", f"/repos/{repo}/statuses/{sha}", token, {
            "state": "success" if state == "pass" else "failure",
            "context": f"ci/{name}",
            "description": "通过" if state == "pass" else "失败",
            "target_url": build_url,
        })
        print(f"[pr_report] status ci/{name} -> {state} (http {code})")


def build_comment(result, build_no, build_url, stages):
    ok = result == "SUCCESS"
    lines = [MARKER,
             ("✅ **CI 通过**" if ok else "❌ **CI 失败**")
             + f" · 构建 [#{build_no}]({build_url})", ""]
    if stages:
        lines.append("| 阶段 | 结果 |")
        lines.append("|---|---|")
        for name, state in stages:
            title = STAGE_TITLES.get(name, name)
            lines.append(f"| {title} | {'✅' if state == 'pass' else '❌'} |")
    if not ok:
        logs_dir = os.environ.get("CI_LOGS_DIR", "ci-logs")
        for name, state in stages:
            if state == "pass":
                continue
            try:
                with open(os.path.join(logs_dir, name + ".log"), encoding="utf-8", errors="replace") as f:
                    data = f.read()
            except OSError:
                continue
            data = data.strip()
            if not data:
                continue
            title = STAGE_TITLES.get(name, name)
            signal, omitted = extract_signals(data)
            if signal:
                # Signal-first: show the lines that actually name the
                # failure, then a smaller tail for surrounding context.
                if omitted:
                    signal += f"\n…(另有 {omitted} 行同类匹配已省略)…"
                lines += ["",
                          f"<details><summary>❌ {title} 失败定位（关键日志行）</summary>",
                          "", "```", signal, "```", "</details>"]
                t = tail_text(data)
                if t and t != signal:
                    lines += ["",
                              f"<details><summary>{title} 日志（末尾，补充上下文）</summary>",
                              "", "```", t, "```", "</details>"]
            else:
                # No known failure signal matched (unknown failure shape):
                # degrade to the plain tail, same as before this change.
                t = tail_text(data)
                if not t:
                    continue
                lines += ["",
                          f"<details><summary>❌ {title} 日志（末尾）</summary>",
                          "", "```", t, "```", "</details>"]
        lines += ["", f"🔗 完整日志: {build_url}console"]
    return "\n".join(lines)


def upsert_comment(repo, pr, token, comment):
    code, comments = gh("GET", f"/repos/{repo}/issues/{pr}/comments?per_page=100", token)
    cid = None
    if isinstance(comments, list):
        for c in comments:
            if MARKER in (c.get("body") or ""):
                cid = c["id"]
                break
    if cid:
        code, _ = gh("PATCH", f"/repos/{repo}/issues/comments/{cid}", token, {"body": comment})
        print(f"[pr_report] updated sticky comment {cid} (http {code})")
    else:
        code, _ = gh("POST", f"/repos/{repo}/issues/{pr}/comments", token, {"body": comment})
        print(f"[pr_report] created sticky comment (http {code})")


def main():
    token = os.environ.get("GH_TOKEN", "")
    repo = os.environ.get("GH_REPO", "")
    logs_dir = os.environ.get("CI_LOGS_DIR", "ci-logs")
    build_url = os.environ.get("BUILD_URL", "")
    build_no = os.environ.get("BUILD_NUMBER", "?")
    result = os.environ.get("RESULT", "SUCCESS")
    change_id = os.environ.get("CHANGE_ID", "").strip()
    sha = os.environ.get("STATUS_SHA", "").strip()

    if not token or not repo:
        print("[pr_report] missing GH_TOKEN/GH_REPO; nothing to do")
        return

    stages = collect_stages(logs_dir)
    if not stages:
        print("[pr_report] no stage status files found")

    if sha:
        post_statuses(repo, sha, token, build_url, stages)

    # Sticky comment only makes sense on a PR build.
    if change_id:
        upsert_comment(repo, change_id, token, build_comment(result, build_no, build_url, stages))


if __name__ == "__main__":
    main()
