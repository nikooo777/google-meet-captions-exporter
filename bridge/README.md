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

One process, one port (`127.0.0.1:6338`), two HTTP endpoints. No token is required
by default; both endpoints validate the request Host and Origin:

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

1. **Unit** at `~/.config/systemd/user/meet-bridge.service`:

   ```ini
   [Unit]
   Description=Google Meet captions MCP bridge
   After=network-online.target
   Wants=network-online.target

   [Service]
   Type=simple
   EnvironmentFile=-%h/.config/meet-bridge/env
   ExecStart=%h/.local/bin/meet-bridge --addr 127.0.0.1:6338
   Restart=on-failure
   RestartSec=2
   NoNewPrivileges=true
   PrivateTmp=true

   [Install]
   WantedBy=default.target
   ```

2. **Enable + start:**

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
| Auth token | `MEET_BRIDGE_TOKEN` env | Unset/empty: no token required. Set: matching bearer token required. |
| Listen address | `--addr` flag or `MEET_BRIDGE_ADDR` env | `127.0.0.1:6338` (loopback only — refuses non-local) |
| Transcript dir | `MEET_BRIDGE_DIR` env | `~/.local/share/meet-bridge/transcripts` (created on start) |

## Wire it into an assistant

Clients connect to the **running** server by URL — they do not spawn it.

### Codex

```sh
codex mcp add meet-transcript --url http://127.0.0.1:6338/mcp
```

### Claude

Add an HTTP MCP server at `http://127.0.0.1:6338/mcp`:

```sh
claude mcp add --transport http meet-transcript http://127.0.0.1:6338/mcp
```

### Extension

The extension streams automatically when you join a call. The default URL is
`http://127.0.0.1:6338`; leave the optional token blank.

### Optional authentication

Set `MEET_BRIDGE_TOKEN` in the daemon's environment to require bearer authentication
on both endpoints. For the systemd unit above, put it in
`~/.config/meet-bridge/env` and restart the service. Restrict that file to mode `0600`.
Use the same token in the extension popup and your MCP client.

For Codex, set the client's `MEET_BRIDGE_TOKEN` environment variable and add
`--bearer-token-env-var MEET_BRIDGE_TOKEN` to the command above. For Claude, add
`--header "Authorization: Bearer <your-token>"`.

When upgrading an existing token-based installation to token-free mode, remove or
empty `MEET_BRIDGE_TOKEN` in the service environment file, restart the daemon, and
register MCP clients without their token options. Other environment settings can
remain in the file.

### Local access

The daemon refuses non-loopback listen addresses and non-loopback Host headers.
Foreign and opaque browser origins are rejected; Chrome extension origins and the
bridge's own origin are allowed. Native clients may omit Origin. POST requests must
use `Content-Type: application/json`; form and plain-text posts are rejected.

Token-free mode trusts other local programs and installed Chrome extensions to
read and submit transcripts. These Host, Origin and content-type checks remain
enabled when token authentication is configured.

## Tools

| Tool | Args | Returns |
|---|---|---|
| `get_recent` | `meeting_id?`, `minutes?`, `count?` | Tail of the transcript. `count` wins over `minutes`; default last 12. `minutes` filters on each entry's last-updated time. |
| `get_since` | `meeting_id?`, `since?` | Incremental delta: only lines added or revised since an opaque revision cursor. First call omits `since` (returns the full transcript); the first line is `cursor: <revision>`. Pass that back as `since` on the next call. `reset: true` means replace the prior baseline with the returned transcript. |
| `get_transcript` | `meeting_id?` | The full transcript. |
| `get_status` | — | Active meetings, entry counts, when each was last updated, and where transcripts are saved on disk. |

`meeting_id` is optional when only one call is active. A trailing ` …` on the last
line marks an entry that may still be changing (Meet revises captions as people
speak).

**Delta pulling with `get_since`.** The bridge assigns increasing revisions when
snapshots arrive and retains each line's revision until its content changes. Caption
corrections are returned even when their wall-clock timestamps are identical.
Call without `since` for a baseline, then reuse the returned cursor for that meeting.
Treat cursors as opaque numbers. If `reset: true` appears, discard the old baseline
and use the returned transcript. Start without a cursor after upgrading the daemon.

Live selection uses capture times rather than packet arrival order. Delayed packets
from older sessions are still archived, but cannot replace a newer live snapshot.
Switching the selected capture, including returning to an existing one, resets the
delta baseline.

## Persistence

Every non-empty transcript push is written under `MEET_BRIDGE_DIR` (default
`~/.local/share/meet-bridge/transcripts`). Each capture session has a unique ID and
its own `<start-time>_<meeting-id>_<session-id>.txt` file. Reloading, rejoining or
clearing starts a new capture file and leaves the earlier file intact, even when
sessions begin within the same second. Existing files are left unchanged on upgrade.
Older extensions without session IDs use the first caption timestamp to distinguish
captures. Writes are atomic (temp file + rename) and unchanged content is skipped.

Disk-write failures appear in the ingest response, extension popup and MCP
`get_status`. The daemon keeps the latest failed snapshot for each capture and retries
every two seconds, even if its tab has closed. Pending retries are held in memory:
restarting the daemon before the disk recovers loses those unsaved snapshots.
Every push reports outstanding save failures across all captures, including those
from before Clear. The popup keeps those warnings separate from streaming status
and clears them only after a newer daemon report confirms recovery.

Streaming begins only after joining a call. Leaving sends a final snapshot marked
inactive. Closed or disconnected tabs expire from active status after about two
minutes without a push; idle homepages and unjoined lobbies do not stream.

## Checks

Run `go test -race ./...` and `go vet ./...` from `bridge/`. Extension lifecycle and
message tests run from the project root with `node --test tests/extension.test.js`.

## Notes

- **Multiple clients can attach at once** (stateless HTTP) — Claude and Codex can both
  read the live feed simultaneously.
- The in-memory snapshots are ephemeral: a capture that stops pushing for ~2min stops being
  reported by the MCP tools, and a restart clears them (the extension repopulates the
  active call within one push interval, ~2s). The on-disk transcript persists regardless.
- Consumption is pull-on-demand: the assistant reads a tool when you ask it something,
  not autonomously.
