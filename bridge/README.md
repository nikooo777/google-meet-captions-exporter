# meet-transcript bridge

Local MCP server that receives the live Google Meet transcript pushed by the
[extension](../) and exposes it to any MCP client (Claude Desktop/Code, Codex, …)
over stdio. Design notes: `../aidocs/003_live_transcript_mcp.md`.

## How it works

A Chrome extension cannot run a server, so the extension **pushes** the transcript
to this process instead:

```
extension service worker  ──HTTP POST /ingest──▶  meet-bridge  ──stdio MCP──▶  assistant
```

`meet-bridge` keeps the latest transcript snapshot in memory (replaced on every
POST, so caption back-tracking is absorbed for free) and serves three tools. It
accepts ingest on loopback only and speaks MCP on stdin/stdout — the MCP client
launches it.

## Build

```sh
cd bridge
go build -o ../meet-bridge ./cmd/meet-bridge
```

Build from inside `bridge/` — the Go module is rooted there, so
`go build ./cmd/meet-bridge` from the repo root fails.

## Configuration

The MCP client launches the binary; you don't run it by hand. It reads:

| Setting | Source | Default |
|---|---|---|
| Auth token | `MEET_BRIDGE_TOKEN` env | none — **required**; without it every `/ingest` returns 401 |
| Ingest address | `--addr` flag or `MEET_BRIDGE_ADDR` env | `127.0.0.1:6338` |

The address must be loopback; the bridge refuses to bind any other interface.
Generate a token (e.g. `openssl rand -hex 16`) and use the **same value** in the
extension popup.

## Wire it into an assistant

### Claude Code

```sh
claude mcp add meet-transcript --env MEET_BRIDGE_TOKEN=your-shared-token -- /absolute/path/to/meet-bridge
```

### Claude Desktop

In `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "meet-transcript": {
      "command": "/absolute/path/to/meet-bridge",
      "env": { "MEET_BRIDGE_TOKEN": "your-shared-token" }
    }
  }
}
```

### Codex

```sh
codex mcp add meet-transcript --env MEET_BRIDGE_TOKEN=your-shared-token -- /absolute/path/to/meet-bridge
```

which writes to `~/.codex/config.toml`:

```toml
[mcp_servers.meet-transcript]
command = "/absolute/path/to/meet-bridge"
env = { MEET_BRIDGE_TOKEN = "your-shared-token" }
```

### Extension

Open the popup, tick **Live transcript bridge (MCP)**, set the URL
(`http://127.0.0.1:6338`) and paste the same token.

## Tools

| Tool | Args | Returns |
|---|---|---|
| `get_recent` | `meeting_id?`, `minutes?`, `count?` | Tail of the transcript. `count` wins over `minutes`; default last 12. `minutes` filters on each entry's last-updated time. |
| `get_since` | `meeting_id?`, `since?` | Incremental delta: only lines added or revised since a cursor. First call omits `since` (returns the full transcript); the result's first line is `cursor: <ms>`. Pass that back as `since` on the next call to get just what changed, plus a fresh cursor. |
| `get_transcript` | `meeting_id?` | The full transcript. |
| `get_status` | — | Active meetings, entry counts, last-update times, and whether ingest is bound. |

`meeting_id` is optional when only one call is active. A trailing ` …` on the last
line marks an entry that may still be changing (Meet revises captions as people
speak).

**Delta pulling with `get_since`.** Because Meet revises a line in place while it's
being spoken, an entry's last-updated time advances each time it changes. `get_since`
returns every entry whose update time is newer than your `cursor`, so a still-settling
line reappears (with updated text) on each call until it stops changing, then drops out.
Loop: call with no `since` once for the baseline, then keep calling with the previous
`cursor` to stay current without re-reading the whole transcript.

## Notes / limits

- Only one MCP client can own the ingest port at a time — run Claude **or** Codex
  against the live feed, not both. A second instance still serves MCP but
  `get_status` reports `ingest_bound: false`.
- The bridge holds no durable state; if it restarts mid-call the extension
  repopulates it within one push interval (~2s).
- Consumption is pull-on-demand: the assistant reads a tool when you ask it
  something, not autonomously.
