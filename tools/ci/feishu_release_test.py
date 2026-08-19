#!/usr/bin/env python3
"""feishu_release.py 的单元测试。

    python3 tools/ci/feishu_release_test.py

只测纯函数（tag 说明取舍 / changelog 分组 / 卡片构造），不碰网络。
"""
import unittest

from feishu_release import (
    build_release_card,
    group_commits,
    strip_version_prefix,
    usable_notes,
)

US = "\x1f"


def log(*pairs):
    """构造 `git log --pretty=format:'%s%x1f%h'` 的输出。"""
    return "\n".join(f"{subj}{US}{sha}" for subj, sha in pairs)


class TestUsableNotes(unittest.TestCase):
    """annotated tag 才有人工写的发版说明；lightweight tag 拿到的是 merge commit
    标题，必须识别出来并回退到自动 changelog，否则大多数发版会推出空白卡片。"""

    def test_人工发版说明可用(self):
        body = "新增 Kimi 供应商支持。\n\n## 新增功能\n\n- 渠道监控配额模式"
        self.assertEqual(usable_notes(body), body.strip())

    def test_merge_commit_标题不可用(self):
        self.assertEqual(usable_notes("Merge pull request #158 from Synflux-AI/sync"), "")

    def test_单行_conventional_commit_不可用(self):
        # lightweight tag 指向普通提交时 %(contents:body) 就是这个形状
        self.assertEqual(usable_notes("fix(gateway): 流中断原因可观测化（#143 PR1）"), "")
        self.assertEqual(usable_notes("chore: 同步上游 main 到 release (2026-08-15)"), "")

    def test_空值与纯空白不可用(self):
        for v in ("", "   \n  ", None):
            self.assertEqual(usable_notes(v), "")

    def test_多行说明即便首行像_commit_也可用(self):
        # 有人可能以 "feat: xxx" 开头再写详细说明，这仍是人工发版说明
        body = "feat: 大版本更新\n\n详细说明若干\n更多内容"
        self.assertTrue(usable_notes(body))


class TestStripVersionPrefix(unittest.TestCase):
    def test_去掉冗余版本前缀(self):
        self.assertEqual(strip_version_prefix("v0.1.24: 修了点东西"), "修了点东西")
        self.assertEqual(strip_version_prefix("0.1.24：修了点东西"), "修了点东西")

    def test_无前缀原样返回(self):
        self.assertEqual(strip_version_prefix("## 版本亮点"), "## 版本亮点")

    def test_空值安全(self):
        self.assertEqual(strip_version_prefix(""), "")
        self.assertEqual(strip_version_prefix(None), "")


class TestGroupCommits(unittest.TestCase):
    def test_按类型分组(self):
        raw = log(
            ("feat: 加了 A", "aaa1111"),
            ("fix(api): 修了 B", "bbb2222"),
            ("perf: 快了 C", "ccc3333"),
            ("feat(ui): 加了 D", "ddd4444"),
        )
        groups, misc = group_commits(raw)
        self.assertEqual(misc, 0)
        self.assertEqual([c["msg"] for c in groups["feat"]], ["加了 A", "加了 D"])
        self.assertEqual(groups["fix"][0]["msg"], "修了 B")
        self.assertEqual(groups["fix"][0]["sha"], "bbb2222")
        self.assertEqual(groups["perf"][0]["msg"], "快了 C")

    def test_其他类型与无前缀计入杂项(self):
        raw = log(
            ("docs: 文档", "a1"),
            ("chore: 杂务", "a2"),
            ("test: 测试", "a3"),
            ("随手改了点东西", "a4"),
            ("feat: 真功能", "a5"),
        )
        groups, misc = group_commits(raw)
        self.assertEqual(misc, 4)
        self.assertEqual(len(groups["feat"]), 1)

    def test_破坏性变更标记不影响解析(self):
        groups, _ = group_commits(log(("feat!: 破坏性变更", "a1"), ("fix(api)!: 另一个", "a2")))
        self.assertEqual(groups["feat"][0]["msg"], "破坏性变更")
        self.assertEqual(groups["fix"][0]["msg"], "另一个")

    def test_支持中文全角冒号(self):
        groups, misc = group_commits(log(("feat：中文冒号", "a1")))
        self.assertEqual(misc, 0)
        self.assertEqual(groups["feat"][0]["msg"], "中文冒号")

    def test_超过上限则截断并记录余量(self):
        raw = log(*[(f"feat: 功能{i}", f"h{i}") for i in range(20)])
        groups, _ = group_commits(raw, cap=15)
        self.assertEqual(len(groups["feat"]), 15)
        self.assertEqual(groups["extra"]["feat"], 5)

    def test_空输入安全(self):
        groups, misc = group_commits("")
        self.assertEqual(misc, 0)
        self.assertEqual(groups["feat"], [])

    def test_忽略格式异常的行(self):
        groups, misc = group_commits("没有分隔符的行\n" + log(("feat: 正常", "a1")))
        self.assertEqual(len(groups["feat"]), 1)
        self.assertEqual(misc, 0)


class TestBuildReleaseCard(unittest.TestCase):
    BASE = {
        "tag": "v0.1.178",
        "prev_tag": "v0.1.177",
        "repo": "Synflux-AI/sub2api",
        "run_url": "https://github.com/Synflux-AI/sub2api/actions/runs/900",
        "ghcr_image": "ghcr.io/synflux-ai/sub2api",
        "dockerhub_image": "",
    }

    @staticmethod
    def _text(card):
        out = [card["card"]["header"]["title"]["content"]]

        def walk(n):
            if isinstance(n, dict):
                if n.get("tag") == "lark_md":
                    out.append(n.get("content", ""))
                for v in n.values():
                    walk(v)
            elif isinstance(n, list):
                for v in n:
                    walk(v)

        walk(card["card"]["elements"])
        return "\n".join(out)

    def test_成功卡片含版本镜像与链接(self):
        card = build_release_card(self.BASE, "success", notes="新增若干能力", groups=None, misc=0)
        self.assertEqual(card["card"]["header"]["template"], "green")
        title = card["card"]["header"]["title"]["content"]
        self.assertIn("sub2api", title)
        self.assertIn("v0.1.178", title)
        self.assertIn("发版成功", title)
        body = self._text(card)
        self.assertIn("新增若干能力", body)
        self.assertIn("0.1.178", body)                        # 镜像 tag 不带 v
        self.assertIn("ghcr.io/synflux-ai/sub2api", body)
        self.assertIn("v0.1.177", body)                       # 版本跨度
        self.assertIn("releases/tag/v0.1.178", body)

    def test_失败卡片为红色且不列镜像(self):
        card = build_release_card(self.BASE, "failure", notes="", groups=None, misc=0)
        self.assertEqual(card["card"]["header"]["template"], "red")
        self.assertIn("发版失败", card["card"]["header"]["title"]["content"])
        self.assertNotIn("docker pull", self._text(card))

    def test_无人工说明时渲染分组_changelog(self):
        groups = {
            "feat": [{"sha": "a1", "msg": "加了 A"}],
            "fix": [{"sha": "b1", "msg": "修了 B"}],
            "perf": [],
            "extra": {},
        }
        card = build_release_card(self.BASE, "success", notes="", groups=groups, misc=3)
        body = self._text(card)
        self.assertIn("加了 A", body)
        self.assertIn("修了 B", body)
        self.assertIn("a1", body)
        self.assertIn("3", body)          # 杂项计数

    def test_人工说明优先于自动_changelog(self):
        groups = {"feat": [{"sha": "a1", "msg": "自动条目"}], "fix": [], "perf": [], "extra": {}}
        card = build_release_card(self.BASE, "success", notes="人工写的说明", groups=groups, misc=0)
        body = self._text(card)
        self.assertIn("人工写的说明", body)
        self.assertNotIn("自动条目", body)

    def test_配置_dockerhub_时同时给出两个拉取地址(self):
        ctx = {**self.BASE, "dockerhub_image": "hedeqiang/sub2api"}
        body = self._text(build_release_card(ctx, "success", notes="x", groups=None, misc=0))
        self.assertIn("hedeqiang/sub2api", body)
        self.assertIn("ghcr.io/synflux-ai/sub2api", body)

    def test_无_dockerhub_时不出现空镜像名(self):
        body = self._text(build_release_card(self.BASE, "success", notes="x", groups=None, misc=0))
        self.assertNotIn("Docker Hub", body)

    def test_首个版本无_prev_tag_时不渲染版本跨度(self):
        ctx = {**self.BASE, "prev_tag": ""}
        body = self._text(build_release_card(ctx, "success", notes="x", groups=None, misc=0))
        self.assertIn("v0.1.178", body)
        self.assertNotIn("→", body)

    def test_过长说明被截断(self):
        card = build_release_card(self.BASE, "success", notes="很长" * 2000, groups=None, misc=0)
        self.assertLess(len(self._text(card)), 3000)

    def test_始终带跳转按钮(self):
        card = build_release_card(self.BASE, "success", notes="x", groups=None, misc=0)
        actions = [e for e in card["card"]["elements"] if e.get("tag") == "action"]
        self.assertTrue(actions)
        urls = [a["url"] for a in actions[0]["actions"]]
        self.assertIn("https://github.com/Synflux-AI/sub2api/releases/tag/v0.1.178", urls)


if __name__ == "__main__":
    unittest.main(verbosity=2)
