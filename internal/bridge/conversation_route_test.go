package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	stat "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/request_stat"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"

	"quasarbridge/internal/wire"
)

func TestDecideKeepsConversationAfterRules(t *testing.T) {
	cfg := DefaultRoutingConfig()
	cfg.Rules = []RoutingRule{{Name: "stop", Route: "yandex", Match: RoutingMatch{StartsWithAny: []string{"стоп"}}}}
	cfg.DefaultRoute = "yandex"
	e := &Engine{}
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text, route, source string
		conversation        bool
	}{
		{"расскажи про марс", "yandex", "default", false},
		{"расскажи про марс", "llm", "conversation", true},
		{"стоп", "yandex", "rule", true},
	} {
		ctx := t.Context()
		if tc.conversation {
			ctx = WithConversation(ctx)
		}
		if got := e.Decide(ctx, tc.text); got.Route != tc.route || got.Source != tc.source {
			t.Errorf("%q conversation=%v: %+v", tc.text, tc.conversation, got)
		}
	}
}

// requestStat reports how the answer to a request played, as the speaker does when the request ends.
func requestStat(id string, finished, interrupted bool) []byte {
	ts := &stat.TRequestStat_TTimestamps{OnSoundPlayerBeginTime: new("120")}
	if finished {
		ts.OnSoundPlayerEndTime = new("4300")
	}
	if interrupted {
		ts.OnInterruptionPhraseSpottedTime = new("2100")
	}
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "stat-" + id}, Event: &client.TClientMessage_TEvent_RequestStat{RequestStat: &stat.TRequestStat{RequestMessageId: id, Timestamps: ts}}}}})
}

func voiceRequest(id string) []byte {
	input := wire.Decode(soundInput(id), wire.ClientToServer).Client
	e := input.GetEvent()
	e.Event = &client.TClientMessage_TEvent_VoiceInput{VoiceInput: e.GetTextInput()}
	return aa(input)
}

func TestNativeKeepsConversationWhenSpeakerListensAfterLLMAnswer(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		keep, listen          bool
		firstRoute            string
		finished, interrupted bool
		text                  bool
		late                  bool
		want                  bool
	}{
		{name: "listen after answer", keep: true, listen: true, firstRoute: "llm", finished: true, want: true},
		{name: "disabled", listen: true, firstRoute: "llm", finished: true},
		{name: "no listen", keep: true, firstRoute: "llm", finished: true},
		{name: "yandex answer", keep: true, listen: true, firstRoute: "yandex", finished: true},
		{name: "interrupted", keep: true, listen: true, firstRoute: "llm", finished: true, interrupted: true},
		{name: "not played", keep: true, listen: true, firstRoute: "llm"},
		{name: "wake word later", keep: true, listen: true, firstRoute: "llm", finished: true, late: true},
		{name: "text", keep: true, listen: true, firstRoute: "llm", finished: true, text: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan bool, 2)
			calls := 0
			n, up, _ := soundSession(t, resolverFunc(func(ctx context.Context, _ string) (Resolution, error) {
				calls++
				got <- Conversation(ctx)
				if calls == 1 {
					return Resolution{Route: tc.firstRoute, Answer: "Ответ."}, nil
				}
				return Resolution{Route: "yandex"}, nil
			}))
			n.KeepConversation, n.ShouldListen = tc.keep, tc.listen
			send := func(b []byte) {
				t.Helper()
				if err := n.Client(t.Context(), websocket.MessageBinary, b); err != nil {
					t.Fatal(err)
				}
			}
			send(voiceRequest("first"))
			if <-got {
				t.Fatal("first request continues nothing")
			}
			// The answer is complete once it is sent to the cloud or synthesized.
			for {
				f := nextSoundFrame(t, up).Client.GetEvent()
				if f.GetTtsGenerate() != nil || f.GetContinueRequest() != nil {
					break
				}
			}
			send(requestStat("first", tc.finished, tc.interrupted))
			if tc.late {
				n.mu.Lock()
				n.played.at = n.played.at.Add(-2 * followGap)
				n.mu.Unlock()
			}
			if tc.text {
				send(soundInput("second"))
			} else {
				send(voiceRequest("second"))
			}
			select {
			case c := <-got:
				if c != tc.want {
					t.Fatalf("conversation=%v, want %v", c, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("second request not routed")
			}
		})
	}
}
