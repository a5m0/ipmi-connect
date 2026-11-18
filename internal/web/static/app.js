// ipmi-connect browser console
import { KEY_TO_HID } from '/keymap.js';
import { setupConnect } from '/connect.js';
import { setupMedia } from '/media.js';
import { setupPaste } from '/paste.js';
import { toast } from '/toast.js';
import { setupView } from '/view.js';

(function() {
  'use strict';

  // Elements
  const canvas = document.getElementById('screenCanvas');
  const ctx = canvas.getContext('2d');
  const canvasWrapper = document.getElementById('canvasWrapper');
  const connectingOverlay = document.getElementById('connectingOverlay');
  const overlayStatus = document.getElementById('overlayStatus');
  const overlaySubtext = document.getElementById('overlaySubtext');

  const statusDot = document.getElementById('statusDot');
  const serverTarget = document.getElementById('serverTarget');
  const vendorBadge = document.getElementById('vendorBadge');

  const statRes = document.getElementById('statRes');
  const statFPS = document.getElementById('statFPS');
  const statLatency = document.getElementById('statLatency');

  // Modals

  const powerModal = document.getElementById('powerModal');
  const powerModalTitle = document.getElementById('powerModalTitle');
  const powerModalMessage = document.getElementById('powerModalMessage');
  const btnClosePowerModal = document.getElementById('btnClosePowerModal');
  const btnCancelPower = document.getElementById('btnCancelPower');
  const btnConfirmPower = document.getElementById('btnConfirmPower');

  // State
  let ws = null;
  let saveDialog = false; // the desktop window saves screenshots with a native Save dialog
  let isConnected = false;
  let currentPowerAction = null;
  let currentPowerTitle = '';
  let paste = null; // paste-as-keystrokes, set up in init

  // sendMessage sends a JSON message on the console socket; false when not connected.
  function sendMessage(msg) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return false;
    ws.send(JSON.stringify(msg));
    return true;
  }

  // Initialize
  // Session: the launch link carries a one-time code in its fragment (#c=...), exchanged once for
  // the per-run token. The token is kept in memory and sessionStorage, which is per tab and per
  // origin (the port included), so a reload stays signed in but closing the tab ends it; it is
  // never put in localStorage or a cookie (browsers share 127.0.0.1 cookies across ports).
  const TOKEN_KEY = 'ipmi-connect-token';
  let sessionToken = null;
  let signedOut = false;
  let connectView = null; // the connection manager
  let media = null;       // the virtual media panel
  let phase = '';         // the server's session phase
  let sessionEnded = false; // the server said the console socket is ending on purpose

  async function ensureSession() {
    try { localStorage.removeItem(TOKEN_KEY); } catch (e) { /* older versions kept it there */ }
    const match = window.location.hash.match(/[#&]c=([0-9a-f]+)/);
    if (match) {
      history.replaceState(null, '', window.location.pathname); // drop the code from the address bar
      const resp = await fetch('/api/session', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code: match[1] })
      });
      if (resp.ok) {
        sessionToken = (await resp.json()).token;
        try { sessionStorage.setItem(TOKEN_KEY, sessionToken); } catch (e) { /* the token lives in memory */ }
        return true;
      }
      overlaySubtext.textContent = (await resp.text()).trim();
    }
    try { sessionToken = sessionStorage.getItem(TOKEN_KEY); } catch (e) { sessionToken = null; }
    return !!sessionToken;
  }

  function showSignedOut(message) {
    signedOut = true;
    sessionToken = null;
    try { sessionStorage.removeItem(TOKEN_KEY); } catch (e) { /* ignore */ }
    if (ws) ws.close();
    if (connectView) connectView.hide();
    statusDot.className = 'status-dot';
    connectingOverlay.classList.remove('hidden');
    overlayStatus.textContent = 'Not signed in';
    overlaySubtext.textContent = message || 'Open the console again from the ipmi-connect app or terminal (each link works once).';
  }

  async function apiFetch(path, options = {}) {
    const headers = Object.assign({}, options.headers, { Authorization: `Bearer ${sessionToken}` });
    const resp = await fetch(path, Object.assign({ cache: 'no-store', credentials: 'omit' }, options, { headers }));
    if (resp.status === 401) showSignedOut();
    return resp;
  }

  async function init() {
    if (!(await ensureSession())) {
      showSignedOut(overlaySubtext.textContent);
      return;
    }
    connectView = setupConnect(apiFetch, pollStatus);
    setupMouseAndKeyboard();
    setupPowerButtons();
    setupMacros();
    setupSessionButtons();
    media = setupMedia(apiFetch);
    paste = setupPaste(sendMessage);
    setupView(canvas, document.getElementById('viewport'), () => isConnected);
    setupFullscreenAndScreenshot();
    pollStatus();
    setInterval(pollStatus, 3000);
  }

  // Session buttons: Disconnect ends the BMC session; Quit stops the app (desktop mode).
  function setupSessionButtons() {
    document.getElementById('btnDisconnect').addEventListener('click', async () => {
      if (!window.confirm('Disconnect from the BMC? Virtual media is ejected and the BMC session is logged out.')) return;
      await apiFetch('/api/disconnect', { method: 'POST' });
      pollStatus();
    });
    document.getElementById('btnQuit').addEventListener('click', async () => {
      if (!window.confirm('Quit ipmi-connect? Any BMC session is logged out first.')) return;
      await apiFetch('/api/shutdown', { method: 'POST' });
      showSignedOut('ipmi-connect has quit. You can close this tab.');
    });
    const modal = document.getElementById('securityModal');
    document.getElementById('securityBadge').addEventListener('click', () => modal.classList.add('open'));
    for (const id of ['btnCloseSecurity', 'btnSecurityDone']) {
      document.getElementById(id).addEventListener('click', () => modal.classList.remove('open'));
    }
  }

  const ENCRYPTION = { tls: 'TLS', 'legacy-tls': 'Obsolete TLS', weak: 'Weak (treat as none)', none: 'None' };
  const AUTH = { ca: 'CA-verified certificate', pinned: 'Pinned certificate', none: 'None' };
  const LEVEL = { secure: 'Encrypted', legacy: 'Obsolete TLS', insecure: 'Unencrypted channels' };

  // showSecurity keeps the header badge and the details table in step with the session.
  function showSecurity(data) {
    const badge = document.getElementById('securityBadge');
    const level = data.security_level || '';
    badge.hidden = !level;
    badge.className = `security-badge ${level}`;
    badge.textContent = LEVEL[level] || 'Security';
    const rows = (data.channels || []).map((c) => {
      const tr = document.createElement('tr');
      const cell = (text, cls) => { const td = document.createElement('td'); td.textContent = text || ''; if (cls) td.className = cls; tr.append(td); };
      cell(c.channel);
      cell(ENCRYPTION[c.encryption] || c.encryption, `enc-${c.encryption}`);
      cell(AUTH[c.auth] || c.auth);
      cell([c.protocol, c.cipher, c.note, c.fingerprint ? `SHA-256 ${c.fingerprint}` : ''].filter(Boolean).join(' · '), 'mono small');
      return tr;
    });
    document.getElementById('securityRows').replaceChildren(...rows);
  }

  // Poll status API
  async function pollStatus() {
    try {
      if (signedOut) return;
      const resp = await apiFetch('/api/status');
      if (!resp.ok) return;
      const data = await resp.json();
      saveDialog = !!data.save_dialog;

      // No BMC: the connection manager; a BMC: the console and its socket.
      const was = phase;
      phase = data.phase;
      if (phase === 'connected') {
        connectView.hide();
        if (was !== 'connected') {
          if (!ws || ws.readyState > WebSocket.OPEN) setupWebSocket();
          media.refresh();
        }
      } else {
        connectView.show(data);
        if (ws && ws.readyState <= WebSocket.OPEN) ws.close();
      }
      showSecurity(data);

      serverTarget.textContent = data.host || '127.0.0.1';
      vendorBadge.textContent = (data.target_type || 'IPMI').toUpperCase();
      // The server decodes the stream, so it reports the BMC's frame rate; the browser only sees
      // changes.
      statFPS.textContent = `FPS: ${data.fps || 0}`;
      showPower(data.power_state);
      // Console notices such as "No video signal" or "View only"; empty when all is well.
      const notice = document.getElementById('statNotice');
      notice.textContent = data.status_text || '';
      notice.hidden = !data.status_text;

      // Canvas size is owned by the video stream: resizing a canvas clears it, so status polls
      // must never touch it.
    } catch (e) {
      console.warn('Status poll error:', e);
    }
  }

  // Power state: a pill in the header. Only Power On (while on) and Shut Down (while off) are
  // disabled: the reading can be up to 15 s old, so the hard actions always stay available.
  function showPower(state) {
    const pill = document.getElementById('powerPill');
    const on = state === 'ON';
    const off = state === 'OFF';
    pill.textContent = on ? 'Power On' : off ? 'Power Off' : 'Power ?';
    pill.classList.toggle('on', on);
    pill.classList.toggle('off', off);
    document.getElementById('btnPowerOn').disabled = on;
    document.getElementById('btnPowerSoft').disabled = off;
  }

  // After a power action the server re-reads the state every second for 30 s; follow it.
  let fastStatusUntil = 0;
  let following = false;
  function followPower() {
    fastStatusUntil = Date.now() + 30000; // repeated actions extend the one running loop
    if (following) return;
    following = true;
    const tick = () => {
      pollStatus();
      if (Date.now() < fastStatusUntil) setTimeout(tick, 1000);
      else following = false;
    };
    setTimeout(tick, 1000);
  }

  // WebSocket Setup. The socket URL carries a single-use ticket that expires in seconds, never
  // the session token: URLs end up in more places than headers do.
  async function setupWebSocket() {
    if (signedOut || phase !== 'connected') return;
    let ticket;
    try {
      const resp = await apiFetch('/api/ws-ticket', { method: 'POST' });
      if (!resp.ok) { if (!signedOut) setTimeout(setupWebSocket, 3000); return; }
      ticket = (await resp.json()).ticket;
    } catch (e) {
      setTimeout(setupWebSocket, 3000);
      return;
    }
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsUrl = `${proto}//${window.location.host}/ws?ticket=${encodeURIComponent(ticket)}`;

    ws = new WebSocket(wsUrl);
    ws.binaryType = 'arraybuffer';

    ws.onopen = () => {
      isConnected = true;
      statusDot.className = 'status-dot connected';
      overlayStatus.textContent = 'Active Console Stream';
      overlaySubtext.textContent = 'Receiving video frames';
      setTimeout(() => {
        connectingOverlay.classList.add('hidden');
      }, 500);
    };

    ws.onclose = (ev) => {
      if (paste) paste.reset();
      if (signedOut) return;
      if (sessionEnded) { // disconnected or signed out: the status poll decides what to show
        sessionEnded = false;
        isConnected = false;
        pollStatus();
        return;
      }
      isConnected = false;
      statusDot.className = 'status-dot';
      connectingOverlay.classList.remove('hidden');
      overlayStatus.textContent = 'Connection Closed';
      overlaySubtext.textContent = 'Attempting to reconnect in 3s...';
      if (!signedOut) setTimeout(() => { if (phase === 'connected') setupWebSocket(); }, 3000);
    };

    ws.onerror = (err) => {
      console.error('WebSocket error:', err);
    };

    ws.onmessage = (event) => {
      if (typeof event.data === 'string') {
        try {
          const msg = JSON.parse(event.data);
          handleTextMessage(msg);
        } catch (e) {}
      } else if (event.data instanceof ArrayBuffer) {
        handleBinaryFrame(event.data);
      }
    };
  }

  // Handle server text message
  function handleTextMessage(msg) {
    if (msg.type === 'resize') {
      canvas.width = msg.width;
      canvas.height = msg.height;
      statRes.textContent = `Resolution: ${msg.width} x ${msg.height}`;
    } else if (msg.type === 'status') {
      overlayStatus.textContent = msg.text || 'Processing...';
    } else if (msg.type === 'session-ended') {
      sessionEnded = true; // the BMC was disconnected or the browser signed out: do not reconnect
    } else if (msg.type === 'typed') {
      paste.onResult(msg);
    } else if (msg.type === 'ping') {
      const now = performance.now();
      const rtt = Math.round(now - msg.client_time);
      statLatency.textContent = `Ping: ${rtt} ms`;
    }
  }

  // Video: the server decodes the BMC stream and sends RGBA.
  //   0x02 full frame: [u16 width][u16 height][RGBA]
  //   0x04 changes:    [u16 count] then per rect [u16 x][u16 y][u16 w][u16 h][RGBA]
  // Messages only update a copy of the screen in memory (frame) and grow a dirty box; the canvas
  // is painted once per display frame with a single putImageData of that box. Painting every
  // 16-pixel strip as it arrived cost about 50 putImageData calls per frame, which starved the
  // main thread in WebKit.
  let frame = null;
  let dirty = null; // {x0, y0, x1, y1}
  let paintQueued = false;

  function markDirty(x, y, w, h) {
    if (!dirty) dirty = { x0: x, y0: y, x1: x + w, y1: y + h };
    else {
      dirty.x0 = Math.min(dirty.x0, x); dirty.y0 = Math.min(dirty.y0, y);
      dirty.x1 = Math.max(dirty.x1, x + w); dirty.y1 = Math.max(dirty.y1, y + h);
    }
    if (!paintQueued) {
      paintQueued = true;
      requestAnimationFrame(paintFrame);
    }
  }

  // paintFrame copies the dirty part of the frame to the canvas. It also runs straight away
  // before a screenshot, so the capture includes updates still waiting for a display frame.
  function paintFrame() {
    paintQueued = false;
    if (!dirty || !frame || frame.width !== canvas.width || frame.height !== canvas.height) {
      dirty = null;
      return;
    }
    const d = dirty;
    dirty = null;
    ctx.putImageData(frame, 0, 0, d.x0, d.y0, d.x1 - d.x0, d.y1 - d.y0);
  }

  function handleBinaryFrame(buf) {
    const view = new DataView(buf);
    const type = view.getUint8(0);
    if (type === 0x02) {
      const w = view.getUint16(1);
      const h = view.getUint16(3);
      if (canvas.width !== w || canvas.height !== h) {
        canvas.width = w; // resizing clears the canvas; the full frame repaints it
        canvas.height = h;
        statRes.textContent = `Resolution: ${w} x ${h}`;
      }
      if (!frame || frame.width !== w || frame.height !== h) frame = new ImageData(w, h);
      frame.data.set(new Uint8Array(buf, 5, w * h * 4));
      markDirty(0, 0, w, h);
    } else if (type === 0x04) {
      if (!frame) return; // changes before any full frame have nothing to apply to
      const fw = frame.width, fh = frame.height, data = frame.data;
      let off = 3;
      for (let n = view.getUint16(1); n > 0; n--) {
        const x = view.getUint16(off), y = view.getUint16(off + 2);
        const w = view.getUint16(off + 4), h = view.getUint16(off + 6);
        off += 8;
        if (x + w > fw || y + h > fh) return; // a rect for another resolution: wait for the full frame
        const row = w * 4;
        if (x === 0 && w === fw) { // whole rows are contiguous in both buffers
          data.set(new Uint8Array(buf, off, row * h), y * fw * 4);
        } else {
          for (let r = 0; r < h; r++) data.set(new Uint8Array(buf, off + r * row, row), ((y + r) * fw + x) * 4);
        }
        off += row * h;
        markDirty(x, y, w, h);
      }
    }
  }

  // Mouse and Keyboard Input Capture
  function setupMouseAndKeyboard() {
    canvas.tabIndex = 1000;
    let currentButtons = 0;

    function send(msg) {
      if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
    }

    function sendPointer(mask, clientX, clientY) {
      const rect = canvas.getBoundingClientRect();
      const canvasX = Math.round((clientX - rect.left) * (canvas.width / rect.width));
      const canvasY = Math.round((clientY - rect.top) * (canvas.height / rect.height));
      send({
        type: 'pointer',
        mask: mask,
        x: Math.max(0, Math.min(canvas.width - 1, canvasX)),
        y: Math.max(0, Math.min(canvas.height - 1, canvasY))
      });
    }

    // Ask the server to release every key and button this tab is holding on the host.
    function releaseAll() {
      currentButtons = 0;
      send({ type: 'release' });
    }

    // RFB button bits: left 1, middle 2, right 4 (the server converts per BMC).
    const BUTTON_BITS = [1, 2, 4];

    canvas.addEventListener('mousemove', (e) => {
      sendPointer(currentButtons, e.clientX, e.clientY);
    });

    canvas.addEventListener('mousedown', (e) => {
      e.preventDefault();
      canvas.focus();
      currentButtons |= BUTTON_BITS[e.button] || 0;
      sendPointer(currentButtons, e.clientX, e.clientY);
    });

    // On window, not the canvas: a button released outside the console must still be released.
    window.addEventListener('mouseup', (e) => {
      const bit = BUTTON_BITS[e.button] || 0;
      if (!(currentButtons & bit)) return;
      currentButtons &= ~bit;
      sendPointer(currentButtons, e.clientX, e.clientY);
    });

    canvas.addEventListener('contextmenu', (e) => e.preventDefault());

    function consoleFocused() {
      return document.activeElement === canvas || canvasWrapper.contains(document.activeElement);
    }

    window.addEventListener('keydown', (e) => {
      if (!consoleFocused()) return;
      // Ctrl/Cmd+Shift+V types the clipboard on the host instead of sending the chord.
      if ((e.ctrlKey || e.metaKey) && e.shiftKey && e.code === 'KeyV') {
        e.preventDefault();
        if (e.repeat) return;
        releaseAll();
        paste.fromClipboard();
        return;
      }
      // Keys belong to the host while the console has focus: stop Backspace, Space, arrows, Tab,
      // F-keys and browser shortcuts acting on this page. (The OS still keeps a few, like Cmd+Q.)
      e.preventDefault();
      if (e.repeat) return; // the host's own key repeat handles held keys
      send({ type: 'key', down: true, hidCode: KEY_TO_HID[e.code] || 0 });
    });

    window.addEventListener('keyup', (e) => {
      if (!consoleFocused()) return;
      e.preventDefault();
      send({ type: 'key', down: false, hidCode: KEY_TO_HID[e.code] || 0 });
      // macOS browsers send no keyup for keys released while Cmd is held, so Cmd+C would leave
      // C held on the host. Releasing Cmd releases everything.
      if (e.code === 'MetaLeft' || e.code === 'MetaRight') releaseAll();
    });

    // Focus leaving the page means keyups will not arrive here: release what is held.
    window.addEventListener('blur', releaseAll);
    document.addEventListener('visibilitychange', () => {
      if (document.hidden) releaseAll();
    });
  }

  // Macro Toolbar
  function setupMacros() {
    document.querySelectorAll('.btn-macro').forEach(btn => {
      btn.addEventListener('click', () => {
        const macro = btn.getAttribute('data-macro');
        if (ws && ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({
            type: 'macro',
            name: macro
          }));
        }
      });
    });
  }

  // Power Controls
  function setupPowerButtons() {
    function openPowerConfirm(action, title, msg) {
      currentPowerAction = action;
      currentPowerTitle = title.replace(/^Confirm /, '');
      powerModalTitle.textContent = title;
      powerModalMessage.textContent = msg;
      powerModal.classList.add('open');
    }

    document.getElementById('btnPowerSoft').addEventListener('click', () => {
      openPowerConfirm('power_soft_off', 'Confirm Shut Down', 'Ask the operating system to shut down cleanly (like pressing the power button)? The host may take a while, or ignore it if no OS is running.');
    });
    document.getElementById('btnPowerOn').addEventListener('click', () => {
      openPowerConfirm('power_on', 'Confirm Power On', 'Power on host system now?');
    });
    document.getElementById('btnPowerOff').addEventListener('click', () => {
      openPowerConfirm('power_off', 'Confirm Force Power Off', 'Immediately cut power to host server? Warning: Unsaved data may be lost.');
    });
    document.getElementById('btnPowerReset').addEventListener('click', () => {
      openPowerConfirm('power_reset', 'Confirm Hard Reset', 'Issue an immediate hardware reset to the host system?');
    });
    document.getElementById('btnPowerCycle').addEventListener('click', () => {
      openPowerConfirm('power_cycle', 'Confirm Power Cycle', 'Turn off and restart the host system?');
    });

    btnClosePowerModal.addEventListener('click', () => powerModal.classList.remove('open'));
    btnCancelPower.addEventListener('click', () => powerModal.classList.remove('open'));

    btnConfirmPower.addEventListener('click', async () => {
      if (!currentPowerAction) return;
      powerModal.classList.remove('open');
      try {
        const resp = await apiFetch('/api/power', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ action: currentPowerAction, confirm: true }) // the user confirmed in the dialog
        });
        const res = await resp.json();
        if (!resp.ok || res.error) {
          toast(`Power action failed: ${res.error || 'server error'}`, 'error', 6000);
        } else {
          toast(`${currentPowerTitle} sent`, 'success');
          followPower();
        }
      } catch (err) {
        toast(`Power action failed: ${err.message}`, 'error', 6000);
      }
    });
  }

  // Fullscreen & Screenshot
  function setupFullscreenAndScreenshot() {

    document.getElementById('btnScreenshot').addEventListener('click', async () => {
      if (saveDialog) {
        // Embedded webviews drop <a download> silently: the server saves its own copy of the
        // screen through the native Save dialog instead.
        try {
          const resp = await apiFetch('/api/screenshot/save', { method: 'POST' });
          const body = await resp.json().catch(() => ({}));
          if (!resp.ok) throw new Error(body.error || `HTTP ${resp.status}`);
          if (body.saved) toast(`Screenshot saved to ${body.path}`);
        } catch (err) {
          toast(`Could not save the screenshot: ${err.message}`, 'error', 6000);
        }
        return;
      }
      paintFrame(); // include changes still waiting for the next display frame
      const a = document.createElement('a');
      a.download = `ipmi-console-${Date.now()}.png`;
      a.href = canvas.toDataURL('image/png');
      a.click();
    });
  }

  // Start app
  window.addEventListener('DOMContentLoaded', init);
})();
