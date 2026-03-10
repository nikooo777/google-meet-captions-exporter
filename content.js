(() => {
  "use strict";

  const SELECTORS = {
    CAPTION_REGION: '[aria-label="Captions"][role="region"]',
    TURN_BLOCK: ".nMcdL",
    SPEAKER_NAME: ".NWpY1d",
    TEXT_CONTAINER: ".ygicle",
  };
  const DEFAULT_AUTO_SAVE_INTERVAL_SECONDS = 2;
  const MIN_AUTO_SAVE_INTERVAL_SECONDS = 1;
  const MAX_AUTO_SAVE_INTERVAL_SECONDS = 60;

  const transcript = [];
  let turnMap = new WeakMap();
  let nodeMap = new WeakMap();

  let captionObserver = null;
  let recording = false;

  let saveInterval = null;
  let autoSaveFilename = null;
  let autoSaveError = null;
  let autoSaveActive = false;
  let autoSaveIntervalMs = DEFAULT_AUTO_SAVE_INTERVAL_SECONDS * 1000;

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
    const segment = { text: textNode.textContent, startTime: Date.now() };
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
        } else {
          const entry = resolveEntry(textNode);
          if (!entry) continue;
          segment = { text: textNode.textContent, startTime: Date.now() };
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

  let saveEpoch = 0;

  async function saveTranscriptToFile() {
    if (!autoSaveActive) return;
    const text = formatTranscript();
    if (!text) return;
    try {
      const response = await chrome.runtime.sendMessage({
        type: "WRITE_AUTO_SAVE_FILE",
        text,
      });
      if (!response?.ok) {
        throw new Error(response?.error || "Write failed");
      }
      autoSaveError = null;
    } catch (err) {
      autoSaveError = err.message || "Write failed";
      stopAutoSave();
    }
  }

  async function saveLoop(epoch) {
    if (epoch !== saveEpoch) return;
    await saveTranscriptToFile();
    if (autoSaveActive && epoch === saveEpoch) {
      saveInterval = setTimeout(() => saveLoop(epoch), autoSaveIntervalMs);
    }
  }

  function startSaveTimer() {
    saveEpoch++;
    autoSaveActive = true;
    if (saveInterval) clearTimeout(saveInterval);
    saveInterval = null;
    saveLoop(saveEpoch);
  }

  // stopAutoSave prevents new writes from starting but does not abort a write
  // already in progress. An in-flight createWritable/write/close will finish
  // with the data it captured before stop. Aborting mid-write could leave the
  // file in a corrupted (partially written) state.
  function stopAutoSave() {
    saveEpoch++;
    autoSaveActive = false;
    if (saveInterval) {
      clearTimeout(saveInterval);
      saveInterval = null;
    }
    autoSaveFilename = null;
    chrome.runtime.sendMessage({ type: "CLEAR_AUTO_SAVE_FILE" }).catch(() => {});
  }

  function startAutoSave(filename, intervalSeconds) {
    const parsedIntervalSeconds = Number.parseInt(String(intervalSeconds), 10);
    const normalizedIntervalSeconds = Number.isFinite(parsedIntervalSeconds)
      ? Math.min(MAX_AUTO_SAVE_INTERVAL_SECONDS, Math.max(MIN_AUTO_SAVE_INTERVAL_SECONDS, parsedIntervalSeconds))
      : DEFAULT_AUTO_SAVE_INTERVAL_SECONDS;
    autoSaveIntervalMs = normalizedIntervalSeconds * 1000;
    autoSaveFilename = filename || "meet-transcript.txt";
    autoSaveError = null;
    startSaveTimer();
    return {
      ok: true,
      filename: autoSaveFilename,
      intervalSeconds: normalizedIntervalSeconds,
    };
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
        autoSaving: autoSaveActive,
        autoSavePending: false,
        autoSaveFilename,
        autoSaveError,
        autoSaveIntervalSeconds: autoSaveIntervalMs / 1000,
      });
      return;
    }

    if (message.type === "GET_TRANSCRIPT") {
      sendResponse({ text: formatTranscript() });
      return;
    }

    if (message.type === "START_AUTO_SAVE") {
      sendResponse(startAutoSave(message.filename, message.intervalSeconds));
      return;
    }

    if (message.type === "STOP_AUTO_SAVE") {
      stopAutoSave();
      autoSaveError = null;
      sendResponse({ ok: true });
      return;
    }

    if (message.type === "DISMISS_AUTO_SAVE_ERROR") {
      autoSaveError = null;
      sendResponse({ ok: true });
      return;
    }

    // Known v1 limitation: text nodes actively being refined at clear time
    // will include their full textContent (including pre-clear words) when
    // the next characterData mutation lazily creates a segment. Meet revises
    // entire node content during refinement, so there's no reliable way to
    // subtract a pre-clear prefix.
    if (message.type === "CLEAR_TRANSCRIPT") {
      transcript.length = 0;
      turnMap = new WeakMap();
      nodeMap = new WeakMap();
      stopAutoSave();
      sendResponse({ ok: true });
      return;
    }
  });

  startBodyObserver();
})();
