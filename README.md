# llm-gateway

An HTTP server that speaks the **OpenAI** (Chat Completions, Responses) and
**Anthropic** (Messages) APIs and answers with the `claude` and `codex`
command-line tools, or with any OpenAI-compatible API.

Point an OpenAI or Anthropic SDK at it and the requests are served by the
CLIs installed and logged in on the machine, with streaming, function
calling, images, web search and structured output.

One static binary, standard library only.

> **Check the terms first.** The CLIs run under your own Claude and ChatGPT
> accounts. Exposing a subscription as a shared API may be against the
> providers' terms of use: check them before serving anyone but yourself.

## Quick start

```sh
go build -o llm-gateway .
./llm-gateway                      # listens on 127.0.0.1:8080
```

```sh
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model": "claude/sonnet", "messages": [{"role": "user", "content": "Hello!"}]}'
```

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8080/v1", api_key="unused")
reply = client.chat.completions.create(
    model="claude/sonnet",
    messages=[{"role": "user", "content": "Hello!"}],
)
```

```python
import anthropic

client = anthropic.Anthropic(base_url="http://127.0.0.1:8080", api_key="unused")
reply = client.messages.create(
    model="claude/sonnet", max_tokens=1024,
    messages=[{"role": "user", "content": "Hello!"}],
)
```

Requirements: Go 1.24+ to build; `claude` (Claude Code) and/or `codex` on the
`PATH`, already logged in.

## Endpoints

| Endpoint | |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions, with streaming |
| `POST /v1/responses` | OpenAI Responses, with streaming and `previous_response_id` |
| `POST /v1/messages` | Anthropic Messages, with streaming |
| `GET /v1/models` | the models the API key can use |
| `GET /health` | status, backends, warm pool, queue |
| `GET /metrics` | Prometheus metrics |
| `POST /mcp/{id}` | internal: the client's functions, served to the CLIs over MCP |

Every `/v1` endpoint is also served without the prefix.

## Models

| Model | Answered by |
|---|---|
| `claude` | the claude CLI with its default model |
| `claude/<model>` | the claude CLI with `--model <model>` (`opus`, `sonnet`, `haiku`, or a full name) |
| `codex` | the codex CLI with its default model |
| `codex/<model>` | the codex CLI with `--model <model>` |
| `<upstream>/<model>` | an OpenAI-compatible API of the configuration |
| `<alias>` | the targets of the alias, tried in order (fallback) |

## Features

| | claude | codex | upstream |
|---|---|---|---|
| Streaming | text deltas | whole messages | text deltas |
| Function calling | native, over MCP | emulated with a JSON schema | passed through |
| Images | data URLs and http(s) URLs | data URLs | passed through |
| Web search | yes, with citations | yes, with citations | no |
| Structured output (`json_schema`) | yes | yes | passed through |
| Reasoning effort | yes | yes | passed through |
| Warm processes | yes | no | n/a |

**Function calling.** The functions belong to the client: the gateway never
runs them. With claude they are published as the tools of an MCP server the
CLI connects to; when the model calls one, the gateway stops the CLI and
returns the call to the client, which answers with the result in the next
request. With codex the reply is shaped by a JSON schema holding the text and
the calls. If a model writes a call as text (`<tool_call>`, `<invoke>`)
instead of using its tool, the gateway recognizes it and returns a real call.

**Conversations.** The CLIs take one user turn, so a conversation is sent as
a transcript. `/v1/responses` keeps the conversation of each response in
memory (`response_ttl`, `response_max`): send `previous_response_id` to
continue it. A stored conversation can be continued only with the API key
that created it. `"store": false` keeps a response out of the store.

**Warm processes.** Starting the claude CLI takes a moment. After a request
the gateway starts another process with the same command line (model, system
prompt, tools, schema) and keeps it idle for the next request of that kind.
`warm` is the number of idle processes kept, `warm_idle` how long they wait.

**Isolation.** Both CLIs are agents that can act on the machine, and the
prompt comes from the client: what they may do is cut down to answering.

- claude runs in an empty directory with no built-in tools and no MCP
  servers but the client's functions, and ignores the user's own settings,
  so that personal hooks, plugins and instructions stay out of the answers
  (`claude_isolate`). With web search it gets `WebSearch`, which runs on the
  provider's side. `WebFetch` runs on this machine and can reach whatever
  it can, internal addresses included: it is off unless `claude_web_fetch`
  is set.
- codex runs in a read-only sandbox, which alone still lets it read every
  file the user can. So it also gets a home of its own, holding a link to
  the credentials and nothing else (no configuration, MCP servers, plugins
  or instructions), and the features that reach the machine (shell, plugins,
  browser and computer use, sub-agents, ...) are disabled (`codex_isolate`).

This is defence in depth on top of tools that were not built to face
untrusted prompts, and it depends on the CLIs' own switches. To serve
clients you do not trust, also run the gateway in a container or as a user
with nothing worth reading.

**Fallback.** A model alias lists several targets. If one fails before
sending anything, the next is tried:

```json
{ "models": { "auto": ["claude/sonnet", "codex", "ollama/llama3.3"] } }
```

**Concurrency.** At most `max_concurrency` backend requests run at once and
at most `max_queue` wait for a slot; beyond that the answer is `503`.

## Configuration

Every setting comes, in order of precedence, from a flag, an environment
variable, the JSON file given with `-config` (or `CONFIG_FILE`), or its
default.

| Flag | Environment | File | Default | |
|---|---|---|---|---|
| `-addr` | `LISTEN_ADDR` | `listen_addr` | `127.0.0.1:8080` | listen address |
| `-claude-bin` | `CLAUDE_BIN` | `claude_bin` | `claude` | claude CLI path |
| `-codex-bin` | `CODEX_BIN` | `codex_bin` | `codex` | codex CLI path |
| `-workdir` | `WORK_DIR` | `work_dir` | an empty temp dir | working directory of the CLIs |
| `-api-key` | `API_KEY` | `api_key` | none | require `Authorization: Bearer <key>` |
| `-max-concurrency` | `MAX_CONCURRENCY` | `max_concurrency` | `4` | concurrent backend requests |
| `-max-queue` | `MAX_QUEUE` | `max_queue` | `64` | requests waiting for a slot |
| `-timeout` | `REQUEST_TIMEOUT` | `request_timeout` | `300` | seconds per request |
| `-warm` | `WARM` | `warm` | `1` | idle claude processes, `0` disables |
| `-warm-idle` | `WARM_IDLE` | `warm_idle` | `300` | seconds a warm process waits |
| `-self-url` | `SELF_URL` | `self_url` | `http://127.0.0.1:<port>` | URL the CLIs reach this server at |
| `-log-level` | `LOG_LEVEL` | `log_level` | `info` | `debug`, `info`, `warn`, `error` |
| `-detach` | | | | run in the background |
| `-log-file` | `LOG_FILE` | `log_file` | user cache dir | log of `-detach` |

File-only settings (see [`config.example.json`](config.example.json)):

| Setting | |
|---|---|
| `api_keys` | several keys, each with a `name`, a `key` or `key_env`, an optional `rpm` limit and the `backends` it may use |
| `backends` | the CLI backends to enable, default `["claude", "codex"]` |
| `models` | aliases: a name and the targets tried in order |
| `upstreams` | OpenAI-compatible APIs: `name`, `base_url`, `api_key_env`, `models` |
| `claude_isolate` | ignore the user's claude settings, default `true` |
| `claude_web_fetch` | with web search, also let claude fetch URLs from this machine, default `false` |
| `codex_isolate` | run codex with an empty home and its machine-reaching features disabled, default `true` |
| `claude_extra_args`, `codex_extra_args` | arguments added to every command line |
| `response_ttl`, `response_max` | how long and how many conversations are kept for `previous_response_id` |
| `metrics_public` | serve `/metrics` without a key, default `true` |

### Security

The default address is the loopback interface. To serve other machines set
`-addr :8080` **and** at least one API key: without a key, anyone who reaches
the server uses your CLI accounts. Keys are accepted as
`Authorization: Bearer <key>` or `x-api-key: <key>`.

Turning `codex_isolate` off, or `claude_web_fetch` on, gives every client
with a key a way to the machine's files or to its network: see *Isolation*.

`/health`, `/metrics` and `/mcp/{id}` take no key. The MCP paths carry an
unguessable id and only describe the functions of a request in progress.

## Running

```sh
./llm-gateway -detach              # background; prints the pid and the log file
./llm-gateway -config config.json
make test                          # go vet + tests, against fake CLIs
```

With Docker the CLIs need their credentials: mount the directories where
they keep them.

```sh
docker build -t llm-gateway .
docker run --rm -p 8080:8080 -e API_KEY=change-me \
  -v ~/.claude:/home/node/.claude -v ~/.claude.json:/home/node/.claude.json \
  -v ~/.codex:/home/node/.codex \
  llm-gateway
```

On macOS the claude CLI keeps its login in the Keychain, which a container
cannot read: log in inside the container (`docker run -it ... claude`) or
run the gateway on the host.

## Metrics

| Metric | |
|---|---|
| `llm_gateway_requests_total{endpoint,backend,status}` | requests by outcome (`ok`, `error`, `timeout`, `canceled`) |
| `llm_gateway_request_duration_seconds{backend}` | histogram of the time to answer |
| `llm_gateway_tokens_total{backend,kind}` | input and output tokens reported by the backends |
| `llm_gateway_fallbacks_total{from}` | requests passed on to the next target |
| `llm_gateway_rejected_total{reason}` | `unauthorized`, `rate_limited`, `queue_full`, `queue_timeout` |
| `llm_gateway_inflight_requests`, `llm_gateway_queued_requests` | load |
| `llm_gateway_warm_processes`, `llm_gateway_stored_responses` | state |

## Limits

- Sampling parameters (`temperature`, `max_tokens`, `stop`, ...) are accepted
  and ignored: the CLIs do not expose them.
- Codex streams whole messages, not deltas, and takes images only as data URLs.
- Replies with emulated functions (codex) are sent when complete.
- Conversations for `previous_response_id` live in memory: a restart loses them.
- No embeddings, files or batch endpoints.
- Token counts include what the CLIs add to the prompt.

## Layout

| File | |
|---|---|
| `main.go`, `config.go`, `detach_*.go` | startup, configuration, background mode |
| `server.go` | routes, keys, queue, fallback, Chat Completions |
| `responses.go`, `anthropic.go` | Responses and Messages APIs |
| `backend.go`, `claude.go`, `codex.go`, `upstream.go` | the backends |
| `pool.go` | warm processes |
| `prompt.go`, `images.go`, `tools.go`, `textcalls.go`, `search.go` | prompt building, images, functions, citations |
| `mcp.go` | the MCP server for the client's functions |
| `store.go`, `metrics.go` | conversations, rate limits, metrics |
