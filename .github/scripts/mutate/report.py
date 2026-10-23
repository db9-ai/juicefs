#!/usr/bin/env python3
"""Preserve and aggregate mutation statistics using workflow artifacts."""

import argparse
import hashlib
import json
import os
from pathlib import Path


CATEGORIES = ("passed", "failed", "compile_error", "out_of_coverage", "skip_by_comment", "others")


def read_stats(path):
    stats = json.loads(Path(path).read_text())
    for key in (*CATEGORIES, "total"):
        if type(stats.get(key)) is not int or stats[key] < 0:
            raise ValueError(f"{path}: invalid {key} count")
    if sum(stats[key] for key in CATEGORIES) != stats["total"]:
        raise ValueError(f"{path}: category counts do not match total")
    return stats


def save(stats_path, output_dir):
    job_name = os.environ["JOB_NAME"]
    report = {
        "job_name": job_name,
        "job_url": os.environ["JOB_URL"],
        "repository": os.environ["GITHUB_REPOSITORY"],
        "run_id": os.environ["GITHUB_RUN_ID"],
        "sha": os.environ["GITHUB_SHA"],
        "stats": read_stats(stats_path),
    }
    directory = Path(output_dir)
    directory.mkdir(parents=True, exist_ok=True)
    filename = hashlib.sha256(job_name.encode()).hexdigest() + ".json"
    (directory / filename).write_text(json.dumps(report, indent=2) + "\n")


def summarize(directory, expected_jobs):
    reports = {}
    for path in sorted(Path(directory).glob("*.json")):
        report = json.loads(path.read_text())
        job_name = report["job_name"]
        if job_name in reports:
            raise ValueError(f"duplicate report for {job_name}")
        for field, variable in (("repository", "GITHUB_REPOSITORY"), ("run_id", "GITHUB_RUN_ID"), ("sha", "GITHUB_SHA")):
            if report[field] != os.environ[variable]:
                raise ValueError(f"{path}: report {field} does not match this workflow")
        # Validate the same counters whether reading a shard or an aggregate.
        stats = report["stats"]
        if any(type(stats.get(key)) is not int or stats[key] < 0 for key in (*CATEGORIES, "total")):
            raise ValueError(f"{path}: invalid statistics")
        if sum(stats[key] for key in CATEGORIES) != stats["total"]:
            raise ValueError(f"{path}: category counts do not match total")
        reports[job_name] = report

    expected = set(expected_jobs)
    unexpected = set(reports) - expected
    if unexpected:
        raise ValueError(f"unexpected reports: {sorted(unexpected)}")
    missing = expected - set(reports)
    print("## Mutation test report\n")
    print("Killed and survived mutants are reported separately from compilation errors and exclusions.\n")
    print("| Job | Score | Killed | Survived | Compile error | Out of coverage | Skip by comment | Others | Total |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    totals = dict.fromkeys((*CATEGORIES, "total"), 0)
    for job_name in sorted(reports):
        report = reports[job_name]
        stats = report["stats"]
        count = stats["passed"] + stats["failed"]
        score = stats["passed"] / count if count else 0
        values = " | ".join(str(stats[key]) for key in (*CATEGORIES, "total"))
        print(f"| [{job_name}]({report['job_url']}) | {score:.2f} | {values} |")
        for key in totals:
            totals[key] += stats[key]
    count = totals["passed"] + totals["failed"]
    score = totals["passed"] / count if count else 0
    values = " | ".join(str(totals[key]) for key in (*CATEGORIES, "total"))
    print(f"| Total | {score:.2f} | {values} |")
    if missing:
        print("\nMissing shard reports: " + ", ".join(sorted(missing)))
    return not missing


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    save_parser = commands.add_parser("save")
    save_parser.add_argument("stats")
    save_parser.add_argument("directory")
    summary_parser = commands.add_parser("summary")
    summary_parser.add_argument("directory")
    args = parser.parse_args()
    if args.command == "save":
        save(args.stats, args.directory)
    elif not summarize(args.directory, json.loads(os.environ["EXPECTED_JOBS"])):
        raise SystemExit(1)


if __name__ == "__main__":
    main()
