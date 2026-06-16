# Google Meet Captions Exporter

Chrome extension that captures closed captions from Google Meet in real time and lets you export them as a transcript.

## Features

- Real-time caption capture via DOM observation
- Export as plain text with timestamps and speaker names
- Copy to clipboard or download as `.txt`
- Automatically enables captions and starts capturing when you join a meeting
- Live transcript access for LLM assistants via a local MCP bridge (Claude, Codex, …)

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

During a call you can let an LLM assistant read the rolling transcript on demand — ask it to help craft a reply and it pulls the latest captions itself. The extension streams the transcript to a small local Go process (`meet-bridge`) that exposes it over the Model Context Protocol, so any MCP client (Claude Desktop/Code, Codex) can attach.

Enable **Live transcript bridge (MCP)** in the popup, set the bridge URL (`http://127.0.0.1:6338`) and a shared token. Build and wiring instructions for the bridge are in [`bridge/README.md`](bridge/README.md).

## Known limitations

- English locale only (the caption region and "Turn on captions" labels are localized)
- The transcript lives only in the tab — export it (Copy / Download) or stream it to the bridge before closing; it is not persisted
- Auto-enabling captions is best-effort; if Meet hides the control behind a menu, turn captions on manually
- Side panel captions only (bottom overlay mode not yet supported)
- Panel close/reopen may duplicate recent entries
- Clearing mid-sentence may include some pre-clear text in the next capture

## Author

[Nikooo777](https://github.com/nikooo777)
