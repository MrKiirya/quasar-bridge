package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	asr "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"

	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"quasarbridge/internal/journal"
)

func TestWakeWordReachesJournal(t *testing.T) {
	w := new(journal.Writer)
	defer w.Close()
	jc := journal.DefaultConfig()
	jc.Enabled = true
	jc.Path = filepath.Join(t.TempDir(), "turns.jsonl")
	if err := w.Configure(jc); err != nil {
		t.Fatal(err)
	}
	trace := newConversation(w)
	cfg := DefaultRoutingConfig()
	cfg.DefaultRoute = "yandex"
	cfg.Rules = append([]RoutingRule{{Name: "comrade", Route: "yandex", Match: RoutingMatch{WakeWordAny: []string{"товарищ"}}}}, cfg.Rules...)
	e := new(Engine)
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	up := make(chan []byte, 16)
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, e)
	n.WakeWord = func(context.Context, time.Time) string { return "товарищ" }
	n.OnRoute = routeHook(trace, nil)
	n.OnWakeWord = trace.wakeWord
	voice := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "voice", StreamId: new(int64(1))}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}}, Format: new("audio/opus")}}}}})
	_ = n.Client(ctx, websocket.MessageBinary, voice)
	<-up
	result := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "voice"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("как дела")}}}}}}})
	_ = n.Server(ctx, websocket.MessageBinary, result)
	<-up
	n.Close()
	data, _ := os.ReadFile(jc.Path)
	if !strings.Contains(string(data), `"event":"wake_word"`) {
		t.Fatalf("no wake_word event:\n%s", data)
	}
}

func TestProxyJournalsWakeWord(t *testing.T) {
	asrFrame := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "voice"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("как дела")}}}}}}})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		sent := false
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
			if !sent {
				sent = true
				_ = c.Write(r.Context(), websocket.MessageBinary, asrFrame)
			}
		}
	}))
	defer upstream.Close()
	w := new(journal.Writer)
	defer w.Close()
	jc := journal.DefaultConfig()
	jc.Enabled = true
	jc.Path = filepath.Join(t.TempDir(), "turns.jsonl")
	if err := w.Configure(jc); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultRoutingConfig()
	cfg.DefaultRoute = "yandex"
	cfg.Rules = append([]RoutingRule{{Name: "comrade", Route: "yandex", Match: RoutingMatch{WakeWordAny: []string{"товарищ"}}}}, cfg.Rules...)
	e := new(Engine)
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Upstream: "ws" + strings.TrimPrefix(upstream.URL, "http") + "/uni.ws", Journal: w}
	p.NewSession = func(ctx context.Context, up, down Sender) Session {
		n := NewNative(ctx, up, down, e)
		n.WakeWord = func(context.Context, time.Time) string { return "товарищ" }
		return n
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/uni.ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	voice := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "voice", StreamId: new(int64(1))}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}}, Format: new("audio/opus")}}}}})
	if err := c.Write(ctx, websocket.MessageBinary, voice); err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		data, _ := os.ReadFile(jc.Path)
		if strings.Contains(string(data), `"event":"route"`) {
			if !strings.Contains(string(data), `"event":"wake_word"`) {
				t.Fatalf("no wake_word event:\n%s", data)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no route")
}
