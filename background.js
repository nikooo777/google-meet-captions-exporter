function isLoopbackHost(host) {
  return host === "127.0.0.1" || host === "localhost" || host === "::1";
}

async function pushToBridge(snapshot, sender) {
  const config = await chrome.storage.local.get(["bridgeUrl", "bridgeToken"]);
  const url = (config.bridgeUrl || "").trim();
  const token = (config.bridgeToken || "").trim();
  if (!url || !token) {
    return { ok: false, reason: "not-configured" };
  }

  let origin = null;
  try {
    const parsed = new URL(url);
    if (!isLoopbackHost(parsed.hostname)) {
      return { ok: false, reason: "not-configured", error: "bridge URL must be loopback" };
    }
    origin = parsed.origin;
  } catch {
    return { ok: false, reason: "not-configured", error: "invalid bridge URL" };
  }

  if (!snapshot.meeting_id && sender?.tab?.id != null) {
    snapshot.meeting_id = "tab-" + sender.tab.id;
  }

  try {
    const response = await fetch(origin + "/ingest", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: "Bearer " + token,
      },
      body: JSON.stringify(snapshot),
    });
    if (!response.ok) {
      return { ok: false, reason: "unreachable", error: "HTTP " + response.status };
    }
    return { ok: true };
  } catch (err) {
    return { ok: false, reason: "unreachable", error: err?.message || "fetch failed" };
  }
}

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  if (message.type === "PUSH_TO_BRIDGE") {
    pushToBridge(message.snapshot, sender).then(sendResponse);
    return true;
  }
});
