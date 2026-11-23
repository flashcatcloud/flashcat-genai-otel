package cloud.flashcat.genai.agent.openai;

import static net.bytebuddy.matcher.ElementMatchers.isMethod;
import static net.bytebuddy.matcher.ElementMatchers.named;
import static net.bytebuddy.matcher.ElementMatchers.takesArgument;
import static net.bytebuddy.matcher.ElementMatchers.takesArguments;

import io.opentelemetry.javaagent.extension.instrumentation.TypeInstrumentation;
import io.opentelemetry.javaagent.extension.instrumentation.TypeTransformer;
import net.bytebuddy.asm.Advice;
import net.bytebuddy.description.type.TypeDescription;
import net.bytebuddy.implementation.bytecode.assign.Assigner;
import net.bytebuddy.matcher.ElementMatcher;

/**
 * Instruments the Chat Completions implementation classes. See {@link ChatCall}.
 *
 * <ul>
 *   <li>Synchronous {@code ChatCompletionServiceImpl} instruments {@code create} and {@code createStreaming}.</li>
 *   <li>Synchronous {@code ChatCompletionServiceImpl$WithRawResponseImpl} instruments {@code create}, used by Spring AI's synchronous calls.</li>
 *   <li>Asynchronous {@code ChatCompletionServiceAsyncImpl} instruments {@code create} and {@code createStreaming}.</li>
 * </ul>
 *
 * <p>A current upstream chat span suppresses new span creation. Streaming calls only add time to
 * first token (TTFT) to that span. When {@code create} calls {@code withRawResponse().create},
 * the inner call uses the span that the outer call already made current.
 */
final class ChatCompletionServiceInstrumentation implements TypeInstrumentation {
    static final String BLOCKING = "com.openai.services.blocking.chat.ChatCompletionServiceImpl";
    static final String BLOCKING_RAW = "com.openai.services.blocking.chat.ChatCompletionServiceImpl$WithRawResponseImpl";
    static final String ASYNC = "com.openai.services.async.chat.ChatCompletionServiceAsyncImpl";
    private static final String PARAMS = "com.openai.models.chat.completions.ChatCompletionCreateParams";

    private final String typeName;

    ChatCompletionServiceInstrumentation(String typeName) {
        this.typeName = typeName;
    }

    @Override
    public ElementMatcher<TypeDescription> typeMatcher() {
        return named(typeName);
    }

    @Override
    public void transform(TypeTransformer transformer) {
        String prefix = ChatCompletionServiceInstrumentation.class.getName();
        ElementMatcher.Junction<net.bytebuddy.description.method.MethodDescription> create =
                isMethod().and(named("create")).and(takesArguments(2)).and(takesArgument(0, named(PARAMS)));
        ElementMatcher.Junction<net.bytebuddy.description.method.MethodDescription> streaming =
                isMethod().and(named("createStreaming")).and(takesArguments(2)).and(takesArgument(0, named(PARAMS)));
        if (BLOCKING.equals(typeName)) {
            transformer.applyAdviceToMethod(create, prefix + "$CreateAdvice");
            transformer.applyAdviceToMethod(streaming, prefix + "$StreamingAdvice");
        } else if (BLOCKING_RAW.equals(typeName)) {
            transformer.applyAdviceToMethod(create, prefix + "$RawCreateAdvice");
        } else {
            transformer.applyAdviceToMethod(create, prefix + "$AsyncCreateAdvice");
            transformer.applyAdviceToMethod(streaming, prefix + "$AsyncStreamingAdvice");
        }
    }

    public static final class CreateAdvice {
        private CreateAdvice() {}

        @Advice.OnMethodEnter(suppress = Throwable.class)
        public static Object onEnter(@Advice.Argument(0) Object params, @Advice.FieldValue("clientOptions") Object clientOptions) {
            return ChatCall.start(params, clientOptions);
        }

        @Advice.OnMethodExit(onThrowable = Throwable.class, suppress = Throwable.class)
        public static void onExit(@Advice.Enter Object call, @Advice.Return Object response, @Advice.Thrown Throwable error) {
            ChatCall.end(call, response, error);
        }
    }

    public static final class RawCreateAdvice {
        private RawCreateAdvice() {}

        @Advice.OnMethodEnter(suppress = Throwable.class)
        public static Object onEnter(@Advice.Argument(0) Object params, @Advice.FieldValue("clientOptions") Object clientOptions) {
            return ChatCall.start(params, clientOptions);
        }

        @Advice.OnMethodExit(onThrowable = Throwable.class, suppress = Throwable.class)
        public static void onExit(
                @Advice.Enter Object call,
                @Advice.Return(readOnly = false, typing = Assigner.Typing.DYNAMIC) Object response,
                @Advice.Thrown Throwable error) {
            response = ChatCall.endRaw(call, response, error);
        }
    }

    public static final class StreamingAdvice {
        private StreamingAdvice() {}

        @Advice.OnMethodEnter(suppress = Throwable.class)
        public static Object onEnter(@Advice.Argument(0) Object params, @Advice.FieldValue("clientOptions") Object clientOptions) {
            return ChatCall.enterStreaming(params, clientOptions);
        }

        @Advice.OnMethodExit(onThrowable = Throwable.class, suppress = Throwable.class)
        public static void onExit(
                @Advice.Enter Object state,
                @Advice.Return(readOnly = false, typing = Assigner.Typing.DYNAMIC) Object stream,
                @Advice.Thrown Throwable error) {
            stream = ChatCall.exitStreaming(state, stream, error);
        }
    }

    public static final class AsyncCreateAdvice {
        private AsyncCreateAdvice() {}

        @Advice.OnMethodEnter(suppress = Throwable.class)
        public static Object onEnter(@Advice.Argument(0) Object params, @Advice.FieldValue("clientOptions") Object clientOptions) {
            return ChatCall.start(params, clientOptions);
        }

        @Advice.OnMethodExit(onThrowable = Throwable.class, suppress = Throwable.class)
        public static void onExit(@Advice.Enter Object call, @Advice.Return Object future, @Advice.Thrown Throwable error) {
            ChatCall.endAsync(call, future, error);
        }
    }

    public static final class AsyncStreamingAdvice {
        private AsyncStreamingAdvice() {}

        @Advice.OnMethodEnter(suppress = Throwable.class)
        public static Object onEnter(@Advice.Argument(0) Object params, @Advice.FieldValue("clientOptions") Object clientOptions) {
            return ChatCall.enterStreaming(params, clientOptions);
        }

        @Advice.OnMethodExit(onThrowable = Throwable.class, suppress = Throwable.class)
        public static void onExit(
                @Advice.Enter Object state,
                @Advice.Return(readOnly = false, typing = Assigner.Typing.DYNAMIC) Object stream,
                @Advice.Thrown Throwable error) {
            stream = ChatCall.exitAsyncStreaming(state, stream, error);
        }
    }
}
