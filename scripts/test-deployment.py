import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('deployment', ROOT / 'scripts/check-deployment.py')
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.env = Path(temp.name) / '.env'
        self.env.write_text('')
        self.env.chmod(0o600)
        self.config = {'services': {
            'platform': {'image': 'example.test/superagent@sha256:' + 'a' * 64,
                         'ports': [{'host_ip': '127.0.0.1', 'target': 8080}],
                         'environment': {'AGENTFLOW_AUTH_REQUIRED': 'true', 'AGENTFLOW_ALLOW_SIGNUP': 'false',
                                         'AGENTFLOW_SECURE_COOKIES': 'true', 'AGENTFLOW_DELIVERY_POLICY': 'controlled_pr',
                                         'AGENTFLOW_SESSION_SECRET': 's' * 64,
                                         'AGENTFLOW_ISOLATED_IMAGE': 'example.test/superagent@sha256:' + 'a' * 64}},
            'mysql': {'ports': [{'host_ip': '127.0.0.1', 'target': 3306}],
                      'environment': {'MYSQL_PASSWORD': 'p' * 48, 'MYSQL_ROOT_PASSWORD': 'r' * 48}},
        }}

    def errors(self, config=None, url='https://agent.example.test'):
        return deployment.violations(config or self.config, url, self.env)

    def test_source_and_digest_configurations_pass(self):
        self.assertEqual(self.errors(), [])
        platform = self.config['services']['platform']
        platform['build'] = {'context': str(ROOT)}
        platform['image'] = platform['environment']['AGENTFLOW_ISOLATED_IMAGE'] = 'superagent-platform:local'
        self.assertEqual(self.errors(), [])

    def test_exposed_ports_host_network_and_unsafe_authority_fail(self):
        for service in ('platform', 'mysql'):
            for host in ('0.0.0.0', '::', ''):
                config = copy.deepcopy(self.config)
                config['services'][service]['ports'][0]['host_ip'] = host
                self.assertTrue(self.errors(config))
            config = copy.deepcopy(self.config)
            config['services'][service]['network_mode'] = 'host'
            self.assertTrue(self.errors(config))
        for key, value in (('AGENTFLOW_AUTH_REQUIRED', 'false'), ('AGENTFLOW_ALLOW_SIGNUP', 'true'),
                           ('AGENTFLOW_SECURE_COOKIES', 'false'), ('AGENTFLOW_DELIVERY_POLICY', 'legacy')):
            config = copy.deepcopy(self.config)
            config['services']['platform']['environment'][key] = value
            self.assertTrue(self.errors(config))

    def test_image_mismatch_and_mutable_release_fail(self):
        self.config['services']['platform']['environment']['AGENTFLOW_ISOLATED_IMAGE'] = 'other:local'
        self.assertTrue(self.errors())
        self.config['services']['platform']['image'] = 'other:local'
        self.assertTrue(self.errors())

    def test_public_url_secrets_and_env_permissions_fail_closed(self):
        for url in ('http://agent.example.test', 'https://user:pass@agent.example.test',
                    'https://agent.example.test/path', 'https://agent.example.test:bad', ''):
            self.assertTrue(self.errors(url=url))
        self.env.chmod(0o644)
        self.assertTrue(self.errors())
        self.env.chmod(0o600)
        self.config['services']['mysql']['environment']['MYSQL_PASSWORD'] = 'REPLACE_ME'
        self.assertTrue(self.errors())

    def test_cli_redacts_invalid_input(self):
        result = subprocess.run([sys.executable, str(ROOT / 'scripts/check-deployment.py')],
                                input='private-value-not-json', text=True, capture_output=True, cwd=self.env.parent)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('private-value', result.stdout + result.stderr)

    def test_real_compose_effective_configuration(self):
        # Uses the real Compose parser, but creates no service or volume and never prints secrets.
        self.env.write_text('\n'.join((
            'LOOPFORGE_DB_PASSWORD=' + 'p' * 48, 'LOOPFORGE_DB_ROOT_PASSWORD=' + 'r' * 48,
            'AGENTFLOW_SESSION_SECRET=' + 's' * 64, 'LOOPFORGE_DATA_DIR=' + str(self.env.parent / 'data'),
            'AGENTFLOW_SECURE_COOKIES=true', 'LOOPFORGE_IMAGE=example.test/superagent@sha256:' + 'a' * 64,
        )) + '\n')
        clean_env = {k: v for k, v in os.environ.items() if not k.startswith(('AGENTFLOW_', 'LOOPFORGE_', 'COMPOSE_'))}
        for source in (False, True):
            command = ['docker', 'compose', '--env-file', str(self.env), '-f', str(ROOT / 'compose.yaml')]
            if source:
                command += ['-f', str(ROOT / 'compose.dev.yaml')]
            result = subprocess.run(command + ['--profile', 'platform', 'config', '--format', 'json'],
                                    capture_output=True, text=True, env=clean_env)
            self.assertEqual(result.returncode, 0, 'Compose configuration failed')
            config = json.loads(result.stdout)
            self.assertEqual(self.errors(config), [])
            image = config['services']['platform']['image']
            self.assertEqual(image, 'superagent-platform:local' if source else 'example.test/superagent@sha256:' + 'a' * 64)


if __name__ == '__main__':
    unittest.main()
