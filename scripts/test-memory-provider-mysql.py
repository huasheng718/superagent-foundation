#!/usr/bin/env python3
"""Verify manual038 + provider intents in a new owned local MySQL schema only."""
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--container', required=True)
parser.add_argument('--report', type=Path, required=True)
args = parser.parse_args()
if not args.container.startswith('superagent-p0-'):
    parser.error('explicit disposable superagent-p0-* MySQL container required')
inspection = json.loads(subprocess.run(['docker', 'inspect', args.container], check=True, capture_output=True, text=True).stdout)[0]
bindings = inspection['HostConfig']['PortBindings'].get('3306/tcp', [])
if not inspection['State']['Running'] or not inspection['Config']['Image'].startswith('mysql:') or len(bindings) != 1 or bindings[0]['HostIp'] != '127.0.0.1':
    raise RuntimeError('running official MySQL with single loopback binding required')
port = int(bindings[0]['HostPort'])
suffix = secrets.token_hex(8)
schema, account, password = 'provider_acceptance_' + suffix, 'memp_' + suffix, secrets.token_hex(24)
root = Path(__file__).resolve().parents[1]
report = {'container': args.container, 'schema': schema, 'port': port, 'passed': False, 'cleanupComplete': False}
started = time.monotonic()


def admin(sql):
    result = subprocess.run(['docker', 'exec', '-i', args.container, 'sh', '-lc', 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names'], input=sql, capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError('owned SQL administration failed; SQL/output omitted')
    return result.stdout.strip()


created = False
try:
    admin(f'CREATE DATABASE `{schema}` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;')
    created = True
    admin(f"CREATE USER '{account}'@'%' IDENTIFIED BY '{password}'; GRANT ALL PRIVILEGES ON `{schema}`.* TO '{account}'@'%';")
    # Apply manual SQL before Open/AutoMigrate; a separate sentinel proves no reset.
    sql = (root / 'migrations/038_governed_memory_provider.sql').read_text()
    admin(f'USE `{schema}`; CREATE TABLE migration_sentinel (id INT PRIMARY KEY, value VARCHAR(32)); INSERT INTO migration_sentinel VALUES (1,"preserved");' + sql)
    report['manual038TablesBeforeOpen'] = admin(f"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='{schema}' AND table_name IN ('memory_provider_mirrors','memory_recall_receipts');") == '2'
    report['mysqlVersion'] = admin('SELECT VERSION();')
    env = os.environ.copy()
    env['AGENTFLOW_MEMORY_PROVIDER_MYSQL_DSN'] = f'{account}:{password}@tcp(127.0.0.1:{port})/{schema}?parseTime=true&charset=utf8mb4&loc=UTC'
    env['AGENTFLOW_MEMORY_PROVIDER_MYSQL_DISPOSABLE'] = '1'
    command = ['go', 'test', '-v', './internal/repo', '-run', '^TestMySQLMemoryProviderIntentAcrossStoresAndFreshProcess$', '-count=1']
    result = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
    output = (result.stdout + result.stderr).replace(password, '[redacted]')
    report.update({'command': ' '.join(command), 'exitCode': result.returncode, 'output': output})
    report['sentinelPreserved'] = admin(f'SELECT value FROM `{schema}`.migration_sentinel WHERE id=1;') == 'preserved'
    report['passed'] = result.returncode == 0 and '--- PASS: TestMySQLMemoryProviderIntentAcrossStoresAndFreshProcess' in output and '--- SKIP:' not in output and report['manual038TablesBeforeOpen'] and report['sentinelPreserved']
    print(output, end='')
    if not report['passed']:
        raise RuntimeError('owned provider MySQL acceptance failed or skipped')
finally:
    if created:
        admin(f"DROP DATABASE IF EXISTS `{schema}`; DROP USER IF EXISTS '{account}'@'%';")
        report['cleanupComplete'] = admin(f"SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='{schema}'; SELECT COUNT(*) FROM mysql.user WHERE user='{account}';").splitlines() == ['0', '0']
    report['elapsedSeconds'] = round(time.monotonic() - started, 3)
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, indent=2) + '\n')
    if not report['cleanupComplete']:
        raise RuntimeError('owned schema/user cleanup readback failed')
