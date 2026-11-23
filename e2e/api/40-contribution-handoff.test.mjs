// Real local native macOS + Docker Linux contribution acceptance; not a separate physical failure domain.
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import { cfg } from '../lib/env.mjs';
import { runContributionHandoff } from '../lib/contribution-handoff.mjs';

const enabled = process.env.AGENTFLOW_E2E_HANDOFF === '1'
  && process.env.AGENTFLOW_E2E_LOCAL_MYSQL_CONTAINER === 'superagent-p0-contribution'
  && ['127.0.0.1', 'localhost'].includes(new URL(cfg.base).hostname) && process.platform === 'darwin';
const phase = process.env.AGENTFLOW_E2E_HANDOFF_PHASE || 'full';
const entrySource = readFileSync(new URL(import.meta.url));
test(`P0-005 native macOS and Docker Linux contribution handoff (${phase})`, {
  skip: !enabled && 'requires disposable superagent-p0-contribution MySQL and explicit AGENTFLOW_E2E_HANDOFF=1', timeout: 420_000,
}, async t => runContributionHandoff(t, { phase, entrySource }));
