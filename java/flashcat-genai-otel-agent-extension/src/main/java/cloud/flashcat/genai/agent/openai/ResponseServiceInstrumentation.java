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
 * Instruments the Responses API implementation methods {@code create(params, requestOptions)}
 * and {@code createStreaming(params, requestOptions)}. The single-argument interface overloads
 * are default methods that delegate to these two-argument methods.
 *
 * <p>The advice is inlined into the openai-java implementation and makes a single helper call.
 * The helper catches exceptions and the advice suppresses them as a fallback.
 */
final class ResponseServiceInstrumentation implements TypeInstrumentation {
    static final String BLOCKING = "com.openai.services.blocking.ResponseServiceImpl";
    static final String ASYNC = "com.openai.services.async.ResponseServiceAsyncImpl";
    private static final String PARAMS = "com.openai.models.responses.ResponseCreateParams";

    private final String typeName;
    private final boolean async;

    ResponseServiceInstrumentation(String typeName, boolean async) {
        this.typeName = typeName;
        this.async = async;
    }

    @Override
    public ElementMatcher<TypeDescription> typeMatcher() {
        return named(typeName);
    }

    @Override
    public void transform(TypeTransformer transformer) {
        String prefix = ResponseServiceInstrumentation.class.getName();
        transformer.applyAdviceToMethod(
                isMethod().and(named("create")).and(takesArguments(2)).and(takesArgument(0, named(PARAMS))),
                prefix + (async ? "$AsyncCreateAdvice" : "$CreateAdvice"));
        transformer.applyAdviceToMethod(
                isMethod().and(named("createStreaming")).and(takesArguments(2)).and(takesArgument(0, named(PARAMS))),
                prefix + (async ? "$AsyncStreamingAdvice" : "$StreamingAdvice"));
    }

    public static final class CreateAdvice {
        private CreateAdvice() {}

        @Advice.OnMethodEnter(suppress = Throwable.class)
        public static Object onEnter(@Advice.Argument(0) Object params, @Advice.FieldValue("clientOptions") Object clientOptions) {
            return ResponsesCall.start(params, clientOptions, false);
        }

        @Advice.OnMethodExit(onThrowable = Throwable.class, suppress = Throwable.class)
        public static void onExit(@Advice.Enter Object call, @Advice.Return Object response, @Advice.Thrown Throwable error) {
            ResponsesCall.end(call, response, error);
        }
    }

    public static final class StreamingAdvice {
        private StreamingAdvice() {}

        @Advice.OnMethodEnter(suppress = Throwable.class)
        public static Object onEnter(@Advice.Argument(0) Object params, @Advice.FieldValue("clientOptions") Object clientOptions) {
            return ResponsesCall.start(params, clientOptions, true);
        }

        @Advice.OnMethodExit(onThrowable = Throwable.class, suppress = Throwable.class)
        public static void onExit(
                @Advice.Enter Object call,
                @Advice.Return(readOnly = false, typing = Assigner.Typing.DYNAMIC) Object stream,
                @Advice.Thrown Throwable error) {
            stream = ResponsesCall.endStreaming(call, stream, error);
        }
    }

    public static final class AsyncCreateAdvice {
        private AsyncCreateAdvice() {}

        @Advice.OnMethodEnter(suppress = Throwable.class)
        public static Object onEnter(@Advice.Argument(0) Object params, @Advice.FieldValue("clientOptions") Object clientOptions) {
            return ResponsesCall.start(params, clientOptions, false);
        }

        @Advice.OnMethodExit(onThrowable = Throwable.class, suppress = Throwable.class)
        public static void onExit(@Advice.Enter Object call, @Advice.Return Object future, @Advice.Thrown Throwable error) {
            ResponsesCall.endAsync(call, future, error);
        }
    }

    public static final class AsyncStreamingAdvice {
        private AsyncStreamingAdvice() {}

        @Advice.OnMethodEnter(suppress = Throwable.class)
        public static Object onEnter(@Advice.Argument(0) Object params, @Advice.FieldValue("clientOptions") Object clientOptions) {
            return ResponsesCall.start(params, clientOptions, true);
        }

        @Advice.OnMethodExit(onThrowable = Throwable.class, suppress = Throwable.class)
        public static void onExit(
                @Advice.Enter Object call,
                @Advice.Return(readOnly = false, typing = Assigner.Typing.DYNAMIC) Object stream,
                @Advice.Thrown Throwable error) {
            stream = ResponsesCall.endAsyncStreaming(call, stream, error);
        }
    }
}
