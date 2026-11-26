package connector

import (
	"context"
	"testing"
)

func TestServerIDFromURL(t *testing.T) {
	tests := map[string]string{
		// Docker hostnames keep the container name, so existing rooms are reused.
		"http://minecraft-mc-1:25580":            "minecraft-mc-1",
		"http://minecraft-s-mcs-1:25580/":        "minecraft-s-mcs-1",
		"http://minecraft-mc-1":                  "minecraft-mc-1",
		"http://100.101.102.103:25581":           "100.101.102.103-25581",
		"https://mc.example.com":                 "mc.example.com",
		"https://mc.example.com/bridge/Survival": "mc.example.com-bridge-survival",
		"http://MyServer.ts.net:25580":           "myserver.ts.net",
	}
	for raw, want := range tests {
		u, err := parseServerURL(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := serverIDFromURL(u); got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
}

func TestParseServerURL(t *testing.T) {
	for _, bad := range []string{
		"minecraft-mc-1:25580",
		"ftp://minecraft-mc-1",
		"http://",
		"http://user:pass@minecraft-mc-1",
		"http://minecraft-mc-1?x=1",
	} {
		if _, err := parseServerURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	u, err := parseServerURL(" https://mc.example.com/bridge/ ")
	if err != nil || u.String() != "https://mc.example.com/bridge" {
		t.Fatalf("got %v %v", u, err)
	}
}

func TestValidateServerURL(t *testing.T) {
	ctx := context.Background()
	allowed := []string{
		"http://minecraft-mc-1:25580",
		"http://localhost:25580",
		"http://127.0.0.1:25580",
		"http://192.168.1.10:25580",
		"http://10.0.0.5",
		"http://100.101.102.103:25580",
		"http://[fd7a:115c:a1e0::1]:25580",
		"http://myserver.tail1234.ts.net:25580",
		"http://mc.home.arpa",
		"https://mc.example.com",
		"https://8.8.8.8",
	}
	for _, raw := range allowed {
		if _, err := validateServerURL(ctx, raw); err != nil {
			t.Errorf("%s should be allowed: %v", raw, err)
		}
	}
	refused := []string{
		"http://8.8.8.8:25580",
		"http://[2001:4860:4860::8888]:25580",
	}
	for _, raw := range refused {
		if _, err := validateServerURL(ctx, raw); err == nil {
			t.Errorf("%s should be refused (plain http to a public address)", raw)
		}
	}
}
