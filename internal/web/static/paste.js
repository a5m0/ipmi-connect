// Type text: the server types text on the host as US-layout key presses. The text box is a
// docked panel under the console, not a dialog, so the screen stays visible while it types.
import { toast } from '/toast.js';

const MAX_CHARS = 16384;
const MAX_MESSAGE_BYTES = 6 * MAX_CHARS + 1024; // the server's WebSocket message limit
const PREFS_KEY = 'ipmi-connect-type-prefs';

function loadPrefs() {
  try { return JSON.parse(localStorage.getItem(PREFS_KEY)) || {}; } catch { return {}; }
}

function savePrefs(prefs) {
  try { localStorage.setItem(PREFS_KEY, JSON.stringify(prefs)); } catch { /* storage blocked */ }
}

// BLOCKED is shown when no way of reading the clipboard worked.
const BLOCKED = 'The browser blocked clipboard access';

// setupPaste wires the type-text panel. apiFetch makes signed-in API requests; nativeClipboard
// says whether the server can read the system clipboard (the desktop window, whose webview
// refuses navigator.clipboard.readText).
export function setupPaste(sendMessage, apiFetch, nativeClipboard = () => false) {
  const panel = document.getElementById('pastePanel');
  const text = document.getElementById('pasteText');
  const count = document.getElementById('pasteCount');
  const typeBtn = document.getElementById('btnPasteType');
  const stopBtn = document.getElementById('btnPasteCancel');
  const enterBox = document.getElementById('pasteEnter');
  const keepBox = document.getElementById('pasteKeep');
  const toggleBtn = document.getElementById('btnPaste');
  let typing = false;
  let currentId = ''; // the paste in flight: replies for any other id are ignored
  let fromPanel = false; // whether the paste in flight came from the text box

  const prefs = loadPrefs();
  enterBox.checked = !!prefs.enter;
  keepBox.checked = !!prefs.keep;
  const rememberPrefs = () => savePrefs({ enter: enterBox.checked, keep: keepBox.checked });
  enterBox.addEventListener('change', rememberPrefs);
  keepBox.addEventListener('change', rememberPrefs);

  function setTyping(on) {
    typing = on;
    typeBtn.disabled = on;
    typeBtn.textContent = on ? 'Typing…' : 'Type It';
    stopBtn.hidden = !on;
  }

  function updateCount() {
    count.textContent = `${[...text.value].length} / ${MAX_CHARS}`;
  }

  function start(value, panelText) {
    if (!value || typing) return false;
    if ([...value].length > MAX_CHARS) {
      toast(`That is more than ${MAX_CHARS} characters`, 'error');
      return false;
    }
    currentId = Math.random().toString(36).slice(2);
    const msg = { type: 'type', id: currentId, text: value };
    if (new TextEncoder().encode(JSON.stringify(msg)).length > MAX_MESSAGE_BYTES) {
      toast('That text is too large to send in one paste', 'error');
      return false;
    }
    if (!sendMessage(msg)) {
      toast('Not connected to the console', 'error');
      return false;
    }
    fromPanel = panelText;
    setTyping(true);
    toast(`Typing ${[...value].length} characters on the host…`);
    return true;
  }

  function typePanel() {
    let value = text.value;
    if (!value) {
      text.focus();
      return;
    }
    if (enterBox.checked && !value.endsWith('\n')) value += '\n';
    start(value, true);
  }

  // readClipboard returns the clipboard's text: the browser's when it allows it, else the native
  // clipboard through the server where there is one. It throws when neither works.
  async function readClipboard() {
    try {
      if (navigator.clipboard?.readText) return await navigator.clipboard.readText();
    } catch { /* permission denied, or a webview that refuses */ }
    if (!apiFetch || !nativeClipboard()) throw new Error(BLOCKED);
    const resp = await apiFetch('/api/clipboard');
    const body = await resp.json().catch(() => ({}));
    if (!resp.ok || typeof body.text !== 'string') throw new Error(body.error || `Could not read the clipboard (HTTP ${resp.status})`);
    return body.text;
  }

  // fillFromClipboard puts the clipboard's text in the box; it returns the error, if any.
  async function fillFromClipboard() {
    try {
      text.value = await readClipboard();
    } catch (err) {
      return err;
    }
    updateCount();
    return null;
  }

  // Toasts sit above the panel while it is open, not over its buttons.
  const root = document.documentElement;
  new ResizeObserver(() => {
    root.style.setProperty('--type-panel-height', `${panel.hidden ? 0 : panel.offsetHeight}px`);
  }).observe(panel);

  function open() {
    panel.hidden = false;
    toggleBtn.classList.add('active');
    // Prefill from the clipboard when the browser allows it; the user can edit before typing.
    if (!text.value) fillFromClipboard();
    updateCount();
    text.focus();
  }

  function close() {
    panel.hidden = true;
    toggleBtn.classList.remove('active');
  }

  toggleBtn.addEventListener('click', () => (panel.hidden ? open() : close()));
  document.getElementById('btnClosePaste').addEventListener('click', close);
  document.getElementById('btnPasteClear').addEventListener('click', () => {
    text.value = '';
    updateCount();
    text.focus();
  });
  document.getElementById('btnPasteClipboard').addEventListener('click', async () => {
    const err = await fillFromClipboard();
    if (err) toast(`${err.message}: paste into the box instead`, 'error');
    text.focus();
  });
  text.addEventListener('input', updateCount);
  text.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      typePanel();
    } else if (e.key === 'Escape') {
      close();
    }
  });
  typeBtn.addEventListener('click', typePanel);
  stopBtn.addEventListener('click', () => {
    if (typing) sendMessage({ type: 'type-cancel' });
  });

  return {
    // fromClipboard types the clipboard straight away (Ctrl/Cmd+Shift+V on the console).
    async fromClipboard() {
      try {
        start(await readClipboard(), false);
      } catch (err) {
        toast(`${err.message}: use the Paste button instead`, 'error');
      }
    },
    // onResult handles the server's {"type":"typed"} reply for the paste in flight.
    onResult(msg) {
      if (!typing || msg.id !== currentId) return;
      setTyping(false);
      if (msg.error) {
        toast(`Typing stopped after ${msg.count || 0} characters: ${msg.error}`, 'error', 6000);
        return;
      }
      if (fromPanel && !keepBox.checked) {
        text.value = '';
        updateCount();
      }
      if (msg.skipped) toast(`Typed ${msg.count} characters; skipped ones a US keyboard cannot type: ${msg.skipped}`, 'error', 8000);
      else toast(`Typed ${msg.count} characters`, 'success');
    },
    // reset abandons a paste when the connection drops: the server has stopped typing it.
    reset() {
      if (!typing) return;
      setTyping(false);
      toast('Connection lost: typing stopped', 'error');
    },
  };
}
