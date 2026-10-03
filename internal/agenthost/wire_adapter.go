// This file is the anticorruption layer between the harness port and the wire.
//
// The two vocabularies are deliberately separate types. `harness` is the
// gateway's driven port: it is written in Go, it carries `time.Time` and
// `[]byte`, and it may grow a field whenever the application needs one. The wire
// types are a contract between two processes that can be upgraded independently,
// so they must not change just because a Go struct did. Translating explicitly,
// in one file, is what keeps a refactor on one side from silently becoming a
// protocol change.
package agenthost

import (
	"context"
	"encoding/json"
	"os"

	"github.com/huanghantao/dsh-gateway/internal/agenthost/hostwire"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// toWireUpdate renders one harness update for the wire.
func toWireUpdate(u harness.Update) hostwire.Update {
	out := hostwire.Update{
		SessionID: u.SessionID,
		Kind:      string(u.Kind),
		Time:      u.Time,
		Text:      u.Text,
		MessageID: u.MessageID,
		Model:     u.Model,
	}
	if u.Tool != nil {
		out.Tool = toWireToolCall(*u.Tool)
	}
	if u.Usage != nil {
		out.Usage = &hostwire.Usage{Used: u.Usage.Used, Size: u.Usage.Size}
	}
	for _, o := range u.Config {
		out.Config = append(out.Config, toWireConfigOption(o))
	}
	return out
}

// fromWireUpdate renders one wire update back into the harness port's shape.
func fromWireUpdate(u hostwire.Update) harness.Update {
	out := harness.Update{
		SessionID: u.SessionID,
		Kind:      harness.UpdateKind(u.Kind),
		Time:      u.Time,
		Text:      u.Text,
		MessageID: u.MessageID,
		Model:     u.Model,
	}
	if u.Tool != nil {
		out.Tool = fromWireToolCall(*u.Tool)
	}
	if u.Usage != nil {
		out.Usage = &harness.Usage{Used: u.Usage.Used, Size: u.Usage.Size}
	}
	for _, o := range u.Config {
		out.Config = append(out.Config, fromWireConfigOption(o))
	}
	return out
}

func toWireToolCall(t harness.ToolCall) *hostwire.ToolCall {
	out := &hostwire.ToolCall{
		ID:      t.ID,
		Title:   t.Title,
		Status:  string(t.Status),
		Input:   t.Input,
		Output:  t.Output,
		IsError: t.IsError,
	}
	// The settled result is opaque to the transport: it is the harness's own
	// vocabulary for how a call ended, and a transport that interpreted it would
	// have to be changed every time that vocabulary grew.
	if raw, err := json.Marshal(t.Result); err == nil && string(raw) != "null" {
		out.Result = raw
	}
	return out
}

func fromWireToolCall(t hostwire.ToolCall) *harness.ToolCall {
	out := &harness.ToolCall{
		ID:      t.ID,
		Title:   t.Title,
		Status:  harness.ToolStatus(t.Status),
		Input:   t.Input,
		Output:  t.Output,
		IsError: t.IsError,
	}
	if len(t.Result) > 0 {
		var result toolresult.Outcome
		if err := json.Unmarshal(t.Result, &result); err == nil {
			out.Result = result
		}
	}
	return out
}

func toWireConfigOption(o harness.ConfigOption) hostwire.ConfigOption {
	out := hostwire.ConfigOption{ID: o.ID, Name: o.Name, Current: o.Current}
	for _, v := range o.Options {
		out.Options = append(out.Options, hostwire.ConfigOptionValue{
			ID:          v.ID,
			Name:        v.Name,
			Description: v.Description,
			Group:       v.Group,
		})
	}
	return out
}

func fromWireConfigOption(o hostwire.ConfigOption) harness.ConfigOption {
	out := harness.ConfigOption{ID: o.ID, Name: o.Name, Current: o.Current}
	for _, v := range o.Options {
		out.Options = append(out.Options, harness.ConfigOptionValue{
			ID:          v.ID,
			Name:        v.Name,
			Description: v.Description,
			Group:       v.Group,
		})
	}
	return out
}

func toWireSessionInfo(i harness.SessionInfo) hostwire.SessionInfo {
	return hostwire.SessionInfo{
		ID:        i.ID,
		Title:     i.Title,
		Workspace: i.Workspace,
		CreatedAt: i.CreatedAt,
		UpdatedAt: i.UpdatedAt,
	}
}

func fromWireSessionInfo(i hostwire.SessionInfo) harness.SessionInfo {
	return harness.SessionInfo{
		ID:        i.ID,
		Title:     i.Title,
		Workspace: i.Workspace,
		CreatedAt: i.CreatedAt,
		UpdatedAt: i.UpdatedAt,
	}
}

func toWireSession(s harness.Session) hostwire.Session {
	out := hostwire.Session{Info: toWireSessionInfo(s.Info)}
	for _, o := range s.Config {
		out.Config = append(out.Config, toWireConfigOption(o))
	}
	return out
}

func fromWireSession(s hostwire.Session) harness.Session {
	out := harness.Session{Info: fromWireSessionInfo(s.Info)}
	for _, o := range s.Config {
		out.Config = append(out.Config, fromWireConfigOption(o))
	}
	return out
}

func toWireCapabilities(c harness.Capabilities) hostwire.Capabilities {
	return hostwire.Capabilities{
		ProtocolVersion: c.ProtocolVersion,
		AgentName:       c.AgentName,
		AgentVersion:    c.AgentVersion,
		CanList:         c.CanList,
		CanResume:       c.CanResume,
		CanClose:        c.CanClose,
		CanPromptImages: c.CanPromptImages,
	}
}

func fromWireCapabilities(c hostwire.Capabilities) harness.Capabilities {
	return harness.Capabilities{
		ProtocolVersion: c.ProtocolVersion,
		AgentName:       c.AgentName,
		AgentVersion:    c.AgentVersion,
		CanList:         c.CanList,
		CanResume:       c.CanResume,
		CanClose:        c.CanClose,
		CanPromptImages: c.CanPromptImages,
	}
}

// childCapabilities reports what the harness advertised, and a zero value when
// it has not completed its handshake — which is a real state, not an error: a
// gateway that connects during a child restart should be told the host is up and
// the harness is not, rather than be refused.
func childCapabilities(h harness.Harness) hostwire.Capabilities {
	return toWireCapabilities(h.Capabilities())
}

func processID() int { return os.Getpid() }

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// errNotAttached is the refusal for a session this host does not hold. It is a
// conflict rather than a not-found because the session may well exist on disk —
// what is missing is this host's attachment to it, which the caller fixes by
// resuming.
func errNotAttached(sessionID string) error {
	return errx.New(errx.KindConflict, hostwire.CodeSessionNotAttached,
		"session "+sessionID+" is not attached to this host")
}

// errDraining refuses work while the host is going away.
func errDraining() error {
	return errx.New(errx.KindUnavailable, "host_draining",
		"the agent host is being redeployed and is not accepting new work; retry in a moment")
}

// toWireError maps an application failure onto the wire, preserving the kind and
// the code so that the layer above re-derives exactly the status it would have
// produced in-process.
//
// This function and fromWireError are the only two places the two vocabularies
// meet, which is what the extra package boundary buys: everything else on either
// side is unaware that a process boundary exists.
func toWireError(err error) *hostwire.Error {
	if err == nil {
		return nil
	}
	if appErr, ok := errx.As(err); ok {
		return hostwire.ClassifyError(err, wireKindOf(appErr.Kind), appErr.Code)
	}
	return hostwire.ErrorOf(err)
}

// wireKindOf renders an application kind as the wire's string.
func wireKindOf(kind errx.Kind) string {
	switch kind {
	case errx.KindInvalid:
		return hostwire.KindInvalid
	case errx.KindNotFound:
		return hostwire.KindNotFound
	case errx.KindConflict:
		return hostwire.KindConflict
	case errx.KindUnauthenticated:
		return hostwire.KindUnauthenticated
	case errx.KindForbidden:
		return hostwire.KindForbidden
	case errx.KindRateLimited:
		return hostwire.KindRateLimited
	case errx.KindUnavailable:
		return hostwire.KindUnavailable
	case errx.KindTimeout:
		return hostwire.KindTimeout
	default:
		return hostwire.KindInternal
	}
}

// fromWireError maps a wire failure onto the application's error vocabulary, so
// that every layer above keeps answering with the status codes it always did.
func fromWireError(e *hostwire.Error) error {
	if e == nil {
		return nil
	}
	return errx.New(kindOf(e.Kind), e.Code, e.Message)
}

func kindOf(kind string) errx.Kind {
	switch kind {
	case hostwire.KindInvalid:
		return errx.KindInvalid
	case hostwire.KindNotFound:
		return errx.KindNotFound
	case hostwire.KindConflict:
		return errx.KindConflict
	case hostwire.KindUnauthenticated:
		return errx.KindUnauthenticated
	case hostwire.KindForbidden:
		return errx.KindForbidden
	case hostwire.KindRateLimited:
		return errx.KindRateLimited
	case hostwire.KindUnavailable:
		return errx.KindUnavailable
	case hostwire.KindTimeout:
		return errx.KindTimeout
	default:
		return errx.KindInternal
	}
}

// permissionsUpstream adapts a Server to the harness's PermissionHandler port:
// the host does not decide, it relays. That is the rule that keeps policy in the
// tier that is authenticated to a person.
type permissionsUpstream struct{ s *Server }

func (p permissionsUpstream) RequestPermission(ctx context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
	return p.s.RequestPermission(ctx, req)
}

// stateUpstream turns the adapter's state transitions into notifications.
type stateUpstream struct{ s *Server }

func (u stateUpstream) PublishState(state harness.State, detail string) {
	u.s.PublishState(state, detail)
}

// updatesUpstream forwards agent activity.
type updatesUpstream struct{ s *Server }

func (u updatesUpstream) Publish(update harness.Update) { u.s.PublishUpdate(update) }
