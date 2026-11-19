# Differences from upstream

The baseline is LoongSuite `alibaba/loongsuite-python-agent@c27eb2a6` (2026-10-22),
`instrumentation-loongsuite/loongsuite-instrumentation-dashscope`, licensed under Apache-2.0.

| File | Baseline | Changes |
|---|---|---|
| `__init__.py` | `__init__.py` | Kept only `Generation.call`, `AioGeneration.call`, and `TextEmbedding.call`. Replaced `ExtendedTelemetryHandler` (from LoongSuite's util fork) with `TelemetryHandler` from the community `opentelemetry-util-genai` package. Renamed the scope to `flashcat.instrumentation.dashscope` |
| `_patch.py` | `patch/generation.py`, `patch/embedding.py` | Replaced `start_llm / stop_llm / fail_llm` with the invocations returned by `handler.inference()` / `handler.embedding()`. Kept streaming `incremental_output` handling. At the default False setting, each chunk is cumulative, so only the last chunk's content and usage are used (baseline `patch/generation.py:70`). Added ERROR span status for DashScope errors returned in the response (`status_code != 200`) without raising an exception |
| `_utils.py` | `utils/generation.py`, `utils/common.py` | Kept only message, parameter, and usage conversion. Added usage mappings from `prompt_tokens_details.cached_tokens` to `cache_read` and from `output_tokens_details.reasoning_tokens` to reasoning |
| `package.py` | `package.py` | Added a version range to `_instruments` (`dashscope >= 1.20, < 3`). The baseline has no version range. Version 2.0.2 has been verified |

The following upstream APIs are not ported: MultiModalConversation, TextReRank (the community util has no rerank operation), and ImageSynthesis / VideoSynthesis / SpeechSynthesizer.

The provider name uses the custom value `dashscope`, which is outside the known semconv values. The Flashcat platform recognizes it.
