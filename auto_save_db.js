(() => {
  const DB_NAME = "meet-captions";
  const STORE_NAME = "state";
  const KEY = "auto-save";

  function openDb() {
    return new Promise((resolve, reject) => {
      const request = indexedDB.open(DB_NAME, 1);
      request.onupgradeneeded = () => {
        const db = request.result;
        if (!db.objectStoreNames.contains(STORE_NAME)) {
          db.createObjectStore(STORE_NAME);
        }
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error || new Error("IndexedDB open failed"));
    });
  }

  async function withStore(mode, operation) {
    const db = await openDb();
    return new Promise((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, mode);
      const store = tx.objectStore(STORE_NAME);
      const request = operation(store);

      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error || new Error("IndexedDB request failed"));
      tx.oncomplete = () => db.close();
      tx.onabort = () => {
        db.close();
        reject(tx.error || request.error || new Error("IndexedDB transaction failed"));
      };
      tx.onerror = () => {
        db.close();
        reject(tx.error || request.error || new Error("IndexedDB transaction failed"));
      };
    });
  }

  async function setState(state) {
    await withStore("readwrite", (store) => store.put(state, KEY));
    return state;
  }

  async function getState() {
    const state = await withStore("readonly", (store) => store.get(KEY));
    return state || null;
  }

  async function clearState() {
    await withStore("readwrite", (store) => store.delete(KEY));
  }

  self.autoSaveDb = {
    setState,
    getState,
    clearState,
    key: KEY,
  };
})();
