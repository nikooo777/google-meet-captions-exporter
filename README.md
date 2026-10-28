# Google Meet Captions Exporter

Chrome extension that captures closed captions from Google Meet in real time and lets you export them as a transcript.

## Features

- Real-time caption capture via DOM observation
- Export as plain text with timestamps and speaker names
- Copy to clipboard or download as `.txt`
- Automatically enables captions and starts capturing when you join a meeting
- Live transcript access for LLM assistants via a local MCP bridge (Claude, Codex, …)
- Separate saved capture sessions, with automatic retries and visible disk-save errors

## Install

1. Open `chrome://extensions`
2. Enable **Developer mode**
3. Click **Load unpacked** and select this directory

## Usage

1. Join a Google Meet call — the extension turns captions on automatically (use the CC button manually if your Meet layout hides the control)
2. Click the extension icon to see status, copy, or download the transcript
3. Optionally point it at a local MCP bridge for live LLM access (see below)

## Export format

```
[10:05:23] Alice: Hello everyone, let's get started.
[10:05:30] Bob: Sounds good, I have the slides ready.
```

## Live transcript bridge (MCP)

During a call you can let an LLM assistant read the rolling transcript on demand — ask it to help craft a reply and it pulls the latest captions itself. The extension streams the transcript to a small always-on local server (`meet-bridge`, typically a systemd user service) that exposes it over the Model Context Protocol on HTTP, so MCP clients (Claude, Codex) connect by URL — several can read at once.

The bridge is enabled by default at `http://127.0.0.1:6338`; no token setup is needed. The popup lets you change the URL or enter an optional token if you enabled authentication on the daemon. Build, run-as-a-service, and client-wiring instructions are in [`bridge/README.md`](bridge/README.md).

Streaming starts after you join a call and stops when you leave. The daemon saves each capture session separately, so reloading, rejoining, or clearing the popup cannot overwrite an earlier session's file. If saving fails, the popup reports the error and the daemon retries the latest snapshot every two seconds.

When upgrading, rebuild and restart the bridge, reload the unpacked extension, and refresh the Meet tab. Start a new `get_since` baseline without a cursor after upgrading.

## Known limitations

- English locale only (the caption region and "Turn on captions" labels are localized)
- Without the bridge, the transcript lives only in the tab; export before closing. With the bridge, successfully written snapshots remain on disk. Up to one push interval of recent speech may be missing.
- Auto-enabling captions is best-effort; if Meet hides the control behind a menu, turn captions on manually
- Side panel captions only (bottom overlay mode not yet supported)
- Panel close/reopen may duplicate recent entries
- Clearing mid-sentence may include some pre-clear text in the next capture

## Checks

Run extension regression tests with `node --test tests/extension.test.js`. In `bridge/`, run `go test -race ./...` and `go vet ./...`.

## Author

[Nikooo777](https://github.com/nikooo777)
