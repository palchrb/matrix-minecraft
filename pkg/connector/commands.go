package connector

import (
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2/commands"
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
