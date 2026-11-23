package cloud.flashcat.genai.agent.openai;

/**
 * Observer for streaming responses. {@link ObservedStream} and {@link ObservedAsyncStream}
 * forward each element and signal when the stream ends.
 *
 * @param <T> the stream element type (Responses event or Chat Completions chunk)
 */
interface StreamObserver<T> {
    /** Receives each element on the consumer thread before the caller sees it. */
    void onItem(T item);

    /**
     * Called with null on exhaustion or close, or with the exception on a read failure.
     * Implementations must be idempotent because the observer may be called more than once.
     */
    void onEnd(Throwable error);
}
