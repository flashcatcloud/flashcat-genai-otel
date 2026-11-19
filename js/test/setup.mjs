// Build, run npm pack, and install the tarball into isolated apps under test/.work/apps/.
// Tests use the packaged SDK like users do, via ESM import, CJS require, or --import registration.
// Setup only contacts the npm registry and needs no model API keys.
import { execFileSync } from 'node:child_process';
import { mkdirSync, readdirSync, rmSync, writeFileSync, existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const work = join(root, 'test', '.work');
const packDir = join(work, 'pack');

function run(cmd, args, cwd) {
  console.log(`$ (${cwd.replace(root, 'js')}) ${cmd} ${args.join(' ')}`);
  execFileSync(cmd, args, { cwd, stdio: 'inherit' });
}

// Pin the versions used to verify each combination.
export const APPS = {
  // OpenAI 6 uses upstream instrumentation, Anthropic 1.8 has built-in instrumentation, and ai 7 uses native @ai-sdk/otel registration.
  // OpenAI Agents 0.21 uses upstream instrumentation. sdk-node covers scenario B and spanProcessor().
  main: {
    'openai': '6.49.0',
    '@anthropic-ai/sdk': '1.8.0',
    '@openai/agents': '0.21.0',
    'zod': '4.6.7',
    'ai': '7.0.170',
    '@ai-sdk/otel': '1.0.170',
    '@opentelemetry/sdk-node': '0.301.1',
  },
  // OpenAI 7 and LangChain use OpenInference.
  oi: {
    'openai': '7.55.0',
    '@arizeai/openinference-instrumentation-openai': '4.4.5',
    '@langchain/core': '1.4.0',
    '@arizeai/openinference-instrumentation-langchain': '4.2.0',
  },
  // OpenAI 7 without OpenInference reports instrumentation_not_installed.
  o7bare: { 'openai': '7.55.0' },
  // Anthropic versions below 0.134 use upstream instrumentation-anthropic.
  anth: { '@anthropic-ai/sdk': '0.133.0' },
  // Bedrock uses upstream instrumentation-aws-sdk, an optional peer.
  bedrock: { '@aws-sdk/client-bedrock-runtime': '3.1167.0', '@opentelemetry/instrumentation-aws-sdk': '0.80.0' },
  // Google GenAI uses built-in Flashcat instrumentation.
  genai: { '@google/genai': '3.6.0' },
  // The MCP client uses built-in Flashcat instrumentation.
  mcp: { '@modelcontextprotocol/sdk': '1.32.2', zod: '3.25.76' },
  // Use this package alone for configuration, semantic API, and masking tests.
  bare: {},
};

function main() {
  run('npm', ['run', 'build'], root);
  rmSync(packDir, { recursive: true, force: true });
  mkdirSync(packDir, { recursive: true });
  run('npm', ['pack', '--pack-destination', packDir], root);
  const tgz = readdirSync(packDir).find((f) => f.endsWith('.tgz'));
  if (!tgz) throw new Error('npm pack 没有产出 tgz');
  const only = process.argv.slice(2);
  for (const [name, deps] of Object.entries(APPS)) {
    if (only.length && !only.includes(name)) continue;
    const dir = join(work, 'apps', name);
    mkdirSync(dir, { recursive: true });
    const pkg = {
      name: `flashcat-otel-test-${name}`,
      private: true,
      dependencies: { '@flashcat/otel': `file:${join(packDir, tgz)}`, ...deps },
    };
    writeFileSync(join(dir, 'package.json'), JSON.stringify(pkg, null, 2));
    if (existsSync(join(dir, 'node_modules', '@flashcat'))) rmSync(join(dir, 'node_modules', '@flashcat'), { recursive: true, force: true });
    rmSync(join(dir, 'package-lock.json'), { force: true });
    run('npm', ['install', '--no-audit', '--no-fund', '--loglevel=error'], dir);
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) main();
