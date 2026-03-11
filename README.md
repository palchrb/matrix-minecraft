# matrix-minecraft

A Matrix-Minecraft bridge built on [mautrix-go](https://github.com/mautrix/go) bridgev2. It automatically discovers Minecraft server containers via Docker labels, bridges chat messages bidirectionally, and shows in-game events (joins, deaths, advancements) in your Matrix room.

## Features

- **Bidirectional chat** – Messages flow between Matrix and Minecraft via RCON (`tellraw`)
- **Auto-discovery** – Finds Minecraft containers by Docker labels, no manual per-server config needed
- **Game events** – Bridges join/leave, death messages, and advancements (configurable)
- **Player avatars** – Fetches Minecraft skins from [Starlight Skins API](https://starlightskins.lunareclipse.studio/) for ghost users
- **Portal avatars** – Set a custom room avatar per server via Docker label
- **Docker event watcher** – Automatically connects/disconnects when containers start/stop
- **Relay mode** – Lets non-logged-in Matrix users chat through the bridge
- **End-to-bridge encryption** – Supports encryption between Matrix clients and the bridge
- **Floodgate support** – Handles Bedrock player names (prefixed with `.`)

## Requirements

- A Matrix homeserver (Synapse, Conduit, etc.) with appservice support
- Docker (the bridge communicates with Minecraft servers via the Docker API)
- Minecraft server(s) running in Docker with RCON enabled

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
      - /var/run/docker.sock:/var/run/docker.sock
    environment:
      - UID=1337
      - GID=1337
      - PROVISIONING_SECRET=your-secret-here
```

> The Docker socket must be mounted so the bridge can discover and tail logs from Minecraft containers.

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
- `admin` – Can log in and provision the bridge

#### Relay mode (important!)

Enable relay mode so all Matrix users in the room can chat, not just the admin:

```yaml
bridge:
    relay:
        enabled: true
        default_relays:
            - "@yourusername:example.com"
```

The bridge adds the sender's display name to the `tellraw` command automatically, so configure `message_formats` to only include the message body (to avoid double names):

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

### 6. Label your Minecraft containers

The bridge discovers Minecraft servers by Docker labels. Add these to your Minecraft server's `docker-compose.yaml`:

```yaml
services:
  mc:
    image: itzg/minecraft-server
    container_name: minecraft-mc-1
    labels:
      mc-bridge.enable: "true"                              # Required – enables discovery
      mc-bridge.name: "Minecraft Server vibb.me"            # Optional – display name in Matrix
      mc-bridge.avatar: "mxc://example.com/room-avatar"     # Optional – room avatar (mxc:// URI)
      mc-bridge.rcon-port: "25575"                           # Optional – defaults to 25575
    environment:
      EULA: "TRUE"
      RCON_ENABLE: "true"
      RCON_PASSWORD: "your-rcon-password"
```

The label prefix (`mc-bridge`) is configurable via `label_prefix` in the network config.

| Label | Required | Description |
|-------|----------|-------------|
| `mc-bridge.enable` | Yes | Must be `"true"` to enable discovery |
| `mc-bridge.name` | No | Display name for the Matrix room (defaults to container name) |
| `mc-bridge.avatar` | No | `mxc://` URI for the room avatar |
| `mc-bridge.rcon-port` | No | RCON port if non-standard (default: 25575) |

The bridge reads `RCON_PASSWORD` from the container's environment variables automatically. Containers without `RCON_PASSWORD` set are skipped.

### 7. Start the bridge

```bash
docker compose up -d matrix-minecraft
```

### 8. Log in via Matrix

Send a message to the bridge bot (`@minecraftbot:example.com`) or use the `login` flow in your Matrix client (Element, etc.):

1. Start a chat with the bridge bot
2. Select "Provisioning Secret" login flow
3. Enter the provisioning secret you configured

The bridge will automatically discover labeled Minecraft containers and create Matrix rooms for each one.

## Network Connector Config

These settings go under the `network:` section in `config.yaml`:

```yaml
network:
    # Docker socket path
    docker_host: unix:///var/run/docker.sock

    # Label prefix on Minecraft containers
    label_prefix: mc-bridge

    # Default RCON port
    default_rcon_port: 25575

    # Tellraw message formatting colors
    prefix_text: "[Matrix]"
    prefix_color: light_purple
    sender_color: aqua
    message_color: white

    # Provisioning secret (prefer PROVISIONING_SECRET env var)
    provisioning_secret: ""

    # Avatar API URL (%s = MC username)
    avatar_api_url: "https://starlightskins.lunareclipse.studio/render/default/%s/bust"

    # Bridge all events (join/leave/death/advancement) or only chat
    bridge_all_events: true
```

## How It Works

1. **Discovery** – On login (and on container start events), the bridge scans Docker for containers with `mc-bridge.enable=true`
2. **Log tailing** – For each server, it attaches to the container's stdout via the Docker API and parses Minecraft log lines with regex
3. **RCON** – Matrix-to-Minecraft messages are sent via RCON `tellraw` commands with formatted JSON
4. **Avatars** – When a Minecraft player first sends a message, their skin is fetched from the Starlight Skins API and set as their ghost user's avatar (cached with ETag, refreshed hourly)
5. **Auto-reconnect** – Docker events (start/stop/die) trigger automatic provisioning or disconnection

## Building from Source

```bash
git clone https://github.com/palchrb/matrix-minecraft.git
cd matrix-minecraft
./build.sh
```

Or build the Docker image:

```bash
docker build -t matrix-minecraft .
```

## Networking

The bridge container needs to:
- Reach your Matrix homeserver (for appservice API)
- Access the Docker socket (`/var/run/docker.sock`)
- Reach Minecraft containers on RCON port (they share the Docker network)

If your homeserver is on the same Docker host, put them on the same Docker network:

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

Make sure your Minecraft server containers are on the same network so the bridge can reach them by container name (used as hostname for RCON).

## Troubleshooting

- **"No RCON_PASSWORD in container env"** – Set `RCON_PASSWORD` as an environment variable on your Minecraft container
- **RCON connection failures** – Ensure RCON is enabled (`RCON_ENABLE=true`) and the bridge can reach the container on the Docker network
- **No containers found** – Check that `mc-bridge.enable=true` label is set and the container is running
- **Avatar fetch errors** – The Starlight Skins API requires valid Minecraft usernames; check `avatar_api_url` in config
- **Double usernames in chat** – Configure relay `message_formats` to use only `{{ .Message }}` (see Relay Mode above)

## License

See [LICENSE](LICENSE).
