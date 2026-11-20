#!/usr/bin/env python3
"""Read effective GitHub delivery policy; never print or persist credentials."""
import argparse
import json
import re
import subprocess
import sys


def github(path):
    result = subprocess.run(['gh', 'api', path], text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError('GitHub policy request failed; verify gh authentication and repository administration access')
    return json.loads(result.stdout)


def evaluate(repository, branch, rules, rulesets):
    required = next((rule.get('parameters', {}) for rule in rules if rule.get('type') == 'pull_request'), {})
    source_ids = {rule.get('ruleset_id') for rule in rules}
    effective = [item for item in rulesets if item.get('id') in source_ids]
    checks = {
        'requiredHumanReview': required.get('required_approving_review_count', 0) >= 1,
        'dismissStaleReviews': required.get('dismiss_stale_reviews_on_push') is True,
        'lastPushRequiresOtherApprover': required.get('require_last_push_approval') is True,
        'resolveReviewThreads': required.get('required_review_thread_resolution') is True,
        'denyForcePush': any(rule.get('type') == 'non_fast_forward' for rule in rules),
        'denyBranchDeletion': any(rule.get('type') == 'deletion' for rule in rules),
        'activeWithoutBypass': bool(effective) and all(item.get('enforcement') == 'active' and item.get('bypass_actors') == [] for item in effective),
    }
    return {'repository': repository, 'branch': branch, 'checks': checks,
            'hostingPolicyPassed': all(checks.values()), 'leastPrivilegeCredentialVerified': False,
            'credentialLimitation': 'Read-only policy inspection cannot certify publisher credential permissions. Provision a repository-selected GitHub App installation or fine-grained token; independently verify its effective rights and denied operations.',
            'effectiveRulesetIds': sorted(value for value in source_ids if value is not None)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('repository', help='owner/repository')
    parser.add_argument('--require-least-privilege', action='store_true', help='fail until constrained credential deployment acceptance exists')
    args = parser.parse_args()
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', args.repository):
        parser.error('expected owner/repository')
    try:
        repo = github('repos/' + args.repository)
        branch = repo['default_branch']
        from urllib.parse import quote
        rules = github('repos/' + args.repository + '/rules/branches/' + quote(branch, safe=''))
        listing = github('repos/' + args.repository + '/rulesets?per_page=100')
        rulesets = [github('repos/' + args.repository + '/rulesets/' + str(item['id'])) for item in listing]
        report = evaluate(args.repository, branch, rules, rulesets)
    except (RuntimeError, ValueError, KeyError, OSError, TypeError):
        print(json.dumps({'error': 'Cannot verify effective GitHub policy; no acceptance claimed'}))
        return 2
    print(json.dumps(report, indent=2))
    return 0 if report['hostingPolicyPassed'] and not args.require_least_privilege else 2


if __name__ == '__main__':
    sys.exit(main())
