# matrix-minecraft
Made a stupid and overengineered bridge between minecraft and matrix... It started out unelegantly tailing the log of an itzg docker run mc server and sending replies via rcon; these days it talks to a small companion plugin instead. Would probably have been smarter just to use one of the existing bridges in the form of paper plugins, but i just wanted to have e2ee so then i made this one insted. Stupid and pointless? Yes..If you want to talk about it, feel free to reach out at [#whatever:vibb.me](https://matrix.to/#/%23whatever:vibb.me)

A Matrix-Minecraft bridge built on [mautrix-go](https://github.com/mautrix/go) bridgev2. A small companion plugin on each Minecraft server streams chat and game events (joins, deaths, advancements) to the bridge and shows Matrix messages in game. Servers are added from Matrix; the bridge needs no Docker socket and no RCON.

## Features

- **Bidirectional chat** – Matrix messages show up in game, in-game chat shows up in Matrix
- **Game events** – Bridges join/leave, death messages, and advancements (configurable)
- **Set up from Matrix** – Add and remove servers by chatting with the bridge bot; one room per server, grouped in a Space
- **One authenticated endpoint per server** – The plugin only reads game events and displays chat; it can't run commands, so a leaked token can't take over your server the way an RCON password can
- **Runs anywhere** – Servers can be on the same Docker network, your LAN, Tailscale, or behind an HTTPS reverse proxy on the internet
- **Robust connection** – Automatic reconnect with replay of events missed while disconnected
- **Player avatars** – Renders each ghost user's avatar from the player's skin (Mojang for Java, GeyserMC for Bedrock/Floodgate)
- **Relay mode** – Lets everyone in the room chat through the bridge
- **End-to-bridge encryption** – Supports encryption between Matrix clients and the bridge
- **Floodgate support** – Handles Bedrock player names (prefixed with `.`)

## Requirements

- A Matrix homeserver (Synapse, Conduit, etc.) with appservice support
- Minecraft server(s) running Spigot, Paper or a Paper fork, with the companion plugin

## Quick Start

### 1. Create the directory structure

```bash
mkdir -p /srv/docker/minecraft/data
cd /srv/docker/minecraft
```

### 2. Create `docker-compose.yaml`

```yaml
services:
  matrix-minecraft:
    image: ghcr.io/palchrb/matrix-minecraft:latest
    container_name: matrix-minecraft
    restart: unless-stopped
    volumes:
      - ./data:/data
    environment:
      - UID=1337
      - GID=1337
      - PROVISIONING_SECRET=your-secret-here
```

### 3. Generate config and registration files

```bash
# First run – generates config.yaml
docker compose up matrix-minecraft
# Edit data/config.yaml (see Configuration below)

# Second run – generates registration.yaml
docker compose up matrix-minecraft
```

The startup script handles this automatically:
1. If `config.yaml` doesn't exist, it generates a default one and exits.
2. If `registration.yaml` doesn't exist, it generates one and exits.
3. On subsequent runs, it starts the bridge normally.

### 4. Register the appservice with your homeserver

Copy `data/registration.yaml` to your homeserver's appservice directory and add it to your homeserver config.

**Synapse** (`homeserver.yaml`):
```yaml
app_service_config_files:
  - /path/to/minecraft-registration.yaml
```

Restart your homeserver after adding the registration.

See the [mautrix documentation](https://docs.mau.fi/bridges/general/registering-appservices.html) for detailed instructions for your homeserver.

### 5. Configure the bridge

Edit `data/config.yaml`. The most important sections:

#### Homeserver settings

```yaml
homeserver:
    address: https://matrix.example.com
    domain: example.com
```

#### Appservice settings

```yaml
appservice:
    address: http://matrix-minecraft:29333
    hostname: 0.0.0.0
    port: 29333
    id: minecraft
    bot:
        username: minecraftbot
        displayname: Minecraft bridge
        avatar: mxc://example.com/your-bot-avatar
```

The `bot.avatar` is also used as the bridge space icon.

#### Bridge permissions

```yaml
bridge:
    permissions:
        "*": relay
        "example.com": user
        "@yourusername:example.com": admin
```

- `relay` – Can receive messages through the relay bot (see Relay Mode below)
- `user` – Can use the bridge
- `admin` – Can log in, add and remove servers

#### Relay mode (important!)

Enable relay mode so all Matrix users in the room can chat, not just the admin:

```yaml
bridge:
    relay:
        enabled: true
        default_relays:
            - "@yourusername:example.com"
```

The bridge adds the sender's display name in game automatically, so configure `message_formats` to only include the message body (to avoid double names):

```yaml
bridge:
    relay:
        message_formats:
            m.text: "{{ .Message }}"
            m.notice: "{{ .Message }}"
            m.emote: "* {{ .Message }}"
            m.file: "sent a file"
            m.image: "sent an image"
            m.audio: "sent audio"
            m.video: "sent a video"
            m.location: "sent a location"
```

To bridge bot messages (`m.notice` type), also set:
```yaml
bridge:
    bridge_notices: true
```

Relays can also be changed per room with the built-in `set-relay` / `unset-relay` commands.

### 6. Install the plugin on your Minecraft servers

With `itzg/minecraft-server`, add the plugin URL to `PLUGINS` and the image downloads it on startup:

```yaml
services:
  mc:
    image: itzg/minecraft-server
    container_name: minecraft-mc-1
    environment:
      EULA: "TRUE"
      TYPE: PAPER
      PLUGINS: |
        https://github.com/palchrb/matrix-minecraft/releases/download/plugin-latest/matrix-minecraft-bridge.jar
```

Otherwise, drop the jar from the [plugin-latest release](https://github.com/palchrb/matrix-minecraft/releases/tag/plugin-latest) into `plugins/`.

On first start the plugin generates a random token and listens on port `25580`. Show the token in the server console:

```
matrixbridge token
```

or from the host with `docker exec <container> rcon-cli matrixbridge token`. For security the command refuses to show the token to players in game, ops included.

Optional settings, on the Minecraft container or in `plugins/MatrixMinecraftBridge/config.yml`:

| Env var | Default | Description |
|---------|---------|-------------|
| `MC_BRIDGE_PORT` | `25580` | Port the plugin listens on |
| `MC_BRIDGE_TOKEN` | generated | Token the bridge must present (overrides the generated one) |

### 7. Make the plugin reachable from the bridge

The plugin speaks plain HTTP. Pick whichever fits:

- **Same Docker host:** put the bridge and the Minecraft containers on the same Docker network and use `http://<container-name>:25580`. Don't publish the port.
- **LAN or Tailscale:** `http://<ip-or-name>:25580`.
- **Over the internet:** put an HTTPS reverse proxy (Caddy, Traefik, nginx) in front of the port and use `https://...`. Turn off response buffering for the `/events` stream if your proxy buffers by default.

The bridge refuses plain `http://` to public addresses, since the token would be sent unencrypted. It's allowed for loopback, private and Tailscale IPs, single-label names like Docker container names, and `.ts.net`/`.local`/`.lan`/`.internal`/`.home.arpa` names.

### 8. Start the bridge

```bash
docker compose up -d matrix-minecraft
```

### 9. Set it up from Matrix

Start a chat with the bridge bot (`@minecraftbot:example.com`). Only bridge admins (see permissions) can do this.

1. **Once:** `login provisioning-secret`, then enter the provisioning secret.
2. **Per server:** `login add-server`, then enter the plugin URL, the token and a name for the room when the bot asks for them.

The bridge checks the URL and token right away, creates a room for the server in the Minecraft space and invites you. Invite whoever else should be in it; with relay mode on, their messages are bridged too. Token messages are redacted from the chat automatically.

Running `login add-server` again with the same URL updates the token or room name.

Other commands:

| Command | Description |
|---------|-------------|
| `list-servers` | Servers with their connection status and time of the last event |
| `remove-server <id>` | Disconnect a server (its room is kept; `delete-portal` in the room removes it) |

#### Upgrading from Docker discovery

Earlier versions discovered servers through Docker labels and used RCON. After upgrading:

1. Add the plugin to each server (step 6) and restart it.
2. Run `login add-server` for each server with `http://<container-name>:25580`. The server ID is derived from the URL host, so it matches the old container-based ID and your existing rooms are reused.
3. Remove the `/var/run/docker.sock` mount from the bridge and the `mc-bridge.*` labels from the Minecraft containers. RCON can stay enabled for itzg's own tools; the bridge no longer uses it.

## Network Connector Config

These settings go under the `network:` section in `config.yaml`:

```yaml
network:
    # How Matrix messages look in game (named Minecraft colors)
    prefix_text: "[Matrix]"
    prefix_color: light_purple
    sender_color: aqua
    message_color: white

    # Provisioning secret (prefer PROVISIONING_SECRET env var)
    provisioning_secret: ""

    # Empty = render avatars from player skins (Mojang / GeyserMC).
    # Or a render service URL (%s = MC username).
    avatar_api_url: ""

    # Bridge all events (join/leave/death/advancement) or only chat
    bridge_all_events: true
```

## How It Works

1. **Servers** – Added with `login add-server` and stored in the bridge database. The server ID (and portal room) is derived from the plugin URL.
2. **Events** – For each server, the bridge subscribes to the plugin's Server-Sent Events stream (`/events`), reconnecting automatically and resuming with `Last-Event-ID` so events missed during a short disconnect are replayed.
3. **Chat to Minecraft** – Matrix messages are posted to the plugin's `/send` endpoint, which shows them with `tellraw`. The text is placed in a component the plugin builds itself, so it can't inject formatting, click events or commands. If the plugin can't show it, the message is marked as not delivered in Matrix.
4. **Avatars** – When a Minecraft player first sends a message, their skin is looked up via Mojang (Java) or GeyserMC (Bedrock), the face is rendered and set as their ghost user's avatar (re-checked hourly, re-uploaded only when the skin changes)

## Building from Source

```bash
git clone https://github.com/palchrb/matrix-minecraft.git
cd matrix-minecraft
./build.sh
```

Build the plugin (needs Java 17+ and Maven):

```bash
mvn -f plugin/pom.xml package   # → plugin/target/matrix-minecraft-bridge.jar
```

Or build the Docker image:

```bash
docker build -t matrix-minecraft .
```

## Networking

The bridge needs to reach your Matrix homeserver and each Minecraft server's plugin port, and the homeserver needs to reach the bridge's appservice port. If your homeserver and Minecraft servers are on the same Docker host, put them on shared Docker networks:

```yaml
services:
  matrix-minecraft:
    # ...
    networks:
      - minecraft
      - matrix  # If your homeserver is also in Docker

networks:
  minecraft:
    external: true
  matrix:
    external: true
```

## Troubleshooting

- **`list-servers` shows ❌** – The status shows the last error. "rejected the token": compare with `/matrixbridge token` and run `login add-server` again. "connection refused" / timeouts: the bridge can't reach the plugin port (network, firewall, or the server is down)
- **"couldn't reach the plugin" when adding a server** – Check that the server log says `[MatrixMinecraftBridge] Bridge endpoint listening on ...` and that the URL works from inside the bridge container
- **"refusing plain http://"** – Use `https://` through a reverse proxy, or a private address (see step 7)
- **Matrix messages marked as not delivered** – The plugin couldn't be reached or couldn't show the message; check `list-servers` and the bridge log
- **Avatar fetch errors** – Avatars are rendered from the player's skin via Mojang (Java) or GeyserMC (Bedrock); offline-mode names without a matching account get the default avatar. A Bedrock player's skin shows up once GeyserMC has seen them on a Geyser server
- **Double usernames in chat** – Configure relay `message_formats` to use only `{{ .Message }}` (see Relay Mode above)

## License

See [LICENSE](LICENSE).
