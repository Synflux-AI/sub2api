#!/usr/bin/env python3
"""发版时往飞书推一张卡片。取代 release.yml 里那个 Telegram 通知步骤。

（原 Telegram 步骤从未真正发出过——它在 TELEGRAM_BOT_TOKEN 为空时 exit 0，而仓库
里从来没有配过这个 secret。）

内容来源分两种，因为本仓的 tag 有两类：
  * annotated tag（如 v0.1.178）——`%(contents:body)` 是人工写的发版说明
    （「## 版本亮点 / ## 新增功能 …」），直接用，这是最好的素材。
  * lightweight tag（如 v0.1.177）——`%(contents:body)` 只是它所指提交的消息体，
    通常是一行 merge commit 标题，毫无信息量。最近 5 个 tag 里有 3 个是这种，
    所以必须回退到按 feat/fix/perf 分组的自动 changelog（移植自原 Jenkinsfile 的
    buildChangelogElements）。

Stdlib only。输入全部来自环境变量（见 main()）。
缺 FEISHU_WEBHOOK 时静默跳过，且任何异常都不让发版流程失败。
"""
import json
import os
import re
import sys
import urllib.request

US = "\x1f"  # git log 里 %x1f 分隔 subject 与 short sha

SECTIONS = (("feat", "✨ 功能"), ("fix", "🐛 修复"), ("perf", "⚡ 优化"))

# 与原 Jenkinsfile 一致：feat(scope)!: msg / feat：msg 都要能解析
_CONVENTIONAL = re.compile(r"^(\w+)(?:\([^)]*\))?!?[:：]\s*")
_VERSION_PREFIX = re.compile(r"^\s*[vV]?\d[\w.\-]*\s*[:：]\s*")

NOTES_LIMIT = 1500  # 飞书卡片能承载更多，但太长的说明在群里刷屏，截断更可读

COLOR = {"success": "green", "failure": "red", "cancelled": "grey"}
EMOJI = {"success": "🚀", "failure": "❌", "cancelled": "🚫"}
STATUS = {"success": "发版成功", "failure": "发版失败", "cancelled": "发版已中断"}


def strip_version_prefix(text):
    """去掉说明里冗余的版本号前缀（"v0.1.24: xxx" -> "xxx"），头部已经显示版本。"""
    return _VERSION_PREFIX.sub("", text or "").strip()


def usable_notes(body):
    """判断 tag 说明是不是人工写的发版说明；不是则返回空串，由调用方回退到 changelog。

    lightweight tag 的 body 是它所指提交的消息，典型形状是单行的 merge commit 标题
    或 conventional commit 标题——这些不能当发版说明用。多行内容一律认为是人工写的。
    """
    text = strip_version_prefix(body)
    if not text:
        return ""
    lines = [ln for ln in text.splitlines() if ln.strip()]
    if len(lines) > 1:
        return text
    only = lines[0].strip()
    if only.startswith("Merge ") or _CONVENTIONAL.match(only):
        return ""
    return text


def group_commits(raw, cap=15):
    """把 `git log --pretty=format:'%s%x1f%h'` 的输出按 commit 类型分组。

    返回 ``(groups, misc)``：``groups`` 含 feat/fix/perf 三个列表与 ``extra``
    （每组被 cap 截掉的条数），``misc`` 是其余类型（docs/chore/ci…）与无前缀提交的总数。
    """
    groups = {key: [] for key, _ in SECTIONS}
    groups["extra"] = {}
    misc = 0
    for line in (raw or "").splitlines():
        if US not in line:
            continue
        subject, _, sha = line.partition(US)
        m = _CONVENTIONAL.match(subject)
        kind = m.group(1).lower() if m else ""
        if kind in groups and kind != "extra":
            groups[kind].append({"sha": sha.strip(), "msg": subject[m.end():].strip()})
        else:
            misc += 1
    for key, _ in SECTIONS:
        if len(groups[key]) > cap:
            groups["extra"][key] = len(groups[key]) - cap
            groups[key] = groups[key][:cap]
    return groups, misc


def _changelog_elements(groups, misc):
    """按原 Jenkins 卡片的两列排版渲染 changelog：左栏短 sha，右栏消息，逐行对齐。"""
    elements = []
    for key, title in SECTIONS:
        items = groups.get(key) or []
        if not items:
            continue
        left = [f"**{title}**"] + [f"**{it['sha']}**" for it in items]
        right = ["　"] + [it["msg"] for it in items]
        extra = (groups.get("extra") or {}).get(key, 0)
        if extra:
            left.append("　")
            right.append(f"…还有 {extra} 条")
        elements.append(
            {
                "tag": "column_set",
                "flex_mode": "none",
                "columns": [
                    {
                        "tag": "column",
                        "width": "72px",
                        "vertical_align": "top",
                        "elements": [{"tag": "div", "text": {"tag": "lark_md", "content": "\n".join(left)}}],
                    },
                    {
                        "tag": "column",
                        "width": "weighted",
                        "weight": 1,
                        "vertical_align": "top",
                        "elements": [{"tag": "div", "text": {"tag": "lark_md", "content": "\n".join(right)}}],
                    },
                ],
            }
        )
    if misc:
        elements.append(
            {"tag": "div", "text": {"tag": "lark_md", "content": f"+ {misc} 项杂项(docs·chore·ci 等)"}}
        )
    return elements


def _div(content):
    return {"tag": "div", "text": {"tag": "lark_md", "content": content}}


def build_release_card(ctx, result, notes, groups, misc):
    """构造发版卡片。人工写的 tag 说明优先；没有才渲染自动 changelog。"""
    tag = ctx["tag"]
    version = tag[1:] if tag.startswith("v") else tag
    color = COLOR.get(result, "grey")
    title = f"sub2api  {tag}  ·  {STATUS.get(result, result)} {EMOJI.get(result, '🔔')}"

    elements = []
    if notes:
        body = notes if len(notes) <= NOTES_LIMIT else notes[:NOTES_LIMIT] + " …"
        elements.append(_div(f"📌 **发版说明**\n{body}"))
        elements.append({"tag": "hr"})
    elif groups:
        changelog = _changelog_elements(groups, misc)
        if changelog:
            elements.extend(changelog)
            elements.append({"tag": "hr"})

    meta = []
    span = f"{ctx['prev_tag']} → " if ctx.get("prev_tag") else ""
    meta.append(f"🏷️ **版本**: {span}**{tag}**")
    if result == "success":
        pulls = []
        if ctx.get("dockerhub_image"):
            pulls.append(f"# Docker Hub\ndocker pull {ctx['dockerhub_image']}:{version}")
            pulls.append(f"# GitHub Container Registry\ndocker pull {ctx['ghcr_image']}:{version}")
        else:
            pulls.append(f"docker pull {ctx['ghcr_image']}:{version}")
        meta.append("🐳 **镜像**\n```bash\n" + "\n".join(pulls) + "\n```")
    elements.append(_div("\n".join(meta)))

    repo = ctx["repo"]
    links = [f"• [GitHub Release](https://github.com/{repo}/releases/tag/{tag})"]
    if ctx.get("dockerhub_image"):
        links.append(f"• [Docker Hub](https://hub.docker.com/r/{ctx['dockerhub_image']})")
    links.append(f"• [GitHub Packages](https://github.com/{repo}/pkgs/container/sub2api)")
    elements.append({"tag": "hr"})
    elements.append(_div("🔗 **相关链接**\n" + "\n".join(links)))

    buttons = [
        {
            "tag": "button",
            "text": {"tag": "plain_text", "content": "查看 Release"},
            "type": "primary",
            "url": f"https://github.com/{repo}/releases/tag/{tag}",
        }
    ]
    if ctx.get("run_url"):
        buttons.append(
            {
                "tag": "button",
                "text": {"tag": "plain_text", "content": "构建日志"},
                "type": "default",
                "url": ctx["run_url"],
            }
        )
    elements.append({"tag": "action", "actions": buttons})

    return {
        "msg_type": "interactive",
        "card": {
            "config": {"wide_screen_mode": True},
            "header": {"template": color, "title": {"tag": "plain_text", "content": title}},
            "elements": elements,
        },
    }


def post_feishu(hook, payload):
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(
        hook, data=body, headers={"Content-Type": "application/json"}, method="POST"
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        return resp.status, resp.read().decode("utf-8", "replace")[:200]


def main():
    hook = os.environ.get("FEISHU_WEBHOOK", "").strip()
    if not hook:
        print("FEISHU_WEBHOOK 未配置，跳过飞书发版通知")
        return 0

    ctx = {
        "tag": os.environ["TAG"],
        "prev_tag": os.environ.get("PREV_TAG", "").strip(),
        "repo": os.environ["GH_REPO"],
        "run_url": os.environ.get("RUN_URL", ""),
        "ghcr_image": os.environ.get("GHCR_IMAGE", ""),
        "dockerhub_image": os.environ.get("DOCKERHUB_IMAGE", "").strip(),
    }
    result = os.environ.get("RESULT", "success")

    notes = usable_notes(os.environ.get("TAG_BODY", ""))
    groups, misc = (None, 0)
    if not notes:
        groups, misc = group_commits(os.environ.get("CHANGELOG_RAW", ""))
        print(f"tag 说明不可用，回退自动 changelog: misc={misc} "
              f"feat={len(groups['feat'])} fix={len(groups['fix'])} perf={len(groups['perf'])}")

    status, body = post_feishu(hook, build_release_card(ctx, result, notes, groups, misc))
    print(f"结果={result} tag={ctx['tag']} 用人工说明={bool(notes)}")
    print(f"飞书响应 {status}: {body}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # 通知失败绝不让发版失败
        print(f"飞书发版通知失败（已忽略）: {type(exc).__name__}: {exc}")
        sys.exit(0)
