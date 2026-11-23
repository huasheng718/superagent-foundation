#!/usr/bin/env python3
"""Run exact local Git + real HTTP import acceptance in a disposable MySQL schema.

Does not restart shared services, change global settings or write hosted Git.
The temporary SQL account is restricted to the generated test schema.
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
    parser.add_argument("--container", required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--flow", choices=("import", "update"), default="import")
    args = parser.parse_args()
    if not args.container.startswith("superagent-p0-"):
        parser.error("explicit superagent-p0-* disposable MySQL container required")
    inspection = json.loads(subprocess.run(["docker", "inspect", args.container], check=True, capture_output=True, text=True).stdout)[0]
    bindings = inspection["HostConfig"]["PortBindings"].get("3306/tcp", [])
    if not inspection["State"]["Running"] or not inspection["Config"]["Image"].startswith("mysql:") or len(bindings) != 1 or bindings[0]["HostIp"] != "127.0.0.1":
        raise RuntimeError("running official MySQL with one loopback binding required")
    port = int(bindings[0]["HostPort"])
    suffix = secrets.token_hex(8)
    schema = "team_import_acceptance_" + suffix
    account = "teamimp_" + suffix
    password = secrets.token_hex(24)
    report = {"container": args.container, "port": port, "schema": schema, "cleanupComplete": False, "passed": False}
    started = time.monotonic()

    def admin(sql):
        result = subprocess.run(["docker", "exec", "-i", args.container, "sh", "-lc", 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names'], input=sql, capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError("disposable SQL administration failed; SQL/output omitted")
        return result.stdout.strip()

    created = False
    try:
        admin(f"CREATE DATABASE `{schema}` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;")
        created = True
        admin(f"CREATE USER '{account}'@'%' IDENTIFIED BY '{password}'; GRANT ALL PRIVILEGES ON `{schema}`.* TO '{account}'@'%';")
        report["mysqlVersion"] = admin("SELECT VERSION();")
        env = os.environ.copy()
        env["AGENTFLOW_TEAM_IMPORT_MYSQL_DSN"] = f"{account}:{password}@tcp(127.0.0.1:{port})/{schema}?parseTime=true&charset=utf8mb4&loc=UTC"
        env["AGENTFLOW_TEAM_IMPORT_MYSQL_DISPOSABLE"] = "1"
        test_name = "TestMySQLTeamImportReviewedRealHTTPFlow" if args.flow == "import" else "TestMySQLTeamUpdateReviewedRealHTTPFlow"
        command = ["go", "test", "-v", "./internal/httpapi", "-run", "^" + test_name + "$", "-count=1"]
        result = subprocess.run(command, cwd=Path(__file__).resolve().parents[1], env=env, capture_output=True, text=True)
        output = (result.stdout + result.stderr).replace(password, "[redacted]")
        report.update({"command": " ".join(command), "exitCode": result.returncode, "output": output})
        report["passed"] = result.returncode == 0 and ("--- PASS: " + test_name) in output and "--- SKIP:" not in output
        print(output, end="", flush=True)
        if not report["passed"]:
            raise RuntimeError("real Git/MySQL/HTTP acceptance failed or skipped")
        return 0
    finally:
        if created:
            admin(f"DROP DATABASE IF EXISTS `{schema}`; DROP USER IF EXISTS '{account}'@'%';")
            remaining = admin(f"SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='{schema}'; SELECT COUNT(*) FROM mysql.user WHERE user='{account}';")
            report["cleanupComplete"] = remaining.splitlines() == ["0", "0"]
            if not report["cleanupComplete"]:
                raise RuntimeError("generated disposable schema/user cleanup readback failed")
        report["elapsedSeconds"] = round(time.monotonic() - started, 3)
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, indent=2) + "\n")
        print("Removed generated TeamAI test schema/user; shared MySQL service unchanged", flush=True)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (RuntimeError, subprocess.CalledProcessError, ValueError, KeyError) as error:
        print("Local TeamAI import acceptance failed: " + type(error).__name__ + ": " + str(error), file=sys.stderr)
        sys.exit(1)
