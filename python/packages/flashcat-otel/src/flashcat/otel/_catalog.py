"""Built-in instrumentation catalog. Load only catalog entries.

Upstream instrumentation, OpenLLMetry, and OpenInference register the same
opentelemetry_instrumentor entry point names (openai, anthropic, and langchain),
so scanning those names cannot distinguish the sources.
"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass(frozen=True)
class PatchTarget:
    module: str
    owner: str
    attr: str


@dataclass(frozen=True)
class Entry:
    lib: str  # Catalog name, also used in disabled_instrumentations.
    source: str  # upstream / flashcat / openinference
    instrumentor: str  # "module:Class"
    target_module: str  # Used to detect whether the target library is installed.
    dist: str  # Target library's distribution name, used to report its version in diagnostics.
    patch_targets: tuple[PatchTarget, ...] = field(default_factory=tuple)
    foreign_prefixes: tuple[str, ...] = field(default_factory=tuple)
    native_since: str | None = None  # Starting with this version, skip patching and report source=native in diagnostics.


CATALOG: tuple[Entry, ...] = (
    Entry(
        "openai", "upstream", "opentelemetry.instrumentation.genai.openai:OpenAIInstrumentor", "openai", "openai",
        (
            PatchTarget("openai.resources.chat.completions", "Completions", "create"),
            PatchTarget("openai.resources.responses", "Responses", "create"),
        ),
        ("opentelemetry.instrumentation.openai", "openinference.instrumentation.openai", "lmnr.", "langfuse.openai"),
    ),
    Entry(
        "anthropic", "upstream", "opentelemetry.instrumentation.genai.anthropic:AnthropicInstrumentor", "anthropic", "anthropic",
        (PatchTarget("anthropic.resources.messages", "Messages", "create"),),
        ("opentelemetry.instrumentation.anthropic", "openinference.instrumentation.anthropic", "lmnr."),
    ),
    Entry(
        "langchain", "upstream", "opentelemetry.instrumentation.genai.langchain:LangChainInstrumentor", "langchain_core", "langchain-core",
        (PatchTarget("langchain_core.callbacks.manager", "BaseCallbackManager", "__init__"),),
        ("opentelemetry.instrumentation.langchain", "openinference.instrumentation.langchain"),
    ),
    Entry(
        "dashscope", "flashcat", "flashcat.instrumentation.dashscope:DashScopeInstrumentor", "dashscope", "dashscope",
        (PatchTarget("dashscope.aigc.generation", "Generation", "call"),),
        ("opentelemetry.instrumentation.dashscope",),
    ),
    Entry(
        "openai-agents", "upstream", "opentelemetry.instrumentation.genai.openai_agents:OpenAIAgentsInstrumentor", "agents", "openai-agents",
        (), ("openinference.instrumentation.openai_agents",),
    ),
    Entry(
        "llama-index", "upstream", "opentelemetry.instrumentation.genai.llama_index:LlamaIndexInstrumentor", "llama_index.core", "llama-index-core",
        (), ("opentelemetry.instrumentation.llamaindex", "openinference.instrumentation.llama_index"),
    ),
    Entry(
        "google-genai", "upstream", "opentelemetry.instrumentation.google_genai:GoogleGenAiSdkInstrumentor", "google.genai", "google-genai",
        (), ("openinference.instrumentation.google_genai",),
    ),
    Entry(
        "bedrock", "upstream", "opentelemetry.instrumentation.genai.bedrock:BedrockInstrumentor", "botocore", "boto3",
        (), ("opentelemetry.instrumentation.bedrock", "openinference.instrumentation.bedrock"),
    ),
    # OpenInference's CrewAI wrapper mode emits only Crew, Task, Agent, Flow, and Tool spans.
    # The model SDK instrumentation records model calls. Event-listener mode would also emit LLM spans and double-count calls.
    Entry(
        "crewai", "openinference", "openinference.instrumentation.crewai:CrewAIInstrumentor", "crewai", "crewai",
        (), ("opentelemetry.instrumentation.crewai",),
    ),
    # MCP 1.x uses Flashcat instrumentation for tools/call client and server spans.
    # MCP 2.x has built-in instrumentation. ContextEnricher adds gen_ai.* attributes to its client spans.
    Entry(
        "mcp", "flashcat", "flashcat.instrumentation.mcp:MCPInstrumentor", "mcp", "mcp",
        (PatchTarget("mcp.client.session", "ClientSession", "call_tool"),),
        ("opentelemetry.instrumentation.mcp", "openinference.instrumentation.mcp"),
        native_since="2.0.0",
    ),
)

# Aliases accepted in user configuration.
ALIASES = {"llama_index": "llama-index", "openai_agents": "openai-agents", "google_genai": "google-genai"}
