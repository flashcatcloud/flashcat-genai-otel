package cloud.flashcat.genai;

import java.lang.management.ManagementFactory;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import java.util.function.LongFunction;
import java.util.function.Supplier;
import java.util.logging.Level;
import java.util.logging.Logger;
import javax.management.Attribute;
import javax.management.AttributeList;
import javax.management.AttributeNotFoundException;
import javax.management.DynamicMBean;
import javax.management.MBeanInfo;
import javax.management.MBeanOperationInfo;
import javax.management.MBeanParameterInfo;
import javax.management.MBeanServer;
import javax.management.ObjectName;
import javax.management.ReflectionException;

/**
 * Diagnostics channel between the application and the extension in Java agent mode.
 *
 * <p>The extension runs in the agent's class loader, so application-side {@link FlashcatGenAI} cannot access its static state.
 * The platform MBeanServer exchanges data using only JDK types (Map, List, String, Number, Boolean). The extension registers a DynamicMBean,
 * and the application invokes {@code diagnostics} and {@code flush} by name.
 *
 * <p>Registration runs on a background thread when the first span starts. Initializing JMX in premain initializes java.util.logging early
 * and conflicts with the JBoss / WildFly log manager. The OTel agent avoids this too.
 */
final class AgentBridge implements DynamicMBean {
    static final String OBJECT_NAME = "cloud.flashcat.genai:type=AgentExtension";
    private static final Logger logger = Logger.getLogger("cloud.flashcat.genai");

    private final Supplier<Map<String, Object>> diagnostics;
    private final LongFunction<Boolean> flush;

    AgentBridge(Supplier<Map<String, Object>> diagnostics, LongFunction<Boolean> flush) {
        this.diagnostics = diagnostics;
        this.flush = flush;
    }

    /** Registers the MBean from the extension on a background thread. */
    static void registerAsync(final AgentBridge bridge) {
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    MBeanServer server = ManagementFactory.getPlatformMBeanServer();
                    ObjectName name = new ObjectName(OBJECT_NAME);
                    if (!server.isRegistered(name)) {
                        server.registerMBean(bridge, name);
                    }
                } catch (Throwable e) {
                    logger.log(Level.FINE, "flashcat-genai-otel: 注册诊断 MBean 失败", e);
                }
            }
        }, "flashcat-genai-otel-mbean");
        t.setDaemon(true);
        t.start();
    }

    /** Reads extension diagnostics from the application. Returns null if the extension has yet to register. */
    @SuppressWarnings("unchecked")
    static Map<String, Object> fetchDiagnostics() {
        try {
            MBeanServer server = ManagementFactory.getPlatformMBeanServer();
            ObjectName name = new ObjectName(OBJECT_NAME);
            if (!server.isRegistered(name)) {
                return null;
            }
            Object r = server.invoke(name, "diagnostics", new Object[0], new String[0]);
            return r instanceof Map ? (Map<String, Object>) r : null;
        } catch (Throwable e) {
            return null;
        }
    }

    /** Requests a flush from the application. Returns null if the extension has yet to register. */
    static Boolean invokeFlush(long timeoutMillis) {
        try {
            MBeanServer server = ManagementFactory.getPlatformMBeanServer();
            ObjectName name = new ObjectName(OBJECT_NAME);
            if (!server.isRegistered(name)) {
                return null;
            }
            Object r = server.invoke(name, "flush", new Object[] {timeoutMillis}, new String[] {"long"});
            return r instanceof Boolean ? (Boolean) r : null;
        } catch (Throwable e) {
            return null;
        }
    }

    /** Waits for the extension to register the MBean, which is useful only shortly after the first span starts. */
    static boolean awaitRegistered(long timeout, TimeUnit unit) {
        long deadline = System.nanoTime() + unit.toNanos(timeout);
        try {
            MBeanServer server = ManagementFactory.getPlatformMBeanServer();
            ObjectName name = new ObjectName(OBJECT_NAME);
            while (System.nanoTime() < deadline) {
                if (server.isRegistered(name)) {
                    return true;
                }
                Thread.sleep(20);
            }
            return server.isRegistered(name);
        } catch (Throwable e) {
            return false;
        }
    }

    @Override
    public Object getAttribute(String attribute) throws AttributeNotFoundException {
        throw new AttributeNotFoundException(attribute);
    }

    @Override
    public void setAttribute(Attribute attribute) throws AttributeNotFoundException {
        throw new AttributeNotFoundException(attribute.getName());
    }

    @Override
    public AttributeList getAttributes(String[] attributes) {
        return new AttributeList();
    }

    @Override
    public AttributeList setAttributes(AttributeList attributes) {
        return new AttributeList();
    }

    @Override
    public Object invoke(String actionName, Object[] params, String[] signature) throws ReflectionException {
        if ("diagnostics".equals(actionName)) {
            return diagnostics.get();
        }
        if ("flush".equals(actionName)) {
            long timeout = params != null && params.length > 0 && params[0] instanceof Long ? (Long) params[0] : 5000L;
            return flush.apply(timeout);
        }
        throw new ReflectionException(new NoSuchMethodException(actionName));
    }

    @Override
    public MBeanInfo getMBeanInfo() {
        return new MBeanInfo(
                AgentBridge.class.getName(),
                "Flashcat GenAI OTel Java Agent 扩展的诊断通道",
                null,
                null,
                new MBeanOperationInfo[] {
                    new MBeanOperationInfo("diagnostics", "返回诊断（契约 §8）", new MBeanParameterInfo[0], "java.util.Map", MBeanOperationInfo.INFO),
                    new MBeanOperationInfo("flush", "flush 导出", new MBeanParameterInfo[] {
                        new MBeanParameterInfo("timeoutMillis", "long", "超时毫秒")
                    }, "java.lang.Boolean", MBeanOperationInfo.ACTION)
                },
                null);
    }
}
