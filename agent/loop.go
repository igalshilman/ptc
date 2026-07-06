package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrMaxRounds is returned when the agent exhausts its round budget. This is a
// DETERMINISTIC give-up — a replay reproduces it — so the Restate handler must
// surface it as a terminal error (not a retryable one), else the invocation
// would retry forever.
var ErrMaxRounds = errors.New("agent did not finish within the round budget")

// ErrBadDecision, when a Model.Decide error wraps it, is fed back to the model as
// an observation instead of aborting the run — letting the model self-correct a
// malformed response. Any other Decide error aborts the loop.
var ErrBadDecision = errors.New("invalid model decision")

// Decision is the model's output each round: ALWAYS a JavaScript program (a
// "mini workflow") that manipulates the registered tools. Thought is optional
// reasoning. The program itself signals completion via its return value (see
// finalAnswer) — the model never returns a final answer directly.
type Decision struct {
	Thought string `json:"thought,omitempty"`
	Code    string `json:"code,omitempty"`
}

// Turn roles in a conversation transcript.
const (
	RoleUser        = "user"        // a human message
	RoleAssistant   = "assistant"   // a program (code) the model proposed
	RoleObservation = "observation" // that program's result or error
	RoleAnswer      = "answer"      // the model's final answer to a user message
)

// Turn is one entry in the agent conversation the model reasons over.
type Turn struct {
	Role    string
	Content string
}

// Conversation is the transcript the model reasons over. For a multi-turn session
// it carries the FULL history (prior user messages, programs, observations, and
// answers) so the agent has memory across invocations. Round is the 1-based
// current round within THIS invocation, set by RunAgent before each model call so
// a Model can name its durable step uniquely per round.
type Conversation struct {
	Round int
	Turns []Turn
}

// NewConversation starts a fresh conversation with a single user message.
func NewConversation(userMessage string) *Conversation {
	return &Conversation{Turns: []Turn{{Role: RoleUser, Content: userMessage}}}
}

// BuildSystemPrompt renders the CodeAct protocol plus each tool's name,
// description, and JSON Schema, so the model knows exactly how to call every
// tool. Shared by the demo and the production model.
func BuildSystemPrompt(specs []ToolSpec) string {
	var b strings.Builder
	b.WriteString("You are a CodeAct agent. You act ONLY by writing small JavaScript programs (mini workflows) that call the tools below.\n")
	b.WriteString(`Each round, respond with EXACTLY ONE JSON object — {"thought":"...","code":"<JavaScript>"} — and nothing else.` + "\n\n")
	b.WriteString("The `code` is a sequence of statements run inside an async function, so top-level `await` is allowed. Write the statements DIRECTLY — do NOT wrap them in a `function` declaration or IIFE. Each tool is an async function; `await` it. The code MUST end by `return`ing a JSON-serializable value:\n")
	b.WriteString("  • to FINISH: `return {done: true, answer: <your final answer>};`\n")
	b.WriteString("  • to CONTINUE (e.g. to inspect a tool result first): `return {done: false, ...anything};` — that value comes back to you as an observation and you write the next program.\n")
	b.WriteString("A single program may chain several tool calls. Example code:\n")
	b.WriteString("    const r = await someTool({key: \"value\"});\n")
	b.WriteString("    return {done: true, answer: r.value};\n")
	b.WriteString("Tool results are captured durably, so retries never repeat a completed tool call.\n\n")
	if len(specs) == 0 {
		b.WriteString("No tools are available.")
		return b.String()
	}
	b.WriteString("Tools (async JS functions; each takes ONE argument matching its JSON Schema):\n")
	for _, s := range specs {
		fmt.Fprintf(&b, "  • %s — %s\n", s.Name, s.Description)
		if len(s.Params) > 0 {
			fmt.Fprintf(&b, "      args schema: %s\n", compactJSON(s.Params))
		} else {
			b.WriteString("      args: none\n")
		}
	}
	b.WriteString("Only these tools exist; there is no other global, network, or filesystem access.")
	return b.String()
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// Limits that keep the model's context window from overflowing regardless of a
// tool's output size or the session length. Raise these for a larger-context model.
const (
	maxObservationChars = 8_000   // cap a single tool result/error fed back to the model
	maxTranscriptChars  = 120_000 // cap total transcript chars sent per model call
)

// clip bounds a string, noting how much was dropped.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n… [truncated; %d bytes total]", len(s))
}

// windowTurns returns the most recent suffix of turns whose total content fits
// maxChars (always keeping at least the last turn). Deterministic, so a replay
// produces the same window; the FULL transcript is still persisted as state —
// only what's sent to the model each call is bounded.
func windowTurns(turns []Turn, maxChars int) []Turn {
	total, start := 0, len(turns)
	for i := len(turns) - 1; i >= 0; i-- {
		total += len(turns[i].Content)
		if total > maxChars && i != len(turns)-1 {
			break
		}
		start = i
	}
	return turns[start:]
}

// Model produces the next Decision given the conversation so far. Implementations:
//   - mockModel (main.go): a scripted planner for the offline demo.
//   - openAIModel (restate_bind.go): a durable restate.Run around an OpenAI call.
type Model interface {
	Decide(ctx context.Context, convo *Conversation) (Decision, error)
}

// RunAgent is THE agent loop. It is a PLAIN Go loop — NOT wrapped in restate.Run.
// Each round it asks the model for a JS program, runs that program in the sandbox
// (which traps back into Go to execute the durable tools), and inspects the
// program's return value: {done:true,...} ends the run, anything else is fed back
// as an observation for the next round. A program that fails is reported to the
// model so it can correct itself.
//
// Where durability lives: ONLY the model call (inside model.Decide) and each tool
// call (inside the tool handler) are restate.Run steps. On crash/replay the whole
// Go loop re-runs deterministically, and every journaled model/tool step returns
// its captured value instead of re-executing. Running the JS program is pure,
// deterministic recomputation and is not itself a durable step.
//
// convo carries the conversation so far (its last turn is normally the current
// user message); RunAgent appends the assistant programs, observations, and the
// final answer to it, so the caller can persist convo.Turns as session state.
//
// logf may be nil.
func RunAgent(ctx context.Context, sb *Sandbox, model Model, convo *Conversation, maxRounds int, logf func(string, ...any)) (string, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// feedback appends a recoverable error to the conversation so the model can
	// try again next round; it never aborts the run.
	feedback := func(round int, msg string) {
		logf("round %d: %s", round, msg)
		convo.Turns = append(convo.Turns, Turn{Role: RoleObservation, Content: clip("ERROR: "+msg, maxObservationChars)})
	}

	for round := 1; round <= maxRounds; round++ {
		convo.Round = round

		dec, err := model.Decide(ctx, convo)
		if err != nil {
			// A malformed decision is deterministic-but-recoverable: feed it back
			// rather than aborting (which, being deterministic, would retry forever).
			if errors.Is(err, ErrBadDecision) {
				feedback(round, err.Error())
				continue
			}
			return "", err
		}
		if strings.TrimSpace(dec.Code) == "" {
			feedback(round, `you returned an empty program; respond with {"code":"..."} or finish by returning {done:true, answer}`)
			continue
		}

		logf("round %d: running generated program (%d bytes)", round, len(dec.Code))
		result, runErr := sb.RunProgram(ctx, dec.Code)
		convo.Turns = append(convo.Turns, Turn{Role: RoleAssistant, Content: dec.Code})

		if runErr != nil {
			// Not fatal: report the failure so the model can fix its program.
			feedback(round, runErr.Error())
			continue
		}
		if answer, done := finalAnswer(result); done {
			logf("round %d: program signaled done", round)
			convo.Turns = append(convo.Turns, Turn{Role: RoleAnswer, Content: answer})
			return answer, nil
		}
		logf("round %d: observation = %s", round, result)
		convo.Turns = append(convo.Turns, Turn{Role: RoleObservation, Content: clip(result, maxObservationChars)})
	}
	return "", ErrMaxRounds
}

// finalAnswer inspects a program's returned JSON. A program signals completion by
// returning {"done": true, "answer": <non-empty JSON>}. A {done:true} with a
// missing, null, or empty answer is NOT treated as done — its value is fed back
// as an observation so the model supplies a real answer instead of silently
// completing with "".
func finalAnswer(result string) (string, bool) {
	var pr struct {
		Done   bool            `json:"done"`
		Answer json.RawMessage `json:"answer"`
	}
	if err := json.Unmarshal([]byte(result), &pr); err != nil || !pr.Done {
		return "", false
	}
	if len(pr.Answer) == 0 || string(pr.Answer) == "null" {
		return "", false // done but no answer — keep going
	}
	var s string
	if json.Unmarshal(pr.Answer, &s) == nil {
		if s == "" {
			return "", false // empty-string answer — keep going
		}
		return s, true // answer was a JSON string — return it unquoted
	}
	return string(pr.Answer), true // answer was an object/array/number
}
