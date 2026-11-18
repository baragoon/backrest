import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

for (const [translation, compilation] of [
  [0, 0],
  [3, 0],
  [0, 4],
  [3, 4],
]) {
  test(`translation=${translation}, compilation=${compilation}`, () => {
    const dir = mkdtempSync(join(tmpdir(), 'translation runner '));
    try {
      const mock = join(dir, 'package manager.cjs');
      const trace = join(dir, 'trace.jsonl');
      writeFileSync(
        mock,
        `
        const fs = require('node:fs');
        fs.appendFileSync(process.env.TRACE, JSON.stringify(process.argv.slice(2)) + '\\n');
        process.exit(process.argv[3] === 'tsx' ? ${translation} : ${compilation});
      `,
      );
      const result = spawnSync(
        process.execPath,
        [fileURLToPath(new URL('./translate.mjs', import.meta.url)), '--reprocess'],
        {
          env: { ...process.env, npm_execpath: mock, TRACE: trace },
          encoding: 'utf8',
        },
      );
      assert.equal(result.status, translation || compilation, result.stderr);
      assert.deepEqual(readFileSync(trace, 'utf8').trim().split('\n').map(JSON.parse), [
        ['exec', 'tsx', 'scripts/translate-gemini.ts', '--reprocess'],
        [
          'exec',
          'paraglide-js',
          'compile',
          '--project',
          './project.inlang',
          '--outdir',
          './src/paraglide',
        ],
      ]);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
}
