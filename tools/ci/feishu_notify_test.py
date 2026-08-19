#!/usr/bin/env python3
"""feishu_notify.py 的单元测试。

仓库没有 pytest 基建，所以用 stdlib unittest 写成可直接运行的脚本：
    python3 tools/ci/feishu_notify_test.py
CI 里由 backend-ci.yml 的 shell job 调用。

只测纯函数（聚合判定 / 失败摘要 / 卡片构造），不碰网络。
"""
import unittest

from feishu_notify import REQUIRED_CHECKS, _first_line, aggregate, build_card, format_failures


def runs(**names):
    """把 {check 名: conclusion} 展开成 check-runs API 的最小形状。

    conclusion 为 None 表示该检查还在跑（status != completed），用于验证"未报告"。
    """
    out = []
    for i, (name, conclusion) in enumerate(names.items()):
        out.append(
            {
                "id": 1000 + i,
                "name": name.replace("_", "-"),
                "status": "completed" if conclusion else "in_progress",
                "conclusion": conclusion,
                "completed_at": f"2026-08-19T10:00:{i:02d}Z",
                "details_url": f"https://github.com/o/r/actions/runs/900/job/{1000 + i}",
            }
        )
    return out


ALL_GREEN = dict.fromkeys((n.replace("-", "_") for n in REQUIRED_CHECKS), "success")


class TestAggregate(unittest.TestCase):
    def test_全绿则整体成功(self):
        overall, results = aggregate(runs(**ALL_GREEN))
        self.assertEqual(overall, "success")
        self.assertEqual(set(results), set(REQUIRED_CHECKS))
        self.assertTrue(all(v == "success" for v in results.values()))

    def test_任一失败则整体失败(self):
        overall, results = aggregate(runs(**{**ALL_GREEN, "test": "failure"}))
        self.assertEqual(overall, "failure")
        self.assertEqual(results["test"], "failure")

    def test_skipped_与_neutral_视为通过(self):
        # GitHub 分支保护把 skipped 当作通过，卡片判定必须一致，否则会把
        # 合得进去的 PR 报成红灯。
        overall, _ = aggregate(runs(**{**ALL_GREEN, "shell": "skipped", "frontend": "neutral"}))
        self.assertEqual(overall, "success")

    def test_cancelled_与_timed_out_视为失败(self):
        for bad in ("cancelled", "timed_out", "action_required"):
            with self.subTest(bad=bad):
                overall, _ = aggregate(runs(**{**ALL_GREEN, "test": bad}))
                self.assertEqual(overall, "failure")

    def test_有检查未报告则整体为_partial(self):
        # Security Scan 轮询超时的场景：不能报绿（没验证过），也不该报红（没失败）。
        partial = {**ALL_GREEN}
        partial["backend_security"] = None
        overall, results = aggregate(runs(**partial))
        self.assertEqual(overall, "partial")
        self.assertIsNone(results["backend-security"])

    def test_完全缺失的检查也算未报告(self):
        subset = {k: "success" for k in list(ALL_GREEN)[:3]}
        overall, results = aggregate(runs(**subset))
        self.assertEqual(overall, "partial")
        self.assertIsNone(results["backend-security"])

    def test_失败优先于未报告(self):
        mixed = {**ALL_GREEN, "test": "failure"}
        mixed["frontend_security"] = None
        overall, _ = aggregate(runs(**mixed))
        self.assertEqual(overall, "failure")

    def test_忽略非必需检查(self):
        extra = runs(**ALL_GREEN) + runs(notify="failure")
        overall, results = aggregate(extra)
        self.assertEqual(overall, "success")
        self.assertNotIn("notify", results)

    def test_同名重复时最新的结论胜出(self):
        # 触发去重后不该再出现同名重复；万一复发，与分支保护一致取最新一份，
        # 而不是让卡片和门禁结论打架。
        dup = runs(**ALL_GREEN)
        old = dict(dup[0], conclusion="failure", completed_at="2026-08-19T09:00:00Z")
        overall, results = aggregate([old] + dup)
        self.assertEqual(overall, "success")
        self.assertEqual(results[dup[0]["name"]], "success")


class TestFirstLine(unittest.TestCase):
    def test_多行提交消息只取标题行(self):
        self.assertEqual(_first_line("docs: 更新说明\n\n正文一\n正文二"), "docs: 更新说明")

    def test_单行原样返回并去空白(self):
        self.assertEqual(_first_line("  fix: 补边界  "), "fix: 补边界")

    def test_空值与_None_返回空串(self):
        self.assertEqual(_first_line(""), "")
        self.assertEqual(_first_line(None), "")


class TestFormatFailures(unittest.TestCase):
    def test_带失败_step(self):
        text = format_failures([("test", ["Integration tests"]), ("golangci-lint", ["golangci-lint"])])
        self.assertIn("test", text)
        self.assertIn("Integration tests", text)
        self.assertIn("golangci-lint", text)

    def test_取不到_step_时只列_job(self):
        text = format_failures([("shell", [])])
        self.assertIn("shell", text)

    def test_无失败返回空串(self):
        self.assertEqual(format_failures([]), "")


class TestBuildCard(unittest.TestCase):
    PR_CTX = {
        "event": "pull_request",
        "run_number": "42",
        "run_url": "https://github.com/o/r/actions/runs/900",
        "pr_number": "161",
        "pr_title": "ci: 消除重复触发",
        "pr_author": "chenguowei",
        "pr_head": "ci/dedupe",
        "pr_base": "release",
    }
    PUSH_CTX = {
        "event": "push",
        "run_number": "43",
        "run_url": "https://github.com/o/r/actions/runs/901",
        "branch": "release",
        "commit_author": "chenguowei",
        "commit_sha": "62c42287",
        "commit_subject": "docs: 更新 CI 说明",
    }

    @staticmethod
    def _text(card):
        """把卡片里所有文本拼起来，便于断言内容出现过。"""
        out = [card["card"]["header"]["title"]["content"]]

        def walk(node):
            if isinstance(node, dict):
                if node.get("tag") == "lark_md":
                    out.append(node.get("content", ""))
                for v in node.values():
                    walk(v)
            elif isinstance(node, list):
                for v in node:
                    walk(v)

        walk(card["card"]["elements"])
        return "\n".join(out)

    def test_PR_成功卡片(self):
        card = build_card(self.PR_CTX, "success", dict.fromkeys(REQUIRED_CHECKS, "success"), [])
        self.assertEqual(card["msg_type"], "interactive")
        self.assertEqual(card["card"]["header"]["template"], "green")
        title = card["card"]["header"]["title"]["content"]
        self.assertIn("sub2api", title)
        self.assertIn("PR #161", title)
        self.assertIn("CI 通过", title)
        body = self._text(card)
        self.assertIn("ci: 消除重复触发", body)
        self.assertIn("chenguowei", body)
        self.assertIn("ci/dedupe", body)
        self.assertIn("release", body)
        self.assertNotIn("失败检查", body)

    def test_PR_失败卡片含失败_job_与_step(self):
        results = {**dict.fromkeys(REQUIRED_CHECKS, "success"), "test": "failure"}
        card = build_card(self.PR_CTX, "failure", results, [("test", ["Integration tests"])])
        self.assertEqual(card["card"]["header"]["template"], "red")
        self.assertIn("CI 失败", card["card"]["header"]["title"]["content"])
        body = self._text(card)
        self.assertIn("失败检查", body)
        self.assertIn("Integration tests", body)

    def test_push_卡片用分支与提交信息(self):
        card = build_card(self.PUSH_CTX, "success", dict.fromkeys(REQUIRED_CHECKS, "success"), [])
        title = card["card"]["header"]["title"]["content"]
        self.assertIn("release", title)
        body = self._text(card)
        self.assertIn("62c42287", body)
        self.assertIn("docs: 更新 CI 说明", body)
        self.assertNotIn("PR #", body)

    def test_partial_用橙色且措辞不报绿(self):
        results = {**dict.fromkeys(REQUIRED_CHECKS, "success"), "backend-security": None}
        card = build_card(self.PR_CTX, "partial", results, [])
        self.assertEqual(card["card"]["header"]["template"], "orange")
        self.assertNotIn("CI 通过", card["card"]["header"]["title"]["content"])
        self.assertIn("backend-security", self._text(card))

    def test_始终带跳转按钮(self):
        card = build_card(self.PR_CTX, "success", dict.fromkeys(REQUIRED_CHECKS, "success"), [])
        actions = [e for e in card["card"]["elements"] if e.get("tag") == "action"]
        self.assertEqual(len(actions), 1)
        self.assertEqual(actions[0]["actions"][0]["url"], self.PR_CTX["run_url"])

    def test_缺失的可选字段整行跳过而不报错(self):
        # Jenkins 版的约定：字段拿不到就跳过该行，绝不失败。
        card = build_card({"event": "push", "run_number": "1", "run_url": "u"}, "success", {}, [])
        self.assertIn("sub2api", card["card"]["header"]["title"]["content"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
