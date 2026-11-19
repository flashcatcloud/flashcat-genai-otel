package cloud.flashcat.otel;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.api.baggage.Baggage;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.api.trace.TracerProvider;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.Scope;
import io.opentelemetry.context.propagation.ContextPropagators;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.io.IOException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.Iterator;
import java.util.List;
import java.util.NoSuchElementException;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutionException;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/** Tests the semantic API and sessions from contract §4 and §5 using the OTel InMemorySpanExporter from sdk-testing. */
class SemanticsTest {
    private InMemorySpanExporter exporter;
    private SessionSpanProcessor session;

    @BeforeEach
    void setUp() {
        TestSupport.reset(null);
        exporter = InMemorySpanExporter.create();
        session = new SessionSpanProcessor();
        SdkTracerProvider tp = SdkTracerProvider.builder()
                .addSpanProcessor(session)
                .addSpanProcessor(SimpleSpanProcessor.create(exporter))
                .build();
        GlobalOpenTelemetry.set(OpenTelemetrySdk.builder().setTracerProvider(tp).build());
    }

    @AfterEach
    void tearDown() {
        TestSupport.reset(null);
    }

    private SpanData span(String name) {
        for (SpanData s : exporter.getFinishedSpanItems()) {
            if (s.getName().equals(name)) {
                return s;
            }
        }
        throw new AssertionError("Missing span " + name + ". Finished spans: " + exporter.getFinishedSpanItems());
    }

    private static String attr(SpanData s, String key) {
        return s.getAttributes().get(AttributeKey.stringKey(key));
    }

    @Test
    void fourSpanKindsNamesAndAttributes() throws Exception {
        Flashcat.agent("kefu", Flashcat.SpanOptions.create().description("客服 Agent"), () -> "ok");
        Flashcat.tool("search_kb", Flashcat.SpanOptions.create().description("知识库"), () -> {});
        Flashcat.workflow("ticket_flow", () -> 1);
        Flashcat.trace("batch_job", Collections.singletonMap("job.id", 7), () -> {});

        SpanData agent = span("invoke_agent kefu");
        assertEquals(SpanKind.INTERNAL, agent.getKind());
        assertEquals("invoke_agent", attr(agent, "gen_ai.operation.name"));
        assertEquals("kefu", attr(agent, "gen_ai.agent.name"));
        assertEquals("客服 Agent", attr(agent, "gen_ai.agent.description"));
        assertEquals("flashcat.otel", agent.getInstrumentationScopeInfo().getName());
        assertEquals("0.1.0", agent.getInstrumentationScopeInfo().getVersion());

        SpanData tool = span("execute_tool search_kb");
        assertEquals("execute_tool", attr(tool, "gen_ai.operation.name"));
        assertEquals("search_kb", attr(tool, "gen_ai.tool.name"));
        assertEquals("function", attr(tool, "gen_ai.tool.type"));
        assertEquals("知识库", attr(tool, "gen_ai.tool.description"));

        SpanData wf = span("invoke_workflow ticket_flow");
        assertEquals("invoke_workflow", attr(wf, "gen_ai.operation.name"));
        assertEquals("ticket_flow", attr(wf, "gen_ai.workflow.name"));

        SpanData job = span("batch_job");
        assertEquals(SpanKind.INTERNAL, job.getKind());
        assertEquals(7L, job.getAttributes().get(AttributeKey.longKey("job.id")));
        assertNull(attr(job, "gen_ai.operation.name"));
        for (SpanData s : exporter.getFinishedSpanItems()) {
            assertEquals(StatusCode.UNSET, s.getStatus().getStatusCode());
        }
    }

    @Test
    void attributesAreSetAtStart() {
        // The orphan count verifies that the session processor can read gen_ai.operation.name in onStart.
        Tracer t = GlobalOpenTelemetry.getOrNoop().getTracer("x");
        Span chat = t.spanBuilder("chat").setAttribute("gen_ai.operation.name", "chat").startSpan();
        chat.end();
        assertEquals(1, session.orphans.get());
        Flashcat.agent("a", () -> {
            Span c = t.spanBuilder("chat").setAttribute("gen_ai.operation.name", "chat").startSpan();
            c.end();
        });
        assertEquals(1, session.orphans.get(), "An inference span with a parent is excluded from the orphan count");
    }

    @Test
    void nestingParentsChildren() {
        Flashcat.agent("kefu", () -> Flashcat.tool("lookup", () -> "x"));
        SpanData agent = span("invoke_agent kefu");
        SpanData tool = span("execute_tool lookup");
        assertEquals(agent.getSpanId(), tool.getParentSpanId());
        assertEquals(agent.getTraceId(), tool.getTraceId());
    }

    static class OrderNotFound extends Exception {
        private static final long serialVersionUID = 1L;

        OrderNotFound(String m) {
            super(m);
        }
    }

    @Test
    void checkedExceptionRethrownUnchangedAndSpanIsError() {
        OrderNotFound boom = new OrderNotFound("no such order");
        OrderNotFound caught = assertThrows(OrderNotFound.class, () -> Flashcat.tool("boom", () -> {
            throw boom;
        }));
        assertSame(boom, caught, "The same exception instance is rethrown");
        SpanData s = span("execute_tool boom");
        assertEquals(StatusCode.ERROR, s.getStatus().getStatusCode());
        assertEquals(OrderNotFound.class.getName(), attr(s, "error.type"));
        assertEquals("exception", s.getEvents().get(0).getName());
    }

    @Test
    void uncheckedAndErrorRethrownUnchanged() throws Exception {
        IllegalStateException ise = new IllegalStateException("x");
        assertSame(ise, assertThrows(IllegalStateException.class, () -> Flashcat.agent("a", () -> {
            throw ise;
        })));
        AssertionError err = new AssertionError("fatal");
        assertSame(err, assertThrows(AssertionError.class, () -> Flashcat.workflow("w", () -> {
            throw err;
        })));
        IOException io = new IOException("io");
        assertSame(io, assertThrows(IOException.class, () -> Flashcat.trace("t", () -> {
            throw io;
        })));
        assertEquals(StatusCode.ERROR, span("invoke_agent a").getStatus().getStatusCode());
        assertEquals(StatusCode.ERROR, span("invoke_workflow w").getStatus().getStatusCode());
        assertEquals(StatusCode.ERROR, span("t").getStatus().getStatusCode());
    }

    @Test
    void contentOffWritesNoContentAttributes() {
        ContentSwitch.setResolved(false);
        Flashcat.agent("a", Flashcat.SpanOptions.create().input("问题"), () -> "答案");
        Flashcat.tool("t", Flashcat.SpanOptions.create().input("q"), () -> "r");
        for (SpanData s : exporter.getFinishedSpanItems()) {
            for (String k : Masker.CONTENT_KEYS) {
                assertNull(attr(s, k), s.getName() + " " + k);
            }
        }
    }

    @Test
    void contentOnWritesMessagesAndToolArgs() {
        ContentSwitch.setResolved(true);
        Flashcat.agent("a", Flashcat.SpanOptions.create().input("问题"), () -> "答案");
        Flashcat.tool("t", Flashcat.SpanOptions.create().input(Collections.singletonMap("q", Arrays.asList(1, 2))), () -> 3);
        SpanData a = span("invoke_agent a");
        assertEquals("[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"问题\"}]}]", attr(a, "gen_ai.input.messages"));
        assertEquals("[{\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"content\":\"答案\"}]}]", attr(a, "gen_ai.output.messages"));
        SpanData t = span("execute_tool t");
        assertEquals("{\"q\": [1, 2]}", attr(t, "gen_ai.tool.call.arguments"));
        assertEquals("3", attr(t, "gen_ai.tool.call.result"));
    }

    @Test
    void serializationIsBoundedAndDoesNotWalkObjects() {
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < 40000; i++) {
            sb.append('x');
        }
        String s = Semantics.serialize(sb.toString());
        assertEquals(32 * 1024 + "…[truncated]".length(), s.length());
        assertTrue(s.endsWith("…[truncated]"));
        Object pojo = new Object() {
            @Override
            public String toString() {
                return "Pojo{id=1}";
            }
        };
        assertEquals("Pojo{id=1}", Semantics.serialize(pojo));
    }

    @Test
    void asyncWithHandle() throws Exception {
        ContentSwitch.setResolved(true);
        FlashcatSpan h = Flashcat.startAgent("async", Flashcat.SpanOptions.create().input("hi"));
        CompletableFuture<String> f = CompletableFuture.supplyAsync(() -> {
            try (Scope ignored = h.context().makeCurrent()) {
                return Flashcat.tool("inner", () -> "done");
            }
        });
        String r = f.whenComplete((v, e) -> {
            if (e != null) {
                h.fail(e);
            } else {
                h.setOutput(v);
                h.end();
            }
        }).get();
        assertEquals("done", r);
        SpanData agent = span("invoke_agent async");
        assertEquals(agent.getSpanId(), span("execute_tool inner").getParentSpanId());
        assertTrue(attr(agent, "gen_ai.output.messages").contains("done"));

        FlashcatSpan failing = Flashcat.startWorkflow("async-fail");
        CompletableFuture<String> bad = CompletableFuture.supplyAsync(() -> {
            throw new IllegalArgumentException("bad");
        });
        bad.whenComplete((v, e) -> failing.fail(e));
        assertThrows(ExecutionException.class, bad::get);
        Thread.sleep(50);
        assertEquals(StatusCode.ERROR, span("invoke_workflow async-fail").getStatus().getStatusCode());
    }

    /** Wraps an iterator with a span handle and ends the span on exhaustion, early close, or an exception. */
    static final class TracedIterator implements Iterator<String>, AutoCloseable {
        private final Iterator<String> inner;
        private final FlashcatSpan span;
        private final List<String> seen = new ArrayList<String>();

        TracedIterator(String name, Iterator<String> inner) {
            this.inner = inner;
            this.span = Flashcat.startTool(name);
        }

        @Override
        public boolean hasNext() {
            boolean more;
            try {
                more = inner.hasNext();
            } catch (RuntimeException e) {
                span.fail(e);
                throw e;
            }
            if (!more) {
                span.setOutput(seen);
                span.end();
            }
            return more;
        }

        @Override
        public String next() {
            try {
                String v = inner.next();
                seen.add(v);
                return v;
            } catch (RuntimeException e) {
                span.fail(e);
                throw e;
            }
        }

        @Override
        public void close() {
            span.setOutput(seen);
            span.close();
        }
    }

    @Test
    void streamingEndsSpanOnExhaustionEarlyCloseAndError() {
        TracedIterator full = new TracedIterator("stream-full", Arrays.asList("a", "b").iterator());
        while (full.hasNext()) {
            full.next();
        }
        assertEquals(StatusCode.UNSET, span("execute_tool stream-full").getStatus().getStatusCode());

        try (TracedIterator early = new TracedIterator("stream-early", Arrays.asList("a", "b", "c").iterator())) {
            early.next();
        }
        span("execute_tool stream-early");

        Iterator<String> broken = new Iterator<String>() {
            int n;

            @Override
            public boolean hasNext() {
                return true;
            }

            @Override
            public String next() {
                if (n++ == 1) {
                    throw new NoSuchElementException("stream broke");
                }
                return "x";
            }
        };
        TracedIterator err = new TracedIterator("stream-error", broken);
        err.next();
        assertThrows(NoSuchElementException.class, err::next);
        assertEquals(StatusCode.ERROR, span("execute_tool stream-error").getStatus().getStatusCode());
        assertEquals(3, exporter.getFinishedSpanItems().size());
    }

    @Test
    void handleIsIdempotentAndNeverThrows() {
        FlashcatSpan h = Flashcat.startTool("t");
        h.end();
        h.end();
        h.fail(new RuntimeException("after end"));
        h.close();
        assertTrue(h.isEnded());
        assertEquals(1, exporter.getFinishedSpanItems().size());
        assertEquals(StatusCode.UNSET, span("execute_tool t").getStatus().getStatusCode());
    }

    @Test
    void sdkFailureDoesNotAffectBusiness() {
        GlobalOpenTelemetry.resetForTest();
        Flashcat.resetForTest();
        GlobalOpenTelemetry.set(new OpenTelemetry() {
            @Override
            public TracerProvider getTracerProvider() {
                throw new IllegalStateException("broken tracer provider");
            }

            @Override
            public ContextPropagators getPropagators() {
                return ContextPropagators.noop();
            }
        });
        assertEquals("still works", Flashcat.agent("a", () -> "still works"));
        int[] ran = {0};
        Flashcat.tool("t", () -> {
            ran[0]++;
        });
        assertEquals(1, ran[0], "The callback runs exactly once");
        FlashcatSpan h = Flashcat.startAgent("x");
        h.setInput("q").setOutput("a").end();
    }

    @Test
    void noopBeforeAnyProviderDoesNotPinGlobal() {
        GlobalOpenTelemetry.resetForTest();
        Flashcat.resetForTest();
        assertEquals(1, (int) Flashcat.agent("a", () -> 1));
        assertFalse(GlobalOpenTelemetry.isSet(), "The semantic API uses getOrNoop to avoid registering a global no-op instance");
    }

    // ------------------------------------------------------------------ Sessions

    @Test
    void sessionAttributesOnSpansInScopeOnly() {
        Tracer t = GlobalOpenTelemetry.getOrNoop().getTracer("other");
        try (Flashcat.Session s = Flashcat.session().conversationId("c-1").userId("u-1").tag("tenant", "a").start()) {
            Flashcat.agent("in", () -> {});
            t.spanBuilder("plain-in-session").startSpan().end();
            try (Flashcat.Session inner = Flashcat.session().tag("team", "kf").start()) {
                t.spanBuilder("nested").startSpan().end();
            }
        }
        Flashcat.agent("out", () -> {});
        for (String n : new String[] {"invoke_agent in", "plain-in-session", "nested"}) {
            SpanData s = span(n);
            assertEquals("c-1", attr(s, "gen_ai.conversation.id"), n);
            assertEquals("u-1", attr(s, "user.id"), n);
            assertEquals("a", attr(s, "flashcat.tag.tenant"), n);
        }
        assertEquals("kf", attr(span("nested"), "flashcat.tag.team"));
        SpanData out = span("invoke_agent out");
        assertNull(attr(out, "gen_ai.conversation.id"));
        assertNull(attr(out, "user.id"));
    }

    @Test
    void existingConversationIdIsNotOverwritten() {
        Tracer t = GlobalOpenTelemetry.getOrNoop().getTracer("anthropic");
        try (Flashcat.Session s = Flashcat.session().conversationId("c-session").start()) {
            t.spanBuilder("chat").setAttribute("gen_ai.conversation.id", "c-native").startSpan().end();
        }
        assertEquals("c-native", attr(span("chat"), "gen_ai.conversation.id"));
    }

    @Test
    void noBaggageByDefaultAndBaggageWhenPropagate() {
        try (Flashcat.Session s = Flashcat.session().conversationId("c-1").userId("u-1").start()) {
            assertTrue(Baggage.current().isEmpty(), "Baggage is off by default");
        }
        try (Flashcat.Session s = Flashcat.session().conversationId("c-2").userId("u-2").propagate(true).start()) {
            assertEquals("c-2", Baggage.current().getEntryValue("gen_ai.conversation.id"));
            assertEquals("u-2", Baggage.current().getEntryValue("user.id"));
        }
        assertTrue(Baggage.current().isEmpty());
    }

    /**
     * Under the Java Agent, the processor can only inherit attributes from the parent span because it
     * cannot read the application's private Context keys. Simulate this with a parent outside the session Context.
     */
    @Test
    void childInheritsSessionAttributesFromLocalParent() {
        Tracer t = GlobalOpenTelemetry.getOrNoop().getTracer("agent-side");
        Span parent = t.spanBuilder("invoke_agent kefu")
                .setAttribute("gen_ai.conversation.id", "c-9").setAttribute("user.id", "u-9")
                .setAttribute("flashcat.tag.tenant", "t1").startSpan();
        try (Scope ignored = Context.root().with(parent).makeCurrent()) {
            Span child = t.spanBuilder("chat gpt").setAttribute("gen_ai.operation.name", "chat").startSpan();
            try (Scope c = child.makeCurrent()) {
                t.spanBuilder("POST").startSpan().end(); // Grandchild spans inherit the attributes too.
            }
            child.end();
        }
        parent.end();
        for (String n : new String[] {"chat gpt", "POST"}) {
            SpanData s = span(n);
            assertEquals("c-9", attr(s, "gen_ai.conversation.id"), n);
            assertEquals("u-9", attr(s, "user.id"), n);
            assertEquals("t1", attr(s, "flashcat.tag.tenant"), n);
        }
    }

    @Test
    void remoteParentIsNotInherited() {
        Tracer t = GlobalOpenTelemetry.getOrNoop().getTracer("x");
        Span local = t.spanBuilder("p").setAttribute("gen_ai.conversation.id", "c").startSpan();
        Span remote = Span.wrap(io.opentelemetry.api.trace.SpanContext.createFromRemoteParent(
                local.getSpanContext().getTraceId(), local.getSpanContext().getSpanId(),
                local.getSpanContext().getTraceFlags(), local.getSpanContext().getTraceState()));
        try (Scope ignored = Context.root().with(remote).makeCurrent()) {
            t.spanBuilder("server").startSpan().end();
        }
        local.end();
        assertNull(attr(span("server"), "gen_ai.conversation.id"));
    }
}
