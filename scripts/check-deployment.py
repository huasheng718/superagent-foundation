#!/usr/bin/env python3
"""Check effective Compose JSON on stdin; never print configuration secrets.

Configuration checks only: this does not certify a running proxy, GitHub identity
or hostile workload containment. Use with ./start.sh check or production.
"""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import sys
from urllib.parse import urlsplit


def violations(config, public_url, env_path=Path('.env')):
    errors = []
    services = config.get('services', {})
    platform = services.get('platform', {})
    env = platform.get('environment', {})
    for key, expected in (
        ('AGENTFLOW_AUTH_REQUIRED', 'true'),
        ('AGENTFLOW_ALLOW_SIGNUP', 'false'),
        ('AGENTFLOW_SECURE_COOKIES', 'true'),
        ('AGENTFLOW_DELIVERY_POLICY', 'controlled_pr'),
    ):
        if str(env.get(key, '')).lower() != expected:
            errors.append(f'{key} must be {expected}')
    for key, minimum in (('AGENTFLOW_SESSION_SECRET', 32),):
        value = str(env.get(key, ''))
        if len(value) < minimum or any(word in value.upper() for word in ('REPLACE', 'CHANGEME')):
            errors.append(f'{key} must be a unique secret of at least {minimum} characters')
    database = services.get('mysql', {}).get('environment', {})
    passwords = [str(database.get(k, '')) for k in ('MYSQL_PASSWORD', 'MYSQL_ROOT_PASSWORD')]
    if any(len(p) < 24 or any(w in p.upper() for w in ('REPLACE', 'CHANGEME')) for p in passwords) or len(set(passwords)) != 2:
        errors.append('MySQL requires distinct non-placeholder passwords of at least 24 characters')
    for name in ('platform', 'mysql'):
        ports = services.get(name, {}).get('ports', [])
        for port in ports:
            try:
                loopback = ipaddress.ip_address(port.get('host_ip', '')).is_loopback
            except (ValueError, AttributeError):
                loopback = False
            if not loopback:
                errors.append(f'{name} published ports must bind to loopback behind the HTTPS proxy')
                break
        if services.get(name, {}).get('network_mode') == 'host':
            errors.append(f'{name} must not use host networking')
    image = platform.get('image', '')
    if not image or env.get('AGENTFLOW_ISOLATED_IMAGE') != image:
        errors.append('Platform and default sandbox must use the same image')
    if not platform.get('build') and not re.fullmatch(r'[^\s@]+@sha256:[0-9a-f]{64}', image):
        errors.append('Prebuilt production image must be pinned by @sha256 digest')
    try:
        url = urlsplit(public_url)
        valid_url = url.scheme == 'https' and url.hostname and not url.username and not url.password and url.path in ('', '/') and not url.query and not url.fragment
        _ = url.port
    except ValueError:
        valid_url = False
    if not valid_url:
        errors.append('LOOPFORGE_PUBLIC_URL must be an HTTPS origin without credentials or subpath')
    if not env_path.is_file() or env_path.stat().st_mode & 0o077 or env_path.stat().st_uid != os.getuid():
        errors.append('.env must be owned by the installation user with mode 0600 (or stricter)')
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--public-url', default='')
    args = parser.parse_args()
    try:
        errors = violations(json.load(sys.stdin), args.public_url)
    except (ValueError, TypeError, AttributeError, OSError):
        print('FAIL: Cannot read effective Compose configuration or .env metadata', file=sys.stderr)
        return 1
    if errors:
        for error in errors:
            print('FAIL: ' + error, file=sys.stderr)
        return 1
    print('PASS: Production configuration checks (not live deployment acceptance)')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
