/**
 * The SDK uses an explicit instrumentation catalog. Before loading an entry, check that its target
 * library is installed and that its version matches a supported instrumentation source.
 *
 * Observations from the installed npm distributions, verified Nov 12, 2026:
 *
 * - `@opentelemetry/instrumentation-openai` 0.23.0 declares support for `openai` `>=4.19.0 <7`.
 *     It patches `OpenAI.Chat.Completions.prototype.create`, `OpenAI.Embeddings.prototype.create`,
 *     and `OpenAI.Responses.prototype.create` when present. It accepts `captureMessageContent: boolean`.
 *     `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` accepts only `true` / `false`, warns on other values,
 *     and overrides the constructor option. This package does not use genai-util or accept `span_only` (see contract §7).
 *     It omits `gen_ai.usage.cache_read.input_tokens`, `gen_ai.response.time_to_first_chunk`, and `gen_ai.request.stream`
 *     (verified against 0.23.0 and main on Nov 17, 2026). openai-extras.ts adds them to the upstream spans.
 * - `@opentelemetry/instrumentation-anthropic` 0.2.0 supports `@anthropic-ai/sdk` `>=0.65.0 <0.134.0`.
 *     It patches `Messages.prototype.create` and `stream` in `resources/messages/messages.js` and `.mjs`.
 *     Its README excludes response attributes, tools, metrics, logs, and content capture.
 *     It records only provider, operation, and request model and omits usage.
 * - `@anthropic-ai/sdk` 1.8.0 (built-in since 0.134) reads
 *     `ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE` when constructing the client (`content`, otherwise `metadata_only`).
 *     `src/internal/tracing.ts` gets the tracer from `trace.getTracerProvider()`.
 * - `@opentelemetry/instrumentation-openai-agents` 0.2.0 supports `@openai/agents` `>=0.14.0 <1`.
 *     It patches `Runner.prototype.run`, `withTrace`, and `getOrCreateTrace`. The constructor accepts
 *     `captureMessageContent: boolean` (the environment also takes true / false) and `disableOpenAITraceExport`.
 *     The Agents SDK still exports traces to OpenAI by default. Set `OPENAI_AGENTS_DISABLE_TRACING=1` to disable it.
 * - `@arizeai/openinference-instrumentation-openai` 4.4.5 declares support for `openai` `^7.0.0 || ^6.0.0 || ^5.0.0`.
 *     It depends on `@opentelemetry/instrumentation` 0.46.0 with import-in-the-middle 1.7.1.
 *     That hook differs from the 0.301 hook registered here (import-in-the-middle 3.x), so automatic patching
 *     works only in CommonJS. See register.ts for the ESM fallback. Capture uses `OPENINFERENCE_HIDE_INPUTS / _OUTPUTS`.
 *     A successful patch sets `openInferencePatched` on the module.
 * - `@arizeai/openinference-instrumentation-langchain` 4.2.0 declares support for
 *     `@langchain/core/dist/callbacks/manager.cjs` `^1.0.0 || ^0.3.0 || ^0.2.0` and patches
 *     `CallbackManager._configureSync`. It matches only .cjs files, so ESM requires manuallyInstrument.
 *     It also depends on `@opentelemetry/instrumentation` 0.46.0.
 * - `@opentelemetry/instrumentation-langchain` returns 404 on npm because the package is private.
 * - `ai` 7.0.170 and `@ai-sdk/otel` 1.0.170 are ESM-only ("type":"module"). Load them synchronously with require(esm).
 *     `registerTelemetry(...integrations)` stores integrations in `globalThis.AI_SDK_TELEMETRY_INTEGRATIONS`.
 *     `new OpenTelemetry(options)` has no input or output recording options.
 *     Per-call `telemetry.recordInputs` and `telemetry.recordOutputs` control recording and default to true.
 *     With content capture off, register a wrapper that sets both options to false on each forwarded event.
 * - `@opentelemetry/instrumentation-aws-sdk` 0.80.0 is an optional peer and exports `AwsInstrumentation`. It patches
 *     `Client.prototype.send` in `@smithy/core` and `@smithy/middleware-stack` / `@aws-sdk/middleware-stack`.
 *     It instruments all AWS SDK v3 clients, including S3 and DynamoDB. For Bedrock Runtime,
 *     Converse and InvokeModel set `gen_ai.operation.name=chat` and usage while omitting content.
 *     Detect the target through `@aws-sdk/client-bedrock-runtime`.
 */

import { augmentOpenInferenceOpenAI, augmentUpstreamOpenAI } from './openai-extras';
import { augmentUpstreamAnthropic } from './anthropic-extras';
import { GoogleGenAIInstrumentation } from './google-genai';
import { McpClientInstrumentation } from './mcp';

export type Source = 'upstream' | 'native' | 'openinference' | 'flashcat';

export interface Choice {
  source: Source;
  kind: 'instrumentation' | 'native' | 'vercel' | 'skip';
  /** Instrumentation package name. */
  pkg?: string;
  /** Class name in the instrumentation package. */
  className?: string;
  /** Whether the instrumentation package is a direct dependency (true) or optional peer (false). */
  bundled?: boolean;
  /** Report this as supported in diagnostics if no instrumentation source supports the installed version. */
  supported?: string;
  /** Constructor options. */
  config?: (contentEnabled: boolean) => Record<string, unknown>;
  /** Optional post-construction setup, such as adding cache tokens and TTFT to upstream OpenAI spans. Failures are ignored. */
  augment?: (inst: any) => unknown;
  /** Built-in instrumentation (source=flashcat). Instantiate directly and resolve the target library from bases. */
  create?: (contentEnabled: boolean, bases: string[]) => any;
}

export interface Probe {
  /** Module to check for an existing wrapper, resolved from the application directory. */
  specifier: string;
  /** Path from the module exports to the target function. */
  path: string[];
}

export interface Entry {
  /** Catalog name used in instrumentations and disabledInstrumentations. */
  lib: string;
  /** npm package name used to detect the target and read its version. */
  target: string;
  choose(version: string): Choice;
  probes: Probe[];
}

type V = [number, number, number];

export function parseVersion(v: string): V {
  const m = /^(\d+)\.(\d+)\.(\d+)/.exec(v.trim());
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : [0, 0, 0];
}

export function gte(v: string, min: string): boolean {
  const a = parseVersion(v);
  const b = parseVersion(min);
  for (let i = 0; i < 3; i++) {
    if (a[i] !== b[i]) return a[i] > b[i];
  }
  return true;
}

const upstreamContent = (on: boolean) => ({ captureMessageContent: on });

export const CATALOG: Entry[] = [
  {
    lib: 'openai',
    target: 'openai',
    choose(v) {
      if (!gte(v, '4.19.0')) return { source: 'upstream', kind: 'skip', supported: '>=4.19.0' };
      if (!gte(v, '7.0.0')) {
        return {
          source: 'upstream',
          kind: 'instrumentation',
          pkg: '@opentelemetry/instrumentation-openai',
          className: 'OpenAIInstrumentation',
          bundled: true,
          config: upstreamContent,
          augment: augmentUpstreamOpenAI,
        };
      }
      if (!gte(v, '8.0.0')) {
        return {
          source: 'openinference',
          kind: 'instrumentation',
          pkg: '@arizeai/openinference-instrumentation-openai',
          className: 'OpenAIInstrumentation',
          bundled: false,
          config: () => ({}),
          augment: augmentOpenInferenceOpenAI,
        };
      }
      return { source: 'openinference', kind: 'skip', supported: '>=4.19.0 <8' };
    },
    probes: [{ specifier: 'openai', path: ['OpenAI', 'Chat', 'Completions', 'prototype', 'create'] }],
  },
  {
    lib: 'anthropic',
    target: '@anthropic-ai/sdk',
    choose(v) {
      if (gte(v, '0.134.0')) return { source: 'native', kind: 'native' };
      if (gte(v, '0.65.0')) {
        return {
          source: 'upstream',
          kind: 'instrumentation',
          pkg: '@opentelemetry/instrumentation-anthropic',
          className: 'AnthropicInstrumentation',
          bundled: true,
          config: () => ({}),
          // Upstream omits usage. anthropic-extras.ts adds token usage and response attributes.
          augment: augmentUpstreamAnthropic,
        };
      }
      return { source: 'upstream', kind: 'skip', supported: '>=0.65.0' };
    },
    probes: [{ specifier: '@anthropic-ai/sdk', path: ['Anthropic', 'Messages', 'prototype', 'create'] }],
  },
  {
    lib: 'openai-agents',
    target: '@openai/agents',
    choose(v) {
      if (gte(v, '0.14.0') && !gte(v, '1.0.0')) {
        return {
          source: 'upstream',
          kind: 'instrumentation',
          pkg: '@opentelemetry/instrumentation-openai-agents',
          className: 'OpenAIAgentsInstrumentation',
          bundled: true,
          config: upstreamContent,
        };
      }
      return { source: 'upstream', kind: 'skip', supported: '>=0.14.0 <1' };
    },
    probes: [{ specifier: '@openai/agents', path: ['Runner', 'prototype', 'run'] }],
  },
  {
    lib: 'langchain',
    target: '@langchain/core',
    choose(v) {
      const major = parseVersion(v);
      const ok = major[0] === 1 || (major[0] === 0 && (major[1] === 2 || major[1] === 3));
      if (!ok) return { source: 'openinference', kind: 'skip', supported: '^0.2.0 || ^0.3.0 || ^1.0.0' };
      return {
        source: 'openinference',
        kind: 'instrumentation',
        pkg: '@arizeai/openinference-instrumentation-langchain',
        className: 'LangChainInstrumentation',
        bundled: false,
        config: () => ({}),
      };
    },
    probes: [{ specifier: '@langchain/core/callbacks/manager', path: ['CallbackManager', '_configureSync'] }],
  },
  {
    lib: 'ai',
    target: 'ai',
    choose(v) {
      if (gte(v, '7.0.0')) return { source: 'native', kind: 'vercel', pkg: '@ai-sdk/otel', bundled: false };
      // ai 6 and earlier use per-call experimental_telemetry and require no SDK registration.
      return { source: 'native', kind: 'skip', supported: '>=7.0.0' };
    },
    probes: [],
  },
  {
    lib: 'bedrock',
    target: '@aws-sdk/client-bedrock-runtime',
    choose() {
      return {
        source: 'upstream',
        kind: 'instrumentation',
        pkg: '@opentelemetry/instrumentation-aws-sdk',
        className: 'AwsInstrumentation',
        bundled: false,
        config: () => ({}),
      };
    },
    probes: [],
  },
  // Google GenAI (@google/genai) has no upstream JS, OpenInference, or native SDK instrumentation, so use Flashcat's built-in instrumentation.
  {
    lib: 'google-genai',
    target: '@google/genai',
    choose(v) {
      if (gte(v, '1.0.0')) return { source: 'flashcat', kind: 'instrumentation', create: () => new GoogleGenAIInstrumentation() };
      return { source: 'flashcat', kind: 'skip', supported: '>=1.0.0' };
    },
    probes: [{ specifier: '@google/genai', path: ['Models', 'prototype', 'generateContentInternal'] }],
  },
  // MCP SDK 1.x has no native instrumentation. Use Flashcat's built-in wrapper for Client.callTool (contract §4.3).
  {
    lib: 'mcp',
    target: '@modelcontextprotocol/sdk',
    choose(v) {
      if (gte(v, '1.0.0') && !gte(v, '2.0.0')) {
        return {
          source: 'flashcat',
          kind: 'instrumentation',
          create: (_content, bases) => {
            const inst = new McpClientInstrumentation();
            inst.instrumentBuilds(bases);
            return inst;
          },
        };
      }
      return { source: 'flashcat', kind: 'skip', supported: '>=1.0.0 <2' };
    },
    probes: [{ specifier: '@modelcontextprotocol/sdk/client/index.js', path: ['Client', 'prototype', 'callTool'] }],
  },
];

/** Accepted library aliases. */
export const ALIASES: Record<string, string> = {
  openai_agents: 'openai-agents',
  '@openai/agents': 'openai-agents',
  '@anthropic-ai/sdk': 'anthropic',
  '@langchain/core': 'langchain',
  vercel: 'ai',
  'vercel-ai': 'ai',
  'aws-sdk': 'bedrock',
};
