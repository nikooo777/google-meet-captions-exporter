const statusEl = document.getElementById("status");
const statusText = document.getElementById("status-text");
const infoEl = document.getElementById("info");
const warningEl = document.getElementById("warning");
const errorEl = document.getElementById("error");
const btnCopy = document.getElementById("btn-copy");
const btnDownload = document.getElementById("btn-download");
const btnClear = document.getElementById("btn-clear");
const bridgeEnable = document.getElementById("bridge-enable");
const bridgeUrlInput = document.getElementById("bridge-url");
const bridgeTokenInput = document.getElementById("bridge-token");
const bridgeStateEl = document.getElementById("bridge-state");
const bridgeContainer = document.getElementById("bridge");

const DEFAULT_BRIDGE_URL = "http://127.0.0.1:6338";
const STATUS_POLL_MS = 2000;

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
  bridgeContainer.classList.add("hidden");
}

function setButtons(entryCount) {
  const hasData = entryCount > 0;
  btnCopy.disabled = !hasData;
  btnDownload.disabled = !hasData;
  btnClear.disabled = !hasData;
  warningEl.classList.toggle("hidden", !hasData);
}

function renderBridgeState(response) {
  bridgeStateEl.className = "bridge-state";
  if (!bridgeEnable.checked) {
    bridgeStateEl.classList.add("hidden");
    return;
  }
  if (response.bridgeReason === "not-configured") {
    bridgeStateEl.textContent = "Not configured — set the bridge URL and token.";
    bridgeStateEl.classList.add("bridge-warn");
  } else if (response.bridgeReason === "unreachable") {
    bridgeStateEl.textContent = "Bridge unreachable — is the MCP client running?";
    bridgeStateEl.classList.add("bridge-error");
  } else if (response.bridgeStreaming) {
    bridgeStateEl.textContent = response.lastBridgeSuccessAt
      ? "Streaming — last push " +
        new Date(response.lastBridgeSuccessAt).toLocaleTimeString("en-US", { hour12: false }) + "."
      : "Streaming…";
    bridgeStateEl.classList.add("bridge-ok");
  } else {
    bridgeStateEl.classList.add("hidden");
  }
}

async function getTranscriptText() {
  const response = await sendMessage({ type: "GET_TRANSCRIPT" });
  return response.text;
}

async function refreshStatus() {
  try {
    const response = await sendMessage({ type: "GET_STATUS" });
    errorEl.classList.add("hidden");
    bridgeContainer.classList.remove("hidden");

    if (response.recording) {
      statusEl.className = "status recording";
      statusText.textContent = "Recording";
    } else {
      statusEl.className = "status waiting";
      statusText.textContent = "Waiting for captions";
    }

    infoEl.textContent = response.entryCount > 0
      ? `${response.entryCount} caption entries captured`
      : "No captions captured yet";

    setButtons(response.entryCount);
    renderBridgeState(response);
  } catch {
    showError("Open a Google Meet tab to start capturing captions");
  }
}

function persistBridgeConfig() {
  chrome.storage.local.set({
    bridgeEnabled: bridgeEnable.checked,
    bridgeUrl: bridgeUrlInput.value.trim() || DEFAULT_BRIDGE_URL,
    bridgeToken: bridgeTokenInput.value.trim(),
  });
}

bridgeEnable.addEventListener("change", () => {
  persistBridgeConfig();
  refreshStatus();
});
bridgeUrlInput.addEventListener("change", persistBridgeConfig);
bridgeTokenInput.addEventListener("change", persistBridgeConfig);

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
    await sendMessage({ type: "CLEAR_TRANSCRIPT" });
    await refreshStatus();
  } catch {
    // no-op
  }
});

async function init() {
  const cfg = await chrome.storage.local.get([
    "bridgeEnabled",
    "bridgeUrl",
    "bridgeToken",
  ]);
  bridgeEnable.checked = cfg.bridgeEnabled !== false;
  bridgeUrlInput.value = cfg.bridgeUrl || DEFAULT_BRIDGE_URL;
  bridgeTokenInput.value = cfg.bridgeToken || "";
  if (!cfg.bridgeUrl) {
    chrome.storage.local.set({ bridgeUrl: DEFAULT_BRIDGE_URL });
  }
  await refreshStatus();
}

let statusPolling = false;
async function pollStatus() {
  if (statusPolling) return;
  statusPolling = true;
  try {
    await refreshStatus();
  } finally {
    statusPolling = false;
  }
}

init();
setInterval(pollStatus, STATUS_POLL_MS);
