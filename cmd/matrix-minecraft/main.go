package main

import (
	"github.com/palchrb/matrix-minecraft/pkg/connector"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
)

// These are set at build time via ldflags in build.sh.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	m := mxmain.BridgeMain{
		Name:        "matrix-minecraft",
		Description: "A Matrix bridge for Minecraft server chat",
		URL:         "https://github.com/palchrb/matrix-minecraft",
		Version:     "0.1.0",
		Connector:   &connector.MCConnector{},
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
