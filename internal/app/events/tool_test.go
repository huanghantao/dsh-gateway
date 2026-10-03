package events

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// TestNewToolDataBoundsBothPayloads: one function shapes both producers' frames,
// so this is the single place the byte budgets are pinned.
func TestNewToolDataBoundsBothPayloads(t *testing.T) {
	limits := config.Limits{ToolInputBytes: 512, ToolOutputBytes: 256}
	data := NewToolData(ToolFacts{
		Phase:  ToolEnded,
		CallID: "c1",
		Tool:   "write",
		Status: "completed",
		Input:  `{"file_path":"/tmp/big.go","content":` + jsonString(strings.Repeat("x", 4096)) + `}`,
		Output: strings.Repeat("line\n", 400),
		Facts:  toolresult.Outcome{ExitCode: 1, HasExitCode: true}.Facts(),
	}, limits)

	if !data.InputTruncated || !data.OutputTruncated {
		t.Fatalf("trim = %+v, want both payloads reported as bounded", data.Trim)
	}
	if len(data.Input) > 512+128 {
		t.Errorf("input is %d bytes, want it bounded near 512", len(data.Input))
	}
	if len(data.Output) > 256+128 {
		t.Errorf("output is %d bytes, want it bounded near 256", len(data.Output))
	}

	// The arguments have to stay parseable: the client reads them to say what the
	// call did.
	var args map[string]any
	if err := json.Unmarshal([]byte(data.Input), &args); err != nil {
		t.Fatalf("bounded input is not JSON: %v (%q)", err, data.Input)
	}
	if args["file_path"] != "/tmp/big.go" {
		t.Errorf("file_path = %v, want the path preserved", args["file_path"])
	}
	if data.ExitCode == nil || *data.ExitCode != 1 {
		t.Errorf("exitCode = %v, want 1", data.ExitCode)
	}
}

// TestNewToolDataLeavesWhatFitsAlone: the common case must not pay for the rare
// one — an ordinary call's payload is byte-identical to what the harness said.
func TestNewToolDataLeavesWhatFitsAlone(t *testing.T) {
	limits := config.Default().Limits
	input := `{"command":"pwd","description":"Print the working directory"}`
	output := "/Users/me/code/api\n"
	data := NewToolData(ToolFacts{
		Phase: ToolEnded, CallID: "c1", Tool: "bash", Status: "completed",
		Input: input, Output: output,
	}, limits)

	if data.Input != input || data.Output != output {
		t.Errorf("payload changed: input %q output %q", data.Input, data.Output)
	}
	if data.InputTruncated || data.OutputTruncated {
		t.Errorf("trim = %+v, want nothing reported as bounded", data.Trim)
	}
}

// TestToolDataOmitsWhatIsNotKnown: a start frame has no outcome, and a frame must
// not carry "exitCode": 0 for a call that never said what its exit status was.
func TestToolDataOmitsWhatIsNotKnown(t *testing.T) {
	data := NewToolData(ToolFacts{
		Phase: ToolStarted, CallID: "c1", Tool: "bash", Status: "in_progress",
		Input: `{"command":"pwd"}`,
	}, config.Default().Limits)

	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"exitCode", "errorCode", "notices", "spillPath", "output"} {
		if strings.Contains(string(encoded), `"`+absent+`"`) {
			t.Errorf("frame carries %q although the call never said: %s", absent, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"phase":"start"`) {
		t.Errorf("frame lost its phase: %s", encoded)
	}
}

func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
