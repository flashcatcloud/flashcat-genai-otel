package cloud.flashcat.genai.it;

import cloud.flashcat.genai.Flashcat;

/**
 * OldApiIT launches this app with an opentelemetry-api version before 1.57, which lacks
 * GlobalOpenTelemetry.getOrNoop. Semantic API calls must still succeed.
 */
public final class OldApiApp {
    private OldApiApp() {}

    public static void main(String[] args) throws Exception {
        String r;
        try (Flashcat.Session s = Flashcat.session().conversationId("c").start()) {
            r = Flashcat.agent("kefu", () -> Flashcat.tool("t", () -> "ok"));
        }
        Flashcat.startAgent("h").setInput("x").end();
        System.out.println("RESULT:" + r);
    }
}
