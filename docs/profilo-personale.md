# Profilo personale e compatibilità legacy

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
| Web search | yes, with citations | with `codex_web_search` | no |
| Structured output (`json_schema`) | yes | yes | passed through |
| Reasoning effort | yes | yes | passed through |
| Output budget | applied by the provider | checked afterwards | passed through |
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
  or instructions), and every feature the installed codex reports as
  enabled is turned off: shell, file and image tools, plugins, browser and
  computer use, sub-agents, and whatever a later version adds. Web search
  needs codex's tool host back on, so it is refused unless the operator
  sets `codex_web_search`. If the features cannot be listed the
  request fails rather than run unprotected, and `codex_extra_args` that
  would widen the sandbox are refused (`codex_isolate`).

This is defence in depth on top of tools that were not built to face
untrusted prompts, and it depends on the CLIs' own switches. To serve
clients you do not trust, also run the gateway in a container or as a user
with nothing worth reading.

**Fallback.** A model alias lists several targets. If one fails before
sending anything, the next is tried (see *Errors* for the exceptions):

```json
{ "models": { "auto": ["claude/sonnet", "codex", "ollama/llama3.3"] } }
```

**Concurrency.** At most `max_concurrency` backend requests run at once and
at most `max_queue` wait for a slot; beyond that the answer is `503`
(`gateway_busy`, with `Retry-After`).

## Output budget, finish reasons and errors

What a client needs to tell a whole answer from a cut or failed one is the
same on the three APIs.

### Output budget

| API | Parameter |
|---|---|
| Chat Completions | `max_completion_tokens`, or the older `max_tokens` |
| Responses | `max_output_tokens` |
| Messages | `max_tokens` |

The budget is optional on all three and must be a positive integer. A
request without one behaves as before: the gateway never invents a budget.
Chat Completions takes either name, or both with the same value; two
different values are a `400`.

| Backend | What the budget does |
|---|---|
| claude | Set as `CLAUDE_CODE_MAX_OUTPUT_TOKENS` for the CLI, which passes it to the provider: each generation is cut at that many tokens, thinking included (the CLI lowers a value above the model's own maximum). The answer comes back with what was written and the finish reason `length`. |
| codex | Codex has no such setting. The answer is held until the turn reports its usage; if its output tokens, reasoning included, exceed the budget it is refused whole with `422 output_limit_exceeded`. The tokens were spent all the same. |
| upstream | Forwarded, under the name the client used on Chat Completions and as `max_tokens` otherwise, unless the upstream's `max_tokens_field` fixes the name. The upstream applies it. |

The text of an answer is never trimmed by the gateway to fit a budget.

When a generation is cut, the claude CLI by itself asks the model to resume
from the cut, up to a few times, and reports the whole as a success. The
gateway stops the CLI at the first cut instead, with or without a budget
(the model's own maximum cuts too): the pieces of a resumed answer are not
one answer.

### Finish reasons

| The answer | Chat Completions `finish_reason` | Responses `status` | Messages `stop_reason` |
|---|---|---|---|
| ended normally | `stop` | `completed` | `end_turn` |
| calls functions | `tool_calls` | `completed` | `tool_use` |
| was cut at the output limit | `length` | `incomplete`, `incomplete_details.reason: max_output_tokens` | `max_tokens` |
| was stopped by a content filter or a refusal | `content_filter` | `incomplete`, `incomplete_details.reason: content_filter` | `refusal` |

A cut answer is a `200` with the text written so far: check the reason
before using it. With streaming, a Responses stream then ends with
`response.incomplete` instead of `response.completed`.

Every backend has to say how its answer ended. One that does not — a
stream closed before its finish reason, a CLI that exits before its result,
a finish reason the gateway does not know — is an error
(`backend_incomplete_response`), never a `stop`.

With claude the answer is whole only if the last message of the turn
stopped for one of the provider's terminal reasons: `end_turn` or
`stop_sequence` (`stop`), `max_tokens` or `model_context_window_exceeded`
(`length`), `refusal` (`content_filter`). That the CLI reports a success is
not enough. `pause_turn` and `tool_use` ask for another round, which the CLI
runs itself: a result that still shows one was left there, and is
incomplete, like `compaction`, an unknown reason or none. The one exception
is a turn that ends on the tool use carrying the structured output of the
result.

### Errors

Errors are `{"error": {"message", "type", "code"}}` (inside
`{"type": "error", ...}` on the Messages API). `code` is stable and meant
for programs; `message` is a fixed text per code. What a backend said about
its failure may quote credentials or the conversation, so it is never sent
to the client: the log records the error code at every log level.

| Status | `code` | |
|---|---|---|
| 400 | `invalid_request_error` | the request is wrong |
| 400 | `backend_invalid_request` | the backend refused the request as it is (model, parameters) |
| 401 | `authentication_error` | the gateway's API key is missing or wrong |
| 403 | `permission_denied` | the key cannot use that model |
| 413 | `request_too_large` | the request body is too large for the gateway |
| 413 | `context_length_exceeded` | the input does not fit the model |
| 422 | `output_limit_exceeded` | the answer exceeded the output budget and was not returned |
| 429 | `gateway_rate_limit` | the key's `rpm` limit; `Retry-After` |
| 429 | `backend_rate_limit` | the provider's temporary limit; `Retry-After` when it gave one |
| 429 | `backend_quota_exceeded` | the provider's allowance is used up (usage limit, credit, billing); `Retry-After` when the reset time is known |
| 502 | `backend_authentication_error` | the backend's own login or key was refused: the operator's to fix |
| 502 | `backend_incomplete_response` | the answer stopped before its end |
| 502 | `backend_error` | any other failure of the backend |
| 503 | `gateway_busy` | no free slot; `Retry-After` |
| 503 | `backend_overloaded` | the provider is overloaded; `Retry-After` when it gave one |
| 504 | `backend_timeout` | `request_timeout`, or the provider's own timeout |

`401` is always about the client's key: a backend that is not logged in is
a `502`. Quota and rate limit are told apart only on evidence: an upstream's
`insufficient_quota`, the claude CLI's rejected usage window with its reset
time, the wording of a CLI's error. Without it a `429` is a rate limit.

**Streaming.** A request that fails before producing anything gets the
status and headers above even with `"stream": true`: the event stream
starts with the first event of the answer (or after five seconds). A
failure after that can only be reported inside the stream, as the last
event: `data: {"error": ...}` with no `finish_reason` and no `[DONE]` on Chat
Completions, `response.failed` on Responses, `error` with no `message_stop`
on Messages. What was streamed before it is not a complete answer.

So a rate limit that the backend reports after its first output arrives
under status `200`, with no `Retry-After` header. The error object carries
the same `code` (`backend_rate_limit`, `backend_quota_exceeded`, ...) and,
when the wait is known, `retry_after` in seconds; that field is in the
error body of the plain responses too, beside the header.

**Fallback and retries.** The gateway never repeats a request on the same
target. The next target of an alias is tried only if the failed one had
sent nothing, so an answer never mixes two backends, and not at all when
the answer exceeded the output budget, which would exceed it elsewhere too.

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
| `upstreams` | OpenAI-compatible APIs: `name`, `base_url`, `api_key_env`, `models`, and `max_tokens_field` (`max_tokens` or `max_completion_tokens`) when the API takes the output budget under one name only |
| `claude_isolate` | ignore the user's claude settings, default `true` |
| `claude_web_fetch` | with web search, also let claude fetch URLs from this machine, default `false` |
| `codex_web_search` | let an isolated codex search the web, which keeps its tool host on, default `false` |
| `codex_isolate` | run codex with an empty home and every enabled feature turned off, default `true` |
| `claude_extra_args`, `codex_extra_args` | arguments added to every command line |
| `response_ttl`, `response_max` | how long and how many conversations are kept for `previous_response_id` |
| `metrics_public` | serve `/metrics` without a key, default `true` |

### Security

The CLI profile requires a loopback listener. For shared access, use the HTTP production profile and project keys. Keys are accepted as
`Authorization: Bearer <key>` or `x-api-key: <key>`.

Turning `codex_isolate` off, or `claude_web_fetch` or `codex_web_search` on,
widens what every client with a key can reach. The first two give
a way to the machine's files or to its network: see *Isolation*.

`/health`, `/metrics` and `/mcp/{id}` take no key. The MCP paths carry an
unguessable id and only describe the functions of a request in progress.

## Running

```sh
./llm-gateway -detach              # background; prints the pid and the log file
./llm-gateway -config config.json
make test                          # go vet + tests, against fake CLIs
```

Use the native binary on the host for personal CLI logins. See the [production quick start](../README.md) for Docker Compose.

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

- Sampling parameters (`temperature`, `top_p`, `stop`, ...) are accepted and
  ignored: the CLIs do not expose them.
- An output budget changes the claude command's environment, so requests
  with different budgets do not share warm processes.
- With codex an answer over the budget costs its tokens and returns nothing.
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
| `errors.go` | finish reasons, error classification and codes |
| `pool.go` | warm processes |
| `prompt.go`, `images.go`, `tools.go`, `textcalls.go`, `search.go` | prompt building, images, functions, citations |
| `mcp.go` | the MCP server for the client's functions |
| `store.go`, `metrics.go` | conversations, rate limits, metrics |
