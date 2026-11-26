package connector

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var helpSectionServers = commands.HelpSection{Name: "Minecraft servers", Order: 15}

var cmdListServers = &commands.FullHandler{
	Func: fnListServers,
	Name: "list-servers",
	Help: commands.HelpMeta{
		Section:     helpSectionServers,
		Description: "List connected Minecraft servers and their status",
	},
	RequiresAdmin: true,
}

var cmdRemoveServer = &commands.FullHandler{
	Func: fnRemoveServer,
	Name: "remove-server",
	Help: commands.HelpMeta{
		Section:     helpSectionServers,
		Description: "Disconnect a Minecraft server (its room is kept)",
		Args:        "<_server ID_>",
	},
	RequiresAdmin: true,
}

var cmdSetAvatar = &commands.FullHandler{
	Func: fnSetAvatar,
	Name: "set-avatar",
	Help: commands.HelpMeta{
		Section: helpSectionServers,
		Description: "Set a server room's avatar: reply to an image with this command, " +
			"or give an mxc:// URI. `none` removes it. The server ID can be left out " +
			"in the server's own room.",
		Args: "[_server ID_] [_mxc://…_ | none]",
	},
	RequiresAdmin: true,
}

// maxAvatarBytes bounds the image downloaded from a replied-to message.
const maxAvatarBytes = 10 * 1024 * 1024

func fnSetAvatar(ce *commands.Event) {
	_, client := adminClient(ce.Ctx, ce.User)
	if client == nil {
		ce.Reply("Not logged in.")
		return
	}
	usage := "Usage: reply to an image with `$cmdprefix set-avatar <server ID>`, or " +
		"`$cmdprefix set-avatar <server ID> <mxc://…|none>`. See `$cmdprefix list-servers` for IDs."

	// The server ID is the first argument, or the room the command was sent in.
	args := ce.Args
	var serverID string
	if len(args) > 0 && !strings.HasPrefix(args[0], "mxc://") && args[0] != "none" {
		serverID, args = args[0], args[1:]
	} else if ce.Portal != nil {
		serverID = string(ce.Portal.ID)
	}
	if serverID == "" || len(args) > 1 {
		ce.Reply(usage)
		return
	}

	var mxc id.ContentURIString
	switch {
	case len(args) == 1 && args[0] == "none":
		// Remove.
	case len(args) == 1:
		uri, err := id.ParseContentURI(args[0])
		if err != nil || uri.IsEmpty() {
			ce.Reply("`%s` is not a valid mxc:// URI.", args[0])
			return
		}
		mxc = uri.CUString()
	case ce.ReplyTo != "":
		var err error
		if mxc, err = avatarFromReply(ce); err != nil {
			ce.Reply("Couldn't use that message as an avatar: %v", err)
			return
		}
	default:
		ce.Reply(usage)
		return
	}

	found, err := client.SetServerAvatar(ce.Ctx, serverID, mxc)
	switch {
	case !found:
		ce.Reply("No server with ID `%s`. See `$cmdprefix list-servers`.", serverID)
	case err != nil:
		ce.Reply("Failed to set avatar: %v", err)
	case mxc == "":
		ce.Reply("Removed the avatar of `%s`.", serverID)
	default:
		ce.Reply("Avatar of `%s` updated.", serverID)
	}
}

// avatarFromReply returns a plain (unencrypted) MXC URI for the image in the
// message the command replies to. Images in encrypted rooms are downloaded,
// decrypted and uploaded again, since a room avatar can't be encrypted.
func avatarFromReply(ce *commands.Event) (id.ContentURIString, error) {
	evt, err := ce.Bot.GetEvent(ce.Ctx, ce.OrigRoomID, ce.ReplyTo)
	if err != nil {
		return "", fmt.Errorf("failed to fetch it: %w", err)
	}
	if evt.Content.Parsed == nil {
		_ = evt.Content.ParseRaw(evt.Type)
	}
	msg := evt.Content.AsMessage()
	if evt.Type != event.EventSticker && msg.MsgType != event.MsgImage {
		return "", fmt.Errorf("it is not an image")
	}
	if msg.File == nil {
		if msg.URL == "" {
			return "", fmt.Errorf("it has no image")
		}
		return msg.URL, nil
	}
	data, err := ce.Bot.DownloadMedia(ce.Ctx, msg.File.URL, msg.File)
	if err != nil {
		return "", fmt.Errorf("failed to download it: %w", err)
	}
	if len(data) > maxAvatarBytes {
		return "", fmt.Errorf("the image is larger than %d MB", maxAvatarBytes/1024/1024)
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("it is not an image (%s)", mime)
	}
	// An empty room ID uploads it unencrypted.
	uri, _, err := ce.Bot.UploadMedia(ce.Ctx, "", data, "avatar", mime)
	if err != nil {
		return "", fmt.Errorf("failed to upload it: %w", err)
	}
	return uri, nil
}

func fnListServers(ce *commands.Event) {
	_, client := adminClient(ce.Ctx, ce.User)
	if client == nil {
		ce.Reply("Not logged in. Start with `$cmdprefix login %s`.", flowProvisioningSecret)
		return
	}
	servers := client.ListServers()
	if len(servers) == 0 {
		ce.Reply("No servers yet. Add one with `$cmdprefix login %s`.", flowAddServer)
		return
	}
	var b strings.Builder
	for _, s := range servers {
		fmt.Fprintf(&b, "* `%s` — %s — %s — %s\n", s.Config.ID, s.Config.Name, s.Config.URL, describeStatus(s))
	}
	ce.Reply(b.String())
}

func describeStatus(s ServerStatus) string {
	if !s.Active {
		return "⏸ not running"
	}
	var state string
	if s.Stream.Connected {
		state = "✅ connected"
	} else if s.Stream.LastError != "" {
		state = "❌ " + s.Stream.LastError
	} else {
		state = "⏳ connecting"
	}
	if !s.Stream.LastEvent.IsZero() {
		state += fmt.Sprintf(", last event %s ago", time.Since(s.Stream.LastEvent).Round(time.Second))
	}
	return state
}

func fnRemoveServer(ce *commands.Event) {
	if len(ce.Args) != 1 {
		ce.Reply("Usage: `$cmdprefix remove-server <server ID>` (see `$cmdprefix list-servers`)")
		return
	}
	_, client := adminClient(ce.Ctx, ce.User)
	if client == nil {
		ce.Reply("Not logged in.")
		return
	}
	removed, err := client.RemoveServer(ce.Ctx, ce.Args[0])
	if err != nil {
		ce.Reply("Failed to remove server: %v", err)
	} else if !removed {
		ce.Reply("No server with ID `%s`. See `$cmdprefix list-servers`.", ce.Args[0])
	} else {
		ce.Reply("Removed `%s`. Its room is kept; run `$cmdprefix delete-portal` in the room to remove it too.", ce.Args[0])
	}
}
