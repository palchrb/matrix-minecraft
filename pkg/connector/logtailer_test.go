package connector

import "testing"

func TestParseLine(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want *ChatLine
	}{
		{
			name: "paper system chat join",
			raw:  "[10:33:33 INFO]: System chat: .palchrb joined the game",
			want: &ChatLine{PlayerName: ".palchrb", Message: "joined the game", Event: EventJoin},
		},
		{
			name: "paper system chat leave",
			raw:  "[10:34:00 INFO]: System chat: .palchrb left the game",
			want: &ChatLine{PlayerName: ".palchrb", Message: "left the game", Event: EventLeave},
		},
		{
			name: "jline prompt redraw with system chat",
			raw:  ">....\r\x1b[K[10:33:33 INFO]: System chat: Sverre joined the game",
			want: &ChatLine{PlayerName: "Sverre", Message: "joined the game", Event: EventJoin},
		},
		{
			name: "vanilla join",
			raw:  "[16:31:38] [Server thread/INFO]: Sverre joined the game",
			want: &ChatLine{PlayerName: "Sverre", Message: "joined the game", Event: EventJoin},
		},
		{
			name: "paper chat not secure",
			raw:  ">....\r\x1b[K[10:15:40 INFO]: [Not Secure] <.palchrb> hei",
			want: &ChatLine{PlayerName: ".palchrb", Message: "hei", Event: EventChat},
		},
		{
			name: "vanilla chat",
			raw:  "[16:31:38] [Server thread/INFO]: <Sverre> hello there",
			want: &ChatLine{PlayerName: "Sverre", Message: "hello there", Event: EventChat},
		},
		{
			name: "system chat death",
			raw:  "[10:40:00 INFO]: System chat: Sverre was slain by Zombie",
			want: &ChatLine{PlayerName: "Sverre", Message: "was slain by Zombie", Event: EventDeath},
		},
		{
			name: "system chat advancement",
			raw:  "[10:40:00 INFO]: System chat: Sverre has made the advancement [Stone Age]",
			want: &ChatLine{PlayerName: "Sverre", Message: "has made the advancement [Stone Age]", Event: EventAdvancement},
		},
		{
			name: "command is ignored",
			raw:  ">....\r\x1b[K[10:15:40 INFO]: Sverre issued server command: /home",
		},
		{
			name: "floodgate login is ignored",
			raw:  "[10:33:33 INFO]: [floodgate] Floodgate player logged in as .palchrb joined (UUID: 00000000-0000-0000-0009-01f775469bda)",
		},
		{
			name: "lost connection is ignored",
			raw:  "[10:34:00 INFO]: .palchrb lost connection: Disconnected",
		},
		{
			name: "no log prefix is ignored",
			raw:  "Sverre joined the game",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := parseLine(tt.raw)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("expected no match, got %+v", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %+v, got no match", *tt.want)
			}
			if got.PlayerName != tt.want.PlayerName || got.Message != tt.want.Message || got.Event != tt.want.Event {
				t.Fatalf("got %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

func TestLooksLikeEvent(t *testing.T) {
	if !looksLikeEvent("[10:33:33 INFO]: Something new: .palchrb joined the game") {
		t.Fatal("expected join line to look like an event")
	}
	if looksLikeEvent("[10:15:40 INFO]: Sverre issued server command: /home") {
		t.Fatal("expected command line not to look like an event")
	}
}
