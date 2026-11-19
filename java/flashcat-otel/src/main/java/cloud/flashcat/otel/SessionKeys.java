package cloud.flashcat.otel;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.context.ContextKey;
import java.util.Map;

/**
 * Private OTel Context keys and span attribute names for session data (contract §5). Baggage is off by default.
 * Depends only on opentelemetry-api so the application-side semantic API and the session processor can share these keys.
 */
final class SessionKeys {
    static final ContextKey<String> CONVERSATION = ContextKey.named("flashcat.conversation_id");
    static final ContextKey<String> USER = ContextKey.named("flashcat.user_id");
    static final ContextKey<Map<String, String>> TAGS = ContextKey.named("flashcat.tags");

    static final AttributeKey<String> ATTR_CONVERSATION = AttributeKey.stringKey("gen_ai.conversation.id");
    static final AttributeKey<String> ATTR_USER = AttributeKey.stringKey("user.id");
    static final String TAG_PREFIX = "flashcat.tag.";

    /** Baggage keys written only when propagate=true. */
    static final String BAGGAGE_CONVERSATION = "gen_ai.conversation.id";
    static final String BAGGAGE_USER = "user.id";

    private SessionKeys() {}
}
