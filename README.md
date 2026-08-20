# HAR MCP Server

> Turn browser network captures into a safe, context‑bounded toolkit for AI agents — redaction by default, never a token bomb.

[![Docker image](https://img.shields.io/badge/docker-ghcr.io%2Ftjamet%2Fhar--mcp-blue?logo=docker&style=flat)](https://github.com/tomkabel/har-mcp/pkgs/container/har-mcp)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/go-1.24.3-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![MCP](https://img.shields.io/badge/MCP-stdio-9b59b6)](https://modelcontextprotocol.io)

A [Model Context Protocol](https://modelcontextprotocol.io) server that lets AI
assistants investigate HTTP traffic captured in
[HAR](https://w3c.github.io/web-performance/) format — without leaking secrets
or drowning the model's context window.

This is a **hardened fork** of [`tjamet/har-mcp`](https://github.com/tjamet/har-mcp).
Where the original served raw entries and unredacted bodies, this version is
rebuilt around two non‑negotiable principles for agent‑side use:

- **Context discipline** — every tool output is *bounded*. A 5 MB capture never
  becomes 1.4 M tokens. Full bodies are content‑addressed and fetched on demand,
  never serialized inline.
- **Privacy by default** — authentication headers, `Set-Cookie`, and sensitive
  query parameters are redacted everywhere they can surface.

---

## Why this fork exists

A HAR file is a complete transcript of everything your browser sent and received:
bearer tokens in `Authorization`, session cookies, signed URLs, and response
bodies that can be megabytes of base64 video. Two things go wrong if you hand
that to an LLM naively:

1. **It blows the context window.** A single `get_request_details` call on a
   large response would dump hundreds of thousands of tokens into the agent's
   context — at the cost of everything else it was reasoning about.
2. **It leaks secrets.** Raw headers and cookies are exactly the credentials an
   agent should never see, let alone echo back.

This fork fixes both at the protocol layer. The result is a server an agent can
use on *any* capture — `load_har` → `list_entries` → `get_request_details` →
`get_response_body` — and stay safe and fast the whole way down.

---

## Features

- **Five focused tools** that form a deliberate triage workflow: index → locate →
  inspect → fetch. No tool returns an unbounded payload.
- **Context‑bounded outputs.** Response bodies and request postData are stored
  in a SHA‑256 content‑addressed `BodyStore` at parse time. `get_request_details`
  returns at most a 4 KB preview plus a `body:<hash>` reference; `get_response_body`
  streams the rest in bounded chunks (default 4 KB, max 64 KB).
- **Redaction everywhere it matters.** `Authorization`, `X-API-Key`,
  `X-Auth-Token`, `Proxy-Authorization`, `Cookie`, and `Set-Cookie` are
  redacted. Sensitive *query values* (`token`, `api_key`, `session`, …) are
  redacted in index URLs, detail URLs, and `queryString`. Auth‑bearing cookies
  are redacted by name.
- **Smart text detection.** Bodies are previewed when the declared MIME type is
  text *or* when a UTF‑8 content sniff says the bytes look like text — so JSON
  served as `application/octet-stream` still previews. Genuinely binary bodies
  (video, images, fonts) stay metadata‑only.
- **Load policies.** `load_har` accepts an optional `policy` to keep matching
  bodies (`excludeMimeTypes`, `maxKeepBytes`) out of the store entirely.
- **Forgiving parser.** Standard `har.HAR` decode first, then a `FlexibleHAR`
  fallback that tolerates float time fields and `content.text` as string or
  base64 — real‑world captures are messy.
- **Multi‑arch container.** `ghcr.io/tjamet/har-mcp` is built for `amd64`,
  `arm64`, `arm/v7`, and `arm/v6` — `CGO_ENABLED=0`, alpine runtime.
- **Stdio‑only JSON‑RPC.** No flags, no HTTP surface, no extra attack area.

---

## Quick start

### Docker (recommended)

```json
{
  "mcpServers": {
    "har-mcp": {
      "command": "docker",
      "args": ["run", "-i", "--rm", "ghcr.io/tjamet/har-mcp"]
    }
  }
}
```

### `go run`

```json
{
  "mcpServers": {
    "har-mcp": {
      "command": "go",
      "args": ["run", "github.com/tomkabel/har-mcp/cmd/har-mcp@main"]
    }
  }
}
```

### Build from source

```bash
go build -o har-mcp ./cmd/har-mcp
```

Then point your MCP client at the built binary with `command: "/path/to/har-mcp"`.

---

## Connecting an MCP client

### Claude Desktop

Add this to your `claude_desktop_config.json` (adjust the path if you built from source):

```json
{
  "mcpServers": {
    "har-mcp": {
      "command": "/path/to/har-mcp"
    }
  }
}
```

### Docker Compose / any MCP‑compatible host

Any client that speaks stdio MCP just needs the command above. There are no
environment variables, no flags, and no network listeners.

---

## Tools

All tools communicate over stdio JSON‑RPC. The intended workflow is a funnel:

```
load_har  ──▶  list_entries  ──▶  get_request_ids  ──▶  get_request_details  ──▶  get_response_body
```

### `load_har`

Load a HAR file from a local path or an `http(s)` URL.

| Parameter | Type   | Notes |
|-----------|--------|-------|
| `source`  | string | **Required.** File path or HTTP URL. |
| `policy`  | object | Optional load policy (see below). |

```json
{
  "source": "/path/to/capture.har",
  "policy": {
    "excludeMimeTypes": ["video/", "image/*"],
    "maxKeepBytes": 1048576
  }
}
```

**Load policy** keeps matching bodies out of the store at parse time. Excluded
bodies still appear in `get_request_details` (previews work from the in‑memory
HAR) but get **no hash** and cannot be fetched with `get_response_body`.

- `excludeMimeTypes` — case‑insensitive MIME prefixes (`"video/"`, `"image/*"`).
- `maxKeepBytes` — bodies larger than this many bytes are not stored (`<= 0` or
  absent: no limit).

### `list_entries`

The primary index. One compact row per entry — method, status, MIME type, size,
timing, and body hash.

| Parameter | Type   | Notes |
|-----------|--------|-------|
| `filter`  | string | Case‑insensitive substring match on the normalized URL (query params included, sensitive values already redacted). |
| `method`  | string | Exact HTTP method filter. |
| `offset`  | number | Row offset (default `0`). |
| `limit`   | number | Max rows (default `200`, max `1000`). |

Returns `{ entries, total, offset, limit, truncated }`. URLs keep query params
— they discriminate requests — but are capped at 100 chars; use
`get_request_details` for the full URL.

### `get_request_ids`

```json
{ "url": "https://api.example.com/users", "method": "GET" }
```

Returns every `request_%d` id for that URL+method (matched on the normalized,
redacted form so it round‑trips from an index row).

### `get_request_details`

```json
{ "request_id": "request_0" }
```

Full details for one request. Auth headers and sensitive query values are
redacted. Text bodies up to 4 KB are fully inlined as `textPreview`; larger
bodies return the first 4 KB (ending with a `[TRUNCATED]` marker) plus a
`content.hash` you pass to `get_response_body`.

### `get_response_body`

```json
{
  "hash": "body:3f2a1c9d8e7b6a5f4c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b",
  "offset": 0,
  "limit": 4096
}
```

Fetch a bounded chunk of a stored body by hash. Text bodies return decoded bytes
between `offset` and `offset+limit`; binary bodies return metadata only.

---

## How it works

```
        ┌──────────────┐
        │   HAR file   │  (path or URL)
        └──────┬───────┘
               │ load_har (+ optional policy)
               ▼
        ┌──────────────┐   two‑pass parse:
        │    Parser    │   har.HAR  →  FlexibleHAR fallback
        └──────┬───────┘
               │ index bodies at parse time
               ▼
        ┌──────────────┐   SHA‑256 content‑addressed
        │  BodyStore   │   dedup; redaction applied to refs
        └──────┬───────┘
               │
   ┌───────────┼─────────────────────────────────────┐
   ▼           ▼                                       ▼
list_entries  get_request_details                  get_response_body
(compact      (≤4 KB preview + hash,               (bounded chunk by hash)
 index)        auth/query redacted)
```

Key invariants:

- **The store is populated once, at parse time.** A new `load_har` resets the
  store and policy, so an old capture's bodies are no longer fetchable.
- **Reads never mutate the store.** `get_*` calls only look references up by
  hash — excluding a body at load time is the *only* way it becomes unfetchable.
- **Truncation is explicit.** Previews cut at a rune boundary (never mid‑rune)
  and end with a `[TRUNCATED]` footer, so a cut document reads as intentional
  rather than broken.

---

## Client‑side usage notes

- **Never attach a `.har` file raw to an agent context** (e.g. `@ capture.har`):
  a 5.7 MB capture is roughly 1.4 M tokens. The MCP tools are the only supported
  ingestion path.
- **No MCP server handy?** Pre‑trim with `jq`:
  - Drop all response bodies and postData text while keeping headers/status:
    ```bash
    jq 'del(.log.entries[].response.content.text, .log.entries[].request.postData.text)' capture.har > trimmed.har
    ```
  - Or keep small bodies and only drop responses over 16 KB:
    ```bash
    jq '(.log.entries[] | select(.response.content.size > 16384) | .response.content.text) = null' capture.har > trimmed.har
    ```

---

## Development

### Run the tests

```bash
go test ./...
go test -v ./...      # verbose (CI form)
go vet ./...
go fmt ./...
```

### Project structure

```
.
├── cmd/
│   └── har-mcp/        # MCP server entrypoint, tool definitions & handlers
├── pkg/
│   └── har/            # HAR parsing & storage library
│       ├── body_store.go
│       ├── parser.go
│       ├── custom_types.go
│       └── parser_test.go
├── Dockerfile          # multi‑stage, CGO_ENABLED=0, alpine runtime
├── example.har         # sample capture for manual testing
├── go.mod
└── README.md
```

### Dependencies

- [`github.com/google/martian/har`](https://github.com/google/martian) — HAR model & decoding
- [`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) — MCP server framework
- [`github.com/stretchr/testify`](https://github.com/stretchr/testify) — test assertions

---

## License

[MIT](./LICENSE) © Thibault Jamet and contributors.
