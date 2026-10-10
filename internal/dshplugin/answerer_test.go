package dshplugin_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/dshplugin"
)

// probeResult is what testdata/answerer_probe.mjs reports.
type probeResult struct {
	// Posted is one entry per request the plugin made to the "gateway".
	Posted []struct {
		URL  string `json:"url"`
		Body struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionId"`
			Questions []struct {
				ID          string `json:"id"`
				Header      string `json:"header"`
				Question    string `json:"question"`
				Detail      string `json:"detail"`
				MultiSelect bool   `json:"multiSelect"`
				Options     []struct {
					Label       string `json:"label"`
					Description string `json:"description"`
					Recommended bool   `json:"recommended"`
				} `json:"options"`
			} `json:"questions"`
		} `json:"body"`
	} `json:"posted"`
	Declined   bool            `json:"declined"`
	Result     json.RawMessage `json:"result"`
	Thrown     string          `json:"thrown"`
	ThrownName string          `json:"thrownName"`
	ThrownCode string          `json:"thrownCode"`
	Error      string          `json:"error"`
}

// runProbe executes the plugin against a request shaped like the harness's own.
//
// The plugin is the only part of this capability that is not Go, and the seam's
// field names are not the model's: the tool renames `multi_select` to
// `multiSelect` before dispatching, which a first version of this plugin got
// wrong in a way nothing but a live run would have caught. So it is run here.
func runProbe(t *testing.T, request map[string]any, answers []map[string]any) probeResult {
	return runProbeScripted(t, request, map[string]any{"answers": answers})
}

// runProbeScripted runs the plugin against a stub gateway whose replies a test
// controls: `answers` for a single reply, `responses` for a sequence.
func runProbeScripted(t *testing.T, request map[string]any, reply map[string]any) probeResult {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the answerer is a JavaScript plugin")
	}

	installation := install(t)
	if err := installation.WriteEndpoint(dshplugin.Endpoint{
		URL:       "http://127.0.0.1:8799/internal/questions",
		Token:     "test-token",
		TimeoutMS: 600_000,
	}); err != nil {
		t.Fatalf("write endpoint: %v", err)
	}
	// The stub gateway answers with whatever the endpoint document carries, so a
	// test controls the reply without a second file format.
	endpoint, err := os.ReadFile(installation.EndpointPath)
	if err != nil {
		t.Fatalf("read endpoint: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(endpoint, &document); err != nil {
		t.Fatalf("decode endpoint: %v", err)
	}
	for key, value := range reply {
		document[key] = value
	}
	patched, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode endpoint: %v", err)
	}
	if err := os.WriteFile(installation.EndpointPath, patched, 0o600); err != nil {
		t.Fatalf("rewrite endpoint: %v", err)
	}

	requestPath := filepath.Join(t.TempDir(), "request.json")
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	if err := os.WriteFile(requestPath, encoded, 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, //nolint:gosec // the path comes from LookPath
		"testdata/answerer_probe.mjs", installation.PluginPath, installation.EndpointPath, requestPath)
	cmd.Dir = "." // the package directory, so the relative testdata path resolves
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}

	var result probeResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("the probe did not report JSON: %v\n%s", err, out)
	}
	if result.Error != "" {
		t.Fatalf("probe reported: %s", result.Error)
	}
	return result
}

// askRequest is the request the seam dispatches, in the seam's spelling.
func askRequest() map[string]any {
	return map[string]any{
		"agentId": "50c7f06a-a732-493e-9d7e-21d64345d4ce",
		"questions": []map[string]any{
			{
				"id":       "style",
				"header":   "回答风格",
				"question": "你希望我平时回答的风格是？",
				"detail":   "这是细则。",
				"options": []map[string]any{
					{"label": "简洁直接，结论先行 (Recommended)", "description": "先给结论。"},
					{"label": "详细解释，带推导过程", "description": "把思路讲清楚。"},
				},
			},
			{
				// The tool renames the model's `multi_select` into this before the
				// seam sees it, which is the case that used to be dropped.
				"id":          "extras",
				"question":    "还要什么？",
				"multiSelect": true,
				"options":     []map[string]any{{"label": "代码"}, {"label": "命令"}},
			},
		},
	}
}

func TestThePluginAsksTheGatewayForTheHarnessShape(t *testing.T) {
	result := runProbe(t, askRequest(), []map[string]any{
		{"id": "style", "selected": []string{"详细解释，带推导过程"}},
		{"id": "extras", "selected": []string{"代码", "命令"}},
	})

	if len(result.Posted) != 1 {
		t.Fatalf("the plugin posted %d times, want once: %+v", len(result.Posted), result.Posted)
	}
	post := result.Posted[0]
	if !strings.HasSuffix(post.URL, "/internal/questions") {
		t.Errorf("posted to %q, want the bridge route", post.URL)
	}
	if post.Body.ID == "" {
		t.Error("the request carries no id; a retry could not be recognised as the same question")
	}
	if post.Body.SessionID != "50c7f06a-a732-493e-9d7e-21d64345d4ce" {
		t.Errorf("sessionId = %q, want the calling agent's id", post.Body.SessionID)
	}
	if len(post.Body.Questions) != 2 {
		t.Fatalf("posted %d questions, want 2", len(post.Body.Questions))
	}

	first := post.Body.Questions[0]
	if first.ID != "style" || first.Header != "回答风格" || first.Detail != "这是细则。" {
		t.Errorf("question 1 = %+v, want the harness's fields carried through", first)
	}
	if len(first.Options) != 2 || !first.Options[0].Recommended {
		t.Errorf("options = %+v, want the recommendation derived", first.Options)
	}
	// The label keeps its suffix: the model matches the label it wrote, so
	// stripping it would produce an answer the model cannot reconcile.
	if first.Options[0].Label != "简洁直接，结论先行 (Recommended)" {
		t.Errorf("label = %q, want it unchanged", first.Options[0].Label)
	}
	if first.Options[0].Description != "先给结论。" {
		t.Errorf("description = %q", first.Options[0].Description)
	}

	second := post.Body.Questions[1]
	if !second.MultiSelect {
		t.Fatal("multiSelect was dropped: the seam spells it multiSelect, and a question the " +
			"operator may answer with two labels would arrive as a single-choice one")
	}

	// The answer goes back to the harness in its own shape, with the empty
	// fields omitted rather than sent as nulls.
	if result.Declined {
		t.Fatal("the plugin declined a request it answered")
	}
	if result.Thrown != "" {
		t.Fatalf("the plugin threw: %s", result.Thrown)
	}
	var answer struct {
		Answers []struct {
			ID       string   `json:"id"`
			Selected []string `json:"selected"`
			Custom   *string  `json:"custom"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(result.Result, &answer); err != nil {
		t.Fatalf("the returned answer is not the harness's shape: %v (%s)", err, result.Result)
	}
	if len(answer.Answers) != 2 || answer.Answers[0].Selected[0] != "详细解释，带推导过程" {
		t.Fatalf("answers = %s", result.Result)
	}
	if answer.Answers[0].Custom != nil {
		t.Error("an unused custom answer was sent as a value rather than omitted")
	}
}

// The redeploy case, which is the whole reason the plugin mints its own request
// id: a gateway that goes away mid-question must cost a retry, not the question.
func TestThePluginAsksAgainWhenTheGatewayLetsTheQuestionGo(t *testing.T) {
	result := runProbeScripted(t, map[string]any{
		"agentId":   "session-1",
		"questions": []map[string]any{{"id": "color", "question": "红色还是蓝色?"}},
	}, map[string]any{
		"responses": []map[string]any{
			// A gateway shutting down, seen from the answerer's side.
			{"outcome": "unanswered", "reason": "cancelled"},
			// Its successor, with the human's answer.
			{"outcome": "answered", "answers": []map[string]any{{"id": "color", "selected": []string{"蓝色"}}}},
		},
	})

	if len(result.Posted) != 2 {
		t.Fatalf("the plugin posted %d times, want a retry: %+v", len(result.Posted), result.Posted)
	}
	// Same id both times: the successor has to recognise the question as the one
	// already on screen rather than parking a second card for it.
	if result.Posted[0].Body.ID != result.Posted[1].Body.ID {
		t.Errorf("the retry used a different id (%q then %q)", result.Posted[0].Body.ID, result.Posted[1].Body.ID)
	}
	if result.Thrown != "" || result.Declined {
		t.Fatalf("the plugin gave up on a question the successor answered: %s", result.Thrown)
	}
	if !strings.Contains(string(result.Result), "蓝色") {
		t.Fatalf("result = %s, want the answer from the second attempt", result.Result)
	}
}

// A stopped turn must read as a stopped turn. Declining instead would be
// indistinguishable from having no answerer at all, and the model would be told
// its question found nobody to answer it when the operator had in fact pressed
// stop.
func TestThePluginReportsAStoppedTurnAsCancelled(t *testing.T) {
	result := runProbeScripted(t, map[string]any{
		"agentId":   "session-1",
		"aborted":   true,
		"questions": []map[string]any{{"id": "color", "question": "红色还是蓝色?"}},
	}, map[string]any{"answers": []map[string]any{{"id": "color", "selected": []string{"蓝色"}}}})

	if len(result.Posted) != 0 {
		t.Errorf("a cancelled turn still asked the gateway %d times", len(result.Posted))
	}
	if result.Declined {
		t.Fatal("the plugin declined, which the harness reports as a missing answerer")
	}
	if result.ThrownCode != "ASK_ABORTED" {
		t.Fatalf("code = %q, want ASK_ABORTED (thrown: %s)", result.ThrownCode, result.Thrown)
	}
	if result.ThrownName != "UserQuestionError" {
		t.Errorf("name = %q, want the harness's own error class", result.ThrownName)
	}
}

// A question nobody answered is final: re-asking it would ask the human to decide
// something the model has already been told is unanswered.
func TestThePluginGivesUpOnATimeout(t *testing.T) {
	result := runProbeScripted(t, map[string]any{
		"agentId":   "session-1",
		"questions": []map[string]any{{"id": "color", "question": "红色还是蓝色?"}},
	}, map[string]any{
		"responses": []map[string]any{{"outcome": "unanswered", "reason": "timeout"}},
	})

	if len(result.Posted) != 1 {
		t.Fatalf("the plugin posted %d times, want once", len(result.Posted))
	}
	if result.Thrown == "" {
		t.Fatal("the plugin did not report an expired question to the model")
	}
	if !strings.Contains(result.Thrown, "did not answer in time") {
		t.Errorf("thrown = %q, want it to say the user did not answer", result.Thrown)
	}
}

func TestThePluginDeclinesWithoutAnEndpoint(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed; the answerer is a JavaScript plugin")
	}
	installation := install(t)
	requestPath := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(requestPath, []byte(`{"questions":[{"id":"a","question":"?"}]}`), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// No endpoint file at all: the gateway has never published one, which is what
	// a switched-off capability looks like from inside the child.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "testdata/answerer_probe.mjs", //nolint:gosec // fixed argv
		installation.PluginPath, filepath.Join(t.TempDir(), "missing.json"), requestPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	var result probeResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("probe did not report JSON: %v\n%s", err, out)
	}
	if !result.Declined {
		t.Fatalf("the plugin did not decline an unconfigured bridge: %s", out)
	}
	if len(result.Posted) != 0 {
		t.Error("the plugin posted to a gateway it was never told about")
	}
}
