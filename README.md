# Google Meet Captions Exporter

Chrome extension that captures closed captions from Google Meet in real time and lets you export them as a transcript.

## Features

- Real-time caption capture via DOM observation
- Export as plain text with timestamps and speaker names
- Copy to clipboard or download as `.txt`
- Auto-save transcript snapshots on a configurable interval
- Direct file auto-save when the browser exposes a file picker
- Internal extension-storage fallback when file saving is unavailable

## Install

1. Open `chrome://extensions`
2. Enable **Developer mode**
3. Click **Load unpacked** and select this directory

## Usage

1. Join a Google Meet call
2. Turn on captions (CC button)
3. Click the extension icon to see status and export
4. Optionally enable auto-save and choose how often it updates the transcript

## Export format

```
[10:05:23] Alice: Hello everyone, let's get started.
[10:05:30] Bob: Sounds good, I have the slides ready.
```

## Auto-save

Click "Enable auto-save" in the popup. You can set the save interval between 1 and 60 seconds (default: 2 seconds).

- If the browser exposes `showSaveFilePicker()`, the extension writes the transcript to the selected file at the chosen interval, overwriting the previous contents.
- If file saving is unavailable in the current browser context, the extension falls back to internal extension storage. The latest saved snapshot can still be copied or downloaded from the popup later.

Internal auto-save is not continuous after the Meet tab is closed, but the last saved snapshot remains recoverable from the popup until you clear it.

## Known limitations

- English locale only (`aria-label="Captions"` is localized)
- Live transcript capture stops on tab close/reload
- File-based auto-save depends on browser support for the file picker API
- Side panel captions only (bottom overlay mode not yet supported)
- Panel close/reopen may duplicate recent entries
- Clearing mid-sentence may include some pre-clear text in the next capture

## Author

[Nikooo777](https://github.com/nikooo777)
