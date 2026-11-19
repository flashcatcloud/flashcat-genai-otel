package cloud.flashcat.otel;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import cloud.flashcat.otel.testing.MiniJson;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Paths;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;

/** Checks every contract §6 fixture in spec/fixtures/masking.json against the Python reference results. */
class MaskingTest {

    @SuppressWarnings("unchecked")
    static Map<String, Object> fixture() throws Exception {
        String dir = System.getProperty("flashcat.test.fixtures.dir");
        String json = new String(Files.readAllBytes(Paths.get(dir, "masking.json")), StandardCharsets.UTF_8);
        return (Map<String, Object>) MiniJson.parse(json);
    }

    @Test
    @SuppressWarnings("unchecked")
    void rulesAreLiterallyTheSameAsPython() throws Exception {
        Map<String, Object> f = fixture();
        assertEquals(f.get("sensitive_key_regex"), Masker.SENSITIVE_KEY.pattern());
        assertEquals(f.get("replacement_l1"), Masker.REDACTED);
        assertEquals(f.get("truncated_attr"), Masker.TRUNCATED_ATTR.getKey());
        assertEquals(((Number) f.get("scan_limit_bytes")).intValue(), MaskingConfig.defaults().getScanLimitBytes());
        assertEquals(((Number) f.get("truncate_bytes")).intValue(), MaskingConfig.defaults().getTruncateBytes());
        List<Object> keys = (List<Object>) f.get("content_keys");
        assertEquals(keys.size(), Masker.CONTENT_KEYS.size());
        assertTrue(Masker.CONTENT_KEYS.containsAll(keys));
        List<Map<String, Object>> patterns = (List<Map<String, Object>>) f.get("content_patterns");
        List<MaskingConfig.Rule> rules = MaskingConfig.DEFAULT_RULES;
        assertEquals(patterns.size(), rules.size());
        for (int i = 0; i < rules.size(); i++) {
            assertEquals(patterns.get(i).get("name"), rules.get(i).getName(), "Masking rules must appear in the same order");
            assertEquals(patterns.get(i).get("regex"), rules.get(i).getRegex(), "Regular expressions must match exactly");
            assertEquals(patterns.get(i).get("replacement"), rules.get(i).getReplacement());
        }
    }

    @Test
    @SuppressWarnings("unchecked")
    void keyCases() throws Exception {
        for (Map<String, Object> c : (List<Map<String, Object>>) fixture().get("key_cases")) {
            String key = (String) c.get("key");
            assertEquals(c.get("masked"), Masker.isSensitiveKey(key), "key=" + key);
        }
    }

    @Test
    @SuppressWarnings("unchecked")
    void contentCases() throws Exception {
        Masker m = new Masker(MaskingConfig.defaults(), true);
        for (Map<String, Object> c : (List<Map<String, Object>>) fixture().get("content_cases")) {
            String in = (String) c.get("input");
            assertEquals(c.get("output"), m.maskContent(in), "input=" + in);
            // Verify the same behavior through the content attribute.
            Attributes masked = m.mask(Attributes.of(AttributeKey.stringKey("gen_ai.input.messages"), in));
            String got = masked == null ? in : masked.get(AttributeKey.stringKey("gen_ai.input.messages"));
            assertEquals(c.get("output"), got);
        }
    }

    @Test
    void contentPatternsOnlyWhenContentEnabledAndOnlyOnContentKeys() {
        Masker off = new Masker(MaskingConfig.defaults(), false);
        assertNull(off.mask(Attributes.of(AttributeKey.stringKey("gen_ai.input.messages"), "手机13812345678")));
        Masker on = new Masker(MaskingConfig.defaults(), true);
        assertNull(on.mask(Attributes.of(AttributeKey.stringKey("http.url"), "手机13812345678")), "L2 masking applies only to content attributes");
    }

    /** Matches Python's test_key_masking_does_not_touch_token_counts. */
    @Test
    void keyMaskingDoesNotTouchTokenCountsAndTruncatesOnCharBoundary() {
        Masker m = new Masker(MaskingConfig.builder().truncateBytes(10).build(), false);
        Attributes in = Attributes.builder()
                .put("http.request.header.authorization", "Bearer abc")
                .put("gen_ai.usage.input_tokens", 5L)
                .put("big", "中文中文中文")
                .build();
        Attributes a = m.mask(in);
        assertEquals("[REDACTED]", a.get(AttributeKey.stringKey("http.request.header.authorization")));
        assertEquals(5L, a.get(AttributeKey.longKey("gen_ai.usage.input_tokens")));
        assertEquals("中文中", a.get(AttributeKey.stringKey("big")));
        assertEquals(Boolean.TRUE, a.get(Masker.TRUNCATED_ATTR));
        assertEquals(1, m.masked.get());
        assertEquals(1, m.truncated.get());
    }

    @Test
    void truncateHandlesSurrogatePairs() {
        // 😀 takes four bytes in UTF-8.
        assertEquals("a", Masker.truncateUtf8("a😀b", 4));
        assertEquals("a😀", Masker.truncateUtf8("a😀b", 5));
        assertNull(Masker.truncateUtf8("a😀b", 6));
    }

    @Test
    void scanLimitOnlyMasksHead() {
        Masker m = new Masker(MaskingConfig.builder().scanLimitBytes(20).build(), true);
        String v = "13812345678 xxxxxxxxx13812345678";
        assertEquals("[REDACTED:phone] xxxxxxxxx13812345678", m.maskContent(v));
    }

    @Test
    void customRuleAndBacktrackingProbe() {
        MaskingConfig cfg = MaskingConfig.builder().addPattern("order", "ORD-\\d{8}", "[REDACTED:order]").build();
        Masker m = new Masker(cfg, true);
        assertEquals("单号 [REDACTED:order]", m.maskContent("单号 ORD-20261112"));
        assertThrows(ConfigException.class, () -> MaskingConfig.builder().addPattern("bad", "(a+a+)+$", "x").build());
        assertThrows(ConfigException.class, () -> MaskingConfig.builder().addPattern("syntax", "([", "x").build());
    }
}
