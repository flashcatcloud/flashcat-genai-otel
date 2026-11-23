package cloud.flashcat.genai.agent.openai;

import com.openai.core.http.StreamResponse;
import java.util.Comparator;
import java.util.Spliterator;
import java.util.function.Consumer;
import java.util.stream.Stream;
import java.util.stream.StreamSupport;

/**
 * Wraps the stream returned by synchronous {@code createStreaming}. The observer sees each
 * element before the caller and ends the span idempotently on exhaustion, {@link #close()},
 * or a read exception.
 */
final class ObservedStream<T> implements StreamResponse<T> {
    private final StreamResponse<T> delegate;
    private final StreamObserver<T> observer;

    ObservedStream(StreamResponse<T> delegate, StreamObserver<T> observer) {
        this.delegate = delegate;
        this.observer = observer;
    }

    @Override
    public Stream<T> stream() {
        Stream<T> inner = delegate.stream();
        // Preserve the original onClose handler, which closes the underlying response.
        return StreamSupport.stream(new ObservedSpliterator<T>(inner.spliterator(), observer), false).onClose(inner::close);
    }

    @Override
    public void close() {
        try {
            observer.onEnd(null);
        } finally {
            delegate.close();
        }
    }

    /** Forwards each element, signals exhaustion, and reports read exceptions before rethrowing them unchanged. */
    static final class ObservedSpliterator<T> implements Spliterator<T> {
        private final Spliterator<T> delegate;
        private final StreamObserver<T> observer;

        ObservedSpliterator(Spliterator<T> delegate, StreamObserver<T> observer) {
            this.delegate = delegate;
            this.observer = observer;
        }

        @Override
        public boolean tryAdvance(Consumer<? super T> action) {
            boolean more;
            try {
                more = delegate.tryAdvance(e -> {
                    observer.onItem(e);
                    action.accept(e);
                });
            } catch (RuntimeException | Error e) {
                observer.onEnd(e);
                throw e;
            }
            if (!more) {
                observer.onEnd(null);
            }
            return more;
        }

        @Override
        public Spliterator<T> trySplit() {
            return null;
        }

        @Override
        public long estimateSize() {
            return delegate.estimateSize();
        }

        /** Omit SIZED so terminal operations such as count() traverse the elements and the observer sees them. */
        @Override
        public int characteristics() {
            return delegate.characteristics() & ~(Spliterator.SIZED | Spliterator.SUBSIZED);
        }

        @Override
        public Comparator<? super T> getComparator() {
            return delegate.getComparator();
        }
    }
}
