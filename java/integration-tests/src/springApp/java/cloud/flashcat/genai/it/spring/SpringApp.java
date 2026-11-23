package cloud.flashcat.genai.it.spring;

import cloud.flashcat.genai.Flashcat;
import cloud.flashcat.genai.FlashcatGenAI;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.OpenTelemetry;
import java.time.Duration;
import org.springframework.boot.CommandLineRunner;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.WebApplicationType;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.context.ConfigurableApplicationContext;

/**
 * SpringStarterIT launches this Spring Boot app with OpenTelemetry Spring Boot Starter and
 * flashcat-genai-otel on the classpath. It runs without the Java Agent.
 */
@SpringBootApplication
public class SpringApp implements CommandLineRunner {
    private final OpenTelemetry openTelemetry;
    private final org.springframework.context.ApplicationContext context;

    public SpringApp(OpenTelemetry openTelemetry, org.springframework.context.ApplicationContext context) {
        this.openTelemetry = openTelemetry;
        this.context = context;
    }

    public static void main(String[] args) {
        SpringApplication app = new SpringApplication(SpringApp.class);
        app.setWebApplicationType(WebApplicationType.NONE);
        ConfigurableApplicationContext ctx = app.run(args);
        ctx.close();
    }

    @Override
    public void run(String... args) throws Exception {
        System.out.println("BEAN:" + openTelemetry.getClass().getName());
        System.out.println("GLOBAL_SET:" + GlobalOpenTelemetry.isSet());
        // Check whether Spring Boot autoconfigures Micrometer Tracing when both dependencies are present.
        String tracer = "class-absent";
        try {
            Class<?> c = Class.forName("io.micrometer.tracing.Tracer");
            String[] names = context.getBeanNamesForType(c);
            tracer = names.length == 0 ? "no-bean" : context.getBean(names[0]).getClass().getName();
        } catch (ClassNotFoundException ignored) {
            // Minimal classpath.
        }
        System.out.println("MICROMETER_TRACER:" + tracer);
        try (Flashcat.Session s = Flashcat.session().conversationId("c-spring").userId("u-spring").tag("tenant", "t2").start()) {
            Flashcat.agent("kefu", () -> {
                Flashcat.tool("lookup_order", () -> "shipped");
                // Use the Starter's OpenTelemetry bean to create a child span. The session processor reads private Context keys.
                openTelemetry.getTracer("app-manual").spanBuilder("manual-child").startSpan().end();
                return "ok";
            });
            // Without an enclosing Flashcat span, the Starter's session processor still reads private Context keys.
            openTelemetry.getTracer("app-manual").spanBuilder("outside-flashcat-span").startSpan().end();
        }
        System.out.println("FLUSH:" + FlashcatGenAI.flush(Duration.ofSeconds(10)));
        System.out.println("RESULT:" + FlashcatGenAI.diagnostics().toJson());
    }
}
