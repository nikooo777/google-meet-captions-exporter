(() => {
  "use strict";

  const SELECTORS = {
    CAPTION_REGION: '[aria-label="Captions"][role="region"]',
    TURN_BLOCK: ".nMcdL",
    SPEAKER_NAME: ".NWpY1d",
    TEXT_CONTAINER: ".ygicle",
  };
  const STREAM_INTERVAL_MS = 2000;
  const CAPTIONS_BUTTON_LABEL = "turn on captions";
  const CAPTION_ENABLE_THROTTLE_MS = 500;

  const transcript = [];
  let turnMap = new WeakMap();
  let nodeMap = new WeakMap();

  let captionObserver = null;
  let recording = false;

  let streamInterval = null;
  let streamEpoch = 0;
  let bridgeStreaming = false;
  let bridgeReason = null;
  let bridgeError = null;
  let lastBridgeSuccessAt = null;

  function ensureEntry(turnEl) {
    let entry = turnMap.get(turnEl);
    if (entry) return entry;
    const nameEl = turnEl.querySelector(SELECTORS.SPEAKER_NAME);
    const speaker = nameEl?.textContent?.trim() || "Unknown";
    entry = { speaker, timestamp: Date.now(), segments: [] };
    transcript.push(entry);
    turnMap.set(turnEl, entry);
    return entry;
  }

  function isCaptionTextNode(node) {
    return (
      node?.nodeType === Node.TEXT_NODE &&
      node.parentElement?.matches?.(SELECTORS.TEXT_CONTAINER)
    );
  }

  function resolveEntry(node) {
    if (!isCaptionTextNode(node)) return null;
    const turn = node.parentElement.closest(SELECTORS.TURN_BLOCK);
    if (!turn) return null;
    return ensureEntry(turn);
  }

  function seedTurn(turnEl) {
    const entry = ensureEntry(turnEl);

    const textContainer = turnEl.querySelector(SELECTORS.TEXT_CONTAINER);
    if (!textContainer) return;

    for (const child of textContainer.childNodes) {
      if (child.nodeType !== Node.TEXT_NODE) continue;
      seedTextNode(child, entry);
    }
  }

  function seedTextNode(textNode, entry) {
    if (nodeMap.has(textNode)) return;
    const now = Date.now();
    const segment = { text: textNode.textContent, startTime: now, updatedAt: now };
    entry.segments.push(segment);
    nodeMap.set(textNode, segment);
  }

  function handleMutations(mutations) {
    for (const mutation of mutations) {
      if (mutation.type === "childList") {
        for (const added of mutation.addedNodes) {
          if (added.nodeType === Node.ELEMENT_NODE) {
            if (added.matches?.(SELECTORS.TURN_BLOCK)) {
              seedTurn(added);
            } else {
              const nested = added.querySelectorAll?.(SELECTORS.TURN_BLOCK);
              if (nested) {
                for (const turn of nested) seedTurn(turn);
              }
            }
          }

          if (added.nodeType === Node.TEXT_NODE) {
            const entry = resolveEntry(added);
            if (!entry) continue;
            seedTextNode(added, entry);
          }
        }
      }

      if (mutation.type === "characterData") {
        const textNode = mutation.target;
        let segment = nodeMap.get(textNode);
        if (segment) {
          segment.text = textNode.textContent;
          segment.updatedAt = Date.now();
        } else {
          const entry = resolveEntry(textNode);
          if (!entry) continue;
          const now = Date.now();
          segment = { text: textNode.textContent, startTime: now, updatedAt: now };
          entry.segments.push(segment);
          nodeMap.set(textNode, segment);
        }
      }
    }
  }

  function bootstrap(region) {
    const turns = region.querySelectorAll(SELECTORS.TURN_BLOCK);
    for (const turn of turns) seedTurn(turn);
  }

  function attachCaptionObserver(region) {
    if (captionObserver) captionObserver.disconnect();

    bootstrap(region);
    recording = true;

    captionObserver = new MutationObserver(handleMutations);
    captionObserver.observe(region, {
      childList: true,
      subtree: true,
      characterData: true,
      characterDataOldValue: true,
    });
  }

  function detachCaptionObserver() {
    if (captionObserver) {
      captionObserver.disconnect();
      captionObserver = null;
    }
    recording = false;
  }

  function startBodyObserver() {
    const existing = document.querySelector(SELECTORS.CAPTION_REGION);
    if (existing) attachCaptionObserver(existing);

    const bodyObserver = new MutationObserver(() => {
      maybeEnableCaptions();
      const region = document.querySelector(SELECTORS.CAPTION_REGION);
      if (region && !captionObserver) {
        attachCaptionObserver(region);
      } else if (!region && captionObserver) {
        detachCaptionObserver();
      }
    });

    bodyObserver.observe(document.body, { childList: true, subtree: true });
  }

  function formatTranscript() {
    const lines = [];
    for (const entry of transcript) {
      const date = new Date(entry.timestamp);
      const time = date.toLocaleTimeString("en-US", { hour12: false });
      const text = entry.segments
        .map((s) => s.text)
        .join("")
        .trim();
      if (!text) continue;
      lines.push(`[${time}] ${entry.speaker}: ${text}`);
    }
    return lines.join("\n");
  }

  function getMeetingId() {
    const match = location.pathname.match(/[a-z]{3}-[a-z]{4}-[a-z]{3}/);
    return match ? match[0] : "";
  }

  function serializeTranscript() {
    const entries = [];
    for (const entry of transcript) {
      const text = entry.segments
        .map((s) => s.text)
        .join("")
        .trim();
      if (!text) continue;
      let updatedAt = entry.timestamp;
      for (const segment of entry.segments) {
        if (segment.updatedAt > updatedAt) updatedAt = segment.updatedAt;
      }
      entries.push({
        speaker: entry.speaker,
        timestamp: entry.timestamp,
        text,
        updated_at: updatedAt,
      });
    }
    return { meeting_id: getMeetingId(), captured_at: Date.now(), entries };
  }

  async function pushToBridge() {
    const snapshot = serializeTranscript();
    try {
      const response = await chrome.runtime.sendMessage({
        type: "PUSH_TO_BRIDGE",
        snapshot,
      });
      if (response?.ok) {
        bridgeReason = null;
        bridgeError = null;
        lastBridgeSuccessAt = Date.now();
      } else {
        bridgeReason = response?.reason || "unreachable";
        bridgeError = response?.error || null;
      }
    } catch (err) {
      bridgeReason = "unreachable";
      bridgeError = err?.message || "send failed";
    }
  }

  async function streamLoop(epoch) {
    if (epoch !== streamEpoch) return;
    await pushToBridge();
    if (bridgeStreaming && epoch === streamEpoch) {
      streamInterval = setTimeout(() => streamLoop(epoch), STREAM_INTERVAL_MS);
    }
  }

  function startBridgeStream() {
    streamEpoch++;
    bridgeStreaming = true;
    bridgeReason = null;
    bridgeError = null;
    if (streamInterval) clearTimeout(streamInterval);
    streamInterval = null;
    streamLoop(streamEpoch);
  }

  function stopBridgeStream() {
    streamEpoch++;
    bridgeStreaming = false;
    if (streamInterval) {
      clearTimeout(streamInterval);
      streamInterval = null;
    }
    bridgeReason = null;
    bridgeError = null;
  }

  async function syncBridgeFromConfig() {
    let cfg;
    try {
      cfg = await chrome.storage.local.get([
        "bridgeEnabled",
        "bridgeUrl",
        "bridgeToken",
      ]);
    } catch {
      return;
    }
    const hasConfig =
      !!cfg.bridgeToken?.trim() && !!cfg.bridgeUrl?.trim();
    const enabled = hasConfig && cfg.bridgeEnabled !== false;
    if (enabled && !bridgeStreaming) {
      startBridgeStream();
    } else if (!enabled && bridgeStreaming) {
      stopBridgeStream();
    }
  }

  chrome.storage.onChanged.addListener((changes, area) => {
    if (area !== "local") return;
    if (changes.bridgeEnabled || changes.bridgeUrl || changes.bridgeToken) {
      syncBridgeFromConfig();
    }
  });

  // Best-effort: turn on Meet captions once when we enter a meeting. Driven by
  // the body observer so it fires on the lobby→meeting transition — the
  // "Turn on captions" button does not exist while you're still in the lobby,
  // which can last arbitrarily long. The control is localized and may live
  // behind the "More options" menu in some layouts, in which case it won't be
  // found — capture still works if the user enables captions manually.
  let captionsClicked = false;
  let lastCaptionAttempt = 0;

  function findCaptionsButton() {
    const els = document.querySelectorAll(
      'button[aria-label], [role="button"][aria-label]'
    );
    for (const el of els) {
      const label = (el.getAttribute("aria-label") || "").toLowerCase();
      if (label.startsWith(CAPTIONS_BUTTON_LABEL)) return el;
    }
    return null;
  }

  function maybeEnableCaptions() {
    if (captionsClicked) return;
    if (document.querySelector(SELECTORS.CAPTION_REGION)) return;
    const now = Date.now();
    if (now - lastCaptionAttempt < CAPTION_ENABLE_THROTTLE_MS) return;
    lastCaptionAttempt = now;
    const button = findCaptionsButton();
    if (button) {
      button.click();
      captionsClicked = true;
    }
  }

  chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
    if (message.type === "GET_STATUS") {
      const entryCount = transcript.filter(
        (e) =>
          e.segments
            .map((s) => s.text)
            .join("")
            .trim().length > 0
      ).length;
      sendResponse({
        recording,
        entryCount,
        bridgeStreaming,
        bridgeReason,
        bridgeError,
        lastBridgeSuccessAt,
      });
      return;
    }

    if (message.type === "GET_TRANSCRIPT") {
      sendResponse({ text: formatTranscript() });
      return;
    }

    // Clearing while a node is mid-revision can fold pre-clear text into the
    // next captured segment; Meet rewrites whole nodes during refinement, so
    // there's no reliable way to subtract a pre-clear prefix.
    if (message.type === "CLEAR_TRANSCRIPT") {
      transcript.length = 0;
      turnMap = new WeakMap();
      nodeMap = new WeakMap();
      if (bridgeStreaming) pushToBridge();
      sendResponse({ ok: true });
      return;
    }
  });

  startBodyObserver();
  maybeEnableCaptions();
  syncBridgeFromConfig();
})();
