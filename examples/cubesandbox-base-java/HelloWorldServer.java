import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.Executors;

/**
 * Minimal HTTP server used by the cubesandbox-base-java demo template.
 *
 * Uses only the JDK built-in {@code com.sun.net.httpserver} API so the image
 * needs no third-party dependencies. Listens on :8080 (override via APP_PORT)
 * and serves a tiny landing page plus a /health endpoint. The Cube readiness
 * probe is served by envd on :49983 — this server is the "real" application
 * traffic endpoint, mirroring how nginx serves :80 in cubesandbox-base-nginx.
 */
public class HelloWorldServer {

    public static void main(String[] args) throws IOException {
        int port = configuredPort();
        HttpServer server = createServer(port);
        server.start();
        System.out.println("HelloWorldServer listening on :" + port
                + " (JVM " + System.getProperty("java.version") + ")");
    }

    /**
     * Creates and configures (but does not start) the HTTP server.
     *
     * Exposed as package-private so that {@link HelloWorldServerTest} can bind
     * to port 0 (OS-assigned), start the server, fire requests, and shut it
     * down cleanly — without touching the real {@code APP_PORT}.
     */
    static HttpServer createServer(int port) throws IOException {
        HttpServer server = HttpServer.create(new InetSocketAddress(port), 0);
        server.createContext("/", exchange -> handleRoot(exchange, port));
        server.createContext("/health", HelloWorldServer::handleHealth);
        server.setExecutor(Executors.newFixedThreadPool(
                Math.max(1, Runtime.getRuntime().availableProcessors())));
        return server;
    }

    private static int configuredPort() {
        String value = System.getenv("APP_PORT");
        if (value == null || value.isBlank()) {
            return 8080;
        }
        try {
            int port = Integer.parseInt(value.trim());
            return port > 0 && port <= 65535 ? port : 8080;
        } catch (NumberFormatException ignored) {
            return 8080;
        }
    }

    private static void handleRoot(HttpExchange exchange, int port) throws IOException {
        String body = "<!doctype html>\n"
                + "<title>cubesandbox-base-java</title>\n"
                + "<h1>Hello from Java inside a CubeSandbox MicroVM</h1>\n"
                + "<p>JVM: " + System.getProperty("java.version") + "</p>\n"
                + "<p>envd is running on :49983, this Java server on :"
                + port + ".</p>\n";
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "text/html; charset=utf-8");
        exchange.sendResponseHeaders(200, bytes.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(bytes);
        }
    }

    private static void handleHealth(HttpExchange exchange) throws IOException {
        exchange.sendResponseHeaders(200, 0);
        try (OutputStream ignored = exchange.getResponseBody()) {
            // Send a zero-length response while allowing HTTP keep-alive.
        }
    }
}
