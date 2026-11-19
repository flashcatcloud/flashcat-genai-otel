package cloud.flashcat.otel;

/**
 * Thrown for invalid explicit arguments, the sole exception under contract §2, item 1.
 *
 * <p>Invalid explicit arguments to {@link FlashcatOtel#init(FlashcatOtel.Options)} and invalid custom {@link MaskingConfig} rules
 * raise this exception to surface during development. Invalid environment settings are reported only in diagnostics {@code problems}.
 */
public class ConfigException extends IllegalArgumentException {
    private static final long serialVersionUID = 1L;

    /**
     * Creates a configuration exception.
     *
     * @param message description in Chinese
     */
    public ConfigException(String message) {
        super(message);
    }
}
