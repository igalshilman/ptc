package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	restate "github.com/restatedev/sdk-go"
)

// Config configures a durable agent Service.
type Config struct {
	Client    openai.Client // an OpenAI(-compatible) client
	Model     string        // model id (default "gpt-4o-mini")
	Tools     []Tool        // developer tools exposed to the agent
	MaxRounds int           // loop budget per message (default 10)
	// Discover, if set, makes each Ask discover handler-tools from the Restate Admin
	// API and merge them with Tools. Discovery is journaled, so the tool set is
	// identical across replays. Use this for a standalone deployment whose target
	// services register alongside the agent (and so aren't visible until after startup).
	Discover *DiscoverConfig
}

// Service is a durable CodeAct agent exposed as a Restate Virtual Object: each
// object key is an independent session whose transcript is durable state. The
// QuickJS engine and the tool set are fixed at construction and shared across
// sessions/invocations.
type Service struct {
	engine    *Engine
	client    openai.Client
	model     string
	tools     []Tool
	maxRounds int
	discover  *DiscoverConfig // optional Admin-API handler discovery (per Ask)
}

// agentSignalsService is the framework's keyless companion service; its resolve/reject
// handlers complete named signals and are annotated for discovery.
const agentSignalsService = "AgentSignals"

// NewService builds the QuickJS engine and assembles the agent from cfg.
func NewService(ctx context.Context, cfg Config) (*Service, error) {
	eng, err := NewEngine(ctx, guestWasm)
	if err != nil {
		return nil, err
	}
	model := cfg.Model
	if model == "" {
		model = "gpt-5"
	}
	rounds := cfg.MaxRounds
	if rounds == 0 {
		rounds = 10
	}
	return &Service{
		engine:    eng,
		client:    cfg.Client,
		model:     model,
		tools:     cfg.Tools,
		maxRounds: rounds,
		discover:  cfg.Discover,
	}, nil
}

// Close releases the engine (call on shutdown).
func (s *Service) Close(ctx context.Context) error { return s.engine.Close(ctx) }

// Definitions returns the Restate services to bind:
//
//   - "Agent" (Virtual Object): the session — Ask (drive a turn), History (read
//     transcript), Reset (clear).
//   - "AgentSignals" (keyless service): resolve / reject complete a named signal by
//     (invocation, name). Annotated with AgentToolAnnotation, so a discovering agent
//     gets them as tools — that's how the `signal` tool's approval is completed.
func (s *Service) Definitions() []restate.ServiceDefinition {
	agent := restate.NewObject("Agent").
		Handler("Ask", restate.NewObjectHandler(s.Ask)).
		Handler("History", restate.NewObjectSharedHandler(s.History)).
		Handler("Reset", restate.NewObjectHandler(s.Reset))
	signals := restate.NewService(agentSignalsService).
		Handler("resolve", restate.NewServiceHandler(resolveSignal, restate.WithMetadata(AgentToolAnnotation, "resolve"))).
		Handler("reject", restate.NewServiceHandler(rejectSignal, restate.WithMetadata(AgentToolAnnotation, "reject")))
	return []restate.ServiceDefinition{agent, signals}
}

// Signal-completion handlers on AgentSignals. resolve/reject a named signal on a target
// invocation (the one blocked in the `signal` tool); each has the full restate.Context
// to issue the ctx-level command, and calling one is a durable service call — so
// discovery exposes them as plain leaf tools.

type signalResolveInput struct {
	Invocation string          `json:"invocation" jsonschema:"description=the invocation id awaiting the signal (from the signal tool's log)"`
	Name       string          `json:"name" jsonschema:"description=the signal name to complete"`
	Value      json.RawMessage `json:"value,omitempty" jsonschema:"description=the JSON value to resolve it with"`
}
type signalRejectInput struct {
	Invocation string `json:"invocation" jsonschema:"description=the invocation id awaiting the signal"`
	Name       string `json:"name" jsonschema:"description=the signal name to reject"`
	Reason     string `json:"reason,omitempty" jsonschema:"description=why it is being rejected"`
}
type signalOK struct {
	OK bool `json:"ok"`
}

func resolveSignal(ctx restate.Context, in signalResolveInput) (signalOK, error) {
	if in.Invocation == "" || in.Name == "" {
		return signalOK{}, restate.TerminalErrorf("resolve needs an invocation id and signal name")
	}
	v := in.Value
	if len(v) == 0 {
		v = json.RawMessage("null")
	}
	restate.ResolveSignal(ctx, in.Invocation, in.Name, v)
	return signalOK{OK: true}, nil
}

func rejectSignal(ctx restate.Context, in signalRejectInput) (signalOK, error) {
	if in.Invocation == "" || in.Name == "" {
		return signalOK{}, restate.TerminalErrorf("reject needs an invocation id and signal name")
	}
	reason := in.Reason
	if reason == "" {
		reason = "rejected"
	}
	restate.RejectSignal(ctx, in.Invocation, in.Name, errors.New(reason))
	return signalOK{OK: true}, nil
}

// AskInput is one message to a session (the object key is the session id).
type AskInput struct {
	Message string `json:"message"`
}

type AskOutput struct {
	Answer string `json:"answer"`
}

const historyKey = "history"

// Ask is the Virtual Object handler (invoked as Agent/<session>/Ask). It loads
// the session transcript from state, appends the new user message, runs the
// CodeAct loop (continuing from prior context), and persists the updated
// transcript on success.
//
// NOTE: Ask is the invocation entry point, NOT a restate.Run — its body (the
// RunAgent loop) runs directly. Durable steps are only the model call (in
// openAIModel.Decide) and each tool call; state writes (Get/Set) are journaled.
// On crash/replay the loop re-runs and every journaled step returns its captured
// value.
func (s *Service) Ask(ctx restate.ObjectContext, in AskInput) (AskOutput, error) {
	history, err := restate.Get[[]Turn](ctx, historyKey)
	if err != nil {
		return AskOutput{}, err
	}
	convo := &Conversation{Turns: append(history, Turn{Role: RoleUser, Content: in.Message})}

	// Resolve this turn's tool set: the static tools plus, if configured, handlers
	// discovered from the Admin API. Discovery is a JOURNALED step, so the tool set —
	// and thus the system prompt and the dispatch table — is identical across replays.
	tools := s.tools
	if s.discover != nil {
		discovered, derr := restate.Run(ctx, func(rc restate.RunContext) ([]handlerDescriptor, error) {
			return fetchHandlers(rc, *s.discover)
		}, restate.WithName("discover"))
		if derr != nil {
			return AskOutput{}, derr
		}
		tools = make([]Tool, 0, len(s.tools)+len(discovered))
		tools = append(tools, s.tools...)
		for _, d := range discovered {
			tools = append(tools, toolFromDescriptor(d))
		}
	}

	inv := &restateInvoker{
		rctx:    ctx,
		tools:   map[string]Tool{},
		pending: map[int]pendingOp{},
		ready:   map[int]readyOp{},
	}
	for _, t := range tools {
		inv.tools[t.Name] = t
		inv.order = append(inv.order, t.Name)
	}
	sb := NewSandbox(s.engine, inv)
	// Freeze the sandbox clock/RNG to replay-stable values: the seed comes from
	// Restate's deterministic per-invocation RNG, and the clock is captured once
	// in a journaled step so `new Date()`/Date.now are stable and realistic.
	now, err := restate.Run(ctx, func(rc restate.RunContext) (int64, error) {
		return time.Now().UnixMilli(), nil
	}, restate.WithName("clock"))
	if err != nil {
		return AskOutput{}, err
	}
	sb.SetDeterminism(restate.Rand(ctx).Int64(), now) // math/rand/v2: Int64, not Int63

	model := &openAIModel{rctx: ctx, client: s.client, model: s.model, system: BuildSystemPrompt(toolSpecs(tools))}

	answer, agentErr := RunAgent(ctx, sb, model, convo, s.maxRounds, func(f string, a ...any) {
		ctx.Log().Info(fmt.Sprintf(f, a...))
	})
	if agentErr != nil {
		// RunAgent's failures (e.g. ErrMaxRounds) are DETERMINISTIC — replaying
		// would reproduce them — so surface as terminal to avoid infinite retries.
		// (We do NOT persist history on failure, so the session is unchanged.)
		if restate.IsTerminalError(agentErr) {
			return AskOutput{}, agentErr
		}
		return AskOutput{}, restate.ToTerminalError(agentErr)
	}

	// Persist the updated transcript (includes the user message + this exchange).
	restate.Set(ctx, historyKey, convo.Turns)
	return AskOutput{Answer: answer}, nil
}

// History returns the session transcript. It's a SHARED (read-only) handler, so
// it can run concurrently with other reads (though it still queues behind an
// in-flight exclusive Ask on the same session key).
func (s *Service) History(ctx restate.ObjectSharedContext, _ restate.Void) ([]Turn, error) {
	return restate.Get[[]Turn](ctx, historyKey)
}

// Reset clears the session transcript, starting a fresh conversation.
func (s *Service) Reset(ctx restate.ObjectContext, _ restate.Void) (restate.Void, error) {
	restate.Clear(ctx, historyKey)
	return restate.Void{}, nil
}

// --- Invoker: dispatches JS tool calls to the registered Tools ---------------

// restateInvoker binds registered tools to the current invocation's context and
// satisfies Invoker. It tracks in-flight operations by handle across the program's
// lifetime; the context.Context passed to Next (carrying no useful state) is ignored,
// the tools get the real restate.Context.
type restateInvoker struct {
	rctx  restate.Context
	tools map[string]Tool
	order []string

	pending map[int]pendingOp // handle -> in-flight durable future
	ready   map[int]readyOp   // handle -> op that failed at submit time (settles immediately)
}

type pendingOp struct {
	fut  anyFuture
	name string // tool name, for a clear invalid-JSON error
}

type readyOp struct {
	err  error
	name string
}

func (r *restateInvoker) Tools() []ToolSpec {
	specs := make([]ToolSpec, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		specs = append(specs, ToolSpec{Name: t.Name, Description: t.Description, Params: t.Params, Result: t.Result})
	}
	return specs
}

// Reset clears leftover in-flight/errored ops from a previous program, so the host's
// handle space realigns with the guest (which resets its handle counter each start).
// Abandoned ops (e.g. Promise.race losers) are simply dropped — their durable futures
// are left in flight (no cleanup, by design).
func (r *restateInvoker) Reset() {
	r.pending = map[int]pendingOp{}
	r.ready = map[int]readyOp{}
}

// Start submits each new op as an in-flight durable Future (Run/Call/CallObject/Timer/
// Awakeable/Signal), keyed by the op's stable handle. An op that can't even be
// submitted (unknown tool / bad args) is recorded as immediately-settled. Non-blocking.
func (r *restateInvoker) Start(calls []ToolCall) {
	for _, c := range calls {
		t, ok := r.tools[c.Tool]
		if !ok {
			r.ready[c.Handle] = readyOp{err: restate.TerminalErrorf("unknown tool %q", c.Tool), name: c.Tool}
			continue
		}
		f, err := r.submit(t, c) // NON-blocking: reserves a journal slot, returns immediately
		if err != nil {
			r.ready[c.Handle] = readyOp{err: err, name: c.Tool}
			continue
		}
		r.pending[c.Handle] = pendingOp{fut: f, name: c.Tool}
	}
}

// Pending reports how many ops are still in flight (submitted futures + not-yet-
// delivered submit failures).
func (r *restateInvoker) Pending() int { return len(r.pending) + len(r.ready) }

// Next drives the in-flight ops until the FIRST settles and returns it. Ops that
// failed at submit time settle first (immediately available), in ascending handle
// order; otherwise restate.WaitFirst races the futures and yields the first to
// complete — in journaled order on replay, so which op "wins" is reproduced. A
// non-nil error is invocation-fatal (e.g. a Restate cancellation).
func (r *restateInvoker) Next(_ context.Context) (StepResult, error) {
	if len(r.ready) > 0 {
		h := -1
		for k := range r.ready {
			if h < 0 || k < h {
				h = k
			}
		}
		op := r.ready[h]
		delete(r.ready, h)
		return StepResult{Handle: h, ErrMsg: op.err.Error(), IsErr: true}, nil
	}

	sels := make([]restate.Future, 0, len(r.pending))
	handles := make([]int, 0, len(r.pending))
	for h, op := range r.pending {
		sels = append(sels, op.fut.selectable())
		handles = append(handles, h)
	}
	fut, cancelled := restate.WaitFirst(r.rctx, sels...)
	if cancelled != nil {
		return StepResult{}, cancelled
	}
	h := -1
	for i := range sels {
		if sels[i] == fut {
			h = handles[i]
			break
		}
	}
	if h < 0 {
		return StepResult{}, restate.TerminalErrorf("WaitFirst returned an unknown future")
	}
	op := r.pending[h]
	delete(r.pending, h)
	v, err := op.fut.resultJSON()
	if err != nil {
		return StepResult{Handle: h, ErrMsg: err.Error(), IsErr: true}, nil
	}
	if len(v) == 0 {
		v = json.RawMessage("null")
	} else if !json.Valid(v) {
		return StepResult{Handle: h, ErrMsg: fmt.Sprintf("tool %q returned invalid JSON", op.name), IsErr: true}, nil
	}
	return StepResult{Handle: h, Value: v}, nil
}

// submit turns one tool call into an in-flight durable Future WITHOUT blocking
// (Run/Call/CallObject/Timer/Awakeable/Signal). A multi-step operation is a handler the
// tool Calls, so it too is just a service-call future the batch driver awaits.
func (r *restateInvoker) submit(t Tool, c ToolCall) (anyFuture, error) {
	if t.submit == nil {
		return nil, restate.TerminalErrorf("tool %q has no submit", c.Tool)
	}
	return t.submit(r.rctx, c.Arg)
}

// --- Model: the OpenAI-backed decision maker ---------------------------------

// openAIModel implements Model: a durable restate.Run around one OpenAI chat
// completion that returns the next Decision as JSON. Journaling the call means a
// replay after a crash returns the recorded decision instead of re-billing OpenAI.
type openAIModel struct {
	rctx   restate.Context
	client openai.Client
	model  string
	system string // system prompt incl. the CodeAct protocol + tool docs
}

func (m *openAIModel) Decide(_ context.Context, convo *Conversation) (Decision, error) {
	// Unique per-round journal-step name (turns-per-round can vary when a round
	// only appends a feedback observation, so key off the round, not turn count).
	stepName := fmt.Sprintf("llm.round.%d", convo.Round)

	// The Run returns the model's reply as a STRING, not json.RawMessage: the reply
	// isn't guaranteed to be valid JSON (a model may emit a bare program or prose),
	// and journaling a json.RawMessage would call MarshalJSON, which validates and
	// panics on non-JSON. We parse leniently below instead.
	raw, err := restate.Run(m.rctx, func(rc restate.RunContext) (string, error) {
		// Send only the most recent slice of the transcript that fits the budget,
		// so long sessions / big observations can't overflow the model's context.
		msgs := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(m.system)}
		for _, t := range windowTurns(convo.Turns, maxTranscriptChars) {
			switch t.Role {
			case RoleUser:
				msgs = append(msgs, openai.UserMessage(t.Content))
			case RoleAssistant:
				msgs = append(msgs, openai.AssistantMessage(t.Content))
			case RoleObservation:
				msgs = append(msgs, openai.UserMessage("Observation:\n"+t.Content))
			case RoleAnswer:
				msgs = append(msgs, openai.AssistantMessage(t.Content))
			}
		}
		resp, err := m.client.Chat.Completions.New(rc, openai.ChatCompletionNewParams{
			Model:    openai.ChatModel(m.model),
			Messages: msgs,
		})
		if err != nil {
			return "", err // non-terminal: Restate retries the Run with backoff
		}
		if len(resp.Choices) == 0 {
			return "", restate.TerminalErrorf("model returned no choices")
		}
		return strings.TrimSpace(resp.Choices[0].Message.Content), nil
	}, restate.WithName(stepName))
	if err != nil {
		return Decision{}, err
	}

	// Parse leniently (models sometimes wrap JSON in prose/``` fences). On failure
	// return an ErrBadDecision so RunAgent feeds it back for self-correction
	// rather than terminating the whole invocation.
	var dec Decision
	if err := json.Unmarshal([]byte(extractJSON(raw)), &dec); err != nil {
		return Decision{}, fmt.Errorf("%w: your reply was not valid JSON — respond with ONLY {\"thought\":...,\"code\":...}; got: %s", ErrBadDecision, truncate(raw, 200))
	}
	return dec, nil
}

// extractJSON strips ``` fences and trims to the outermost {...} so a decision
// wrapped in prose still parses.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	if start, end := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}'); start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
