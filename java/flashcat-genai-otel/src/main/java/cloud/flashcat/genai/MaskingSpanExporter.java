package cloud.flashcat.genai;

import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.trace.data.DelegatingSpanData;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import java.util.ArrayList;
import java.util.Collection;
import java.util.List;

/**
 * Rewrites span attributes before sending them to the delegate exporter (AI root marker, masking, and truncation, contract §5.1 and §6).
 *
 * <p>Java's {@code SpanData} is immutable and processors cannot modify ended spans, so this class wraps the exporter as in Go.
 * The wrapper applies only to Flashcat's export pipeline. Masking runs on the batch processing thread, freeing the application thread from that work.
 */
final class MaskingSpanExporter implements SpanExporter {
    private final SpanExporter inner;
    private final Masker masker;
    private final GenAiAncestry ancestry;

    MaskingSpanExporter(SpanExporter inner, Masker masker, GenAiAncestry ancestry) {
        this.inner = inner;
        this.masker = masker;
        this.ancestry = ancestry;
    }

    @Override
    public CompletableResultCode export(Collection<SpanData> spans) {
        List<SpanData> out = new ArrayList<SpanData>(spans.size());
        for (SpanData sd : spans) {
            SpanData item = sd;
            try {
                Attributes original = sd.getAttributes();
                Attributes masked = masker.mask(original);
                Attributes attrs = masked != null ? masked : original;
                // Set the AI root marker for spans that gain GenAI attributes after start, using their final attributes and the ancestry recorded at start.
                if (original.get(GenAiAncestry.ROOT) == null && GenAiAncestry.isGenAi(original)) {
                    attrs = attrs.toBuilder().put(GenAiAncestry.ROOT, !ancestry.has(sd.getSpanContext())).build();
                }
                if (attrs != original) {
                    item = new RewrittenSpanData(sd, attrs);
                }
            } catch (Throwable ignored) {
                // Pass a span through unchanged if rewriting it fails so the other spans can still be exported.
            }
            out.add(item);
        }
        return inner.export(out);
    }

    @Override
    public CompletableResultCode flush() {
        return inner.flush();
    }

    @Override
    public CompletableResultCode shutdown() {
        return inner.shutdown();
    }

    @Override
    public String toString() {
        return "FlashcatMaskingSpanExporter{" + inner + "}";
    }

    /** SpanData with overridden attributes. */
    static final class RewrittenSpanData extends DelegatingSpanData {
        private final Attributes attributes;
        private final int total;

        RewrittenSpanData(SpanData delegate, Attributes attributes) {
            super(delegate);
            this.attributes = attributes;
            this.total = delegate.getTotalAttributeCount() + Math.max(0, attributes.size() - delegate.getAttributes().size());
        }

        @Override
        public Attributes getAttributes() {
            return attributes;
        }

        @Override
        public int getTotalAttributeCount() {
            return total;
        }
    }
}
