package acp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/agenthost/acp"
	"github.com/huanghantao/dsh-gateway/internal/dshplugin"
	"github.com/huanghantao/dsh-gateway/internal/harness"
)

// This is the live proof of the question path, and it is the test that would
// notice a DeepSeek Harness upgrade moving the seam underneath it.
//
// Nothing about the capability is covered by the ACP surface: the gateway mounts
// an answerer plugin of its own into the child (internal/dshplugin) and answers
// what it asks over loopback. So the only honest test stands up both halves —
// a real `dsh --profile acp` child with the generated overlay, and the gateway's
// side of the bridge — and asks a real model to ask a real question.
//
// It is opt-in for the same reason the prompt test is: it costs a model call.
func TestAskUserQuestionEndToEnd(t *testing.T) {
	if os.Getenv("DSH_GATEWAY_TEST_LLM") != "1" {
		t.Skip("set DSH_GATEWAY_TEST_LLM=1 to run a real model turn")
	}

	// The gateway's half of the bridge, reduced to what the contract is: it
	// receives a question and answers it. The answer quotes a label the model
	// itself offered, because the model matches its own words and a test that
	// invented one would prove nothing.
	asks := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		select {
		case asks <- body:
		default:
		}

		label := "yes"
		questions, _ := body["questions"].([]any)
		if len(questions) > 0 {
			question, _ := questions[0].(map[string]any)
			options, _ := question["options"].([]any)
			if len(options) > 0 {
				option, _ := options[0].(map[string]any)
				if text, ok := option["label"].(string); ok {
					label = text
				}
			}
		}
		answers := []map[string]any{{"id": questionID(body), "selected": []string{label}}}
		_ = json.NewEncoder(w).Encode(map[string]any{"outcome": "answered", "answers": answers})
	}))
	defer server.Close()

	// The child's half: the plugin the gateway installs, and the endpoint that
	// points it at the bridge above.
	installation, err := dshplugin.Install(t.TempDir())
	if err != nil {
		t.Fatalf("install the answerer: %v", err)
	}
	if err := installation.WriteEndpoint(dshplugin.Endpoint{
		URL:       server.URL,
		Token:     "test-token",
		TimeoutMS: 120_000,
	}); err != nil {
		t.Fatalf("write the endpoint: %v", err)
	}

	sink := &recorder{}
	a := startHarnessWith(t, sink, func(opts *acp.Options) {
		opts.Args = installation.Args(opts.Profile)
		opts.ExtraEnv = append(opts.ExtraEnv, installation.Env()...)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	session, err := a.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = a.CloseSession(context.Background(), session.Info.ID) }()

	// The turn blocks until the answer arrives, so it runs on its own goroutine
	// and the assertions happen while it waits.
	type settled struct {
		stop string
		err  error
	}
	done := make(chan settled, 1)
	go func() {
		stop, err := a.Prompt(ctx, session.Info.ID, []harness.PromptBlock{{
			Type: "text",
			Text: "Call the ask_user_question tool exactly once, with one question: " +
				"id=style, question=Which style?, options: terse / detailed. " +
				"Wait for the answer, then reply with exactly one short sentence naming it.",
		}})
		done <- settled{stop, err}
	}()

	var ask map[string]any
	select {
	case ask = <-asks:
	case <-time.After(4 * time.Minute):
		t.Fatal("the harness never asked a question through the installed answerer")
	}

	questions, _ := ask["questions"].([]any)
	if len(questions) != 1 {
		t.Fatalf("the answerer received %d questions, want 1: %v", len(questions), ask)
	}
	question, _ := questions[0].(map[string]any)
	if question["id"] != "style" || question["question"] != "Which style?" {
		t.Errorf("question = %v, want the model's own id and text", question)
	}
	if sessionID, _ := ask["sessionId"].(string); sessionID != session.Info.ID {
		t.Errorf("sessionId = %q, want the asking session %q: the card belongs to it",
			sessionID, session.Info.ID)
	}
	options, _ := question["options"].([]any)
	if len(options) != 2 {
		t.Fatalf("options = %v, want the two the model offered", options)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the turn failed after being answered: %v", got.err)
		}
		t.Logf("turn settled with stopReason=%q", got.stop)
	case <-time.After(2 * time.Minute):
		t.Fatal("the turn never settled after the question was answered")
	}

	// The answer has to arrive as *the model's own tool result*, which is the
	// only thing that closes the loop.
	var result string
	for _, update := range sink.snapshot() {
		if update.Kind == harness.UpdateTool && update.Tool != nil &&
			update.Tool.Title == "ask_user_question" && update.Tool.Output != "" {
			result = update.Tool.Output
		}
	}
	if result == "" {
		t.Fatal("no tool result for ask_user_question reached the ACP client")
	}
	if !strings.Contains(result, `"id":"style"`) || !strings.Contains(result, `"selected"`) {
		t.Errorf("the tool result is not the harness's answer shape: %s", result)
	}
	t.Logf("tool result: %s", result)
}

// questionID reads the id of the first question in a bridge request.
func questionID(body map[string]any) string {
	questions, _ := body["questions"].([]any)
	if len(questions) == 0 {
		return ""
	}
	question, _ := questions[0].(map[string]any)
	id, _ := question["id"].(string)
	return id
}
