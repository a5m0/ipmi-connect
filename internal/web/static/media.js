// Virtual media panel: one row per drive with Insert/Eject, the recent images, the transfer
// graph and USB reset. Everything from the server is inserted with textContent, never as HTML.
import { createPicker } from '/picker.js';

const TARGET_NAMES = { cd: 'CD/DVD', removable: 'Removable disk', floppy: 'Floppy' };
const TARGET_HINTS = { cd: '.iso images', removable: '.img disk images', floppy: 'small .img images' };
const TARGETS = ['cd', 'removable', 'floppy'];
const POLL_OPEN_MS = 1000;   // while the panel is open: the graph is live
const POLL_CLOSED_MS = 3000; // otherwise: just the toolbar badge (1 s while a drive is mapped)

export function formatBytes(n) {
  if (n < 1024) return `${Math.round(n)} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  Object.assign(node, props);
  for (const c of children) node.append(c);
  return node;
}

function baseName(path) { return path.split('/').pop(); }

export function setupMedia(apiFetch) {
  const button = document.getElementById('btnMedia');
  const badge = document.getElementById('mediaBadge');
  const modal = document.getElementById('mediaModal');
  const imagesBody = document.getElementById('mediaImages');
  const drivesBox = document.getElementById('mediaDrives');
  const errorLine = document.getElementById('mediaError');
  const rates = document.getElementById('mediaRates');
  const graph = document.getElementById('mediaGraph');
  const statMedia = document.getElementById('statMedia');
  const pick = createPicker(apiFetch);
  let status = null;
  let busy = false; // an action is in flight: keep its message until it answers
  let actionError = ''; // the last action's error: shown until the next action, not cleared by polls
  let timer = null;
  let imagesKey = ''; // what the image rows were last built from
  const chosen = {};  // drive picked in each image's dropdown, kept across rebuilds
  const writable = {}; // per drive: insert the next image writable (confirmed)
  const cards = {};    // per drive: { key, node, counters }
  const lastCount = {}; // per drive: { at, read, written } from the previous poll, for its rate

  function showError(text) {
    errorLine.textContent = text || '';
    errorLine.hidden = !text;
  }

  async function call(method, path, body) {
    const options = { method };
    if (body !== undefined) {
      options.headers = { 'Content-Type': 'application/json' };
      options.body = JSON.stringify(body);
    }
    const resp = await apiFetch(path, options);
    const res = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(res.error || `Failed (HTTP ${resp.status})`);
    return res;
  }

  // act runs an action, then shows the status it returns (or its error).
  async function act(method, path, body, pending, done = '') {
    busy = true;
    actionError = '';
    showError(pending || '');
    try {
      render(await call(method, path, body));
      showError(done);
    } catch (e) {
      actionError = e.message;
      showError(e.message);
    } finally {
      busy = false;
    }
  }

  // insert asks for a file and puts it into drive t, adding it to the recent list on the way.
  async function insert(t) {
    const path = await pick('file', `Insert into ${TARGET_NAMES[t]} (${TARGET_HINTS[t]})`);
    if (!path) return;
    const w = Boolean(writable[t]) && t !== 'cd';
    await act('POST', `/api/media/drives/${t}`, { path, read_only: !w, confirm_writable: w },
      `Inserting ${baseName(path)}… (the BMC can take a few seconds to free a drive)`);
  }

  function writableBox(t) {
    const box = el('input', { type: 'checkbox', checked: Boolean(writable[t]) });
    box.addEventListener('change', () => {
      if (box.checked && !window.confirm(`Let the host write to the image you insert into ${TARGET_NAMES[t]}? It can change or erase everything in it.`)) {
        box.checked = false;
      }
      writable[t] = box.checked;
    });
    return el('label', { className: 'media-check media-writable', title: 'Off: the host can only read the image' }, box, ' Writable');
  }

  // A drive's rate since the previous poll, from its byte counters.
  function driveRate(d) {
    const now = Date.now();
    const prev = lastCount[d.target];
    lastCount[d.target] = { at: now, read: d.bytes_read, written: d.bytes_written, image: d.image_id };
    if (!prev || prev.image !== d.image_id || now <= prev.at) return null;
    const secs = (now - prev.at) / 1000;
    return { read: Math.max(0, d.bytes_read - prev.read) / secs, written: Math.max(0, d.bytes_written - prev.written) / secs };
  }

  function counterText(d) {
    const rate = driveRate(d);
    let text = `read ${formatBytes(d.bytes_read)}`;
    if (!d.read_only) text += ` · written ${formatBytes(d.bytes_written)}`;
    if (rate && rate.read + rate.written > 0) {
      text += ` · ${formatBytes(rate.read + rate.written)}/s`;
    } else if (rate) {
      text += ' · idle';
    }
    return text;
  }

  // driveCard builds drive t's row; it is rebuilt only when what it shows changes, and its
  // counters are updated in place, so a click or a ticked box is never lost to a poll.
  function driveCard(t) {
    const d = status.drives.find((x) => x.target === t);
    const key = JSON.stringify(d ? [d.image_id, d.mounting, d.read_only] : null);
    let card = cards[t];
    if (!card || card.key !== key) {
      const icon = el('span', { className: `media-drive-icon ${t}`, ariaHidden: 'true' });
      const name = el('div', { className: 'media-drive-name', textContent: TARGET_NAMES[t] });
      const body = el('div', { className: 'media-drive-body' }, name);
      const actions = el('div', { className: 'media-drive-actions' });
      let counters = null;
      if (!d) {
        body.append(el('div', { className: 'media-drive-state empty', textContent: `Empty: takes ${TARGET_HINTS[t]}` }));
        if (t !== 'cd') actions.append(writableBox(t));
        const ins = el('button', { className: 'btn btn-primary media-insert', textContent: 'Insert Image…' });
        ins.addEventListener('click', () => insert(t));
        actions.append(ins);
      } else {
        const mode = d.mounting ? 'inserting…' : d.read_only ? 'read-only' : 'writable';
        body.append(el('div', { className: 'media-drive-state' },
          el('span', { className: 'media-drive-image', textContent: d.image }),
          el('span', { className: `media-tag ${d.read_only ? '' : 'warn'}`, textContent: mode })));
        counters = el('div', { className: 'media-counters mono' });
        body.append(counters);
        const eject = el('button', { className: 'btn btn-secondary media-eject', textContent: d.mounting ? 'Cancel' : 'Eject' });
        eject.addEventListener('click', () => act('DELETE', `/api/media/drives/${t}`));
        actions.append(eject);
      }
      card = { key, node: el('div', { className: `media-drive${d ? ' loaded' : ''}` }, icon, body, actions), counters };
      card.node.dataset.drive = t;
      cards[t] = card;
    }
    if (card.counters && d && !d.mounting) card.counters.textContent = counterText(d);
    return card.node;
  }

  function imageRow(img) {
    const access = el('td');
    if (img.kind === 'iso') {
      access.textContent = 'read-only';
      access.title = 'ISO images are always read-only';
    } else {
      const ro = el('input', { type: 'checkbox', checked: !img.read_only, disabled: Boolean(img.mapped_to) });
      ro.title = img.mapped_to ? 'Eject it to change this' : 'Let the host write to this image';
      ro.addEventListener('change', () => {
        if (ro.checked && !window.confirm(`Let the host write to ${img.name}? It can change or erase everything in the image.`)) {
          ro.checked = false;
          return;
        }
        act('PATCH', `/api/media/images/${encodeURIComponent(img.id)}`, { read_only: !ro.checked, confirm_writable: ro.checked });
      });
      access.append(el('label', { className: 'media-check' }, ro, ' Writable'));
    }

    const actions = el('td', { className: 'actions' });
    if (img.mapped_to) {
      actions.append(el('span', { className: 'empty', textContent: `in ${TARGET_NAMES[img.mapped_to] || img.mapped_to}` }));
    } else {
      const free = img.targets.filter((t) => !status.drives.some((d) => d.target === t));
      if (free.length) {
        if (!free.includes(chosen[img.id])) chosen[img.id] = free[0];
        const ins = el('button', { className: 'btn btn-secondary media-reinsert' });
        const label = () => { ins.textContent = `Insert into ${TARGET_NAMES[chosen[img.id]]}`; };
        label();
        ins.addEventListener('click', () => act('POST', `/api/media/drives/${chosen[img.id]}`, { image_id: img.id },
          `Inserting ${img.name}… (the BMC can take a few seconds to free a drive)`));
        if (free.length > 1) {
          const select = el('select', { title: 'Drive' });
          for (const t of free) select.append(el('option', { value: t, textContent: TARGET_NAMES[t] }));
          select.value = chosen[img.id];
          select.addEventListener('change', () => { chosen[img.id] = select.value; label(); });
          actions.append(select);
        }
        actions.append(ins);
      } else {
        actions.append(el('span', { className: 'empty', textContent: 'its drive is in use' }));
      }
      const remove = el('button', { className: 'btn btn-secondary', textContent: 'Remove', title: 'Take it off this list (the file is kept)' });
      remove.addEventListener('click', () => act('DELETE', `/api/media/images/${encodeURIComponent(img.id)}`));
      actions.append(remove);
    }
    const name = el('td', { textContent: img.name, title: img.path });
    return el('tr', {}, name, el('td', { className: 'mono', textContent: formatBytes(img.size) }), access, actions);
  }

  function drawGraph(history) {
    // Draw at the canvas's displayed size and the screen's pixel ratio, so text is not stretched.
    const ratio = window.devicePixelRatio || 1;
    const w = graph.clientWidth || 720;
    const h = graph.clientHeight || 90;
    if (graph.width !== Math.round(w * ratio) || graph.height !== Math.round(h * ratio)) {
      graph.width = Math.round(w * ratio);
      graph.height = Math.round(h * ratio);
    }
    const ctx = graph.getContext('2d');
    ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
    ctx.clearRect(0, 0, w, h);
    const samples = history.slice(-300);
    const max = Math.max(1, ...samples.map((s) => Math.max(s.read, s.written)));
    const css = getComputedStyle(document.documentElement);
    const line = (key, colour) => {
      ctx.strokeStyle = colour;
      ctx.lineWidth = 1.5;
      ctx.beginPath();
      samples.forEach((s, i) => {
        const x = w - (samples.length - 1 - i) * (w / 299);
        const y = h - 4 - (s[key] / max) * (h - 12);
        if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
      });
      ctx.stroke();
    };
    line('read', css.getPropertyValue('--accent').trim() || '#38bdf8');
    line('written', css.getPropertyValue('--warning').trim() || '#f59e0b');
    ctx.fillStyle = css.getPropertyValue('--text-muted').trim() || '#64748b';
    ctx.font = '10px monospace';
    ctx.fillText(`${formatBytes(max)}/s`, 6, 12);
    ctx.fillText('5 min', 6, h - 6);

    const last = samples[samples.length - 1] || { read: 0, written: 0 };
    rates.replaceChildren(
      el('span', { className: 'read', textContent: `read ${formatBytes(last.read)}/s` }), ' · ',
      el('span', { className: 'write', textContent: `write ${formatBytes(last.written)}/s` }));
  }

  // showRate puts a small live bandwidth readout in the stats bar while any drive is mapped.
  function showRate(st, mapped) {
    statMedia.hidden = mapped === 0;
    if (mapped === 0) return;
    const history = st.history || [];
    const last = history[history.length - 1] || { read: 0, written: 0 };
    const busyNow = last.read + last.written > 0;
    statMedia.textContent = busyNow
      ? `VM ↓ ${formatBytes(last.read)}/s ↑ ${formatBytes(last.written)}/s`
      : 'VM idle';
    statMedia.title = busyNow
      ? 'Virtual media transfer (read from / written to your images)'
      : 'Virtual media is mapped; the host is not reading it right now';
    statMedia.classList.toggle('active', busyNow);
  }

  function render(st) {
    status = st;
    const mapped = st.drives.filter((d) => !d.mounting).length;
    badge.textContent = String(mapped);
    badge.hidden = mapped === 0;
    showRate(st, mapped);
    if (mapped > 0 !== fastPoll) schedule();
    if (!modal.classList.contains('open')) return;

    drivesBox.replaceChildren(...TARGETS.map(driveCard));
    // Rebuild the image rows only when they would change: rebuilding every poll would close an
    // open dropdown under the user.
    const key = JSON.stringify([st.images, st.drives.map((d) => d.target)]);
    if (key !== imagesKey) {
      imagesKey = key;
      imagesBody.replaceChildren(...(st.images.length
        ? st.images.map(imageRow)
        : [el('tr', {}, el('td', { colSpan: 4, className: 'empty', textContent: 'Images you insert are listed here, to insert again with one click.' }))]));
    }
    drawGraph(st.history || []);
    if (!busy) showError(actionError || st.error || '');
  }

  async function poll() {
    try {
      const resp = await apiFetch('/api/media');
      if (resp.status === 501) { stop(); button.hidden = true; return; } // no virtual media (or no BMC)
      button.hidden = false;
      if (resp.ok) render(await resp.json());
    } catch (e) {
      console.warn('Media poll error:', e);
    }
  }

  let fastPoll = false;

  function stop() {
    clearInterval(timer);
    timer = null;
  }

  // Poll every second while the panel is open or a drive is mapped (the stats bar shows the
  // rate); otherwise every few seconds.
  function schedule() {
    stop();
    fastPoll = Boolean(status && status.drives.some((d) => !d.mounting));
    timer = setInterval(poll, modal.classList.contains('open') || fastPoll ? POLL_OPEN_MS : POLL_CLOSED_MS);
  }

  function open() {
    modal.classList.add('open');
    actionError = '';
    imagesKey = ''; // rows are not kept up to date while closed
    for (const t of TARGETS) delete cards[t];
    poll();
    schedule();
  }

  function close() {
    modal.classList.remove('open');
    schedule();
  }

  button.addEventListener('click', open);
  document.getElementById('btnCloseMedia').addEventListener('click', close);
  document.getElementById('btnMediaDone').addEventListener('click', close);
  document.getElementById('mediaAddForm').addEventListener('submit', (e) => {
    e.preventDefault();
    const input = document.getElementById('mediaPath');
    const path = input.value.trim();
    if (!path) return;
    const w = document.getElementById('mediaAddWritable').checked;
    if (w && !window.confirm('Add the image writable? The host can change or erase everything in it.')) return;
    act('POST', '/api/media/images', { path, read_only: !w, confirm_writable: w })
      .then(() => { if (errorLine.hidden) input.value = ''; });
  });
  const createForm = document.getElementById('mediaCreateForm');
  const sizeSelect = document.getElementById('mediaCreateSize');
  const folderInput = document.getElementById('mediaCreateFolder');
  const browseFolder = document.getElementById('btnBrowseFolder');
  createForm.addEventListener('change', () => {
    const fromFolder = createForm.elements.createKind.value === 'folder';
    sizeSelect.hidden = fromFolder;
    folderInput.hidden = !fromFolder;
    browseFolder.hidden = !fromFolder;
  });
  browseFolder.addEventListener('click', async () => {
    const path = await pick('dir', 'Choose the folder to copy into the image');
    if (path) folderInput.value = path;
  });
  document.getElementById('btnBrowseSave').addEventListener('click', async () => {
    const label = document.getElementById('mediaCreateLabel').value.trim().toLowerCase();
    const path = await pick('save', 'Where should the new image go?', `${label || 'rescue'}.img`);
    if (path) document.getElementById('mediaCreatePath').value = path;
  });
  createForm.addEventListener('submit', (e) => {
    e.preventDefault();
    const path = document.getElementById('mediaCreatePath').value.trim();
    if (!path) return;
    const body = { path, label: document.getElementById('mediaCreateLabel').value.trim() };
    if (createForm.elements.createKind.value === 'folder') body.folder = folderInput.value.trim();
    else body.size = Number(sizeSelect.value);
    act('POST', '/api/media/create', body, 'Creating image…', 'Image created: insert it from Recent images.');
  });
  document.getElementById('btnUsbReset').addEventListener('click', () => {
    act('POST', '/api/media/usb-reset', undefined, 'Resetting USB…',
      'USB reset sent: the host sees the virtual media device reconnect; images stay mapped.');
  });

  poll();
  schedule();
  return {
    // refresh restarts polling after a (re)connection: a BMC without virtual media stops it.
    refresh() { imagesKey = ''; button.hidden = true; poll(); schedule(); },
  };
}
