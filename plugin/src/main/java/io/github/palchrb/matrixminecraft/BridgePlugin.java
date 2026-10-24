package io.github.palchrb.matrixminecraft;

import java.io.File;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.util.Properties;

import org.bukkit.plugin.java.JavaPlugin;

/**
 * Companion plugin for matrix-minecraft. Exposes game events as a
 * Server-Sent Events stream that the bridge subscribes to, so the bridge no
 * longer has to scrape the server console.
 */
public final class BridgePlugin extends JavaPlugin {
    private EventServer server;

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

        String token = resolveToken();
        if (token == null) {
            getLogger().severe("No token configured: set token in config.yml, MC_BRIDGE_TOKEN, "
                    + "RCON_PASSWORD or rcon.password. Event stream disabled.");
            return;
        }

        try {
            server = new EventServer(getLogger(), bind, port, token, getDescription().getVersion());
            server.start();
        } catch (IOException e) {
            getLogger().severe("Failed to start event server on " + bind + ":" + port + ": " + e);
            server = null;
            return;
        }
        getServer().getPluginManager().registerEvents(new GameEventListener(this, server), this);
        getLogger().info("Streaming game events on " + bind + ":" + port + "/events");
    }

    @Override
    public void onDisable() {
        if (server != null) {
            server.stop();
            server = null;
        }
    }

    private String resolveToken() {
        String token = getConfig().getString("token", "");
        if (!token.isBlank()) {
            return token;
        }
        for (String env : new String[] {"MC_BRIDGE_TOKEN", "RCON_PASSWORD"}) {
            String v = System.getenv(env);
            if (v != null && !v.isBlank()) {
                return v;
            }
        }
        File props = new File("server.properties");
        if (props.isFile()) {
            Properties p = new Properties();
            try (InputStream in = new FileInputStream(props)) {
                p.load(in);
                String v = p.getProperty("rcon.password", "");
                if (!v.isBlank()) {
                    return v;
                }
            } catch (IOException e) {
                getLogger().warning("Could not read server.properties: " + e);
            }
        }
        return null;
    }
}
