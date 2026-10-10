// Package dshplugin installs the DeepSeek Harness plugin that lets the agent ask
// the operator a question, and owns the contract between that plugin and the
// gateway.
//
// Why a plugin is needed at all: DeepSeek Harness exposes the model's question
// tool (`ask_user_question`) through a Cordis seam, `ctx.userQuestions`, whose
// answerers are contributed by whoever composes the profile. The `acp` profile
// the gateway drives mounts neither the tool nor an answerer — and ACP itself has
// no way to ask a question, only to request permission — so on that profile the
// model cannot ask anything. The gateway therefore supplies the missing answerer
// as a plugin of its own and mounts it with a `--patch` overlay it generates. The
// alternative, an MCP server of the gateway's own, would work but would present a
// different tool to the model than the one the desktop offers; see docs/adr/0009.
//
// Everything here is deliberately boring: two generated files under the state
// directory, one environment variable, and one HTTP contract. The interesting
// half is internal/app/questions, which is where a question becomes a card on a
// phone; the half here is only the wire into the harness.
//
// The generated files are rewritten whenever their content changes, which makes
// upgrading the gateway a matter of restarting it: the running copy of the plugin
// cannot drift from the binary that wrote it.
package dshplugin

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/huanghantao/dsh-gateway/internal/atomicfile"
)

// answererSource is the plugin itself. It is embedded rather than installed from
// a path beside the binary so that a gateway built as a single artifact stays a
// single artifact: `go install`-style deployments have no directory of extras to
// keep in step.
//
//go:embed answerer.mjs
var answererSource []byte

// The names Install writes under the state directory. They are not configurable:
// a path an operator can move is a path the child and the gateway can disagree
// about, and nothing here is meant to be hand-edited.
const (
	// DirName is the directory, inside the state directory, that holds them.
	DirName = "dsh"
	// pluginFile is the answerer plugin, mounted by the generated patch.
	pluginFile = "ask-answerer.mjs"
	// patchFile is the overlay that mounts it.
	patchFile = "ask-answerer.patch.yml"
	// endpointFile is what the plugin reads to find the gateway. It is the only
	// one of the three that holds a secret, and the only one rewritten per run.
	endpointFile = "ask-answerer.endpoint.json"
)

// EnvEndpoint names the environment variable that tells the plugin where the
// endpoint file is.
//
// The file path travels through the environment because that is the one channel
// the gateway already owns for the child it causes to be started, and because a
// path — unlike a URL or a token — does not go stale across a gateway restart.
const EnvEndpoint = "DSH_GATEWAY_QUESTIONS_ENDPOINT_FILE"

// The rows the generated patch contributes.
const (
	// ToolRowID mounts the harness's own question tool. The id matches the one
	// the shipped Web presets use, so that a future profile which mounts the tool
	// itself collides visibly rather than registering a second copy silently.
	ToolRowID = "tool-ask-user"
	// toolPackage is that tool.
	toolPackage = "@deepseek-ai/dsh-tool-ask-user"
	// RowID is this gateway's plugin row. It is also the plugin's own `name`
	// export, which is the convention the shipped bundles follow.
	RowID = "dsh-gateway-questions"
)

// EndpointVersion is the shape of the endpoint document. The plugin tolerates a
// newer document by ignoring what it does not know, so a change here is only
// needed when a field changes meaning.
const EndpointVersion = 1

// Installation is what Install produced: the files the child needs, and the
// arguments and environment that point at them.
type Installation struct {
	// Dir is the directory holding the generated files.
	Dir string
	// PluginPath is the answerer plugin.
	PluginPath string
	// PatchPath is the overlay that mounts it.
	PatchPath string
	// EndpointPath is the file WriteEndpoint writes and the plugin reads.
	EndpointPath string
}

// Endpoint is what the answerer reads to find the gateway: where to ask, what to
// authenticate with, and how long the gateway will hold a question open.
//
// It is a file rather than an environment variable because the token changes
// every time the gateway starts and the child is built to outlive exactly that
// event. The plugin re-reads it per question, so a redeploy mid-question costs a
// retry rather than an outage.
type Endpoint struct {
	Version int `json:"version"`
	// URL is the loopback address of the question bridge.
	URL string `json:"url"`
	// Token is a per-process bearer credential. It is 256 bits of randomness and
	// exists only in this file, which is written 0600.
	Token string `json:"token"`
	// TimeoutMS is how long the gateway will hold a question open, so that the
	// plugin can wait slightly longer than the authority on the subject.
	TimeoutMS int64 `json:"timeoutMs"`
}

// Child describes how the harness child must be started for the question
// capability to work.
//
// It exists because two processes have to agree about that and only one of them
// starts the child: the agent host spawns it, and the gateway answers what it
// asks. Deriving both from one function of the same configuration is what stops
// them disagreeing — a gateway that answers nothing because the host mounted
// nothing is a bug that looks exactly like a plugin that failed to load.
type Child struct {
	// Enabled reports whether the answerer is mounted at all.
	Enabled bool
	// Installation is where the generated files live, and is the zero value when
	// the capability is off.
	Installation Installation
	// Args is the child's complete command line.
	Args []string
	// Env is appended to the child's environment.
	Env []string
}

// Start prepares the child's command line and environment.
//
// A disabled capability still returns the profile argument: the two processes
// must produce the same command line either way, and "no arguments" would mean
// "the adapter's default" — a third answer to a question with two.
func Start(stateDir, profile string, enabled bool) (Child, error) {
	if !enabled {
		// The paths are still reported: the gateway removes the endpoint file
		// when the capability is off, and a child started before the switch was
		// flipped would otherwise keep reading a credential nothing accepts.
		return Child{Args: []string{"--profile", profile}, Installation: paths(stateDir)}, nil
	}
	installation, err := Install(stateDir)
	if err != nil {
		return Child{}, err
	}
	return Child{
		Enabled:      true,
		Installation: installation,
		Args:         installation.Args(profile),
		Env:          installation.Env(),
	}, nil
}

// Install writes the plugin and the patch overlay into stateDir, and reports the
// paths the child will need.
//
// Both files are rewritten only when their content differs, so a restart is not
// a stream of pointless fsyncs — and, more usefully, an operator can tell from
// the file's timestamp whether a gateway upgrade actually changed the plugin.
func Install(stateDir string) (Installation, error) {
	installation := paths(stateDir)
	if err := os.MkdirAll(installation.Dir, 0o700); err != nil {
		return Installation{}, fmt.Errorf("dshplugin: create %s: %w", installation.Dir, err)
	}

	if err := writeIfChanged(installation.PluginPath, answererSource); err != nil {
		return Installation{}, err
	}
	if err := writeIfChanged(installation.PatchPath, patchDocument(installation.PluginPath)); err != nil {
		return Installation{}, err
	}
	return installation, nil
}

// paths reports where the generated files live, without touching the filesystem.
func paths(stateDir string) Installation {
	dir := filepath.Join(stateDir, DirName)
	return Installation{
		Dir:          dir,
		PluginPath:   filepath.Join(dir, pluginFile),
		PatchPath:    filepath.Join(dir, patchFile),
		EndpointPath: filepath.Join(dir, endpointFile),
	}
}

// Args returns the dsh arguments that compose the named profile together with
// this installation's overlay.
func (i Installation) Args(profile string) []string {
	return []string{"--profile", profile, "--patch", i.PatchPath}
}

// Env returns the child environment that points the plugin at its endpoint.
func (i Installation) Env() []string {
	return []string{EnvEndpoint + "=" + i.EndpointPath}
}

// WriteEndpoint publishes where the plugin should ask, and with what.
//
// The file holds a live credential, so it is written 0600 and atomically: a
// plugin that reads a half-written document would think the bridge was gone and
// decline a question the operator was perfectly able to answer.
func (i Installation) WriteEndpoint(ep Endpoint) error {
	if ep.Version == 0 {
		ep.Version = EndpointVersion
	}
	document, err := jsonBytes(ep)
	if err != nil {
		return err
	}
	if err := atomicfile.WriteFile(i.EndpointPath, document, 0o600); err != nil {
		return fmt.Errorf("dshplugin: write endpoint: %w", err)
	}
	return nil
}

// patchDocument renders the overlay that mounts the tool and the answerer.
//
// The header is for the operator who finds this file while working out why their
// agent is not asking questions: it says where the file came from and what to
// edit instead.
func patchDocument(pluginPath string) []byte {
	var b strings.Builder
	b.WriteString("# Generated by dsh-gateway. Do not edit: this file is rewritten whenever\n")
	b.WriteString("# the gateway starts with different content.\n")
	b.WriteString("#\n")
	b.WriteString("# It mounts two rows into the DSH profile the gateway drives:\n")
	b.WriteString("#   " + ToolRowID + " — the harness's model-facing ask_user_question tool, which the\n")
	b.WriteString("#     `acp` profile does not compose on its own.\n")
	b.WriteString("#   " + RowID + " — this gateway's answerer, which forwards each question to the\n")
	b.WriteString("#     phone and returns the answer to the waiting tool call.\n")
	b.WriteString("#\n")
	b.WriteString("# Turning the capability off is a gateway setting (`dsh.questions.enabled`),\n")
	b.WriteString("# not an edit here: an edited copy is overwritten at the next start.\n")
	b.WriteString("- insert:\n")
	b.WriteString("    - id: " + ToolRowID + "\n")
	b.WriteString("      name: " + yamlString(toolPackage) + "\n")
	b.WriteString("    - id: " + RowID + "\n")
	b.WriteString("      name: " + yamlString(fileURL(pluginPath)) + "\n")
	return []byte(b.String())
}

// fileURL renders an absolute path as the URL the profile loader resolves.
//
// A percent-encoded file URL rather than a bare path, because the state
// directory is an operator's choice and may contain a space or a non-ASCII
// character; the loader accepts either form, and this one is unambiguous.
func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// yamlString quotes a value for YAML, using the single-quoted style in which the
// only escape is a doubled quote.
func yamlString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// jsonBytes renders a document with the trailing newline that makes a file
// pleasant to read, diff and append to.
func jsonBytes(value any) ([]byte, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("dshplugin: encode endpoint: %w", err)
	}
	return append(encoded, '\n'), nil
}

// writeIfChanged writes data to path unless the file already holds exactly it.
func writeIfChanged(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) { //nolint:gosec // a generated file in the gateway's own state directory
		return nil
	}
	if err := atomicfile.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("dshplugin: write %s: %w", path, err)
	}
	return nil
}
