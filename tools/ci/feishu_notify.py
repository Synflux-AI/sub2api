#!/usr/bin/env python3
"""把「整体 CI 结果」推一张飞书交互卡片，等同原 Jenkinsfile 里 notifyFeishu 的精简状态卡。

为什么需要聚合：GitHub 侧的必需检查分散在两个 workflow ——
``CI``（shell / test / frontend / golangci-lint）与 ``Security Scan``
（backend-security / frontend-security）。任一 workflow 单独发卡都会误报：
CI 全绿而 Security Scan 挂掉时会推出一张绿卡。所以本脚本不看当前 workflow 的
结果，而是直接读 head SHA 上的 check-runs，凑齐 REQUIRED_CHECKS 六项后再发。

为什么不用 ``on: workflow_run`` 做聚合（GitHub 的标准做法）：
``workflow_run`` 只从**默认分支**读取 workflow 文件，而本仓默认分支是 ``main``
（上游同步分支），团队主线是 ``release``。只合进 release 的 workflow 会静默永不
触发。所以改成由 CI workflow 末尾的 notify job 调用本脚本，轮询等另一个 workflow
的两项检查落地。Security Scan 约 35s、CI 约 10min 且同时起跑，实际几乎零等待。

失败时列出失败的 job 与具体失败的 step：check-run 的 ``id`` 就是 Actions 的 job id，
``GET /actions/jobs/{id}`` 直接返回 ``steps[].conclusion``，不必下载日志。

Stdlib only。输入全部来自环境变量（见 main()）。
缺 FEISHU_WEBHOOK 时静默跳过，且任何异常都不让 CI 失败。
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request

API = "https://api.github.com"

# 与 release 分支保护里的 required checks 一一对应。改这里要同时改分支保护，
# 否则卡片和门禁的判定会打架。
REQUIRED_CHECKS = (
    "test",
    "frontend",
    "golangci-lint",
    "shell",
    "backend-security",
    "frontend-security",
)

# GitHub 分支保护把 skipped / neutral 也算通过，卡片判定必须一致，
# 否则会把合得进去的 PR 报成红灯。
PASSING = frozenset({"success", "skipped", "neutral"})

COLOR = {"success": "green", "failure": "red", "partial": "orange"}
EMOJI = {"success": "✅", "failure": "❌", "partial": "⚠️"}
STATUS = {"success": "CI 通过", "failure": "CI 失败", "partial": "CI 未完整报告"}


def latest_runs(check_runs, required=REQUIRED_CHECKS):
    """按名字取每项必需检查「最新一次已完成」的 check-run。

    同名重复本应在触发去重后消失；万一复发，与分支保护一致取最新一份，而不是
    让卡片和门禁结论打架。只在已完成的里面挑，避免一个还在跑的重复项盖掉已有结论。
    """
    out = {}
    for name in required:
        done = [
            c
            for c in check_runs
            if c.get("name") == name and c.get("status") == "completed"
        ]
        out[name] = (
            max(done, key=lambda c: (c.get("completed_at") or "", c.get("id") or 0))
            if done
            else None
        )
    return out


def aggregate(check_runs, required=REQUIRED_CHECKS):
    """判定整体结果。

    返回 ``(overall, results)``：``overall`` 为 success / failure / partial，
    ``results`` 是 检查名 -> conclusion（未报告为 None）。

    partial 表示有检查没报告（轮询超时或 workflow 没触发）——既不能报绿（没验证过），
    也不该报红（没失败）。失败优先于未报告。
    """
    results = {n: (cr.get("conclusion") if cr else None) for n, cr in latest_runs(check_runs, required).items()}
    if any(c is not None and c not in PASSING for c in results.values()):
        overall = "failure"
    elif any(c is None for c in results.values()):
        overall = "partial"
    else:
        overall = "success"
    return overall, results


def format_failures(failures):
    """把 [(job 名, [失败 step 名])] 渲染成 lark_md。取不到 step 就只列 job。"""
    if not failures:
        return ""
    lines = ["❌ **失败检查**"]
    for job, steps in failures:
        lines.append(f"· `{job}`" + (f" → {' / '.join(steps)}" if steps else ""))
    return "\n".join(lines)


def build_card(ctx, overall, results, failures):
    """构造飞书交互卡片。沿用 Jenkins 版的约定：字段拿不到就跳过该行，绝不失败。"""
    ref = f"PR #{ctx['pr_number']}" if ctx.get("pr_number") else (ctx.get("branch") or "build")
    title = f"sub2api  {ref}  ·  {STATUS[overall]} {EMOJI[overall]}"

    meta = []
    if ctx.get("pr_number"):
        suffix = f": {ctx['pr_title']}" if ctx.get("pr_title") else ""
        meta.append(f"🔀 **PR**: #{ctx['pr_number']}{suffix}")
        if ctx.get("pr_author"):
            meta.append(f"👤 **提交人**: {ctx['pr_author']}")
        if ctx.get("pr_head"):
            arrow = f" → {ctx['pr_base']}" if ctx.get("pr_base") else ""
            meta.append(f"🌿 **分支**: {ctx['pr_head']}{arrow}")
    else:
        if ctx.get("branch"):
            meta.append(f"🌿 **分支**: {ctx['branch']}")
        if ctx.get("commit_author"):
            meta.append(f"👤 **提交人**: {ctx['commit_author']}")
        if ctx.get("commit_sha") or ctx.get("commit_subject"):
            meta.append(
                f"📝 **提交**: {ctx.get('commit_sha', '')}  {ctx.get('commit_subject', '')}".rstrip()
            )
    if ctx.get("event"):
        meta.append(f"🔔 **触发**: {ctx['event']}")
    if ctx.get("run_number"):
        meta.append(f"🧱 **构建**: #{ctx['run_number']}")

    elements = [{"tag": "div", "text": {"tag": "lark_md", "content": "\n".join(meta)}}]

    unreported = [n for n, c in results.items() if c is None]
    if unreported:
        elements.append({"tag": "hr"})
        elements.append(
            {
                "tag": "div",
                "text": {
                    "tag": "lark_md",
                    "content": "⚠️ **未报告**: " + ", ".join(f"`{n}`" for n in unreported),
                },
            }
        )

    detail = format_failures(failures)
    if detail:
        elements.append({"tag": "hr"})
        elements.append({"tag": "div", "text": {"tag": "lark_md", "content": detail}})

    elements.append(
        {
            "tag": "action",
            "actions": [
                {
                    "tag": "button",
                    "text": {"tag": "plain_text", "content": "Open in GitHub Actions"},
                    "type": "primary",
                    "url": ctx.get("run_url", ""),
                }
            ],
        }
    )

    return {
        "msg_type": "interactive",
        "card": {
            "config": {"wide_screen_mode": True},
            "header": {"template": COLOR[overall], "title": {"tag": "plain_text", "content": title}},
            "elements": elements,
        },
    }


def _get(url, token):
    req = urllib.request.Request(url, headers={"Accept": "application/vnd.github+json"})
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=20) as resp:
        return json.load(resp)


def fetch_check_runs(repo, sha, token):
    data = _get(f"{API}/repos/{repo}/commits/{sha}/check-runs?per_page=100", token)
    return data.get("check_runs", [])


def fetch_failed_steps(repo, job_id, token):
    """check-run 的 id 就是 Actions job id，所以能直接拿到失败的 step 名。"""
    try:
        job = _get(f"{API}/repos/{repo}/actions/jobs/{job_id}", token)
    except (urllib.error.URLError, urllib.error.HTTPError, ValueError):
        return []
    return [s.get("name", "") for s in job.get("steps", []) if s.get("conclusion") == "failure"]


def post_feishu(hook, payload):
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(
        hook, data=body, headers={"Content-Type": "application/json"}, method="POST"
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        return resp.status, resp.read().decode("utf-8", "replace")[:200]


def _first_line(text):
    """提交消息是整段的（含 body），卡片只要标题那一行。"""
    lines = (text or "").splitlines()
    return lines[0].strip() if lines else ""


def main():
    hook = os.environ.get("FEISHU_WEBHOOK", "").strip()
    if not hook:
        # fork PR 拿不到 secret，或还没配置——静默跳过，不算失败。
        print("FEISHU_WEBHOOK 未配置，跳过飞书通知")
        return 0

    repo = os.environ["GH_REPO"]
    sha = os.environ["HEAD_SHA"]
    token = os.environ.get("GH_TOKEN", "")
    timeout = int(os.environ.get("POLL_TIMEOUT", "180"))

    ctx = {
        "event": os.environ.get("EVENT_NAME", ""),
        "run_number": os.environ.get("RUN_NUMBER", ""),
        "run_url": os.environ.get("RUN_URL", ""),
        "pr_number": os.environ.get("PR_NUMBER", ""),
        "pr_title": _first_line(os.environ.get("PR_TITLE", "")),
        "pr_author": os.environ.get("PR_AUTHOR", ""),
        "pr_head": os.environ.get("PR_HEAD", ""),
        "pr_base": os.environ.get("PR_BASE", ""),
        "branch": os.environ.get("BRANCH", ""),
        "commit_author": os.environ.get("COMMIT_AUTHOR", ""),
        "commit_sha": os.environ.get("COMMIT_SHA", "")[:9],
        "commit_subject": _first_line(os.environ.get("COMMIT_SUBJECT", "")),
    }

    # 等另一个 workflow 的两项检查落地。超时就按 partial 发（橙色 + 列出未报告项），
    # 而不是卡住或谎报绿灯。
    deadline = time.monotonic() + timeout
    check_runs = []
    while True:
        check_runs = fetch_check_runs(repo, sha, token)
        overall, results = aggregate(check_runs)
        if overall != "partial" or time.monotonic() >= deadline:
            break
        print(f"等待未报告的检查: {[n for n, c in results.items() if c is None]}")
        time.sleep(10)

    failures = []
    latest = latest_runs(check_runs)
    for name, concl in results.items():
        if concl is not None and concl not in PASSING:
            cr = latest.get(name) or {}
            failures.append((name, fetch_failed_steps(repo, cr.get("id"), token) if cr.get("id") else []))

    payload = build_card(ctx, overall, results, failures)
    status, body = post_feishu(hook, payload)
    print(f"整体结果={overall} 明细={results}")
    print(f"飞书响应 {status}: {body}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # 通知失败绝不让 CI 失败
        print(f"飞书通知失败（已忽略）: {type(exc).__name__}: {exc}")
        sys.exit(0)
