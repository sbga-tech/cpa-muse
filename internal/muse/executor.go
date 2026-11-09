package muse

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/sbga-tech/cpa-muse/internal/host"
)

// Executor format names as declared to the host.
const (
	FormatCodex          = "codex"
	FormatOpenAIResponse = "openai-response"
	FormatClaude         = "claude"
)

// Executor serves every Muse request routed to the plugin.
type Executor struct {
	cfg   *Config
	quota *quotaStore
}

// NewExecutor binds an executor to one configuration snapshot. Quota
// observations survive reconfiguration when the previous store is passed.
func NewExecutor(cfg *Config, quota *quotaStore) *Executor {
	if quota == nil {
		quota = &quotaStore{}
	}
	return &Executor{cfg: cfg, quota: quota}
}

// QuotaStore returns the passive quota store for reuse across reconfiguration.
func (e *Executor) QuotaStore() *quotaStore { return e.quota }

// Config returns the configuration snapshot the executor serves.
func (e *Executor) Config() *Config { return e.cfg }

// InputFormats are the executor input formats declared at registration. The
// first entry is the target the host translates undeclared client formats to.
//
// openai-response is always declared: the host drops identity frames when it
// translates a plugin's codex stream to openai-response (it treats an
// unchanged frame as a translator-registry fallback), so Responses clients
// must reach the plugin natively. In legacy mode the plugin converts them to
// codex itself (legacyPlanner.fromResponses).
func (c *Config) InputFormats() []string {
	formats := []string{FormatCodex, FormatOpenAIResponse}
	if c.MessagesMode() == "enabled" {
		formats = append(formats, FormatClaude)
	}
	return formats
}

// Call is one executor invocation from the host.
type Call struct {
	Req  pluginapi.ExecutorRequest
	Host host.Client
	// Model is the upstream Muse model after prefix/alias resolution, without suffix.
	Model string
	// ThinkingLevel is a CPA "(level)" suffix the host left unapplied, if any.
	ThinkingLevel string
	Stream        bool
}

func (e *Executor) newCall(req pluginapi.ExecutorRequest, callbackID string, stream bool) *Call {
	requested := strings.TrimSpace(req.Model)
	model, ok := e.cfg.RouteModel(requested)
	if !ok {
		model = UpstreamModel(requested)
	}
	return &Call{
		Req:           req,
		Host:          host.NewHTTPClient(req.HTTPClient, callbackID),
		Model:         model,
		ThinkingLevel: thinkingSuffix(requested),
		Stream:        stream,
	}
}

// Execute runs a non-streaming request.
func (e *Executor) Execute(ctx context.Context, req pluginapi.ExecutorRequest, callbackID string) (pluginapi.ExecutorResponse, error) {
	ctx = WithRequestState(ctx)
	call := e.newCall(req, callbackID, false)
	surface, err := e.surfaceFor(ctx, call)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	return surface.execute(ctx, call)
}

// ExecuteStream opens a streaming request. Upstream failures before the first
// downstream byte are returned as errors so the host can surface their status;
// afterwards chunks are pushed to the host stream identified by streamID.
func (e *Executor) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest, callbackID, streamID string) (http.Header, error) {
	// The stream outlives the RPC call; its context ends when the stream ends.
	ctx, cancel := context.WithCancel(WithRequestState(ctx))
	call := e.newCall(req, callbackID, true)
	surface, err := e.surfaceFor(ctx, call)
	if err != nil {
		cancel()
		return nil, err
	}
	stream, err := surface.open(ctx, call)
	if err != nil {
		cancel()
		return nil, err
	}
	emitter := host.Emitter{StreamID: streamID}
	go func() {
		defer cancel()
		defer stream.close()
		errRun := stream.run(func(chunk []byte) error {
			if len(chunk) == 0 {
				return nil
			}
			return emitter.Emit(chunk)
		})
		emitter.Close(ABIError(errRun))
	}()
	return stream.headers, nil
}

// CountTokens answers a count-tokens request.
func (e *Executor) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest, callbackID string) (pluginapi.ExecutorResponse, error) {
	ctx = WithRequestState(ctx)
	call := e.newCall(req, callbackID, false)
	surface, err := e.surfaceFor(ctx, call)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	return surface.count(ctx, call)
}

// surface is one wire protocol to Meta (legacy Responses, native Responses, Messages).
type surface interface {
	execute(ctx context.Context, call *Call) (pluginapi.ExecutorResponse, error)
	open(ctx context.Context, call *Call) (*openStream, error)
	count(ctx context.Context, call *Call) (pluginapi.ExecutorResponse, error)
}

// openStream is an upstream stream whose first response is already validated.
type openStream struct {
	headers http.Header
	// run pumps downstream chunks through sink until the stream ends. A
	// returned error is reported to the host as the stream's terminal error.
	run func(sink func([]byte) error) error
	// release closes the upstream body.
	release func()
	once    sync.Once
}

func (s *openStream) close() {
	s.once.Do(func() {
		if s.release != nil {
			s.release()
		}
	})
}

func (e *Executor) surfaceFor(ctx context.Context, call *Call) (surface, error) {
	format := strings.ToLower(strings.TrimSpace(call.Req.Format))
	if format != FormatClaude {
		if (format == FormatCodex || format == FormatOpenAIResponse || format == "") && e.cfg.ResponsesMode() == "disabled" {
			return nil, errorf(http.StatusForbidden, "disabled", "Meta Responses is disabled on this gateway")
		}
		// Responses compaction: expand CPA compaction capsules into input and
		// serve compaction triggers with a summary request.
		compact, err := e.prepareCompaction(ctx, call)
		if err != nil {
			return nil, err
		}
		if compact != nil {
			return compact, nil
		}
	}
	switch format {
	case FormatClaude:
		if e.cfg.MessagesMode() != "enabled" {
			return nil, errorf(http.StatusBadRequest, "unsupported_format", "Meta Messages is not enabled; set meta-messages.mode: enabled")
		}
		return &messagesSurface{exec: e}, nil
	case FormatOpenAIResponse:
		if e.cfg.ResponsesMode() == "disabled" {
			return nil, errorf(http.StatusForbidden, "disabled", "Meta Responses is disabled on this gateway")
		}
		if selection := e.profileSelection(call); selection.Enabled {
			return &responsesSurface{exec: e, planner: profilePlanner{selection: selection}}, nil
		}
		if e.cfg.ResponsesMode() == "native" {
			return &responsesSurface{exec: e, planner: nativePlanner{}}, nil
		}
		return &responsesSurface{exec: e, planner: legacyPlanner{fromResponses: true}}, nil
	case FormatCodex, "":
		if e.cfg.ResponsesMode() == "disabled" {
			return nil, errorf(http.StatusForbidden, "disabled", "Meta Responses is disabled on this gateway")
		}
		return &responsesSurface{exec: e, planner: legacyPlanner{}}, nil
	default:
		return nil, errorf(http.StatusBadRequest, "unsupported_format", "unsupported executor format %q", call.Req.Format)
	}
}

// profileSelection resolves the Muse profile for the request's downstream key.
func (e *Executor) profileSelection(call *Call) ProfileSelection {
	return e.cfg.ResolveProfile(downstreamPrincipal(e.cfg, call.Req))
}

// thinkingSuffix returns the level in "model(level)". The host strips the
// suffix itself when it applied thinking, so a remaining suffix is ours to apply.
func thinkingSuffix(name string) string {
	if strings.HasSuffix(name, ")") {
		if open := strings.LastIndex(name, "("); open > 0 {
			return strings.ToLower(strings.TrimSpace(name[open+1 : len(name)-1]))
		}
	}
	return ""
}
