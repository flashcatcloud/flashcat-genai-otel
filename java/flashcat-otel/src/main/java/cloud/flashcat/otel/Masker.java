package cloud.flashcat.otel;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.AttributeType;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.common.AttributesBuilder;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.atomic.AtomicLong;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Implements masking and truncation (contract §6), matching Python's {@code MaskingWrapper._mask}.
 * A key name match yields {@code [REDACTED]}. Otherwise, if content capture is enabled and the key holds content,
 * apply L2 rules to the first scanLimit characters. Apply L3 truncation last. Count a span once as masked if any
 * value changes and once as truncated if any value is cut.
 */
final class Masker {
    /**
     * Python 3's {@code \d} matches all Unicode decimal digits, while Java defaults to ASCII.
     * L2 patterns use UNICODE_CHARACTER_CLASS to match Python. Explicit ranges such as {@code [A-Za-z]} are unaffected.
     */
    static final int CONTENT_FLAGS = Pattern.UNICODE_CHARACTER_CLASS;

    /** L1 matches the final segment exactly to preserve counters such as gen_ai.usage.input_tokens. */
    static final Pattern SENSITIVE_KEY = Pattern.compile(
            "(^|[._-])(authorization|api[-_]?key|apikey|password|passwd|secret|client[-_]?secret|"
                    + "token|access[-_]?token|refresh[-_]?token|auth[-_]?token|cookie|set[-_]?cookie|private[-_]?key)$",
            Pattern.CASE_INSENSITIVE | Pattern.UNICODE_CASE);

    static final Set<String> CONTENT_KEYS = Collections.unmodifiableSet(new HashSet<String>(Arrays.asList(
            "gen_ai.input.messages",
            "gen_ai.output.messages",
            "gen_ai.system_instructions",
            "gen_ai.tool.call.arguments",
            "gen_ai.tool.call.result",
            "gen_ai.retrieval.query.text",
            "gen_ai.retrieval.documents")));

    static final String REDACTED = "[REDACTED]";
    static final AttributeKey<Boolean> TRUNCATED_ATTR = AttributeKey.booleanKey("flashcat.content_truncated");

    private final MaskingConfig cfg;
    private final boolean contentEnabled;
    private final List<Pattern> patterns = new ArrayList<Pattern>();
    private final List<String> replacements = new ArrayList<String>();
    final AtomicLong masked = new AtomicLong();
    final AtomicLong truncated = new AtomicLong();

    Masker(MaskingConfig cfg, boolean contentEnabled) {
        this.cfg = cfg;
        this.contentEnabled = contentEnabled;
        List<MaskingConfig.Rule> all = new ArrayList<MaskingConfig.Rule>(cfg.getContentRules());
        all.addAll(cfg.getExtraRules());
        for (MaskingConfig.Rule r : all) {
            patterns.add(Pattern.compile(r.getRegex(), CONTENT_FLAGS));
            replacements.add(Matcher.quoteReplacement(r.getReplacement()));
        }
    }

    /** Checks the L1 rule. */
    static boolean isSensitiveKey(String key) {
        return SENSITIVE_KEY.matcher(key).find();
    }

    /** L2 applies each rule in order to the first scanLimit characters of a content value and appends the untouched suffix. */
    String maskContent(String v) {
        int limit = cfg.getScanLimitBytes();
        String head = v;
        String tail = "";
        if (v.length() > limit) {
            int cut = limit;
            // Keep the surrogate pair intact.
            if (Character.isHighSurrogate(v.charAt(cut - 1))) {
                cut--;
            }
            head = v.substring(0, cut);
            tail = v.substring(cut);
        }
        for (int i = 0; i < patterns.size(); i++) {
            head = patterns.get(i).matcher(head).replaceAll(replacements.get(i));
        }
        return head + tail;
    }

    /** L3 truncates values exceeding the UTF-8 byte limit at a valid character boundary. Returns null if the value fits. */
    static String truncateUtf8(String s, int limit) {
        // For a quick upper bound, allow 3 bytes per char. A 2-char surrogate pair takes 4 bytes.
        if ((long) s.length() * 3 <= limit) {
            return null;
        }
        int bytes = 0;
        int i = 0;
        int n = s.length();
        while (i < n) {
            char c = s.charAt(i);
            int len;
            int step = 1;
            if (c < 0x80) {
                len = 1;
            } else if (c < 0x800) {
                len = 2;
            } else if (Character.isHighSurrogate(c) && i + 1 < n && Character.isLowSurrogate(s.charAt(i + 1))) {
                len = 4;
                step = 2;
            } else {
                len = 3;
            }
            if (bytes + len > limit) {
                return s.substring(0, i);
            }
            bytes += len;
            i += step;
        }
        return null;
    }

    static int utf8Length(String s) {
        return s.getBytes(StandardCharsets.UTF_8).length;
    }

    /** Processes one string value, returning the original object if unchanged. */
    String maskValue(String key, String v, boolean[] cutFlag) {
        String nv = v;
        if (cfg.isKeyMasking() && isSensitiveKey(key)) {
            nv = REDACTED;
        } else if (contentEnabled && CONTENT_KEYS.contains(key)) {
            nv = maskContent(nv);
        }
        String cut = truncateUtf8(nv, cfg.getTruncateBytes());
        if (cut != null) {
            nv = cut;
            cutFlag[0] = true;
        }
        return nv;
    }

    /**
     * Processes a set of attributes. Returns null if none change so the caller can reuse the original.
     */
    Attributes mask(Attributes attrs) {
        if (attrs == null || attrs.isEmpty()) {
            return null;
        }
        Map<AttributeKey<String>, String> changed = null;
        boolean[] cut = new boolean[] {false};
        for (Map.Entry<AttributeKey<?>, Object> e : attrs.asMap().entrySet()) {
            AttributeKey<?> k = e.getKey();
            if (k.getType() != AttributeType.STRING || !(e.getValue() instanceof String)) {
                continue;
            }
            String v = (String) e.getValue();
            String nv = maskValue(k.getKey(), v, cut);
            if (!nv.equals(v)) {
                if (changed == null) {
                    changed = new LinkedHashMap<AttributeKey<String>, String>();
                }
                @SuppressWarnings("unchecked")
                AttributeKey<String> sk = (AttributeKey<String>) k;
                changed.put(sk, nv);
            }
        }
        if (changed == null && !cut[0]) {
            return null;
        }
        AttributesBuilder b = attrs.toBuilder();
        if (changed != null) {
            masked.incrementAndGet();
            for (Map.Entry<AttributeKey<String>, String> e : changed.entrySet()) {
                b.put(e.getKey(), e.getValue());
            }
        }
        if (cut[0]) {
            truncated.incrementAndGet();
            b.put(TRUNCATED_ATTR, true);
        }
        return b.build();
    }

    /** Probes custom patterns at init and rejects those with excessive backtracking, matching Python's _probe_timeout. */
    static void probe(Pattern p) {
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < 2000; i++) {
            sb.append('a');
        }
        sb.append('@');
        for (int i = 0; i < 30; i++) {
            sb.append('1');
        }
        sb.append('!');
        final long deadline = System.nanoTime() + 50_000_000L;
        boolean slow = false;
        try {
            p.matcher(new DeadlineCharSequence(sb.toString(), deadline)).find();
        } catch (ProbeTimeout e) {
            slow = true;
        }
        if (slow || System.nanoTime() > deadline) {
            throw new ConfigException("脱敏正则 '" + p.pattern() + "' 在探测样本上耗时过长，可能存在回溯，请改成线性写法");
        }
    }

    /** Java regex matching cannot be interrupted. Check the deadline on character access and throw on timeout so even exponential backtracking is detected within 50 ms. */
    private static final class DeadlineCharSequence implements CharSequence {
        private final String s;
        private final long deadline;
        private int reads;

        DeadlineCharSequence(String s, long deadline) {
            this.s = s;
            this.deadline = deadline;
        }

        @Override
        public char charAt(int index) {
            if ((++reads & 0x3ff) == 0 && System.nanoTime() > deadline) {
                throw new ProbeTimeout();
            }
            return s.charAt(index);
        }

        @Override
        public int length() {
            return s.length();
        }

        @Override
        public CharSequence subSequence(int start, int end) {
            return new DeadlineCharSequence(s.substring(start, end), deadline);
        }

        @Override
        public String toString() {
            return s;
        }
    }

    private static final class ProbeTimeout extends RuntimeException {
        private static final long serialVersionUID = 1L;

        ProbeTimeout() {
            super(null, null, false, false);
        }
    }
}
