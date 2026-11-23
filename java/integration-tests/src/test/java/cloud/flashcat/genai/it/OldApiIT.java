package cloud.flashcat.genai.it;

import static cloud.flashcat.genai.it.ItSupport.list;
import static cloud.flashcat.genai.it.ItSupport.prop;
import static org.junit.jupiter.api.Assertions.assertEquals;

import java.util.HashMap;
import org.junit.jupiter.api.Test;

/**
 * The semantic API falls back to GlobalOpenTelemetry.get() without throwing when an application
 * pins opentelemetry-api to 1.50.0, for example through an older Spring Boot BOM.
 */
class OldApiIT {
    @Test
    void semanticApiWorksWithOpenTelemetryApi150() throws Exception {
        ItSupport.Result r = ItSupport.runJava(list(), prop("it.oldApi.classpath"), "cloud.flashcat.genai.it.OldApiApp", list(),
                new HashMap<String, String>(), 60);
        assertEquals(0, r.exitCode, r.output);
        assertEquals("ok", r.line("RESULT:"), r.output);
    }
}
