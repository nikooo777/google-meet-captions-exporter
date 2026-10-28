function isLoopbackHost(host) {
  return host === "127.0.0.1" || host === "localhost" || host === "::1";
}

async function pushToBridge(snapshot) {
  if (!snapshot?.meeting_id || !snapshot?.session_id) {
    return { ok: false, reason: "not-in-meeting" };
  }
  const config = await chrome.storage.local.get(["bridgeUrl", "bridgeToken"]);
  const url = (config.bridgeUrl || "http://127.0.0.1:6338").trim();
  const token = (config.bridgeToken || "").trim();
  if (!url) {
    return { ok: false, reason: "not-configured" };
  }

  let origin = null;
  try {
    const parsed = new URL(url);
    if (parsed.protocol !== "http:" || !isLoopbackHost(parsed.hostname)) {
      return { ok: false, reason: "not-configured", error: "bridge URL must be loopback" };
    }
    origin = parsed.origin;
  } catch {
    return { ok: false, reason: "not-configured", error: "invalid bridge URL" };
  }

  try {
    const headers = { "Content-Type": "application/json" };
    if (token) headers.Authorization = "Bearer " + token;
    const response = await fetch(origin + "/ingest", {
      method: "POST",
      headers,
      body: JSON.stringify(snapshot),
    });
    if (response.status === 401) {
      return { ok: false, reason: "authentication", error: "The bridge requires a token. Enter it below, or disable token authentication in the service." };
    }
    if (!response.ok) {
      return { ok: false, reason: "unreachable", error: "HTTP " + response.status };
    }
    const result = await response.json();
    const persistence = {
      persistenceRevision: result.persistence_revision,
      persistenceErrors: result.persistence_errors,
    };
    const saveError = result.persistence_error || Object.values(result.persistence_errors || {})[0];
    if (saveError) {
      return { ok: false, reason: "persistence", error: saveError, ...persistence };
    }
    if (!result.ok) {
      return { ok: false, reason: "unreachable", error: "Bridge rejected the transcript" };
    }
    return { ok: true, ...persistence };
  } catch (err) {
    return { ok: false, reason: "unreachable", error: err?.message || "fetch failed" };
  }
}

chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message.type === "PUSH_TO_BRIDGE") {
    pushToBridge(message.snapshot).then(sendResponse).catch((err) => {
      sendResponse({ ok: false, reason: "unreachable", error: err?.message || "Bridge request failed" });
    });
    return true;
  }
});
