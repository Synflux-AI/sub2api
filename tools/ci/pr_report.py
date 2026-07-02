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
import urllib.error
import urllib.request

API = "https://api.github.com"
MARKER = "<!-- sub2api-ci-report -->"
MAX_LOG_CHARS = 3000  # cap each embedded log tail (GitHub comment limit is 65536)

STAGE_TITLES = {
    "backend-unit": "后端单元测试",
    "golangci-lint": "golangci-lint",
    "govulncheck": "govulncheck",
    "frontend": "前端 lint/typecheck/单测",
    "audit": "pnpm audit",
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


def tail(path, max_chars=MAX_LOG_CHARS):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            data = f.read()
    except OSError:
        return ""
    data = data.strip()
    if len(data) > max_chars:
        data = "…(truncated)…\n" + data[-max_chars:]
    return data


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
            t = tail(os.path.join(logs_dir, name + ".log"))
            if not t:
                continue
            lines += ["",
                      f"<details><summary>❌ {STAGE_TITLES.get(name, name)} 日志（末尾）</summary>",
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
