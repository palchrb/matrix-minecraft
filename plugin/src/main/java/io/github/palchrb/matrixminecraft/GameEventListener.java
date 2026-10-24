package io.github.palchrb.matrixminecraft;

import java.lang.reflect.Method;
import java.util.logging.Level;

import com.google.gson.JsonObject;
import org.bukkit.ChatColor;
import org.bukkit.GameRule;
import org.bukkit.entity.Player;
import org.bukkit.event.EventHandler;
import org.bukkit.event.EventPriority;
import org.bukkit.event.Listener;
import org.bukkit.event.entity.PlayerDeathEvent;
import org.bukkit.event.player.AsyncPlayerChatEvent;
import org.bukkit.event.player.PlayerAdvancementDoneEvent;
import org.bukkit.event.player.PlayerJoinEvent;
import org.bukkit.event.player.PlayerQuitEvent;

/**
 * Turns Bukkit events into JSON for the bridge. Everything listens at
 * MONITOR priority so we see the final result after other plugins (vanish,
 * chat formatters, mutes) have had their say: a cancelled chat or a null
 * join message is not bridged.
 *
 * <p>Each event is sent as {"type", "player", "message", "ts"}. For chat,
 * "message" is what the player typed; for everything else it's the full
 * broadcast text, e.g. "Sverre was slain by Zombie".
 */
final class GameEventListener implements Listener {
    private final BridgePlugin plugin;
    private final EventServer server;

    GameEventListener(BridgePlugin plugin, EventServer server) {
        this.plugin = plugin;
        this.server = server;
    }

    private void send(String type, Player player, String message) {
        if (message == null) {
            return;
        }
        message = ChatColor.stripColor(message).trim();
        if (message.isEmpty()) {
            return;
        }
        JsonObject obj = new JsonObject();
        obj.addProperty("type", type);
        obj.addProperty("player", player.getName());
        obj.addProperty("message", message);
        obj.addProperty("ts", System.currentTimeMillis());
        server.publish(obj);
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onChat(AsyncPlayerChatEvent e) {
        send("chat", e.getPlayer(), e.getMessage());
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onJoin(PlayerJoinEvent e) {
        send("join", e.getPlayer(), e.getJoinMessage());
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onQuit(PlayerQuitEvent e) {
        send("leave", e.getPlayer(), e.getQuitMessage());
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onDeath(PlayerDeathEvent e) {
        send("death", e.getEntity(), e.getDeathMessage());
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onAdvancement(PlayerAdvancementDoneEvent e) {
        Player p = e.getPlayer();
        if (Boolean.FALSE.equals(p.getWorld().getGameRuleValue(GameRule.ANNOUNCE_ADVANCEMENTS))) {
            return;
        }
        send("advancement", p, advancementText(e));
    }

    /**
     * Returns the chat text for an advancement, or null if it isn't announced
     * (recipes, hidden advancements). Paper and Spigot expose this through
     * different, incompatible APIs, so both are looked up reflectively.
     */
    private String advancementText(PlayerAdvancementDoneEvent e) {
        try {
            // Paper: message() is the exact announcement Component, null if
            // it won't be announced.
            Method message = e.getClass().getMethod("message");
            Object component = message.invoke(e);
            if (component == null) {
                return null;
            }
            Class<?> componentClass = Class.forName("net.kyori.adventure.text.Component");
            Class<?> plainClass = Class.forName(
                    "net.kyori.adventure.text.serializer.plain.PlainTextComponentSerializer");
            Object plain = plainClass.getMethod("plainText").invoke(null);
            return (String) plainClass.getMethod("serialize", componentClass).invoke(plain, component);
        } catch (NoSuchMethodException | ClassNotFoundException notPaper) {
            // Fall through to the Spigot API.
        } catch (ReflectiveOperationException | RuntimeException ex) {
            plugin.getLogger().log(Level.FINE, "Paper advancement lookup failed", ex);
        }

        try {
            // Spigot: Advancement#getDisplay() with shouldAnnounceChat(),
            // getTitle() and getType().
            Object display = e.getAdvancement().getClass().getMethod("getDisplay").invoke(e.getAdvancement());
            if (display == null) {
                return null;
            }
            Class<?> dc = display.getClass();
            if (!(Boolean) dc.getMethod("shouldAnnounceChat").invoke(display)) {
                return null;
            }
            String title = (String) dc.getMethod("getTitle").invoke(display);
            String type = String.valueOf(dc.getMethod("getType").invoke(display));
            String verb = switch (type) {
                case "GOAL" -> "has reached the goal";
                case "CHALLENGE" -> "has completed the challenge";
                default -> "has made the advancement";
            };
            return e.getPlayer().getName() + " " + verb + " [" + ChatColor.stripColor(title) + "]";
        } catch (ReflectiveOperationException | RuntimeException ex) {
            plugin.getLogger().log(Level.FINE, "Spigot advancement lookup failed", ex);
            return null;
        }
    }
}
