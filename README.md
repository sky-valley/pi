# pi (Go)

> **A pure-Go (golang) port of [pi](https://github.com/earendil-works/pi)** — the `@earendil-works/pi-ai`
> unified multi-provider LLM API, the `pi-agent-core` agent loop, and the `pi-coding-agent` CLI —
> running natively in Go with **no Node.js runtime**. Looking for "pi for Go" / a "pi-go"? This is it.

A pure-Go port of [pi](https://github.com/earendil-works/pi), Mario Zechner's agent
harness and self-extensible coding agent — built so heavy Go shops can run pi natively
without wrapping a Node.js process.

It mirrors pi's architecture as faithful, idiomatic Go: the same unified streaming
protocol, the same agent loop semantics, the same tool contracts, and the same built-in
coding tools — leaning into Go's strengths (channels for the event stream, `context.Context`
for cancellation, goroutines for parallel tool execution).

## Layout

| Package | Mirrors | What it is |
|---|---|---|
| [`ai`](ai/) | `@earendil-works/pi-ai` | Unified multi-provider LLM API: message/content/tool/model types, the channel-based `EventStream`, JSON-Schema tool validation, model catalog + cost, provider registry. |
| [`ai/providers`](ai/providers/) | `pi-ai/providers` | Concrete providers, all with real `net/http` SSE: **faux** (deterministic test double), **Anthropic** Messages, **OpenAI** Chat Completions, **OpenAI** Responses (GPT-5/o-series), **Google** Gemini. These four wire APIs cover ~816 of pi's 999 catalog models. |
| [`agent`](agent/) | `@earendil-works/pi-agent-core` | The agent runtime: low-level `AgentLoop`, the stateful `Agent` with tool calling, hooks, steering/follow-up queues, sequential + parallel tool execution. |
| [`coding`](coding/) | `@earendil-works/pi-coding-agent` | The coding agent: the eight built-in tools (`read`, `write`, `edit`, `bash`, `powershell`, `ls`, `find`, `grep`), the system prompt builder (with AGENTS.override.md/AGENTS.md/CLAUDE.md context files + Agent Skills), model resolver, the session runner, and JSONL session persistence. |
| [`cmd/pi`](cmd/pi/) | `pi` CLI | The `pi` command — print mode, interactive REPL with slash commands, session resume, `pi models`, `pi sessions`. |
| [`env`](env/) | `@earendil-works/pi-env` | A remote execution environment over SSH: `ConnectSSH` deploys a small daemon to the remote host and `RemoteExecutionEnv` runs files, commands, terminals and watches through it. A separate module (`go get github.com/sky-valley/pi/env`), with its own tags (`env/vX.Y.Z`). The daemon is ported to Go as well (`env/daemon`, `env/cmd/pi-env`), and its builds for eight platforms are embedded, so nothing needs Node or Rust on either end. |

Alongside those, [`difftest/`](difftest/) is the differential harness: for
identical inputs it captures the exact HTTP request body that **real pi** builds
and that this port builds, canonicalizes both, and diffs them. pi is ground
truth. It is a separate Go module, so `go get github.com/sky-valley/pi` does not
download it.

## Build & test

```bash
go build ./...
go test ./...          # race-clean; live-API tests skip without keys
go build -o pi ./cmd/pi
```

## Usage

```bash
export ANTHROPIC_API_KEY=sk-ant-...        # or OPENAI_API_KEY, GROQ_API_KEY, …

pi "explain what this repo does"            # single prompt (print mode)
pi                                          # interactive REPL (slash commands: /model, /think, /new, …)
pi -m anthropic/claude-opus-4-5 "..."       # pick a model
pi -m openai/gpt-5 --think high "..."       # OpenAI Responses API + reasoning
pi --think high "refactor foo.go"           # set reasoning level
pi -c "and now add tests"                   # continue the most recent session
pi models claude                            # list catalog models matching "claude"
pi sessions                                 # list saved sessions for this directory
```

Sessions are persisted as append-only JSONL under `~/.pi/agent/sessions/--<cwd>--/`,
matching pi's on-disk format (`-c`/`--continue` resumes the latest; `--resume PATH`
resumes a specific file). Project context files (`AGENTS.md`/`CLAUDE.md` — an
`AGENTS.override.md` replaces both within its directory — discovered up
the directory tree) and Agent Skills (`.pi/skills/*/SKILL.md`) are folded into the system
prompt exactly as upstream pi does.

The model catalog (999 models across 35 providers) is embedded from pi's generated
catalog. API keys are resolved from the same environment variables pi uses
(`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, …).

### Provider attribution

Matching upstream pi, requests to OpenRouter, NVIDIA, Cloudflare, Vercel AI
Gateway, and OpenCode carry attribution headers by default (`HTTP-Referer:
https://pi.dev`, `X-OpenRouter-Title: pi`, `x-opencode-session`, etc.). Set
`PI_TELEMETRY=0` to disable them, or override any of them per-request via
`SessionOptions.Headers` / `model.Headers` (both take precedence). Other
providers (Anthropic, OpenAI, Google) are unaffected.

## Using the library

```go
import "github.com/sky-valley/pi/coding"

model, _ := coding.ResolveModel("anthropic/claude-sonnet-4-5")
sess, err := coding.NewSession(coding.SessionOptions{Model: model}) // ANTHROPIC_API_KEY
if err != nil {
    return err
}
text, err := sess.RunPrint(ctx, os.Stdout, "list the Go files and summarize them")
```

The built-in API providers (Anthropic, OpenAI Chat Completions + Responses,
Google) register themselves when the `coding` package is imported — the same
import side effect upstream pi has when importing `@earendil-works/pi-ai`. If
you use the `ai` package directly without `coding`, blank-import the providers:
`import _ "github.com/sky-valley/pi/ai/providers"`.

### SDK usage (embedding pi in your app, e.g. with OpenAI)

`coding.NewSession` is the SDK facade (modeled on pi's `createAgentSession`). It
returns a structured `RunResult` rather than writing to a writer, supports custom
tools, streaming via `Subscribe`, and exposes the full provider control surface.

```go
model, _ := coding.ResolveModel("openai/gpt-5")            // uses the Responses API

sess, err := coding.NewSession(coding.SessionOptions{
    Model: model,                                           // auth: OPENAI_API_KEY

    // Tool selection (allow/deny/none + your own tools), mirrors createAgentSession.
    // Entries may be * patterns; a list of only +name/-name entries edits the
    // default set instead, and mixing the two forms is an error.
    ToolNames:   []string{"read", "bash"},                 // or NoTools: coding.NoToolsAll
    CustomTools: []agent.AgentTool{myTool},

    // Per-request provider controls.
    ThinkingLevel:  agent.ThinkHigh,                        // clamped to model support
    Temperature:    ptr(0.2),
    MaxTokens:      ptr(2000),
    SessionID:      "chat-42",                              // enables OpenAI prompt caching
    Headers:        map[string]string{"OpenAI-Organization": "org-..."},
    MaxRetries:     4,                                      // backoff honors Retry-After
    TimeoutMs:      120000,
    OnPayload:      func(p any, m *ai.Model) (any, error) { return p, nil }, // inspect/rewrite request
    Compaction:     coding.DefaultCompactionSettings(),     // auto context-window compaction
})
if err != nil {
    return err                                             // e.g. an invalid ToolNames list
}

sess.Subscribe(func(ctx context.Context, e agent.AgentEvent) error {
    if e.Type == agent.EvMessageUpdate && e.AssistantMessageEvent != nil &&
        e.AssistantMessageEvent.Type == ai.EventTextDelta {
        fmt.Print(e.AssistantMessageEvent.Delta)            // stream tokens to your UI
    }
    return nil
})

res, err := sess.Run(ctx, "refactor foo.go and add a test")
// res.Text, res.ToolCalls, res.Messages, res.StopReason,
// res.Usage (tokens + $cost, aggregated across the whole tool loop)
```

A session holds no API key, as pi's `createAgentSession` takes none: each
request resolves its provider's auth itself, from the environment by default
(Cloudflare's account and gateway ids too). To hand it a key, set it on the
session's model runtime, as pi's `modelRuntime.setRuntimeApiKey` does:

```go
credentials := coding.NewRuntimeCredentials(nil)
credentials.SetRuntimeApiKey(model.Provider, key)
sess, err := coding.NewSession(coding.SessionOptions{
    Model:  model,
    Models: ai.BuiltinModels(&ai.CreateModelsOptions{Credentials: credentials}),
})
```

Mid-session you can `sess.SetModel`, `sess.SetThinkingLevel`, `sess.Steer`,
`sess.FollowUp`, `sess.Continue`, `sess.Abort`, and persist/resume via
`coding.StartSession` + `sess.Record` / `coding.LoadSessionTree` +
`sess.LoadBranch(tree.BuildProjection())` (which also resumes the session's
compaction). A complete
runnable example is in [`examples/sdk`](examples/sdk/main.go).

## Fidelity notes

This port reproduces pi's core engine and coding agent faithfully:

- **Streaming protocol** — the `start → *_delta → done/error` event protocol, the
  `EventStream` buffering/result semantics, and partial-message snapshots match pi exactly
  (validated by porting pi's own faux provider, which is the protocol's reference spec).
- **Agent loop** — turn lifecycle, tool batching, sequential/parallel execution ordering,
  `terminate` early-stop, steering/follow-up queues, and the `before/afterToolCall` hooks
  follow `agent-loop.ts` line-for-line.
- **Tools** — schemas, descriptions, truncation rules (2000 lines / 50KB), and the edit
  tool's unique/non-overlapping match semantics match the originals.
- **Providers** — Anthropic, OpenAI (both Chat Completions with full `OpenAICompletionsCompat`
  auto-detection and Responses), and Google request construction, SSE parsing, cache control,
  thinking/reasoning, tool streaming, retries (honoring `Retry-After`), usage and cost are
  ported from the TS providers and tested against mock SSE servers replicating the real wire format.
- **SDK surface** — `coding.NewSession` mirrors `createAgentSession` (tool allow/deny/none +
  custom tools, thinking-level clamping), `Run` returns a structured result with aggregated
  usage/cost, and the full provider control surface (temperature, maxTokens, headers, retries,
  prompt caching, `onPayload`/`onResponse`, context compaction) is plumbed end-to-end.

Not yet ported from the (much larger) upstream: the interactive TUI and the host/mode layer
that drives it, the extensions runtime, and the wire APIs whose transport or credential chain
Go cannot reach without a third-party dependency (AWS Bedrock's SigV4 + eventstream framing,
OpenAI Codex's WebSocket + zstd). OAuth token acquisition, Azure's Responses API (its Chat
Completions deployments are served) and the Mistral/Vertex/Radius adapters are in scope and
queued — see `docs/UPSTREAM.md`. The architecture leaves clean seams
for all of these: providers register through `ai.RegisterApiProvider`, tools are plain
`agent.AgentTool` values, and the agent loop is provider- and tool-agnostic.

**Project trust.** pi discovers project-local resources under `<cwd>/.pi` only once a project
is trusted, and answers *untrusted* for any host with no UI to prompt with. This port is
headless, so it takes that same default: `<cwd>/.pi/skills` is **not** scanned unless you opt
in with `SessionOptions.TrustProject` (or call `coding.LoadSkillsWithTrust`). Opt in only for
repositories you trust — a project skill's name and description are rendered into the system
prompt. The interactive trust prompt and trust store are host surface and remain unported.

## License

MIT, matching upstream pi.
