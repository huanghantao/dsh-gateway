package dshplugin_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/huanghantao/dsh-gateway/internal/dshplugin"
)

/* ------------------------------------------------------------------ patch */

// row is the shape the profile loader reads, which is why the generated patch is
// parsed rather than string-matched: a patch that is merely *plausible* would
// fail at the child's next boot, in a process the gateway cannot debug.
type patchFile []struct {
	Insert []struct {
		ID       string `yaml:"id"`
		Name     string `yaml:"name"`
		Disabled any    `yaml:"disabled"`
	} `yaml:"insert"`
}

func install(t *testing.T) dshplugin.Installation {
	t.Helper()
	installation, err := dshplugin.Install(t.TempDir())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return installation
}

func TestInstallMountsTheToolAndTheAnswerer(t *testing.T) {
	installation := install(t)

	raw, err := os.ReadFile(installation.PatchPath)
	if err != nil {
		t.Fatalf("read patch: %v", err)
	}
	var patch patchFile
	if err := yaml.Unmarshal(raw, &patch); err != nil {
		t.Fatalf("the generated patch is not valid YAML: %v\n%s", err, raw)
	}
	if len(patch) != 1 || len(patch[0].Insert) != 2 {
		t.Fatalf("patch = %s, want one insert of two rows", raw)
	}

	tool := patch[0].Insert[0]
	if tool.ID != dshplugin.ToolRowID || tool.Name != "@deepseek-ai/dsh-tool-ask-user" {
		t.Fatalf("first row = %+v, want the harness's question tool", tool)
	}
	answerer := patch[0].Insert[1]
	if answerer.ID != dshplugin.RowID {
		t.Fatalf("second row id = %q, want %q", answerer.ID, dshplugin.RowID)
	}
	if answerer.Name != "file://"+installation.PluginPath {
		t.Fatalf("second row name = %q, want the installed plugin", answerer.Name)
	}

	// Neither row may carry a `disabled` key: a generated patch that can be
	// partly applied is worse than one that fails outright, because the failure
	// would be a model with a question tool and no answerer.
	for _, row := range patch[0].Insert {
		if row.Disabled != nil {
			t.Fatalf("row %q carries disabled: %v", row.ID, row.Disabled)
		}
	}
}

func TestPatchQuotesAPathThatNeedsIt(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state dir")
	installation, err := dshplugin.Install(stateDir)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	raw, err := os.ReadFile(installation.PatchPath)
	if err != nil {
		t.Fatalf("read patch: %v", err)
	}
	var patch patchFile
	if err := yaml.Unmarshal(raw, &patch); err != nil {
		t.Fatalf("a state directory with a space produced invalid YAML: %v\n%s", err, raw)
	}
	// The URL is percent-encoded so that the loader resolves it, and the YAML is
	// quoted so that a space cannot end the value.
	name := patch[0].Insert[1].Name
	if !strings.Contains(name, "%20") {
		t.Fatalf("name = %q, want a percent-encoded file URL", name)
	}
	if !strings.HasPrefix(name, "file://") {
		t.Fatalf("name = %q, want a file URL", name)
	}
}

func TestInstallRewritesNothingWhenNothingChanged(t *testing.T) {
	dir := t.TempDir()
	first, err := dshplugin.Install(dir)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	before, err := os.Stat(first.PluginPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	second, err := dshplugin.Install(dir)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if second.PluginPath != first.PluginPath || second.PatchPath != first.PatchPath {
		t.Fatal("two installs into one state directory disagreed about where the files go")
	}
	after, err := os.Stat(second.PluginPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged plugin was rewritten, which makes its timestamp useless as a version marker")
	}
}

/* --------------------------------------------------------------- endpoint */

func TestWriteEndpointIsReadableByThePluginAndNobodyElse(t *testing.T) {
	installation := install(t)
	want := dshplugin.Endpoint{
		URL:       "http://127.0.0.1:8787/internal/questions",
		Token:     strings.Repeat("ab", 32),
		TimeoutMS: 600_000,
	}
	if err := installation.WriteEndpoint(want); err != nil {
		t.Fatalf("write endpoint: %v", err)
	}

	info, err := os.Stat(installation.EndpointPath)
	if err != nil {
		t.Fatalf("stat endpoint: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("endpoint mode = %o, want 600: it holds a live credential", mode)
	}

	raw, err := os.ReadFile(installation.EndpointPath)
	if err != nil {
		t.Fatalf("read endpoint: %v", err)
	}
	var got dshplugin.Endpoint
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the endpoint document is not valid JSON: %v\n%s", err, raw)
	}
	if got.Version != dshplugin.EndpointVersion {
		t.Fatalf("version = %d, want %d", got.Version, dshplugin.EndpointVersion)
	}
	if got.URL != want.URL || got.Token != want.Token || got.TimeoutMS != want.TimeoutMS {
		t.Fatalf("endpoint round-tripped as %+v, want %+v", got, want)
	}
}

/* ----------------------------------------------------------- child wiring */

func TestArgsAndEnvPointAtTheInstallation(t *testing.T) {
	installation := install(t)

	args := strings.Join(installation.Args("acp"), " ")
	if args != "--profile acp --patch "+installation.PatchPath {
		t.Fatalf("args = %q", args)
	}

	env := installation.Env()
	if len(env) != 1 {
		t.Fatalf("env = %v, want exactly the endpoint path", env)
	}
	if env[0] != dshplugin.EnvEndpoint+"="+installation.EndpointPath {
		t.Fatalf("env = %q", env[0])
	}
}

/* ------------------------------------------------------------- the plugin */

func TestThePluginNeverSerialisesTheAgentContext(t *testing.T) {
	installation := install(t)
	source, err := os.ReadFile(installation.PluginPath)
	if err != nil {
		t.Fatalf("read plugin: %v", err)
	}
	text := string(source)

	// The request carries the calling agent as a live Cordis context. Reading a
	// documented property off it is fine; treating it as data is not, and it
	// fails at the one moment that matters — a tool call already waiting on a
	// human. This assertion is the regression guard for a bug that was found by
	// running exactly that: JSON.stringify(request) threw "cannot get property
	// toJSON without inject" and the question never reached the phone.
	for _, forbidden := range []string{"JSON.stringify(request", "JSON.stringify({ request", "structuredClone(request"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("the plugin serialises the request, which carries a live agent context: %s", forbidden)
		}
	}

	// It must register the answerer on the harness's waterfall, and it must
	// reach it through ctx.on rather than by importing DSH internals: the event
	// name is the seam, and a rename there degrades to "no answerer" instead of
	// failing the child's boot.
	if !strings.Contains(text, `ctx.on("user-questions/request"`) {
		t.Fatal("the plugin does not register an answerer on user-questions/request")
	}
	// It may import Node's own builtins, which are always resolvable, and nothing
	// else: a bare specifier would need the profile's module resolution to serve
	// it, and a failure there is a failure of the whole child's boot.
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "import ") {
			continue
		}
		if !strings.Contains(trimmed, `from "node:`) {
			t.Fatalf("the plugin imports a non-builtin module: %s", trimmed)
		}
	}
}
