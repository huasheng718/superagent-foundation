#!/usr/bin/env python3
"""Run budget or release durability acceptance in an explicitly disposable MySQL container.

Creates a random schema and least-privilege temporary SQL user, passes its DSN
only through the child environment, and removes both after the test. Does not
read an application DSN or print credential values.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--container", required=True, help="explicit disposable superagent-p0-* MySQL container")
    parser.add_argument("--report", type=Path, help="optional sanitized JSON evidence output")
    parser.add_argument("--suite", choices=("budget", "releases", "candidates", "distribution", "release-distribution", "source-guard", "extractions"), default="budget", help="durability contract to verify")
    args = parser.parse_args()
    if not args.container.startswith("superagent-p0-"):
        parser.error("only an explicitly named superagent-p0-* disposable container is accepted")
    probe = subprocess.run(
        ["docker", "inspect", "--format", "{{json .State.Running}} {{json .Config.Image}} {{json .HostConfig.PortBindings}}", args.container],
        text=True, capture_output=True, check=True,
    ).stdout.strip()
    running, image, binding_json = probe.split(" ", 2)
    if running != "true" or not json.loads(image).startswith("mysql:"):
        raise RuntimeError("disposable container must already be running an official MySQL image")
    bindings = json.loads(binding_json).get("3306/tcp", [])
    if len(bindings) != 1 or bindings[0]["HostIp"] != "127.0.0.1":
        raise RuntimeError("disposable MySQL must have exactly one loopback-only port binding")
    port = int(bindings[0]["HostPort"])
    suffix = secrets.token_hex(8)
    prefix = {"budget":"budget","releases":"release","candidates":"candidate","distribution":"distribution","release-distribution":"distribution","source-guard":"distribution","extractions":"extraction"}[args.suite]
    schema = prefix + "_acceptance_" + suffix
    user = prefix + "_" + suffix
    password = secrets.token_hex(24)

    def sql(statement):
        result = subprocess.run(
            ["docker", "exec", "-i", args.container, "sh", "-lc", 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names'],
            input=statement, text=True, capture_output=True,
        )
        if result.returncode:
            raise RuntimeError(f"disposable SQL administration failed (exit {result.returncode}); credential-bearing input/output omitted")
        return result.stdout.strip()

    created = False
    report = {"suite": args.suite, "container": args.container, "host": "127.0.0.1", "port": port, "schema": schema, "cleanupComplete": False}
    started = time.monotonic()
    try:
        # Identifiers/password are generated hexadecimal values, never external
        # SQL fragments; no privileged credential is copied out of the container.
        sql(f"CREATE DATABASE `{schema}` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;")
        created = True
        sql(f"CREATE USER '{user}'@'%' IDENTIFIED BY '{password}';\n"
            f"GRANT ALL PRIVILEGES ON `{schema}`.* TO '{user}'@'%';\n")
        if args.suite == "extractions":
            migration = Path(__file__).resolve().parents[1] / "migrations/039_knowledge_extractions.sql"
            sql(f"USE `{schema}`;\n" + migration.read_text())
            report["manualMigration"] = migration.name
        report["mysqlVersion"] = sql("SELECT VERSION();")
        print(f"Disposable MySQL {report['mysqlVersion']} on 127.0.0.1:{port}; isolated schema {schema}", flush=True)
        env = os.environ.copy()
        # Do not use AGENTFLOW_MYSQL_DSN or an existing application's schema.
        env_prefix = {"budget":"AGENTFLOW_BUDGET_MYSQL","releases":"AGENTFLOW_RELEASE_MYSQL","candidates":"AGENTFLOW_CANDIDATE_MYSQL","distribution":"AGENTFLOW_DISTRIBUTION_MYSQL","release-distribution":"AGENTFLOW_DISTRIBUTION_MYSQL","source-guard":"AGENTFLOW_DISTRIBUTION_MYSQL","extractions":"AGENTFLOW_EXTRACTION_MYSQL"}[args.suite]
        test_name = {"budget":"TestMySQLExecutionBudgetCrossStoreAndProcessRestart","releases":"TestMySQLTeamReleaseCompareAndSwapAcrossStores","candidates":"TestMySQLSkillCandidateApprovalAcrossStores","distribution":"TestMySQLDistributionPolicyCrossStore","release-distribution":"TestMySQLReleaseDistributionCurrentReadAcrossStores","source-guard":"TestMySQLCatalogDistributionGuardCurrentRead","extractions":"TestMySQLKnowledgeExtraction(AtomicReplayAndCurrentReads|Rollback)"}[args.suite]
        env[env_prefix + "_DSN"] = f"{user}:{password}@tcp(127.0.0.1:{port})/{schema}?parseTime=true&charset=utf8mb4&loc=UTC"
        env[env_prefix + "_DISPOSABLE"] = "1"
        result = subprocess.run(
            ["go", "test", "-v", "./internal/repo", "-run", "^" + test_name + "$", "-count=1"],
            cwd=Path(__file__).resolve().parents[1], env=env, text=True, capture_output=True,
        )
        print(result.stdout, end="", flush=True)
        print(result.stderr, end="", file=sys.stderr, flush=True)
        report.update({"testExitCode": result.returncode, "testOutput": result.stdout + result.stderr, "command": "go test -v ./internal/repo -run ^" + test_name + "$ -count=1"})
        return result.returncode
    finally:
        if created:
            sql(f"DROP DATABASE IF EXISTS `{schema}`;\nDROP USER IF EXISTS '{user}'@'%';\n")
            report["cleanupComplete"] = True
            print("Removed disposable acceptance schema and temporary SQL user", flush=True)
        report["elapsedSeconds"] = round(time.monotonic() - started, 3)
        if args.report is not None:
            args.report.parent.mkdir(parents=True, exist_ok=True)
            args.report.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (RuntimeError, subprocess.CalledProcessError, ValueError, KeyError) as exc:
        print(f"MySQL acceptance failed: {type(exc).__name__}: {exc}", file=sys.stderr)
        sys.exit(1)
