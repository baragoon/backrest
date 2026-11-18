import { spawnSync } from 'node:child_process';

// npm_execpath points to the package manager's JS entry point, including on Windows.
function run(args) {
  // npm requires -- to separate options from the command
  const isNpm = process.env.npm_execpath?.includes('npm');
  const separator = isNpm ? ['--'] : [];
  const result = spawnSync(process.execPath, [process.env.npm_execpath, 'exec', ...separator, ...args], {
    stdio: 'inherit',
  });
  if (result.error) console.error(result.error);
  return result.status ?? 1;
}

const translationStatus = run(['tsx', 'scripts/translate-gemini.ts', ...process.argv.slice(2)]);

// Compile translations regardless of translation status so messages saved during
// partial runs are reflected in src/paraglide/. Report failure if either step failed.
const compilationStatus = run([
  'paraglide-js',
  'compile',
  '--project',
  './project.inlang',
  '--outdir',
  './src/paraglide',
]);
process.exitCode = translationStatus || compilationStatus;
