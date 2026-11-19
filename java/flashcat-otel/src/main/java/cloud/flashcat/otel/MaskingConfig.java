package cloud.flashcat.otel;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.regex.Pattern;
import java.util.regex.PatternSyntaxException;

/**
 * Configures the three layers of masking and truncation (contract §6) for Flashcat's export pipeline only.
 *
 * <ul>
 *   <li>L1 checks the final attribute key segment and replaces string values with {@code [REDACTED]} for keys such as authorization, api_key, password, and token.
 *   <li>L2 runs only when content capture is enabled. It masks email addresses, national ID numbers, bank card numbers, and mobile phone numbers in order in the first 256 KiB of each content attribute. Custom rules can be appended.
 *   <li>L3 truncates string attributes over 512 KiB in UTF-8 bytes and sets {@code flashcat.content_truncated=true}.
 * </ul>
 *
 * <pre>{@code
 * MaskingConfig masking = MaskingConfig.builder()
 *     .addPattern("order_no", "ORD-\\d{8}", "[REDACTED:order]")
 *     .build();
 * }</pre>
 */
public final class MaskingConfig {
    /** L2 replacement rule with a name, a regex, and literal replacement text. {@code $} references are left uninterpreted. */
    public static final class Rule {
        private final String name;
        private final String regex;
        private final String replacement;

        Rule(String name, String regex, String replacement) {
            this.name = name;
            this.regex = regex;
            this.replacement = replacement;
        }

        /** @return the rule name */
        public String getName() {
            return name;
        }

        /** @return the regular expression */
        public String getRegex() {
            return regex;
        }

        /** @return the replacement text */
        public String getReplacement() {
            return replacement;
        }
    }

    /**
     * Default L2 rules match Python's {@code DEFAULT_PATTERNS} in {@code _processors.py} exactly, including order.
     * Digit boundaries use lookaround because {@code \b} fails when digits in JSON sit next to a {@code \\uXXXX} escape for a Chinese character.
     */
    static final List<Rule> DEFAULT_RULES;

    static {
        List<Rule> rules = new ArrayList<Rule>();
        rules.add(new Rule("email", "[A-Za-z0-9._%+\\-]+@[A-Za-z0-9.\\-]+\\.[A-Za-z]{2,}", "[REDACTED:email]"));
        rules.add(new Rule(
                "cn_id_card",
                "(?<!\\d)[1-9]\\d{5}(?:19|20)\\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\\d|3[01])\\d{3}[\\dXx](?![\\dXx])",
                "[REDACTED:id_card]"));
        rules.add(new Rule("bank_card", "(?<!\\d)(?:\\d{4}[ \\-]?){3}\\d{4,7}(?!\\d)", "[REDACTED:bank_card]"));
        rules.add(new Rule("cn_mobile", "(?<!\\d)1[3-9]\\d{9}(?!\\d)", "[REDACTED:phone]"));
        DEFAULT_RULES = Collections.unmodifiableList(rules);
    }

    private final boolean keyMasking;
    private final List<Rule> contentRules;
    private final List<Rule> extraRules;
    private final int scanLimitBytes;
    private final int truncateBytes;

    private MaskingConfig(Builder b) {
        this.keyMasking = b.keyMasking;
        this.contentRules = Collections.unmodifiableList(new ArrayList<Rule>(b.contentRules));
        this.extraRules = Collections.unmodifiableList(new ArrayList<Rule>(b.extraRules));
        this.scanLimitBytes = b.scanLimitBytes;
        this.truncateBytes = b.truncateBytes;
    }

    /** @return the defaults with all three layers enabled and the default rules */
    public static MaskingConfig defaults() {
        return builder().build();
    }

    /** @return a builder */
    public static Builder builder() {
        return new Builder();
    }

    /** @return whether L1 key-name masking is enabled */
    public boolean isKeyMasking() {
        return keyMasking;
    }

    /** @return the default L2 rules, which {@link Builder#clearDefaultPatterns()} can remove */
    public List<Rule> getContentRules() {
        return contentRules;
    }

    /** @return the appended custom rules */
    public List<Rule> getExtraRules() {
        return extraRules;
    }

    /** @return the L2 scan limit per value in characters, matching Python */
    public int getScanLimitBytes() {
        return scanLimitBytes;
    }

    /** @return the L3 per-value limit in UTF-8 bytes */
    public int getTruncateBytes() {
        return truncateBytes;
    }

    /** Builder for {@link MaskingConfig}. */
    public static final class Builder {
        private boolean keyMasking = true;
        private final List<Rule> contentRules = new ArrayList<Rule>(DEFAULT_RULES);
        private final List<Rule> extraRules = new ArrayList<Rule>();
        private int scanLimitBytes = 256 * 1024;
        private int truncateBytes = 512 * 1024;

        private Builder() {}

        /**
         * Controls L1 key-name masking, which is enabled by default.
         *
         * @param on whether to enable L1 key-name masking
         * @return this
         */
        public Builder keyMasking(boolean on) {
            this.keyMasking = on;
            return this;
        }

        /**
         * Clears the default L2 rules, leaving only the rules supplied with {@link #addPattern}.
         *
         * @return this
         */
        public Builder clearDefaultPatterns() {
            contentRules.clear();
            return this;
        }

        /**
         * Adds an L2 rule. {@link #build()} compiles the regex, probes it for excessive backtracking, and throws {@link ConfigException} if the regex is invalid.
         *
         * @param name rule name
         * @param regex Java regular expression. Prefer linear-time patterns and avoid nested quantifiers
         * @param replacement literal replacement text
         * @return this
         */
        public Builder addPattern(String name, String regex, String replacement) {
            if (name == null || regex == null || replacement == null) {
                throw new ConfigException("脱敏规则的 name / regex / replacement 都不能为 null");
            }
            extraRules.add(new Rule(name, regex, replacement));
            return this;
        }

        /**
         * Sets the maximum L2 scan length per value (default 256 KiB).
         *
         * @param n positive scan limit
         * @return this
         */
        public Builder scanLimitBytes(int n) {
            if (n <= 0) {
                throw new ConfigException("scanLimitBytes 必须大于 0，收到 " + n);
            }
            this.scanLimitBytes = n;
            return this;
        }

        /**
         * Sets the L3 per-value limit in UTF-8 bytes (default 512 KiB).
         *
         * @param n positive truncation limit
         * @return this
         */
        public Builder truncateBytes(int n) {
            if (n <= 0) {
                throw new ConfigException("truncateBytes 必须大于 0，收到 " + n);
            }
            this.truncateBytes = n;
            return this;
        }

        /**
         * Builds the configuration. Throws {@link ConfigException} for invalid custom regexes or excessive backtracking.
         *
         * @return the masking configuration
         */
        public MaskingConfig build() {
            for (Rule r : extraRules) {
                Pattern p;
                try {
                    p = Pattern.compile(r.regex, Masker.CONTENT_FLAGS);
                } catch (PatternSyntaxException e) {
                    throw new ConfigException("脱敏正则 '" + r.regex + "' 不合法：" + e.getDescription());
                }
                Masker.probe(p);
            }
            return new MaskingConfig(this);
        }
    }
}
