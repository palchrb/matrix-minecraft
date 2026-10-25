package io.github.palchrb.matrixminecraft;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.ArrayDeque;
import java.util.Deque;
import java.util.Set;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import java.util.function.Supplier;
import java.util.logging.Logger;

import com.google.gson.Gson;
import com.google.gson.GsonBuilder;
import com.google.gson.JsonObject;
import com.google.gson.JsonParseException;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

/**
 * The plugin's HTTP API, built on the JDK HTTP server. Every endpoint needs
 * "Authorization: Bearer &lt;token&gt;".
 *
 * <ul>
 * <li>GET /health returns plugin and server info, so the bridge can verify
 * the URL and token.</li>
 * <li>GET /events is a Server-Sent Events stream with one "data:" line of
 * JSON per game event and an "id:" of the form "&lt;boot&gt;-&lt;seq&gt;". A
 * reconnecting client that sends Last-Event-ID from the same boot gets the
 * events it missed replayed from a small in-memory history.</li>
 * <li>POST /send shows a chat message from Matrix in game. It only ever
 * displays text; there is deliberately no way to run commands.</li>
 * </ul>
 */
final class EventServer {
    private static final int HISTORY_SIZE = 256;
    private static final int MAX_CLIENTS = 8;
    private static final int CLIENT_QUEUE_SIZE = 1024;
    private static final long PING_SECONDS = 15;
    private static final int THREADS = 32;
    private static final int MAX_BODY_BYTES = 64 * 1024;
    private static final long AUTH_FAILURE_DELAY_MS = 1000;

    /** Receives chat messages from Matrix; implemented by the plugin. */
    interface ChatSink {
        void broadcast(SendRequest request) throws Exception;
    }

    /** Body of POST /send. */
    static final class SendRequest {
        String sender;
        String message;
        Format format;

        static final class Format {
            String prefix;
            String prefix_color;
            String sender_color;
            String message_color;
        }
    }

    private final Logger log;
    private final String token;
    private final Supplier<JsonObject> healthInfo;
    private final ChatSink chatSink;
    private final String bootId = Long.toString(System.currentTimeMillis(), 36);
    private final Gson gson = new GsonBuilder().disableHtmlEscaping().create();
    private final HttpServer http;
    // A fixed pool bounds how many requests (including deliberately slowed
    // failed logins) can be in flight at once.
    private final ExecutorService executor = Executors.newFixedThreadPool(THREADS, r -> {
        Thread t = new Thread(r, "matrix-bridge-http");
        t.setDaemon(true);
        return t;
    });

    // Guarded by this.
    private long seq;
    private final Deque<Event> history = new ArrayDeque<>();
    private final Set<Client> clients = ConcurrentHashMap.newKeySet();

    private record Event(long seq, String json) {}

    private static final class Client {
        final BlockingQueue<Event> queue = new LinkedBlockingQueue<>(CLIENT_QUEUE_SIZE);
        volatile boolean overflowed;
    }

    EventServer(Logger log, String bind, int port, String token,
            Supplier<JsonObject> healthInfo, ChatSink chatSink) throws IOException {
        this.log = log;
        this.token = token;
        this.healthInfo = healthInfo;
        this.chatSink = chatSink;
        this.http = HttpServer.create(new InetSocketAddress(bind, port), 0);
        http.setExecutor(executor);
        http.createContext("/health", this::handleHealth);
        http.createContext("/events", this::handleEvents);
        http.createContext("/send", this::handleSend);
    }

    void start() {
        http.start();
    }

    void stop() {
        http.stop(0);
        executor.shutdownNow();
    }

    /** Queues an event for every connected client. Safe to call from any thread. */
    void publish(JsonObject payload) {
        synchronized (this) {
            Event ev = new Event(++seq, gson.toJson(payload));
            history.addLast(ev);
            if (history.size() > HISTORY_SIZE) {
                history.removeFirst();
            }
            for (Client c : clients) {
                if (!c.queue.offer(ev)) {
                    c.overflowed = true;
                }
            }
        }
    }

    /**
     * Checks the bearer token. On failure it sends 401 after a short delay,
     * which together with the bounded thread pool caps how fast anyone can
     * guess, and returns false.
     */
    private boolean authorize(HttpExchange ex) throws IOException {
        String header = ex.getRequestHeaders().getFirst("Authorization");
        if (header != null && header.startsWith("Bearer ")) {
            byte[] got = header.substring("Bearer ".length()).getBytes(StandardCharsets.UTF_8);
            if (MessageDigest.isEqual(got, token.getBytes(StandardCharsets.UTF_8))) {
                return true;
            }
        }
        log.warning("Rejected request with invalid token from " + ex.getRemoteAddress());
        try {
            Thread.sleep(AUTH_FAILURE_DELAY_MS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
        ex.sendResponseHeaders(401, -1);
        return false;
    }

    private void handleHealth(HttpExchange ex) throws IOException {
        try {
            if (!authorize(ex)) {
                return;
            }
            JsonObject body = healthInfo.get();
            body.addProperty("boot", bootId);
            sendJSON(ex, 200, body);
        } finally {
            ex.close();
        }
    }

    private void handleSend(HttpExchange ex) throws IOException {
        try {
            if (!"POST".equals(ex.getRequestMethod())) {
                ex.sendResponseHeaders(405, -1);
                return;
            }
            if (!authorize(ex)) {
                return;
            }
            byte[] raw = readBody(ex.getRequestBody());
            if (raw == null) {
                ex.sendResponseHeaders(413, -1);
                return;
            }
            SendRequest req;
            try {
                req = gson.fromJson(new String(raw, StandardCharsets.UTF_8), SendRequest.class);
            } catch (JsonParseException e) {
                req = null;
            }
            if (req == null || req.sender == null || req.sender.isBlank()
                    || req.message == null || req.message.isBlank()) {
                sendError(ex, 400, "sender and message are required");
                return;
            }
            try {
                chatSink.broadcast(req);
            } catch (Exception e) {
                log.warning("Failed to show Matrix message in game: " + e);
                sendError(ex, 503, "server could not display the message");
                return;
            }
            JsonObject ok = new JsonObject();
            ok.addProperty("ok", true);
            sendJSON(ex, 200, ok);
        } finally {
            ex.close();
        }
    }

    private void handleEvents(HttpExchange ex) throws IOException {
        try {
            if (!"GET".equals(ex.getRequestMethod())) {
                ex.sendResponseHeaders(405, -1);
                return;
            }
            if (!authorize(ex)) {
                return;
            }
            if (clients.size() >= MAX_CLIENTS) {
                ex.sendResponseHeaders(503, -1);
                return;
            }

            Client client = new Client();
            register(client, ex.getRequestHeaders().getFirst("Last-Event-ID"));
            log.info("Bridge connected from " + ex.getRemoteAddress());
            try {
                ex.getResponseHeaders().set("Content-Type", "text/event-stream");
                ex.getResponseHeaders().set("Cache-Control", "no-cache");
                // Stops nginx and similar reverse proxies from buffering the stream.
                ex.getResponseHeaders().set("X-Accel-Buffering", "no");
                ex.sendResponseHeaders(200, 0);
                OutputStream out = ex.getResponseBody();
                write(out, "retry: 2000\n: connected " + bootId + "\n\n");
                while (!client.overflowed) {
                    Event ev = client.queue.poll(PING_SECONDS, TimeUnit.SECONDS);
                    if (ev == null) {
                        write(out, ": ping\n\n");
                    } else {
                        write(out, "id: " + bootId + "-" + ev.seq() + "\ndata: " + ev.json() + "\n\n");
                    }
                }
                log.warning("Bridge client fell too far behind, dropping connection");
            } catch (IOException | InterruptedException e) {
                // Client went away or the server is stopping.
            } finally {
                clients.remove(client);
                log.info("Bridge disconnected from " + ex.getRemoteAddress());
            }
        } finally {
            ex.close();
        }
    }

    /**
     * Adds the client and, if it is resuming from this boot, queues the
     * events it missed. Done under the same lock as publish() so nothing is
     * lost or duplicated in between.
     */
    private synchronized void register(Client client, String lastEventId) {
        long resumeFrom = -1;
        if (lastEventId != null && lastEventId.startsWith(bootId + "-")) {
            try {
                resumeFrom = Long.parseLong(lastEventId.substring(bootId.length() + 1));
            } catch (NumberFormatException ignored) {
                // Unknown id, don't replay.
            }
        }
        if (resumeFrom >= 0) {
            for (Event ev : history) {
                if (ev.seq() > resumeFrom) {
                    client.queue.offer(ev);
                }
            }
        }
        clients.add(client);
    }

    /** Reads at most MAX_BODY_BYTES; returns null if the body is larger. */
    private static byte[] readBody(InputStream in) throws IOException {
        byte[] data = in.readNBytes(MAX_BODY_BYTES + 1);
        return data.length > MAX_BODY_BYTES ? null : data;
    }

    private void sendError(HttpExchange ex, int status, String message) throws IOException {
        JsonObject body = new JsonObject();
        body.addProperty("error", message);
        sendJSON(ex, status, body);
    }

    private void sendJSON(HttpExchange ex, int status, JsonObject body) throws IOException {
        byte[] out = gson.toJson(body).getBytes(StandardCharsets.UTF_8);
        ex.getResponseHeaders().set("Content-Type", "application/json");
        ex.sendResponseHeaders(status, out.length);
        ex.getResponseBody().write(out);
    }

    private static void write(OutputStream out, String s) throws IOException {
        out.write(s.getBytes(StandardCharsets.UTF_8));
        out.flush();
    }
}
