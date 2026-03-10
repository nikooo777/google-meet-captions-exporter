# Google Meet Captions Exporter

Chrome extension that captures closed captions from Google Meet in real time and lets you export them as a transcript.

## Features

- Real-time caption capture via DOM observation
- Export as plain text with timestamps and speaker names
- Copy to clipboard or download as `.txt`
- No special permissions required

## Install

1. Open `chrome://extensions`
2. Enable **Developer mode**
3. Click **Load unpacked** and select this directory

## Usage

1. Join a Google Meet call
2. Turn on captions (CC button)
3. Click the extension icon to see status and export

## Export format

```
[10:05:23] Alice: Hello everyone, let's get started.
[10:05:30] Bob: Sounds good, I have the slides ready.
```

## Known limitations

- English locale only (`aria-label="Captions"` is localized)
- Transcript is lost on tab close/reload — export before leaving
- Side panel captions only (bottom overlay mode not yet supported)
- Panel close/reopen may duplicate recent entries
