#!/usr/bin/env python3
"""Normal blob backup/restore and quiesced retention in disposable MySQL only.

No production DSN, global database settings, shared service restart or network
fault injection. Creates two random schemas and one restricted temporary user.
"""
import argparse
import hashlib
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
    args = parser.parse_args()
    if not args.container.startswith("superagent-p0-"):
        parser.error("explicit superagent-p0-* disposable container required")
    inspected = json.loads(subprocess.run(["docker", "inspect", args.container], check=True, capture_output=True, text=True).stdout)[0]
    bindings = inspected["HostConfig"]["PortBindings"].get("3306/tcp", [])
    if not inspected["State"]["Running"] or not inspected["Config"]["Image"].startswith("mysql:") or len(bindings) != 1 or bindings[0]["HostIp"] != "127.0.0.1":
        raise RuntimeError("running official MySQL with a single loopback binding required")
    port = int(bindings[0]["HostPort"])
    suffix = secrets.token_hex(8)
    source = "operations_acceptance_" + suffix
    restored = source + "_restored"
    user = "ops_" + suffix
    password = secrets.token_hex(24)
    report = {"container": args.container, "port": port, "sourceSchema": source, "restoredSchema": restored, "cleanupComplete": False, "phases": [], "collectionMode": "quiesced; no concurrent writers or production scheduler", "testCutoffHours": 24}
    started = time.monotonic()

    def admin(sql):
        result = subprocess.run(["docker", "exec", "-i", args.container, "sh", "-lc", 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names'], input=sql, capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError("private disposable SQL administration failed; input/output omitted")
        return result.stdout.strip()

    def restricted(command, schema, payload=b""):
        # Password travels through stdin; it never appears in process argv,
        # retained dump, terminal output or report.
        shell = 'IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec "$@"'
        result = subprocess.run(["docker", "exec", "-i", args.container, "sh", "-lc", shell, "sh", command, "-u" + user, "--max-allowed-packet=64M"] + (["--single-transaction", "--skip-lock-tables", "--no-tablespaces", "--set-gtid-purged=OFF", "--hex-blob", schema] if command == "mysqldump" else [schema]), input=password.encode() + b"\n" + payload, capture_output=True)
        if result.returncode:
            raise RuntimeError("restricted disposable dump/restore failed; credential-bearing output omitted")
        return result.stdout

    def phase(name, schema):
        env = os.environ.copy()
        env["AGENTFLOW_OPERATIONS_MYSQL_DSN"] = f"{user}:{password}@tcp(127.0.0.1:{port})/{schema}?parseTime=true&charset=utf8mb4&loc=UTC"
        env["AGENTFLOW_OPERATIONS_MYSQL_DISPOSABLE"] = "1"
        env["AGENTFLOW_OPERATIONS_MYSQL_PHASE"] = name
        command = ["go", "test", "-v", "./internal/repo", "-run", "^TestMySQLContributionBlobBackupRestoreAndQuiescedGC$", "-count=1"]
        result = subprocess.run(command, cwd=Path(__file__).resolve().parents[1], env=env, capture_output=True, text=True)
        output = (result.stdout + result.stderr).replace(password, "[redacted]")
        report["phases"].append({"phase": name, "command": " ".join(command), "exitCode": result.returncode, "output": output})
        print(output, end="", flush=True)
        if result.returncode or "--- PASS: TestMySQLContributionBlobBackupRestoreAndQuiescedGC" not in output or "--- SKIP:" in output:
            raise RuntimeError("normal operations phase failed or skipped: " + name)

    created = False
    try:
        for schema in (source, restored):
            admin(f"CREATE DATABASE `{schema}` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;")
            created = True
        admin(f"CREATE USER '{user}'@'%' IDENTIFIED BY '{password}'; GRANT ALL PRIVILEGES ON `{source}`.* TO '{user}'@'%'; GRANT ALL PRIVILEGES ON `{restored}`.* TO '{user}'@'%';")
        report["mysqlVersion"] = admin("SELECT VERSION();")
        phase("seed", source)
        dump = restricted("mysqldump", source)
        report["backupBytes"] = len(dump)
        report["backupSHA256"] = hashlib.sha256(dump).hexdigest()
        # Only our private source schema is removed. The receiver cannot read
        # source bytes during the recovery check; it depends on the dump.
        admin(f"DROP DATABASE `{source}`;")
        report["sourceSchemaRemovedBeforeRestore"] = True
        restricted("mysql", restored, dump)
        phase("restored", restored)
        phase("collected", restored)
        report["passed"] = True
        return 0
    finally:
        if created:
            admin(f"DROP DATABASE IF EXISTS `{source}`; DROP DATABASE IF EXISTS `{restored}`; DROP USER IF EXISTS '{user}'@'%';")
            report["cleanupComplete"] = True
        report["elapsedSeconds"] = round(time.monotonic() - started, 3)
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, indent=2) + "\n")
        print("Removed only generated operation schemas/user; shared MySQL service unchanged", flush=True)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (RuntimeError, subprocess.CalledProcessError, ValueError, KeyError) as error:
        print("Local operations acceptance failed: " + type(error).__name__ + ": " + str(error), file=sys.stderr)
        sys.exit(1)
