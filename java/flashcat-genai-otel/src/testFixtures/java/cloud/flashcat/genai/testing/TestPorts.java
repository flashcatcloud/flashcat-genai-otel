package cloud.flashcat.genai.testing;

import com.sun.net.httpserver.HttpServer;
import java.io.IOException;
import java.net.BindException;
import java.net.InetSocketAddress;
import java.net.ServerSocket;

/** Tests can restrict listening ports with TEST_PORT_BASE / TEST_PORT_LAST; by default the OS assigns them. */
public final class TestPorts {
    private TestPorts() {}

    private static int first() {
        String value = System.getenv("TEST_PORT_BASE");
        return value == null ? 0 : Integer.parseInt(value);
    }

    private static int last() {
        String value = System.getenv("TEST_PORT_LAST");
        return value == null ? first() : Integer.parseInt(value);
    }

    public static HttpServer httpServer() throws IOException {
        for (int port = first(); port <= last(); port++) {
            try {
                return HttpServer.create(new InetSocketAddress("127.0.0.1", port), 0);
            } catch (BindException e) {
                if (port == last()) {
                    throw e;
                }
            }
        }
        throw new IOException("no free test port available");
    }

    public static ServerSocket serverSocket() throws IOException {
        for (int port = first(); port <= last(); port++) {
            ServerSocket socket = new ServerSocket();
            socket.setReuseAddress(true);
            try {
                socket.bind(new InetSocketAddress("127.0.0.1", port));
                return socket;
            } catch (BindException e) {
                socket.close();
                if (port == last()) {
                    throw e;
                }
            }
        }
        throw new IOException("no free test port available");
    }
}
