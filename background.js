importScripts("auto_save_db.js");

async function writeAutoSaveFile(text) {
  if (!text) return { ok: true };

  try {
    const state = await self.autoSaveDb.getState();
    if (!state) {
      return { error: "Auto-save file handle is missing" };
    }

    if (state.mode === "internal") {
      await self.autoSaveDb.setState({
        ...state,
        text,
        updatedAt: Date.now(),
      });
      return { ok: true, filename: state.filename || "Extension storage (download later)" };
    }

    const handle = state.handle;
    if (!handle) {
      return { error: "Auto-save file handle is missing" };
    }

    const writable = await handle.createWritable();
    await writable.write(text);
    await writable.close();

    return { ok: true, filename: state.filename || handle.name || null };
  } catch (err) {
    return { error: err?.message || "Write failed" };
  }
}

async function clearAutoSaveFile() {
  try {
    await self.autoSaveDb.clearState();
    return { ok: true };
  } catch (err) {
    return { error: err?.message || "Failed to clear auto-save state" };
  }
}

async function getAutoSaveState() {
  try {
    const state = await self.autoSaveDb.getState();
    if (!state) return { ok: true, state: null };
    return {
      ok: true,
      state: {
        mode: state.mode || "file",
        filename: state.filename || null,
        text: state.text || "",
        updatedAt: state.updatedAt || null,
      },
    };
  } catch (err) {
    return { error: err?.message || "Failed to read auto-save state" };
  }
}

chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message.type === "WRITE_AUTO_SAVE_FILE") {
    writeAutoSaveFile(message.text).then(sendResponse);
    return true;
  }

  if (message.type === "CLEAR_AUTO_SAVE_FILE") {
    clearAutoSaveFile().then(sendResponse);
    return true;
  }

  if (message.type === "GET_AUTO_SAVE_STATE") {
    getAutoSaveState().then(sendResponse);
    return true;
  }
});
