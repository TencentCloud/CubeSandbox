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
        int port = Integer.parseInt(System.getenv().getOrDefault("APP_PORT", "8080"));
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
        server.createContext("/", HelloWorldServer::handleRoot);
        server.createContext("/health", HelloWorldServer::handleHealth);
        server.setExecutor(Executors.newFixedThreadPool(4));
        return server;
    }

    private static void handleRoot(HttpExchange exchange) throws IOException {
        String body = "<!doctype html>\n"
                + "<title>cubesandbox-base-java</title>\n"
                + "<h1>Hello from Java inside a CubeSandbox MicroVM</h1>\n"
                + "<p>JVM: " + System.getProperty("java.version") + "</p>\n"
                + "<p>envd is running on :49983, this Java server on :"
                + System.getenv().getOrDefault("APP_PORT", "8080") + ".</p>\n";
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "text/html; charset=utf-8");
        exchange.sendResponseHeaders(200, bytes.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(bytes);
        }
    }

    private static void handleHealth(HttpExchange exchange) throws IOException {
        exchange.sendResponseHeaders(200, -1);
        exchange.close();
    }
}
