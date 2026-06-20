# meet-transcript bridge

Always-on local server that receives the live Google Meet transcript pushed by the
[extension](../) and exposes it over the Model Context Protocol (MCP) so any MCP
client (Claude, Codex, …) can read it. It runs **standalone** — typically as a
systemd user service — independent of any client. Design notes:
`../aidocs/003_live_transcript_mcp.md`.

## How it works

A Chrome extension can't run a server, so the extension **pushes** the transcript to
this process, and clients **read** it over MCP — both against the same process:

```
extension service worker ──POST /ingest──▶  meet-bridge  ◀──MCP (/mcp)── Claude / Codex
```

One process, one port (`127.0.0.1:6338`), two HTTP endpoints, both behind the shared
bearer token:

- `POST /ingest` — the extension pushes full transcript snapshots (each replaces the
  stored one, so caption back-tracking is absorbed for free).
- `/mcp` — MCP over Streamable HTTP. It's **stateless**, so several clients can attach
  at once.

## Build & install

```sh
cd bridge
go build -o ~/.local/bin/meet-bridge ./cmd/meet-bridge
```

## Run as a systemd user service (always on)

1. **Token env file** — pick a token (use the *same* value in the extension popup):

   ```sh
   mkdir -p ~/.config/meet-bridge
   printf 'MEET_BRIDGE_TOKEN=%s\n' "$(openssl rand -hex 16)" > ~/.config/meet-bridge/env
   chmod 600 ~/.config/meet-bridge/env
   ```

2. **Unit** at `~/.config/systemd/user/meet-bridge.service`:

   ```ini
   [Unit]
   Description=Google Meet captions MCP bridge
   After=network-online.target
   Wants=network-online.target

   [Service]
   Type=simple
   EnvironmentFile=%h/.config/meet-bridge/env
   ExecStart=%h/.local/bin/meet-bridge --addr 127.0.0.1:6338
   Restart=on-failure
   RestartSec=2
   NoNewPrivileges=true
   PrivateTmp=true

   [Install]
   WantedBy=default.target
   ```

3. **Enable + start:**

   ```sh
   systemctl --user daemon-reload
   systemctl --user enable --now meet-bridge.service
   systemctl --user status meet-bridge.service
   ```

   - Keep it running while logged out: `loginctl enable-linger "$USER"`.
   - Logs: `journalctl --user -u meet-bridge -f`.

### Configuration

| Setting | Source | Default |
|---|---|---|
| Auth token | `MEET_BRIDGE_TOKEN` env | **required** (non-empty, or the process exits) |
| Listen address | `--addr` flag or `MEET_BRIDGE_ADDR` env | `127.0.0.1:6338` (loopback only — refuses non-local) |
| Transcript dir | `MEET_BRIDGE_DIR` env | `~/.local/share/meet-bridge/transcripts` (created on start) |

## Wire it into an assistant

Clients connect to the **running** server by URL — they do not spawn it.

### Codex

```sh
export MEET_BRIDGE_TOKEN=<your-token>   # add to your shell profile so Codex sees it
codex mcp add meet-transcript --url http://127.0.0.1:6338/mcp --bearer-token-env-var MEET_BRIDGE_TOKEN
```

`--bearer-token-env-var` makes Codex read the token from that env var at connect time
and send it as `Authorization: Bearer …`.

### Claude

Add an HTTP MCP server at `http://127.0.0.1:6338/mcp` with an
`Authorization: Bearer <token>` header — e.g. (flags vary by version):

```sh
claude mcp add --transport http meet-transcript http://127.0.0.1:6338/mcp --header "Authorization: Bearer <your-token>"
```

### Extension

Popup → **Live transcript bridge (MCP)** → URL `http://127.0.0.1:6338`, paste the same
token.

## Tools

| Tool | Args | Returns |
|---|---|---|
| `get_recent` | `meeting_id?`, `minutes?`, `count?` | Tail of the transcript. `count` wins over `minutes`; default last 12. `minutes` filters on each entry's last-updated time. |
| `get_since` | `meeting_id?`, `since?` | Incremental delta: only lines added or revised since a cursor. First call omits `since` (returns the full transcript); the result's first line is `cursor: <ms>`. Pass that back as `since` on the next call to get just what changed, plus a fresh cursor. |
| `get_transcript` | `meeting_id?` | The full transcript. |
| `get_status` | — | Active meetings, entry counts, when each was last updated, and where transcripts are saved on disk. |

`meeting_id` is optional when only one call is active. A trailing ` …` on the last
line marks an entry that may still be changing (Meet revises captions as people
speak).

**Delta pulling with `get_since`.** Because Meet revises a line in place while it's
being spoken, an entry's last-updated time advances each time it changes. `get_since`
returns every entry whose update time is newer than your `cursor`, so a still-settling
line reappears (with updated text) on each call until it stops changing, then drops out.
Loop: call with no `since` once for the baseline, then keep calling with the previous
`cursor` to stay current without re-reading the whole transcript.

## Persistence

Every non-empty transcript push is also written to disk, so no meeting is lost even
though the in-memory snapshots are ephemeral. Each meeting maps to one file under the
transcript dir (`MEET_BRIDGE_DIR`, default `~/.local/share/meet-bridge/transcripts`),
named `<start-time>_<meeting-id>.txt`. Writes are atomic (temp file + rename) and
skipped when the content hasn't changed, so the file is updated in place as the call
progresses and caption back-tracking never produces duplicates.

## Notes

- **Multiple clients can attach at once** (stateless HTTP) — Claude and Codex can both
  read the live feed simultaneously.
- The in-memory snapshots are ephemeral: a meeting that goes quiet past ~2min stops being
  reported by the MCP tools, and a restart clears them (the extension repopulates the
  active call within one push interval, ~2s). The on-disk transcript persists regardless.
- Consumption is pull-on-demand: the assistant reads a tool when you ask it something,
  not autonomously.
