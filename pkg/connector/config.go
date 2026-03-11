package connector

import (
	_ "embed"

	"go.mau.fi/util/configupgrade"
)

//go:embed example-config.yaml
var ExampleConfig string

type Config struct {
	// Docker settings
	DockerHost  string `yaml:"docker_host"`  // unix:///var/run/docker.sock
	LabelPrefix string `yaml:"label_prefix"` // mc-bridge

	// RCON default settings
	DefaultRCONPort int `yaml:"default_rcon_port"` // 25575

	// Message formatting in Minecraft (tellraw colors)
	PrefixText   string `yaml:"prefix_text"`   // [Matrix]
	PrefixColor  string `yaml:"prefix_color"`  // light_purple
	SenderColor  string `yaml:"sender_color"`  // aqua
	MessageColor string `yaml:"message_color"` // white

	// Provisioning — also read from PROVISIONING_SECRET env variable
	ProvisioningSecret string `yaml:"provisioning_secret"`

	// Avatar API URL (%s is replaced with MC username)
	AvatarAPIURL string `yaml:"avatar_api_url"`

	// Bridge all events (join/leave/death/advancement) in addition to chat.
	// Set to false to only bridge chat messages.
	BridgeAllEvents bool `yaml:"bridge_all_events"`
}

func upgradeConfig(helper configupgrade.Helper) {
	helper.Copy(configupgrade.Str, "docker_host")
	helper.Copy(configupgrade.Str, "label_prefix")
	helper.Copy(configupgrade.Int, "default_rcon_port")
	helper.Copy(configupgrade.Str, "prefix_text")
	helper.Copy(configupgrade.Str, "prefix_color")
	helper.Copy(configupgrade.Str, "sender_color")
	helper.Copy(configupgrade.Str, "message_color")
	helper.Copy(configupgrade.Str, "provisioning_secret")
	helper.Copy(configupgrade.Str, "avatar_api_url")
	helper.Copy(configupgrade.Bool, "bridge_all_events")
}

func (mc *MCConnector) GetConfig() (string, any, configupgrade.Upgrader) {
	return ExampleConfig, &mc.Config, configupgrade.SimpleUpgrader(upgradeConfig)
}
