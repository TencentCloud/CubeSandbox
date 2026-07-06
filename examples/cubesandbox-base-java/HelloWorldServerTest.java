import com.sun.net.httpserver.HttpServer;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

/**
 * Standalone test for {@link HelloWorldServer}.
 *
 * Uses only JDK built-in APIs (no JUnit) to stay consistent with the
 * zero-dependency philosophy of the template. Run with:
 *
 * <pre>
 *   javac HelloWorldServer.java HelloWorldServerTest.java
 *   java HelloWorldServerTest
 * </pre>
 *
 * Exit code 0 = all tests passed; non-zero = at least one failure.
 */
public class HelloWorldServerTest {

    private static int failures = 0;

    public static void main(String[] args) throws Exception {
        HttpServer server = HelloWorldServer.createServer(0); // port 0 → OS picks a free port
        server.start();
        int port = server.getAddress().getPort();
        String base = "http://127.0.0.1:" + port;

        HttpClient client = HttpClient.newBuilder()
                .connectTimeout(java.time.Duration.ofSeconds(5))
                .build();

        try {
            testRootReturnsOkWithExpectedContent(client, base);
            testRootContentTypeIsHtmlUtf8(client, base);
            testHealthReturnsOk(client, base);
            testConcurrentRequestsAllSucceed(client, base);
        } finally {
            server.stop(0);
        }

        if (failures > 0) {
            System.err.println("\n" + failures + " test(s) FAILED");
            System.exit(1);
        } else {
            System.out.println("\nAll tests passed");
        }
    }

    // ------------------------------------------------------------------
    // Individual tests
    // ------------------------------------------------------------------

    private static void testRootReturnsOkWithExpectedContent(HttpClient client, String base)
            throws Exception {
        HttpResponse<String> resp = sendGet(client, base + "/");

        assertEquals("root status", 200, resp.statusCode());

        String body = resp.body();
        assertContains("root body", body, "Hello from Java inside a CubeSandbox MicroVM");
        assertContains("root body", body, "cubesandbox-base-java");
        assertContains("root body", body, "JVM:");
    }

    private static void testRootContentTypeIsHtmlUtf8(HttpClient client, String base)
            throws Exception {
        HttpResponse<String> resp = sendGet(client, base + "/");

        String ct = resp.headers().firstValue("Content-Type").orElse("");
        assertEquals("root Content-Type", "text/html; charset=utf-8", ct);
    }

    private static void testHealthReturnsOk(HttpClient client, String base)
            throws Exception {
        HttpResponse<String> resp = sendGet(client, base + "/health");

        assertEquals("health status", 200, resp.statusCode());
    }

    /**
     * Fires 32 concurrent GET / requests and verifies that every one returns
     * 200. This directly exercises the thread-pool executor — with the old
     * {@code setExecutor(null)} (single-threaded dispatcher) the test still
     * passes but requests are serialised; with the fixed thread pool they are
     * handled in parallel.
     */
    private static void testConcurrentRequestsAllSucceed(HttpClient client, String base)
            throws Exception {
        final int numRequests = 32;
        ExecutorService pool = Executors.newFixedThreadPool(numRequests);
        CountDownLatch ready = new CountDownLatch(1);
        CountDownLatch done = new CountDownLatch(numRequests);

        List<Future<HttpResponse<String>>> futures = new ArrayList<>(numRequests);
        for (int i = 0; i < numRequests; i++) {
            futures.add(pool.submit(() -> {
                ready.await();                       // all threads start together
                HttpResponse<String> r = sendGet(client, base + "/");
                done.countDown();
                return r;
            }));
        }

        ready.countDown();                           // release all threads at once
        boolean finished = done.await(15, TimeUnit.SECONDS);
        pool.shutdown();

        if (!finished) {
            fail("concurrent: not all requests completed within 15s");
            return;
        }

        int ok = 0;
        for (Future<HttpResponse<String>> f : futures) {
            HttpResponse<String> r = f.get();
            if (r.statusCode() == 200) {
                ok++;
            }
        }

        assertEquals("concurrent requests that returned 200", numRequests, ok);
    }

    // ------------------------------------------------------------------
    // Helpers
    // ------------------------------------------------------------------

    private static HttpResponse<String> sendGet(HttpClient client, String url)
            throws Exception {
        HttpRequest req = HttpRequest.newBuilder()
                .uri(URI.create(url))
                .timeout(java.time.Duration.ofSeconds(10))
                .GET()
                .build();
        return client.send(req, HttpResponse.BodyHandlers.ofString(StandardCharsets.UTF_8));
    }

    private static void assertEquals(String label, int expected, int actual) {
        if (expected != actual) {
            fail(label + ": expected " + expected + ", got " + actual);
        } else {
            System.out.println("  [OK] " + label + " = " + actual);
        }
    }

    private static void assertEquals(String label, String expected, String actual) {
        if (!expected.equals(actual)) {
            fail(label + ": expected \"" + expected + "\", got \"" + actual + "\"");
        } else {
            System.out.println("  [OK] " + label + " = \"" + actual + "\"");
        }
    }

    private static void assertContains(String label, String haystack, String needle) {
        if (!haystack.contains(needle)) {
            fail(label + ": missing substring \"" + needle + "\"\nbody:\n" + haystack);
        } else {
            System.out.println("  [OK] " + label + " contains \"" + needle + "\"");
        }
    }

    private static void fail(String message) {
        failures++;
        System.err.println("  [FAIL] " + message);
    }
}
