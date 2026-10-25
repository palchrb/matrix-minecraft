package io.github.palchrb.matrixminecraft;

import java.io.IOException;
import java.security.SecureRandom;
import java.util.Base64;
import java.util.Set;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

import com.google.gson.Gson;
import com.google.gson.GsonBuilder;
import com.google.gson.JsonArray;
import com.google.gson.JsonObject;
import org.bukkit.Bukkit;
import org.bukkit.command.Command;
import org.bukkit.command.CommandSender;
import org.bukkit.command.ConsoleCommandSender;
import org.bukkit.command.RemoteConsoleCommandSender;
import org.bukkit.plugin.java.JavaPlugin;

/**
 * Companion plugin for matrix-minecraft. Streams game events to the bridge
 * and shows Matrix messages in game, over one token-protected HTTP endpoint,
 * so the bridge needs neither console scraping nor RCON.
 */
public final class BridgePlugin extends JavaPlugin {
    private static final Set<String> COLORS = Set.of(
            "black", "dark_blue", "dark_green", "dark_aqua", "dark_red", "dark_purple",
            "gold", "gray", "dark_gray", "blue", "green", "aqua", "red", "light_purple",
            "yellow", "white");
    private static final int MAX_SENDER_LENGTH = 64;
    private static final int MAX_MESSAGE_LENGTH = 2000;

    private final Gson gson = new GsonBuilder().disableHtmlEscaping().create();
    private EventServer server;
    private String token;

    @Override
    public void onEnable() {
        saveDefaultConfig();

        String bind = getConfig().getString("bind", "0.0.0.0");
        int port = getConfig().getInt("port", 25580);
        String envPort = System.getenv("MC_BRIDGE_PORT");
        if (envPort != null && !envPort.isBlank()) {
            try {
                port = Integer.parseInt(envPort.trim());
            } catch (NumberFormatException e) {
                getLogger().warning("Ignoring invalid MC_BRIDGE_PORT: " + envPort);
            }
        }

        token = resolveToken();
        if (token.length() < 24) {
            getLogger().warning("The bridge token is short; anyone who can reach port " + port
                    + " could guess it. Prefer the generated token or at least 24 random characters.");
        }

        try {
            server = new EventServer(getLogger(), bind, port, token, this::healthInfo, this::broadcast);
            server.start();
        } catch (IOException e) {
            getLogger().severe("Failed to start bridge endpoint on " + bind + ":" + port + ": " + e);
            server = null;
            return;
        }
        getServer().getPluginManager().registerEvents(new GameEventListener(this, server), this);
        getLogger().info("Bridge endpoint listening on " + bind + ":" + port
                + ". Run /matrixbridge token in the console to show the token.");
    }

    @Override
    public void onDisable() {
        if (server != null) {
            server.stop();
            server = null;
        }
    }

    @Override
    public boolean onCommand(CommandSender sender, Command command, String label, String[] args) {
        if (args.length == 1 && args[0].equalsIgnoreCase("token")) {
            // The token is the only key to the bridge endpoint, so keep it
            // off players' screens (and out of command block output): only
            // the server console and RCON may read it.
            if (!(sender instanceof ConsoleCommandSender) && !(sender instanceof RemoteConsoleCommandSender)) {
                sender.sendMessage("For security, the bridge token can only be shown in the server console "
                        + "(or via RCON, e.g. docker exec <container> rcon-cli matrixbridge token).");
                return true;
            }
            sender.sendMessage("Matrix bridge token: " + token);
            return true;
        }
        return false;
    }

    /**
     * Token precedence: MC_BRIDGE_TOKEN, then config.yml, then a random token
     * generated on first start and saved to config.yml.
     */
    private String resolveToken() {
        String env = System.getenv("MC_BRIDGE_TOKEN");
        if (env != null && !env.isBlank()) {
            return env.trim();
        }
        String configured = getConfig().getString("token", "");
        if (!configured.isBlank()) {
            return configured.trim();
        }
        byte[] raw = new byte[32];
        new SecureRandom().nextBytes(raw);
        String generated = Base64.getUrlEncoder().withoutPadding().encodeToString(raw);
        getConfig().set("token", generated);
        saveConfig();
        getLogger().info("Generated a new bridge token and saved it to config.yml: " + generated);
        return generated;
    }

    private JsonObject healthInfo() {
        JsonObject info = new JsonObject();
        info.addProperty("plugin", "matrix-minecraft-bridge");
        info.addProperty("version", getDescription().getVersion());
        info.addProperty("server", Bukkit.getName() + " " + Bukkit.getBukkitVersion());
        info.addProperty("motd", Bukkit.getMotd());
        return info;
    }

    /**
     * Shows a Matrix message to everyone in game. Runs tellraw on the main
     * thread with a component built here from plain strings, so the sender
     * can't inject click events or commands, and waits for it to finish so
     * the bridge only reports success once the message was shown.
     */
    private void broadcast(EventServer.SendRequest req) throws Exception {
        EventServer.SendRequest.Format f = req.format != null ? req.format : new EventServer.SendRequest.Format();
        String sender = clean(req.sender, MAX_SENDER_LENGTH);
        String message = clean(req.message, MAX_MESSAGE_LENGTH);

        JsonArray parts = new JsonArray();
        parts.add("");
        String prefix = f.prefix != null ? clean(f.prefix, 32) : "[Matrix]";
        if (!prefix.isEmpty()) {
            parts.add(part(prefix + " ", color(f.prefix_color, "light_purple"), true));
        }
        parts.add(part("<" + sender + "> ", color(f.sender_color, "aqua"), false));
        parts.add(part(message, color(f.message_color, "white"), false));
        String command = "tellraw @a " + gson.toJson(parts);

        Future<Boolean> result = Bukkit.getScheduler().callSyncMethod(this,
                () -> Bukkit.dispatchCommand(Bukkit.getConsoleSender(), command));
        if (!result.get(5, TimeUnit.SECONDS)) {
            throw new IllegalStateException("tellraw was not executed");
        }
    }

    private static JsonObject part(String text, String color, boolean bold) {
        JsonObject o = new JsonObject();
        o.addProperty("text", text);
        o.addProperty("color", color);
        if (bold) {
            o.addProperty("bold", true);
        }
        return o;
    }

    private static String color(String requested, String fallback) {
        return requested != null && COLORS.contains(requested) ? requested : fallback;
    }

    /** Drops control characters (except newlines) and caps the length. */
    private static String clean(String s, int maxLength) {
        StringBuilder b = new StringBuilder(Math.min(s.length(), maxLength));
        s.codePoints()
                .filter(cp -> cp == '\n' || !Character.isISOControl(cp))
                .limit(maxLength)
                .forEach(b::appendCodePoint);
        return b.toString().strip();
    }
}
