# flashcat-instrumentation-dashscope

OpenTelemetry GenAI instrumentation for the native DashScope SDK from Alibaba Cloud Bailian, built on the community `opentelemetry-util-genai` package.
Typically installed with `pip install 'flashcat-otel[dashscope]'` and loaded automatically by `flashcat.otel.init()`.

```python
from flashcat.instrumentation.dashscope import DashScopeInstrumentor
DashScopeInstrumentor().instrument()
```

Supports `Generation.call` / `AioGeneration.call` (including streaming, with correct handling of `incremental_output`) and `TextEmbedding.call`.
