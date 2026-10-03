package v1

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/audit"
	"github.com/huanghantao/dsh-gateway/internal/authn"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
)

// Workspace is an allowlisted working root.
//
// The client picks from this list and may never supply a path of its own. That
// choice is deliberate: the ACP client supplies an absolute cwd to DSH, so
// accepting a client-provided path would turn the session-creation endpoint into
// a way to point an agent with shell access at any directory on the machine.
type Workspace struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Exists is false when a configured root has gone away, so the UI can grey it
	// out instead of failing later.
	Exists bool `json:"exists"`
}

func describeWorkspace(path string) Workspace {
	ws := Workspace{Path: path, Name: filepath.Base(path)}
	if ws.Name == "." || ws.Name == string(filepath.Separator) || ws.Name == "" {
		ws.Name = path
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		ws.Exists = true
	}
	return ws
}

// deviceView is the wire shape of an enrolled device.
type deviceView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"createdAt"`
	LastSeen  time.Time  `json:"lastSeen"`
	ExpiresAt time.Time  `json:"expiresAt"`
	Revoked   bool       `json:"revoked"`
	Current   bool       `json:"current"`
	UserAgent string     `json:"userAgent,omitempty"`
	ExpiresIn *time.Time `json:"-"`
}

func toDeviceView(d authn.Device, currentID string) deviceView {
	return deviceView{
		ID:        d.ID,
		Name:      d.Name,
		CreatedAt: d.CreatedAt.UTC(),
		LastSeen:  d.LastSeen.UTC(),
		ExpiresAt: d.ExpiresAt.UTC(),
		Revoked:   d.Revoked,
		Current:   d.ID == currentID,
		UserAgent: d.UserAgent,
	}
}

// handleHealthz reports liveness. It answers 200 for as long as the process can
// serve a request, which is the only question a liveness probe should ask.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.deps.Version,
		"uptime":  time.Since(s.deps.StartedAt).Round(time.Second).String(),
	})
}

// handleReadyz reports whether the harness is usable.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Draining is reported as not-ready: the deploy script polls this endpoint
	// to learn when the old process has gone, and a gateway that is still
	// finishing a turn must not answer "ready" to a question it is asked in
	// order to replace it. The harness is unaffected and is reported separately.
	draining := s.deps.Lifecycle != nil && s.deps.Lifecycle.Draining()
	ready := s.deps.Ready != nil && s.deps.Ready() && !draining
	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "unavailable"
	}
	body := map[string]any{
		"status":  state,
		"harness": string(s.deps.Harness.State()),
		// droppedEventCount surfaces backpressure. An operator seeing this climb
		// knows a client is falling behind, which is otherwise invisible.
		"droppedEvents": s.deps.Bus.Dropped(),
	}
	if !ready {
		switch {
		case draining:
			body["detail"] = "the gateway is draining for a redeploy"
			body["draining"] = true
		default:
			body["detail"] = "the harness process has not completed its handshake"
		}
	}
	_ = httpcore.RespondJSON(w, status, body)
}

// pairRequest is the pairing body.
type pairRequest struct {
	Code       string `json:"code"`
	DeviceName string `json:"deviceName"`
}

// pairResponse returns the device and, exactly once, its token.
type pairResponse struct {
	Device deviceView `json:"device"`
	// Token is shown once and never stored in recoverable form. A browser can
	// ignore it because the cookie is already set; a native app must keep it.
	Token string `json:"token"`
}

// handlePair exchanges a one-time code for a device credential.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if err := httpcore.DecodeJSON(w, r, &req); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	if req.Code == "" {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "code_required", "a pairing code is required"))
		return
	}

	key := clientKey(r)
	device, token, err := s.deps.Pairing.Redeem(r.Context(), key, req.Code, req.DeviceName, r.UserAgent())
	if err != nil {
		s.deps.Audit.Record(r.Context(), audit.EventPairFailed, "", map[string]any{
			"clientIp": key,
			"reason":   errx.CodeOf(err),
		})
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	s.setSessionCookie(w, r, token, device.ExpiresAt)
	s.deps.Audit.Record(r.Context(), audit.EventPaired, device.ID, map[string]any{
		"clientIp":   key,
		"deviceName": device.Name,
		"userAgent":  r.UserAgent(),
	})
	s.deps.Logger.Info("device paired", "device", device.ID, "name", device.Name, "client_ip", key)

	_ = httpcore.RespondJSON(w, http.StatusCreated, pairResponse{
		Device: toDeviceView(device, device.ID),
		Token:  token,
	})
}

// handleMe returns the calling device, plus the deployment's limits and
// features.
//
// The device fields are FLAT and identical to an entry in GET /devices. That is
// not cosmetic: an earlier revision returned `deviceId`/`deviceName`/`pairedAt`
// here while the device list used `id`/`name`/`createdAt`, so one entity had two
// shapes and a client that decoded both with the same function silently failed
// on this one. A real browser test caught it — the app paired successfully and
// then reported "the server returned an unexpected principal".
//
// One entity, one shape.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnauthenticated, "authentication_required", ""))
		return
	}

	// Every field a /devices entry carries, so a client can decode both with one
	// function. The two booleans are stated rather than omitted: the calling
	// device is by definition the current one and not revoked, and a client that
	// had to special-case their absence would be the bug this comment prevents.
	body := map[string]any{
		"id":        principal.DeviceID,
		"name":      principal.DeviceName,
		"createdAt": principal.PairedAt.UTC(),
		"expiresAt": principal.ExpiresAt.UTC(),
		"revoked":   false,
		"current":   true,
	}
	if devices, err := s.deps.Auth.List(r.Context()); err == nil {
		for _, d := range devices {
			if d.ID == principal.DeviceID {
				body["lastSeen"] = d.LastSeen.UTC()
				if d.UserAgent != "" {
					body["userAgent"] = d.UserAgent
				}
				break
			}
		}
	}
	if _, ok := body["lastSeen"]; !ok {
		// The store could not be read. Fall back to the pairing time rather than
		// dropping the field, which would break the shape the test pins.
		body["lastSeen"] = principal.PairedAt.UTC()
	}

	// limits and features are reported here so the client does not have to
	// hardcode deployment policy. The composer needs the prompt limit to warn
	// before a send is rejected, and the UI needs to know which optional surfaces
	// are switched on rather than probing and failing.
	body["limits"] = map[string]any{
		"maxPromptBytes": s.deps.Config.Limits.MaxPromptBytes,
		"maxBodyBytes":   s.deps.Config.Limits.MaxBodyBytes,
		"maxImageBytes":  s.imageLimit(),
		"transcriptPage": s.deps.Config.Transcript.PageSize,
	}
	body["features"] = map[string]any{
		"transcript":            s.deps.Config.Transcript.Enabled,
		"desktopUI":             s.deps.Config.DesktopUI.Enabled,
		"imagePrompts":          s.deps.Harness.Capabilities().CanPromptImages,
		"approvalTimeoutSecs":   int(s.deps.Config.Session.ApprovalTimeout.Std().Seconds()),
		"sessionIdleTimeoutSec": int(s.deps.Config.Session.IdleTimeout.Std().Seconds()),
		// Zero means scoped approvals are off, and a client that reads it must
		// not offer the choices that would create one.
		"approvalGrantTTLSecs": int(s.deps.Config.Session.ApprovalGrantTTL.Std().Seconds()),
		// How many prompts may wait behind a running turn. Zero tells a client
		// to disable its composer mid-turn rather than collecting text the
		// gateway will refuse.
		"promptQueueDepth": s.deps.Config.Session.PromptQueueDepth,
		"revertEnabled":    s.deps.Config.Changes.Revert.Enabled,
	}

	_ = httpcore.RespondJSON(w, http.StatusOK, body)
}

// handleListDevices returns every enrolled device.
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	devices, err := s.deps.Auth.List(r.Context())
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	views := make([]deviceView, 0, len(devices))
	for _, d := range devices {
		views = append(views, toDeviceView(d, principal.DeviceID))
	}
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"devices": views})
}

// handleRevokeDevice disables a device.
func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "device_id_required", "a device id is required"))
		return
	}

	if err := s.deps.Auth.Revoke(r.Context(), id); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	s.deps.Audit.Record(r.Context(), audit.EventRevoked, id, map[string]any{
		"clientIp": clientKey(r),
	})
	s.deps.Logger.Info("device revoked", "device", id)

	// A revoked device must stop receiving notifications at the same moment it
	// stops being able to call the API, or the next approval is announced on a
	// phone that is no longer allowed to answer it.
	if s.deps.Push != nil {
		if dropped, err := s.deps.Push.ForgetDevice(id); err != nil {
			s.deps.Logger.Warn("could not drop a revoked device's push subscriptions", "error", err.Error())
		} else if dropped > 0 {
			s.deps.Logger.Info("dropped push subscriptions for a revoked device", "device", id, "count", dropped)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleWorkspaces lists the allowlisted roots.
func (s *Server) handleWorkspaces(w http.ResponseWriter, _ *http.Request) {
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"workspaces": s.workspaces})
}

// setSessionCookie writes the device credential as a browser cookie.
//
// Secure is set whenever the gateway is reached over HTTPS. DSH's own GUI cookie
// omits it because the shipped server is loopback HTTP; the gateway has no such
// excuse, and a session cookie that can travel in clear is a session cookie that
// will.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	secure := s.deps.Config.SecureCookies()
	if !secure && s.deps.Resolver != nil && s.deps.Resolver.Scheme(r) == "https" {
		// The configured public URL may be absent while the request still arrived
		// over TLS through the tunnel. Believing that is safe only because the
		// resolver already verified the peer is a trusted proxy.
		secure = true
	}

	// Secure is conditional because a loopback-only development run legitimately
	// uses plain HTTP; over any real deployment it is set, and the resolver only
	// reports "https" for a peer it has already verified is our own proxy.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is set whenever the request arrived over TLS
		Name:     s.deps.Config.Auth.CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   secure,
		// Strict, not Lax: nothing in this app is reached by following an external
		// link into a state-changing page, so the stricter value costs nothing and
		// closes cross-site request forgery without a token dance.
		SameSite: http.SameSiteStrictMode,
	})
}
