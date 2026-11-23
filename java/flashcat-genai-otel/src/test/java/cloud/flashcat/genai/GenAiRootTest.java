package cloud.flashcat.genai;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.api.trace.propagation.W3CTraceContextPropagator;
import io.opentelemetry.context.Context;
import io.opentelemetry.sdk.trace.ReadableSpan;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

class GenAiRootTest {
    private final SessionSpanProcessor session = new SessionSpanProcessor();
    private final SdkTracerProvider provider = SdkTracerProvider.builder().addSpanProcessor(session).build();
    private final Tracer tracer = provider.get("test");

    @AfterEach
    void tearDown() {
        provider.close();
    }

    @Test
    void propagatesAncestryThroughOrdinarySpans() {
        Span agent = tracer.spanBuilder("agent").setAttribute("gen_ai.operation.name", "invoke_agent").startSpan();
        Span bridge = tracer.spanBuilder("ordinary").setParent(Context.root().with(agent)).startSpan();
        Span llm = startChat(Context.root().with(bridge));

        assertEquals(true, root(agent));
        assertNull(root(bridge));
        assertEquals(false, root(llm));
        llm.end();
        bridge.end();
        agent.end();
    }

    @Test
    void treatsParentFromTraceparentAsRemote() {
        Span agent = tracer.spanBuilder("agent").setAttribute("gen_ai.operation.name", "invoke_agent").startSpan();
        Span bridge = tracer.spanBuilder("ordinary").setParent(Context.root().with(agent)).startSpan();
        assertTrue(session.ancestry.has(bridge.getSpanContext()));
        String traceparent = "00-" + bridge.getSpanContext().getTraceId() + "-" + bridge.getSpanContext().getSpanId() + "-01";
        Context remote = W3CTraceContextPropagator.getInstance().extract(Context.root(), traceparent,
                (header, key) -> "traceparent".equals(key) ? header : null);
        Span llm = startChat(remote);

        assertTrue(((ReadableSpan) llm).getParentSpanContext().isRemote());
        assertEquals(bridge.getSpanContext().getSpanId(), ((ReadableSpan) llm).getParentSpanContext().getSpanId());
        assertEquals(true, root(llm));
        llm.end();
        bridge.end();
        agent.end();
    }

    @Test
    void checksParentsCurrentAttributesAtChildStart() {
        Span parent = tracer.spanBuilder("late-agent").startSpan();
        Context parentContext = Context.root().with(parent);
        Span before = startChat(parentContext);
        parent.setAttribute("gen_ai.operation.name", "invoke_agent");
        Span after = startChat(parentContext);

        assertEquals(true, root(before));
        assertEquals(false, root(after));
        before.end();
        after.end();
        parent.end();
    }

    @Test
    void recognizesLangChain4jScopeAtStart() {
        Tracer openInference = provider.get("com.arize.langchain4j");
        Span chain = openInference.spanBuilder("chain").startSpan();
        Span llm = openInference.spanBuilder("llm").setParent(Context.root().with(chain)).startSpan();

        assertEquals(true, root(chain));
        assertEquals(false, root(llm));
        llm.end();
        chain.end();
    }

    @Test
    void matchesExporterAttributeRules() {
        for (String key : new String[] {"flashcat.span.kind", "ai.operationId", "gen_ai.usage.input_tokens", "llm.token_count.total",
                "openinference.span.kind", "traceloop.entity.name", "langfuse.observation.level"}) {
            assertTrue(GenAiAncestry.isGenAi(Attributes.of(AttributeKey.longKey(key), 1L)), key);
        }
        assertFalse(GenAiAncestry.isGenAi(Attributes.of(SessionKeys.ATTR_CONVERSATION, "session")));
        assertFalse(GenAiAncestry.isGenAi(Attributes.of(GenAiAncestry.ROOT, true)));
        AttributeKey<String> aiModel = AttributeKey.stringKey("ai.model.id");
        AttributeKey<String> operation = AttributeKey.stringKey("operation.name");
        assertFalse(GenAiAncestry.isGenAi(Attributes.of(aiModel, "gpt-4o")));
        assertFalse(GenAiAncestry.isGenAi(Attributes.of(operation, "ai.generateText")));
        assertTrue(GenAiAncestry.isGenAi(Attributes.of(aiModel, "gpt-4o", operation, "ai.generateText")));
    }

    @Test
    void evictsOldestDescendantAfter65536Starts() {
        Span agent = tracer.spanBuilder("agent").setAttribute("gen_ai.operation.name", "invoke_agent").startSpan();
        Context parent = Context.root().with(agent);
        Span oldest = tracer.spanBuilder("ordinary").setParent(parent).startSpan();
        oldest.end();
        for (int i = 1; i < 65536; i++) {
            tracer.spanBuilder("ordinary").setParent(parent).startSpan().end();
        }
        assertTrue(session.ancestry.has(oldest.getSpanContext()));
        Span newest = tracer.spanBuilder("ordinary").setParent(parent).startSpan();
        newest.end();
        assertFalse(session.ancestry.has(oldest.getSpanContext()));
        assertTrue(session.ancestry.has(newest.getSpanContext()));
        agent.end();
    }

    private Span startChat(Context parent) {
        return tracer.spanBuilder("chat").setParent(parent).setAttribute("gen_ai.operation.name", "chat").startSpan();
    }

    private static Boolean root(Span span) {
        return ((ReadableSpan) span).getAttribute(GenAiAncestry.ROOT);
    }
}
