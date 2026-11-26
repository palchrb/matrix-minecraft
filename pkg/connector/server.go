package connector

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ServerConfig is one Minecraft server added with "login add-server". It is
// stored in the admin UserLogin's metadata.
type ServerConfig struct {
	// ID is the portal ID, derived from the URL. For a Docker hostname like
	// http://minecraft-mc-1:25580 it equals the container name, which is
	// what portals were keyed by before servers were configured from
	// Matrix, so existing rooms are reused.
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token"`
}

// defaultPluginPort is left out of server IDs so http://host:25580 and
// http://host map to the same ID.
const defaultPluginPort = "25580"

var serverIDCleaner = regexp.MustCompile(`[^a-z0-9._-]+`)

// serverIDFromURL derives a stable portal ID from the plugin URL: the host,
// plus the port if it isn't the default and the path if there is one, so
// several servers behind one host or reverse proxy get distinct IDs.
func serverIDFromURL(u *url.URL) string {
	id := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != defaultPluginPort {
		id += "-" + port
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		id += "-" + strings.ReplaceAll(strings.ToLower(p), "/", "-")
	}
	return strings.Trim(serverIDCleaner.ReplaceAllString(id, "-"), "-")
}

// parseServerURL checks the shape of a plugin URL and normalises it (no
// trailing slash, query or fragment).
func parseServerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL must start with http:// or https://")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("URL has no host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("URL must not contain credentials, a query or a fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// validateServerURL parses the URL and refuses plain HTTP to anything that
// isn't on a private network, since the token would cross the internet in
// clear text.
func validateServerURL(ctx context.Context, raw string) (*url.URL, error) {
	u, err := parseServerURL(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "http" && !isPrivateHost(ctx, u.Hostname()) {
		return nil, fmt.Errorf("refusing plain http:// to public host %s: the token would be sent "+
			"unencrypted. Use https:// through a reverse proxy, or a private address "+
			"(Docker network, LAN, Tailscale)", u.Hostname())
	}
	return u, nil
}

var tailscaleCGNAT = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || tailscaleCGNAT.Contains(ip)
}

// isPrivateHost reports whether a host is reachable only on a private
// network: private/loopback/Tailscale IPs, single-label names (Docker
// service and container names), well-known private suffixes, or names that
// only resolve to private addresses.
func isPrivateHost(ctx context.Context, host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".ts.net", ".local", ".lan", ".internal", ".home.arpa", ".localhost"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !isPrivateIP(a.IP) {
			return false
		}
	}
	return true
}
