const statusEl = document.getElementById("status");
const statusText = document.getElementById("status-text");
const infoEl = document.getElementById("info");
const warningEl = document.getElementById("warning");
const errorEl = document.getElementById("error");
const btnCopy = document.getElementById("btn-copy");
const btnDownload = document.getElementById("btn-download");
const btnClear = document.getElementById("btn-clear");

const autoSaveOff = document.getElementById("auto-save-off");
const autoSavePending = document.getElementById("auto-save-pending");
const autoSaveOn = document.getElementById("auto-save-on");
const autoSaveErrorEl = document.getElementById("auto-save-error");
const autoSaveRetryPending = document.getElementById("auto-save-retry-pending");
const autoSaveFilenameEl = document.getElementById("auto-save-filename");
const autoSaveIntervalInput = document.getElementById("auto-save-interval");
const autoSaveIntervalActiveEl = document.getElementById("auto-save-interval-active");
const autoSaveContainer = document.getElementById("auto-save");
const DEFAULT_WARNING_TEXT =
  "Export your transcript before closing the Meet tab — it cannot be recovered after.";
const DEFAULT_AUTO_SAVE_INTERVAL_SECONDS = 2;
const MIN_AUTO_SAVE_INTERVAL_SECONDS = 1;
const MAX_AUTO_SAVE_INTERVAL_SECONDS = 60;
const AUTO_SAVE_INTERVAL_STORAGE_KEY = "autoSaveIntervalSeconds";
let recoveryText = "";

function normalizeAutoSaveIntervalSeconds(value) {
  const parsed = Number.parseInt(String(value), 10);
  if (!Number.isFinite(parsed)) return DEFAULT_AUTO_SAVE_INTERVAL_SECONDS;
  return Math.min(MAX_AUTO_SAVE_INTERVAL_SECONDS, Math.max(MIN_AUTO_SAVE_INTERVAL_SECONDS, parsed));
}

function getPreferredAutoSaveIntervalSeconds() {
  return normalizeAutoSaveIntervalSeconds(
    window.localStorage.getItem(AUTO_SAVE_INTERVAL_STORAGE_KEY)
  );
}

function setPreferredAutoSaveIntervalSeconds(value) {
  const seconds = normalizeAutoSaveIntervalSeconds(value);
  window.localStorage.setItem(AUTO_SAVE_INTERVAL_STORAGE_KEY, String(seconds));
  autoSaveIntervalInput.value = String(seconds);
  return seconds;
}

function showError(msg) {
  errorEl.textContent = msg;
  errorEl.classList.remove("hidden");
  statusEl.className = "status";
  statusText.textContent = "";
  infoEl.textContent = "";
  warningEl.classList.add("hidden");
  btnCopy.disabled = true;
  btnDownload.disabled = true;
  btnClear.disabled = true;
  autoSaveContainer.classList.add("hidden");
}

function setButtons(entryCount) {
  const hasData = entryCount > 0;
  btnCopy.disabled = !hasData;
  btnDownload.disabled = !hasData;
  btnClear.disabled = !hasData;
  warningEl.classList.toggle("hidden", !hasData);
}

function showAutoSaveState(response) {
  autoSaveOff.classList.add("hidden");
  autoSavePending.classList.add("hidden");
  autoSaveOn.classList.add("hidden");
  autoSaveErrorEl.classList.add("hidden");
  autoSaveRetryPending.classList.add("hidden");

  if (response.autoSaving) {
    autoSaveOn.classList.remove("hidden");
    autoSaveFilenameEl.textContent = response.autoSaveFilename || "unknown";
    autoSaveIntervalActiveEl.textContent = `${response.autoSaveIntervalSeconds || DEFAULT_AUTO_SAVE_INTERVAL_SECONDS}s`;
  } else if (response.autoSavePending) {
    if (response._inlinePending) {
      autoSavePending.classList.remove("hidden");
    } else {
      autoSaveRetryPending.classList.remove("hidden");
    }
  } else if (response.autoSaveError) {
    autoSaveErrorEl.classList.remove("hidden");
    autoSaveErrorEl.querySelector(".auto-save-error-msg").textContent =
      "Auto-save stopped: " + response.autoSaveError;
  } else {
    autoSaveOff.classList.remove("hidden");
  }
}

let currentTabId = null;

async function getTabId() {
  if (currentTabId) return currentTabId;
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (!tab?.id) throw new Error("no-tab");
  currentTabId = tab.id;
  return tab.id;
}

async function sendMessage(message) {
  const tabId = await getTabId();
  return chrome.tabs.sendMessage(tabId, message);
}

async function getRecoveryState() {
  return chrome.runtime.sendMessage({ type: "GET_AUTO_SAVE_STATE" });
}

async function getTranscriptText() {
  if (recoveryText) return recoveryText;
  const response = await sendMessage({ type: "GET_TRANSCRIPT" });
  return response.text;
}

async function handleStartAutoSave() {
  try {
    let filename = "Extension storage (download later)";
    const intervalSeconds = setPreferredAutoSaveIntervalSeconds(autoSaveIntervalInput.value);

    if (typeof window.showSaveFilePicker === "function") {
      const handle = await window.showSaveFilePicker({
        suggestedName: "meet-transcript.txt",
        types: [{ description: "Text files", accept: { "text/plain": [".txt"] } }],
      });

      filename = handle.name;
      await autoSaveDb.setState({
        mode: "file",
        handle,
        filename,
      });
    } else {
      await autoSaveDb.setState({
        mode: "internal",
        filename,
        text: recoveryText,
        updatedAt: null,
      });
    }

    const response = await sendMessage({
      type: "START_AUTO_SAVE",
      filename,
      intervalSeconds,
    });

    if (response.ok) {
      recoveryText = "";
      showAutoSaveState({
        autoSaving: true,
        autoSaveFilename: response.filename,
        autoSaveIntervalSeconds: response.intervalSeconds,
      });
    } else {
      throw new Error(response?.error || "Failed to start auto-save");
    }
  } catch (err) {
    if (err?.name === "AbortError") {
      showAutoSaveState({});
      return;
    }

    await autoSaveDb.clearState().catch(() => {});
    showAutoSaveState({
      autoSaveError: err?.message || "Auto-save failed",
    });
  }
}

async function refreshStatus() {
  try {
    const response = await sendMessage({ type: "GET_STATUS" });
    recoveryText = "";
    errorEl.classList.add("hidden");
    autoSaveContainer.classList.remove("hidden");
    warningEl.textContent = DEFAULT_WARNING_TEXT;

    if (response.recording) {
      statusEl.className = "status recording";
      statusText.textContent = "Recording";
    } else {
      statusEl.className = "status waiting";
      statusText.textContent = "Waiting for captions";
    }

    if (response.autoSaving && response.autoSaveFilename?.startsWith("Extension storage")) {
      warningEl.textContent =
        "Internal auto-save is enabled. Download the transcript when you're done.";
    }

    if (response.autoSaving && response.autoSaveIntervalSeconds) {
      setPreferredAutoSaveIntervalSeconds(response.autoSaveIntervalSeconds);
    }

    infoEl.textContent = response.entryCount > 0
      ? `${response.entryCount} caption entries captured`
      : "No captions captured yet";

    setButtons(response.entryCount);
    showAutoSaveState(response);
  } catch {
    try {
      const recovery = await getRecoveryState();
      const state = recovery?.state;
      if (recovery?.ok && state?.mode === "internal" && state.text) {
        recoveryText = state.text;
        errorEl.classList.add("hidden");
        autoSaveContainer.classList.add("hidden");
        statusEl.className = "status waiting";
        statusText.textContent = "Recovered auto-save";
        infoEl.textContent = "Recovered the last internally saved transcript snapshot";
        warningEl.textContent =
          "This recovery copy may be slightly stale. Download it before clearing it.";
        warningEl.classList.remove("hidden");
        setButtons(1);
        return;
      }
    } catch {
      // no-op
    }

    showError("Open a Google Meet tab to start capturing captions");
  }
}

autoSaveIntervalInput.value = String(getPreferredAutoSaveIntervalSeconds());
autoSaveIntervalInput.addEventListener("change", () => {
  setPreferredAutoSaveIntervalSeconds(autoSaveIntervalInput.value);
});

document.getElementById("btn-auto-save").addEventListener("click", handleStartAutoSave);

document.getElementById("btn-auto-save-cancel").addEventListener("click", async () => {
  try {
    await sendMessage({ type: "STOP_AUTO_SAVE" });
    await refreshStatus();
  } catch { /* no-op */ }
});

document.getElementById("btn-auto-save-stop").addEventListener("click", async () => {
  try {
    await sendMessage({ type: "STOP_AUTO_SAVE" });
    await refreshStatus();
  } catch { /* no-op */ }
});

document.getElementById("btn-auto-save-retry").addEventListener("click", handleStartAutoSave);

document.getElementById("btn-auto-save-dismiss").addEventListener("click", async () => {
  try {
    await sendMessage({ type: "DISMISS_AUTO_SAVE_ERROR" });
    await refreshStatus();
  } catch { /* no-op */ }
});

document.getElementById("btn-auto-save-retry2").addEventListener("click", handleStartAutoSave);

document.getElementById("btn-auto-save-cancel2").addEventListener("click", async () => {
  try {
    await sendMessage({ type: "STOP_AUTO_SAVE" });
    await refreshStatus();
  } catch { /* no-op */ }
});

btnCopy.addEventListener("click", async () => {
  try {
    const text = await getTranscriptText();
    await navigator.clipboard.writeText(text);
    btnCopy.textContent = "Copied!";
    setTimeout(() => { btnCopy.textContent = "Copy to Clipboard"; }, 1500);
  } catch {
    btnCopy.textContent = "Failed";
    setTimeout(() => { btnCopy.textContent = "Copy to Clipboard"; }, 1500);
  }
});

btnDownload.addEventListener("click", async () => {
  try {
    const text = await getTranscriptText();
    const blob = new Blob([text], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const timestamp = new Date().toISOString().slice(0, 19).replace(/:/g, "-");
    const a = document.createElement("a");
    a.href = url;
    a.download = `meet-transcript-${timestamp}.txt`;
    a.click();
    URL.revokeObjectURL(url);
  } catch {
    // no-op
  }
});

btnClear.addEventListener("click", async () => {
  try {
    if (recoveryText) {
      await chrome.runtime.sendMessage({ type: "CLEAR_AUTO_SAVE_FILE" });
      recoveryText = "";
    } else {
      await sendMessage({ type: "CLEAR_TRANSCRIPT" });
    }
    await refreshStatus();
  } catch {
    // no-op
  }
});

refreshStatus();
