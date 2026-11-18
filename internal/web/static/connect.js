// Connection manager: choose a BMC, confirm its certificate and any obsolete protocols, connect.
// Server data goes into the page with textContent only. The password lives in the form field
// until the request is sent, then the field is cleared; it is never stored.

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  Object.assign(node, props);
  for (const c of children) node.append(c);
  return node;
}

function shortDate(s) {
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? '' : d.toISOString().slice(0, 10);
}

export function setupConnect(apiFetch, onConnected) {
  const view = document.getElementById('connectView');
  const form = document.getElementById('connectForm');
  const address = document.getElementById('cAddress');
  const user = document.getElementById('cUser');
  const password = document.getElementById('cPassword');
  const vendor = document.getElementById('cVendor');
  const save = document.getElementById('cSave');
  const error = document.getElementById('connectError');
  const connectBtn = document.getElementById('btnConnect');
  const cancelBtn = document.getElementById('btnCancelConnect');
  const consentModal = document.getElementById('consentModal');
  const consentBody = document.getElementById('consentBody');
  const consentAccept = document.getElementById('btnConsentAccept');
  let pending = null; // the request awaiting consent (without the password once answered)
  let busy = false;

  function showError(text) {
    error.textContent = text || '';
    error.hidden = !text;
  }

  async function json(resp) {
    try { return await resp.json(); } catch { return {}; }
  }

  async function loadLists() {
    const resp = await apiFetch('/api/profiles');
    if (!resp.ok) return;
    const data = await json(resp);
    const profiles = document.getElementById('profileList');
    profiles.replaceChildren(...(data.profiles || []).map((p) => {
      const use = el('button', { type: 'button', className: 'link-button', textContent: p.name });
      use.title = `${p.user}@${p.address} (${p.vendor})${p.fingerprints && p.fingerprints.length ? `\nPinned: ${p.fingerprints.join(', ')}` : ''}`;
      use.addEventListener('click', () => fill(p.address, p.user, p.vendor));
      const del = el('button', { type: 'button', className: 'link-button danger', textContent: 'delete' });
      del.addEventListener('click', async () => {
        if (!window.confirm(`Delete the saved device "${p.name}"?`)) return;
        await apiFetch(`/api/profiles/${encodeURIComponent(p.id)}`, { method: 'DELETE' });
        loadLists();
      });
      return el('li', {}, use, el('span', { className: 'muted', textContent: ` ${p.user}@${p.address}` }), ' ', del);
    }));
    if (!profiles.children.length) profiles.append(el('li', { className: 'muted', textContent: 'None yet.' }));

    const recents = document.getElementById('recentList');
    recents.replaceChildren(...(data.recents || []).map((r) => {
      const use = el('button', { type: 'button', className: 'link-button', textContent: `${r.user}@${r.address}` });
      use.addEventListener('click', () => fill(r.address, r.user, r.vendor));
      return el('li', {}, use, el('span', { className: 'muted', textContent: ` ${r.vendor}` }));
    }));
    if (!recents.children.length) recents.append(el('li', { className: 'muted', textContent: 'None yet.' }));

    const tresp = await apiFetch('/api/trust');
    const trust = document.getElementById('trustList');
    const devices = tresp.ok ? ((await json(tresp)).devices || []) : [];
    trust.replaceChildren(...devices.map((d) => {
      const what = [];
      if (d.fingerprints && d.fingerprints.length) what.push(`${d.fingerprints.length} pinned certificate${d.fingerprints.length > 1 ? 's' : ''}`);
      if (d.legacy_tls) what.push('obsolete TLS allowed');
      if (d.insecure_channels) what.push('unencrypted channels allowed');
      const forget = el('button', { type: 'button', className: 'link-button danger', textContent: 'forget' });
      forget.addEventListener('click', async () => {
        if (!window.confirm(`Forget the certificates and permissions for ${d.device}? The next connection will ask again.`)) return;
        await apiFetch(`/api/trust/${encodeURIComponent(d.device)}`, { method: 'DELETE' });
        loadLists();
      });
      const item = el('li', {}, el('strong', { textContent: d.device }), el('span', { className: 'muted', textContent: ` ${what.join(', ')} ` }), forget);
      item.title = (d.fingerprints || []).join('\n');
      return item;
    }));
    if (!trust.children.length) trust.append(el('li', { className: 'muted', textContent: 'No device trusted yet.' }));
  }

  function fill(addr, usr, vnd) {
    address.value = addr;
    user.value = usr || 'root';
    vendor.value = vnd || 'auto';
    password.value = '';
    password.focus();
  }

  document.getElementById('btnClearRecents').addEventListener('click', async () => {
    await apiFetch('/api/recents', { method: 'DELETE' });
    loadLists();
  });

  function setBusy(on) {
    busy = on;
    connectBtn.disabled = on;
    connectBtn.textContent = on ? 'Connecting…' : 'Connect';
    cancelBtn.hidden = !on;
  }

  async function connect(req) {
    setBusy(true);
    showError('');
    try {
      const resp = await apiFetch('/api/connect', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(req),
      });
      const data = await json(resp);
      if (resp.status === 409 && data.consent) {
        pending = req;
        showConsent(data.consent);
        return;
      }
      pending = null;
      password.value = '';
      if (!resp.ok) {
        showError(data.error || `Could not connect (HTTP ${resp.status})`);
        return;
      }
      if (save.checked) {
        await apiFetch('/api/profiles', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: req.address, address: req.address, user: req.user, vendor: req.vendor }),
        });
        save.checked = false;
      }
      onConnected();
    } catch (e) {
      showError(`Could not connect: ${e.message}`);
    } finally {
      setBusy(false);
    }
  }

  form.addEventListener('submit', (e) => {
    e.preventDefault();
    if (busy) return;
    connect({
      address: address.value.trim(), user: user.value.trim(), password: password.value, vendor: vendor.value,
      trust_fingerprints: [], replace_fingerprints: false, allow_legacy_tls: false, allow_insecure_channels: false,
    });
  });
  cancelBtn.addEventListener('click', () => apiFetch('/api/disconnect', { method: 'POST' }));

  // showConsent lists every decision; Trust and Connect stays disabled until each is ticked.
  function showConsent(c) {
    const checks = [];
    const box = (label) => {
      const input = el('input', { type: 'checkbox' });
      input.addEventListener('change', update);
      checks.push(input);
      return el('label', { className: 'media-check consent-check' }, input, ` ${label}`);
    };
    const parts = [];
    let changed = false;
    for (const d of c.certificates || []) {
      changed = changed || d.status === 'changed';
      const rows = [
        ['Endpoint', `${d.endpoint} (${d.protocol}, ${d.cipher})`],
        ['Subject', d.cert.subject], ['Issuer', d.cert.issuer],
        ['Valid', `${shortDate(d.cert.not_before)} to ${shortDate(d.cert.not_after)}`],
        ['Key', d.cert.key], ['SHA-256', d.cert.fingerprint],
      ];
      if (d.status === 'changed') rows.push(['Pinned before', (d.pinned || []).join(', ')]);
      const table = el('table', { className: 'cert-table' },
        ...rows.map(([k, v]) => el('tr', {}, el('th', { textContent: k }), el('td', { className: k === 'SHA-256' || k === 'Pinned before' ? 'mono' : '', textContent: v || '' }))));
      if (d.status === 'changed') {
        parts.push(el('div', { className: 'consent-warning' },
          el('strong', { textContent: `The certificate of ${c.device} has CHANGED.` }),
          el('p', { textContent: 'Someone may be intercepting your connection. It can also happen after a BMC firmware update or certificate regeneration. Replace the pinned certificate only if you know why it changed.' })));
        parts.push(table, box('I know why the certificate changed: replace the pinned one with this one'));
      } else {
        parts.push(el('p', { textContent: `The certificate of ${c.device} is not trusted yet: ${d.reason || 'unknown issuer'}. Compare the fingerprint with the one the BMC shows (for example in its web interface, or with openssl on a trusted machine) before trusting it.` }));
        parts.push(table, box('Trust this certificate for this device'));
      }
    }
    if ((c.legacy_tls || []).length) {
      parts.push(el('h4', { className: 'media-heading', textContent: 'Obsolete TLS' }),
        el('p', { textContent: `The BMC only supports ${c.legacy_tls.join('; ')}. These protocol versions and ciphers have known weaknesses.` }),
        box('Allow obsolete TLS for this device'));
    }
    if ((c.insecure_channels || []).length) {
      parts.push(el('h4', { className: 'media-heading', textContent: 'Unencrypted channels' }),
        el('ul', { className: 'consent-list' }, ...c.insecure_channels.map((t) => el('li', { textContent: t }))),
        el('p', { textContent: 'Anyone who can see the network between you and the BMC can watch the screen and keystrokes. Use a dedicated, trusted management network.' }),
        box('Allow these channels for this device'));
    }
    consentBody.replaceChildren(...parts);
    document.getElementById('consentTitle').textContent = changed ? 'Certificate changed' : `Confirm ${c.device}`;
    consentAccept.textContent = changed ? 'Replace and Connect' : 'Trust and Connect';

    function update() { consentAccept.disabled = !checks.every((x) => x.checked); }
    update();
    consentAccept.onclick = () => {
      if (!pending || consentAccept.disabled) return;
      const req = Object.assign({}, pending);
      req.trust_fingerprints = (c.certificates || []).map((d) => d.cert.fingerprint);
      req.replace_fingerprints = changed;
      req.allow_legacy_tls = req.allow_legacy_tls || (c.legacy_tls || []).length > 0;
      req.allow_insecure_channels = req.allow_insecure_channels || (c.insecure_channels || []).length > 0;
      consentModal.classList.remove('open');
      connect(req);
    };
    consentModal.classList.add('open');
  }

  const closeConsent = () => {
    consentModal.classList.remove('open');
    pending = null;
    password.value = '';
    showError('Not connected: the BMC was not trusted.');
  };
  document.getElementById('btnCloseConsent').addEventListener('click', closeConsent);
  document.getElementById('btnConsentCancel').addEventListener('click', closeConsent);

  return {
    show(status) {
      document.getElementById('appVersion').textContent = status.version ? `version ${status.version}` : '';
      document.getElementById('btnQuit').hidden = !status.desktop;
      if (view.hidden) {
        view.hidden = false;
        loadLists();
        if (!address.value) address.focus();
      }
      if (status.last_error && !busy && error.hidden) showError(status.last_error);
    },
    hide() { view.hidden = true; },
    isShown() { return !view.hidden; },
  };
}
