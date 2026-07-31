#!/usr/bin/env python3
"""Unit tests for tools/ci/pr_report.py's log-excerpt logic.

Run with:
  python3 -m unittest tools/ci/test_pr_report.py
  python3 tools/ci/test_pr_report.py

These pin the regression this change fixes: a failing stage's PR-comment
excerpt must surface the actual failing test name even when it's buried
behind thousands of characters of unrelated log noise (the internal/service
incident this fix was written for), while still degrading gracefully to a
plain tail when no known failure signal is present.

Stdlib only, no network.
"""
import os
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pr_report  # noqa: E402


def noise_lines(min_chars, prefix="[GeminiOAuth] inferGoogleOneTier tier=1 region=us "):
    """Build a block of repeated INFO-log noise at least min_chars long,
    mimicking the structured logging that buried the real failure in the
    incident this fix addresses."""
    lines = []
    size = 0
    i = 0
    while size < min_chars:
        line = f"{prefix}iter={i}"
        lines.append(line)
        size += len(line) + 1
        i += 1
    return "\n".join(lines)


class ExtractSignalsTest(unittest.TestCase):
    def test_go_fail_buried_by_noise_is_extracted(self):
        # Reproduces the incident shape: a "--- FAIL: TestX" line, then
        # >3000 chars of unrelated INFO noise, then the per-package "FAIL"
        # summary line go test prints at the very end.
        log = (
            "=== RUN   TestSomething\n"
            "--- FAIL: TestSomething (0.01s)\n"
            + noise_lines(3500) + "\n"
            "FAIL\tgithub.com/Wei-Shaw/sub2api/internal/service\t177.745s\n"
            "make: *** [Makefile:18: test-unit] Error 1\n"
        )
        self.assertGreater(len(log), 3000)

        signal, omitted = pr_report.extract_signals(log)
        self.assertIn("TestSomething", signal)
        self.assertIn("FAIL\tgithub.com/Wei-Shaw/sub2api/internal/service", signal)
        self.assertEqual(omitted, 0)

        # Sanity check that this is a real regression test: the pre-fix
        # behaviour (a plain tail of the last MAX_LOG_CHARS chars) genuinely
        # misses the test name entirely -- that's what made the original PR
        # comment useless during the incident.
        pre_fix_tail = pr_report.tail_text(log)
        self.assertNotIn("TestSomething", pre_fix_tail)

    def test_go_subtest_and_panic_and_race_forms(self):
        log = (
            "    --- FAIL: TestParent/sub_case (0.00s)\n"
            "panic: runtime error: invalid memory address\n"
            "WARNING: DATA RACE\n"
        )
        signal, omitted = pr_report.extract_signals(log)
        self.assertIn("TestParent/sub_case", signal)
        self.assertIn("panic:", signal)
        self.assertIn("DATA RACE", signal)
        self.assertEqual(omitted, 0)

    def test_testify_assert_block(self):
        log = (
            noise_lines(200) + "\n"
            "    Error Trace:\t/app/internal/service/foo_test.go:42\n"
            "    Error:      \tNot equal:\n"
            "    Test:       \tTestFoo_DoesThing\n"
            + noise_lines(200) + "\n"
        )
        signal, omitted = pr_report.extract_signals(log)
        self.assertIn("Error Trace:", signal)
        self.assertIn("Test:", signal)
        self.assertIn("TestFoo_DoesThing", signal)

    def test_vitest_shaped_log(self):
        log = (
            "RUN v1.6.0\n"
            + noise_lines(500, prefix="stdout noise line ") + "\n"
            "FAIL  src/components/Foo.test.tsx > Foo > renders\n"
            "AssertionError: expected 1 to be 2\n"
            " ✕ Foo > renders\n"
            "Unhandled Error: something blew up\n"
            + noise_lines(500, prefix="more noise line ") + "\n"
        )
        signal, omitted = pr_report.extract_signals(log)
        self.assertIn("FAIL  src/components/Foo.test.tsx", signal)
        self.assertIn("AssertionError", signal)
        self.assertIn("✕", signal)
        self.assertIn("Unhandled Error", signal)
        self.assertEqual(omitted, 0)

    def test_no_signal_falls_back_to_tail(self):
        log = "some build tool crashed with no known signature\n" + noise_lines(4000, prefix="unrelated stdout ")
        signal, omitted = pr_report.extract_signals(log)
        self.assertEqual(signal, "")
        self.assertEqual(omitted, 0)

        # Graceful degradation: with no signal lines, callers fall back to
        # the same plain-tail behaviour the script always had.
        t = pr_report.tail_text(log)
        self.assertTrue(t)
        self.assertTrue(log.strip().endswith(t[len("…(truncated)…\n"):]))

    def test_dedup_and_cap(self):
        # A flaky retry prints the same failure line many times; dedup
        # should collapse exact repeats, and the line cap should still kick
        # in for genuinely distinct lines, reporting how many were omitted.
        repeated = "\n".join(["--- FAIL: TestFlaky (0.01s)"] * 50)
        distinct = "\n".join(f"--- FAIL: TestUnique{i} (0.01s)" for i in range(60))
        log = repeated + "\n" + distinct

        signal, omitted = pr_report.extract_signals(log)

        # Dedup: only one copy of the repeated line survives.
        self.assertEqual(signal.count("TestFlaky"), 1)
        # Cap: 1 deduped "TestFlaky" + 60 distinct = 61 matches total,
        # which exceeds MAX_SIGNAL_LINES, so some must be reported omitted.
        self.assertGreater(omitted, 0)
        self.assertLessEqual(signal.count("\n") + 1, pr_report.MAX_SIGNAL_LINES)


class BuildCommentIntegrationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self._old_ci_logs_dir = os.environ.get("CI_LOGS_DIR")
        os.environ["CI_LOGS_DIR"] = self.tmp

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)
        if self._old_ci_logs_dir is None:
            os.environ.pop("CI_LOGS_DIR", None)
        else:
            os.environ["CI_LOGS_DIR"] = self._old_ci_logs_dir

    def _write_log(self, name, content):
        with open(os.path.join(self.tmp, name + ".log"), "w", encoding="utf-8") as f:
            f.write(content)

    def test_comment_surfaces_buried_go_failure(self):
        log = (
            "--- FAIL: TestGeminiOAuthThing (0.02s)\n"
            + noise_lines(3500) + "\n"
            "FAIL\tgithub.com/Wei-Shaw/sub2api/internal/service\t177.745s\n"
        )
        self._write_log("backend-unit", log)
        comment = pr_report.build_comment("FAILURE", "42", "http://jenkins/job/42/", [("backend-unit", "fail")])
        self.assertIn("TestGeminiOAuthThing", comment)
        self.assertIn("失败定位", comment)
        self.assertLess(len(comment), 65536)

    def test_comment_falls_back_to_tail_when_no_signal(self):
        log = "totally generic crash with no known signature\n" + noise_lines(1000)
        self._write_log("govulncheck", log)
        comment = pr_report.build_comment("FAILURE", "42", "http://jenkins/job/42/", [("govulncheck", "fail")])
        self.assertIn("日志（末尾）", comment)
        self.assertNotIn("失败定位", comment)

    def test_comment_stays_within_size_budget_with_four_failing_stages(self):
        big_log = "--- FAIL: TestX (0.0s)\n" + noise_lines(6000) + "\nFAIL\tpkg\t1s\n"
        stages = []
        for name in ("backend-unit", "golangci-lint", "govulncheck", "frontend"):
            self._write_log(name, big_log)
            stages.append((name, "fail"))
        comment = pr_report.build_comment("FAILURE", "42", "http://jenkins/job/42/", stages)
        self.assertLess(len(comment), 65536)


if __name__ == "__main__":
    unittest.main()
