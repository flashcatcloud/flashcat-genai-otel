package cloud.flashcat.genai.agent.openai;

import com.openai.core.http.AsyncStreamResponse;
import java.util.Optional;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.Executor;

/**
 * Wraps the asynchronous client's {@code createStreaming} stream and its subscriber handler.
 * The observer sees each element first. Both {@code onComplete} (success or failure) and
 * {@link #close()} notify the observer that the stream has ended.
 */
final class ObservedAsyncStream<T> implements AsyncStreamResponse<T> {
    private final AsyncStreamResponse<T> delegate;
    private final StreamObserver<T> observer;

    ObservedAsyncStream(AsyncStreamResponse<T> delegate, StreamObserver<T> observer) {
        this.delegate = delegate;
        this.observer = observer;
    }

    @Override
    public AsyncStreamResponse<T> subscribe(Handler<? super T> handler) {
        delegate.subscribe(new ObservedHandler<T>(handler, observer));
        return this;
    }

    @Override
    public AsyncStreamResponse<T> subscribe(Handler<? super T> handler, Executor executor) {
        delegate.subscribe(new ObservedHandler<T>(handler, observer), executor);
        return this;
    }

    @Override
    public CompletableFuture<Void> onCompleteFuture() {
        return delegate.onCompleteFuture();
    }

    @Override
    public void close() {
        try {
            observer.onEnd(null);
        } finally {
            delegate.close();
        }
    }

    /** Forwards elements. On completion, notifies the observer before the subscriber. */
    static final class ObservedHandler<T> implements Handler<T> {
        private final Handler<? super T> delegate;
        private final StreamObserver<T> observer;

        ObservedHandler(Handler<? super T> delegate, StreamObserver<T> observer) {
            this.delegate = delegate;
            this.observer = observer;
        }

        @Override
        public void onNext(T item) {
            observer.onItem(item);
            delegate.onNext(item);
        }

        @Override
        public void onComplete(Optional<Throwable> error) {
            observer.onEnd(error.isPresent() ? error.get() : null);
            delegate.onComplete(error);
        }
    }
}
