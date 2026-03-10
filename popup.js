const statusEl = document.getElementById("status");
const statusText = document.getElementById("status-text");
const infoEl = document.getElementById("info");
const warningEl = document.getElementById("warning");
const errorEl = document.getElementById("error");
const btnCopy = document.getElementById("btn-copy");
const btnDownload = document.getElementById("btn-download");
const btnClear = document.getElementById("btn-clear");

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
}

function setButtons(entryCount) {
  const hasData = entryCount > 0;
  btnCopy.disabled = !hasData;
  btnDownload.disabled = !hasData;
  btnClear.disabled = !hasData;
  warningEl.classList.toggle("hidden", !hasData);
}

async function sendMessage(message) {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (!tab?.id) throw new Error("no-tab");
  return chrome.tabs.sendMessage(tab.id, message);
}

async function refreshStatus() {
  try {
    const response = await sendMessage({ type: "GET_STATUS" });
    errorEl.classList.add("hidden");

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
  } catch {
    showError("Open a Google Meet tab to start capturing captions");
  }
}

btnCopy.addEventListener("click", async () => {
  try {
    const response = await sendMessage({ type: "GET_TRANSCRIPT" });
    await navigator.clipboard.writeText(response.text);
    btnCopy.textContent = "Copied!";
    setTimeout(() => { btnCopy.textContent = "Copy to Clipboard"; }, 1500);
  } catch {
    btnCopy.textContent = "Failed";
    setTimeout(() => { btnCopy.textContent = "Copy to Clipboard"; }, 1500);
  }
});

btnDownload.addEventListener("click", async () => {
  try {
    const response = await sendMessage({ type: "GET_TRANSCRIPT" });
    const blob = new Blob([response.text], { type: "text/plain" });
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

refreshStatus();
