import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import report


class MutationReportTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.environment = patch.dict(os.environ, {
            "JOB_NAME": "pkg/chunk/disk_cache_test.go-1-8",
            "JOB_URL": "https://github.com/example/repo/actions/runs/1/job/2",
            "GITHUB_REPOSITORY": "example/repo",
            "GITHUB_RUN_ID": "1",
            "GITHUB_SHA": "abc123",
        })
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.stats = dict(zip(report.CATEGORIES, (5, 3, 2, 4, 1, 1)))
        self.stats["total"] = 16
        self.stats["score"] = 0.625
        self.stats_path = self.root / "stats.json"
        self.stats_path.write_text(json.dumps(self.stats))
        self.output_dir = self.root / "reports"

    def save(self, job_name):
        with patch.dict(os.environ, {"JOB_NAME": job_name}):
            report.save(self.stats_path, self.output_dir)

    def summary(self, expected):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            success = report.summarize(self.output_dir, expected)
        return success, output.getvalue()

    def test_shards_preserve_all_categories_and_aggregate(self):
        jobs = ["pkg/chunk/disk_cache_test.go-1-8", "pkg/chunk/disk_cache_test.go-2-8"]
        for job in jobs:
            self.save(job)
        paths = list(self.output_dir.glob("*.json"))
        self.assertEqual(len(paths), 2)
        self.assertEqual(json.loads(paths[0].read_text())["stats"], self.stats)
        success, output = self.summary(jobs)
        self.assertTrue(success)
        self.assertIn("| Total | 0.62 | 10 | 6 | 4 | 8 | 2 | 2 | 32 |", output)
        self.assertIn(os.environ["JOB_URL"], output)

    def test_missing_shard_is_reported_and_fails(self):
        self.save("one")
        success, output = self.summary(["one", "two"])
        self.assertFalse(success)
        self.assertIn("Missing shard reports: two", output)
        self.assertIn("| Total |", output)

    def test_duplicate_shard_fails(self):
        self.save("one")
        path = next(self.output_dir.glob("*.json"))
        (self.output_dir / "duplicate.json").write_text(path.read_text())
        with self.assertRaisesRegex(ValueError, "duplicate"):
            self.summary(["one"])

    def test_different_run_is_rejected(self):
        self.save("one")
        with patch.dict(os.environ, {"GITHUB_RUN_ID": "2"}):
            with self.assertRaisesRegex(ValueError, "run_id"):
                self.summary(["one"])

    def test_missing_stats_cannot_be_saved_as_success(self):
        with self.assertRaises(FileNotFoundError):
            report.save(self.root / "missing.json", self.output_dir)

    def test_corrupt_counts_cannot_be_saved(self):
        self.stats["total"] += 1
        self.stats_path.write_text(json.dumps(self.stats))
        with self.assertRaisesRegex(ValueError, "category counts"):
            report.save(self.stats_path, self.output_dir)

    def test_no_killed_or_survived_mutants_keeps_exclusions(self):
        self.stats["passed"] = self.stats["failed"] = 0
        self.stats["total"] = 8
        self.stats_path.write_text(json.dumps(self.stats))
        self.save("one")
        success, output = self.summary(["one"])
        self.assertTrue(success)
        self.assertIn("| Total | 0.00 | 0 | 0 | 2 | 4 | 1 | 1 | 8 |", output)


if __name__ == "__main__":
    unittest.main()
