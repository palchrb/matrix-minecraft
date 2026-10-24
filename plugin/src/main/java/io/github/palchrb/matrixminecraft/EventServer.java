package io.github.palchrb.matrixminecraft;

import java.io.IOException;
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
import java.util.logging.Logger;

import com.google.gson.Gson;
import com.google.gson.JsonObject;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

/**
 * Minimal Server-Sent Events endpoint built on the JDK HTTP server.
 *
 * <p>GET /events streams one "data:" line of JSON per game event, with an
 * "id:" of the form "&lt;boot&gt;-&lt;seq&gt;". A reconnecting client that
 * sends Last-Event-ID from the same boot gets the events it missed replayed
 * from a small in-memory history. GET /health returns plugin info so the
 * bridge can detect whether the plugin is installed.
 */
final class EventServer {
    private static final int HISTORY_SIZE = 256;
    private static final int MAX_CLIENTS = 8;
    private static final int CLIENT_QUEUE_SIZE = 1024;
    private static final long PING_SECONDS = 15;

    private final Logger log;
    private final String token;
    private final String version;
    private final String bootId = Long.toString(System.currentTimeMillis(), 36);
    private final Gson gson = new Gson();
    private final HttpServer http;
    private final ExecutorService executor = Executors.newCachedThreadPool(r -> {
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

    EventServer(Logger log, String bind, int port, String token, String version) throws IOException {
        this.log = log;
        this.token = token;
        this.version = version;
        this.http = HttpServer.create(new InetSocketAddress(bind, port), 0);
        http.setExecutor(executor);
        http.createContext("/health", this::handleHealth);
        http.createContext("/events", this::handleEvents);
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

    private boolean authorized(HttpExchange ex) {
        String header = ex.getRequestHeaders().getFirst("Authorization");
        if (header == null || !header.startsWith("Bearer ")) {
            return false;
        }
        byte[] got = header.substring("Bearer ".length()).getBytes(StandardCharsets.UTF_8);
        return MessageDigest.isEqual(got, token.getBytes(StandardCharsets.UTF_8));
    }

    private void handleHealth(HttpExchange ex) throws IOException {
        try {
            if (!authorized(ex)) {
                ex.sendResponseHeaders(401, -1);
                return;
            }
            JsonObject body = new JsonObject();
            body.addProperty("plugin", "matrix-minecraft-bridge");
            body.addProperty("version", version);
            body.addProperty("boot", bootId);
            byte[] out = gson.toJson(body).getBytes(StandardCharsets.UTF_8);
            ex.getResponseHeaders().set("Content-Type", "application/json");
            ex.sendResponseHeaders(200, out.length);
            ex.getResponseBody().write(out);
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
            if (!authorized(ex)) {
                ex.sendResponseHeaders(401, -1);
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

    private static void write(OutputStream out, String s) throws IOException {
        out.write(s.getBytes(StandardCharsets.UTF_8));
        out.flush();
    }
}
