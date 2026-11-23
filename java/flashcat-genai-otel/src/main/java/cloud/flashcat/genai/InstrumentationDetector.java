package cloud.flashcat.genai;

import java.net.URL;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.logging.Logger;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Populates diagnostics {@code instrumentations} (contract §8). The Java agent, Flashcat extension, model SDKs, and frameworks supply the instrumentation.
 * This class detects known libraries on the classpath, reports the instrumentation source per API, and identifies model calls that will produce no spans.
 *
 * <p>Contract §8 requires a {@code problems} entry when a detected library lacks its instrumentation or a model SDK or API falls outside instrumentation coverage.
 * Log one WARN line per cause.
 *
 * <p>Looks up .class resources by name to avoid triggering class loading during agent startup.
 * Nested JARs in a Spring Boot executable JAR are visible only to the application's class loader. Diagnostics prefer the calling thread's context class loader.
 * In agent mode, the extension checks again when the first span starts on an application thread, then reports libraries missed at startup and logs WARNs.
 */
final class InstrumentationDetector {
    private static final Logger logger = Logger.getLogger("cloud.flashcat.genai");
    private static final Pattern JAR_VERSION = Pattern.compile("-(\\d+(?:\\.\\d+)+[^/!]*?)\\.jar");
    /** System property set when the agent loads the extension's openai-java instrumentation (Responses and Chat Completions fallback). See FlashcatOpenAiInstrumentationModule. */
    static final String PROP_OPENAI_INSTALLED = "cloud.flashcat.genai.agent.instrumentation.openai-java";
    /** First runtime error from the extension's openai-java instrumentation, such as an incompatible openai-java version. */
    static final String PROP_OPENAI_ERROR = "cloud.flashcat.genai.agent.instrumentation.openai-java.error";
    /** Name of the extension's openai-java instrumentation. Set otel.instrumentation.&lt;name&gt;.enabled=false to disable it. */
    static final String OPENAI_INSTRUMENTATION_NAME = "flashcat-openai-java";
    private static final Set<String> WARNED = Collections.synchronizedSet(new HashSet<String>());

    private InstrumentationDetector() {}

    /** Detected instrumentation entries and their associated problems. */
    static final class Result {
        final List<Map<String, Object>> instrumentations = new ArrayList<Map<String, Object>>();
        final List<String> problems = new ArrayList<String>();
    }

    static Result inspect(boolean agentMode, ClassLoader cl) {
        Result r = new Result();
        ClassLoader loader = cl != null ? cl : ClassLoader.getSystemClassLoader();

        // The Java agent instruments Chat Completions and Embeddings through openai-java-1.1, which wraps only clients created by OpenAIOkHttpClient.builder().
        // The Flashcat extension instruments the Responses API on implementation classes and adds Chat Completions spans for clients upstream instrumentation did not wrap.
        Map<String, Object> openai = entry("openai-java", "upstream");
        openai.put("api", "chat_completions, embeddings");
        Map<String, Object> responses = entry("openai-java", "flashcat");
        responses.put("api", "responses");
        URL u = find(loader, "com/openai/client/OpenAIClient.class");
        if (u == null) {
            openai.put("status", "not_present");
            responses.put("status", "not_present");
        } else {
            String ver = version(u);
            openai.put("lib_version", ver);
            responses.put("lib_version", ver);
            if (!agentMode) {
                openai.put("status", "instrumentation_not_installed");
                openai.put("hint", "openai-java 的插桩在 OpenTelemetry Java Agent 里：用 -javaagent 启动并加载 Flashcat 扩展");
                responses.put("status", "instrumentation_not_installed");
                r.problems.add("检测到 openai-java " + versionText(ver) + "，但没有 OpenTelemetry Java Agent，模型调用不会产生 span："
                        + "openai-java 的插桩在 Agent 与 Flashcat 扩展里，请用 -javaagent 启动并加 -Dotel.javaagent.extensions=flashcat-genai-otel-agent-extension.jar");
            } else {
                openai.put("status", "installed");
                String error = property(PROP_OPENAI_ERROR);
                if (error != null) {
                    // Contract §8 defines not_patched for a library in use whose instrumentation failed to take effect.
                    responses.put("status", "not_patched");
                    responses.put("error", error);
                    r.problems.add("Flashcat 的 openai-java 插桩在运行时出错，模型调用可能没有 span（openai-java " + versionText(ver) + " 可能不在兼容范围内）：" + error);
                } else if ("installed".equals(property(PROP_OPENAI_INSTALLED))) {
                    responses.put("status", "installed");
                    responses.put("hint", "Flashcat 扩展挂在 openai-java 实现类上：Responses API 全部覆盖；Chat Completions 在客户端没经过 Agent 上游插桩包装时"
                            + "（直接 new OpenAIClientImpl、withOptions、withRawResponse，例如 Spring AI）由扩展补 span，经过上游包装时只补流式首 Token 时间");
                } else if (AgentDetector.isExtensionActive()) {
                    responses.put("status", "disabled");
                    r.problems.add("检测到 openai-java " + versionText(ver) + "，但 Flashcat 的 Responses API 插桩没有装载"
                            + "（可能设置了 otel.instrumentation.flashcat-openai-java.enabled=false）："
                            + "OTel Java Agent 自带的 openai-java 插桩只覆盖 Chat Completions 与 Embeddings，client.responses() 的调用不会产生模型 span");
                } else {
                    responses.put("status", "instrumentation_not_installed");
                    r.problems.add("检测到 openai-java " + versionText(ver) + "，但没有加载 Flashcat 扩展：OTel Java Agent 自带的 openai-java 插桩"
                            + "只覆盖 Chat Completions 与 Embeddings，client.responses() 的调用不会产生模型 span。"
                            + "请加 -Dotel.javaagent.extensions=flashcat-genai-otel-agent-extension.jar");
                }
            }
        }
        r.instrumentations.add(openai);
        r.instrumentations.add(responses);

        // The Java agent's aws-sdk-2.2 instrumentation covers Bedrock.
        Map<String, Object> bedrock = entry("bedrock", "upstream");
        u = find(loader, "software/amazon/awssdk/services/bedrockruntime/BedrockRuntimeClient.class");
        if (u == null) {
            bedrock.put("status", "not_present");
        } else {
            bedrock.put("lib_version", version(u));
            if (agentMode) {
                bedrock.put("status", "installed");
            } else {
                bedrock.put("status", "instrumentation_not_installed");
                bedrock.put("hint", "Bedrock 的插桩在 OpenTelemetry Java Agent 里：用 -javaagent 启动并加载 Flashcat 扩展");
                r.problems.add("检测到 Bedrock Runtime SDK，但没有 OpenTelemetry Java Agent，模型调用不会产生 span：Bedrock 的插桩在 Agent 里，请用 -javaagent 启动");
            }
        }
        r.instrumentations.add(bedrock);

        // The Anthropic Java SDK has built-in instrumentation that uses GlobalOpenTelemetry.
        Map<String, Object> anthropic = entry("anthropic-java", "native");
        u = find(loader, "com/anthropic/client/AnthropicClient.class");
        if (u == null) {
            anthropic.put("status", "not_present");
        } else {
            anthropic.put("lib_version", version(u));
            anthropic.put("status", "installed");
        }
        r.instrumentations.add(anthropic);

        // The Spring AI 2.x OpenAI module constructs OpenAIClientImpl directly, bypassing the agent's instrumentation of OpenAIOkHttpClient.builder().
        // It uses withRawResponse().create for synchronous calls and the async client for streaming. Only the Flashcat extension's instrumentation on the implementation classes can produce model spans.
        // Spring AI's Micrometer observations (ChatClient, advisors, and tools) require micrometer-tracing-bridge-otel to produce OTel spans.
        Map<String, Object> springAi = entry("spring-ai", "native");
        u = find(loader, "org/springframework/ai/chat/model/ChatModel.class");
        if (u == null) {
            springAi.put("status", "not_present");
        } else {
            String ver = version(u);
            springAi.put("lib_version", ver);
            boolean bridge = find(loader, "io/micrometer/tracing/otel/bridge/OtelTracer.class") != null;
            if (agentMode && "installed".equals(property(PROP_OPENAI_INSTALLED)) && property(PROP_OPENAI_ERROR) == null) {
                springAi.put("source", "flashcat");
                springAi.put("status", "installed");
                springAi.put("hint", "Spring AI 的 OpenAI 模型调用由 Flashcat 扩展的 openai-java 插桩出 span；Spring AI 自带的 Observation（ChatClient、advisor、工具）"
                        + (bridge ? "经 micrometer-tracing-bridge-otel 转成 OTel span（flashcat-genai-otel 未验证）" : "没有桥到 OTel，这几类框架层 span 不会有"));
            } else if (bridge) {
                springAi.put("status", "installed");
                springAi.put("hint", "检测到 micrometer-tracing-bridge-otel：Spring AI 的 Observation 经它变成 OTel span（这条链路 flashcat-genai-otel 未验证）");
            } else {
                springAi.put("status", "instrumentation_not_installed");
                springAi.put("hint", "Spring AI 的 OpenAI 模型调用要用 OpenTelemetry Java Agent + Flashcat 扩展启动才有 span");
                r.problems.add("检测到 Spring AI " + versionText(ver) + "，但它的模型调用不会产生 span：Spring AI 2.x 的 OpenAI 模块直接 new OpenAIClientImpl，"
                        + "不经过 OTel Java Agent 自带插桩包装的 OpenAIOkHttpClient.builder()，只有 Flashcat 扩展挂在 openai-java 实现类上的插桩能覆盖。"
                        + "请用 -javaagent 启动并加 -Dotel.javaagent.extensions=flashcat-genai-otel-agent-extension.jar");
            }
        }
        r.instrumentations.add(springAi);

        // LangChain4j uses OpenInference instrumentation. Without a global registration hook, users must attach listeners to the
        // model builder or AiServices. The SDK can only detect the instrumentation and give setup instructions in the hint.
        Map<String, Object> lc4j = entry("langchain4j", "openinference");
        u = find(loader, "dev/langchain4j/model/chat/ChatModel.class");
        if (u == null) {
            u = find(loader, "dev/langchain4j/model/chat/ChatLanguageModel.class");
        }
        if (u == null) {
            lc4j.put("status", "not_present");
        } else {
            lc4j.put("lib_version", version(u));
            if (find(loader, "com/arize/instrumentation/langchain4j/LangChain4jInstrumentor.class") != null) {
                lc4j.put("status", "installed");
                lc4j.put("hint", "已检测到 OpenInference 的 langchain4j 插桩：把 LangChain4jInstrumentor.instrument(traceConfig).createModelListener() "
                        + "挂到 ChatModel builder 的 listeners 上，createAiServiceListeners() 注册到 AiServices；traceConfig 的 hideInputs / hideOutputs "
                        + "按 FlashcatGenAI.captureContentEnabled() 取反设置，否则正文开关不生效");
            } else {
                lc4j.put("status", "instrumentation_not_installed");
                lc4j.put("hint", "添加依赖 com.arize:openinference-instrumentation-langchain4j，并把它的 listener 挂到 ChatModel builder 上（见 README 的 LangChain4j 一节）");
                r.problems.add("检测到 LangChain4j " + versionText(version(u)) + "，但它的模型调用不会产生 span：请添加依赖 com.arize:openinference-instrumentation-langchain4j，"
                        + "并把 LangChain4jInstrumentor 创建的 listener 挂到 ChatModel builder 上（LangChain4j 没有全局注册入口）");
            }
        }
        r.instrumentations.add(lc4j);
        return r;
    }

    /** Logs one WARN per problem (contract §8). */
    static void warnOnce(List<String> problems) {
        for (String p : problems) {
            if (WARNED.add(p)) {
                logger.warning("flashcat-genai-otel: " + p);
            }
        }
    }

    /** Resets warning deduplication for tests. */
    static void resetForTest() {
        WARNED.clear();
    }

    private static String versionText(String ver) {
        return ver == null ? "" : ver;
    }

    private static String property(String name) {
        try {
            return System.getProperty(name);
        } catch (SecurityException e) {
            return null;
        }
    }

    private static Map<String, Object> entry(String lib, String source) {
        Map<String, Object> m = new LinkedHashMap<String, Object>();
        m.put("lib", lib);
        m.put("source", source);
        return m;
    }

    private static URL find(ClassLoader cl, String resource) {
        try {
            URL u = cl.getResource(resource);
            if (u == null && cl != ClassLoader.getSystemClassLoader()) {
                u = ClassLoader.getSystemClassLoader().getResource(resource);
            }
            return u;
        } catch (Throwable e) {
            return null;
        }
    }

    /** Extracts the version from a JAR filename (e.g. 2.20.1 from openai-java-core-2.20.1.jar). Returns null if unavailable. */
    static String version(URL u) {
        String s = u.toString();
        int bang = s.lastIndexOf(".jar!");
        if (bang < 0) {
            return null;
        }
        String jarPart = s.substring(0, bang + 4);
        int slash = jarPart.lastIndexOf('/');
        String file = slash >= 0 ? jarPart.substring(slash) : jarPart;
        Matcher m = JAR_VERSION.matcher(file);
        String found = null;
        while (m.find()) {
            found = m.group(1);
        }
        return found;
    }
}
