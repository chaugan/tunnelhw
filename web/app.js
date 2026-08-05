"use strict";

// The per-launch session secret, injected server-side; echoed on every
// state-changing request (CSRF defence, ARCHITECTURE.md §7).
const CSRF = document.querySelector('meta[name="csrf"]').content;

async function api(path, body) {
  const opts = { headers: {} };
  if (body !== undefined) {
    opts.method = "POST";
    opts.headers["Content-Type"] = "application/json";
    opts.headers["X-TunnelHW-CSRF"] = CSRF;
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { data = { error: text }; }
  if (!res.ok) {
    const err = new Error(data.error || res.status + " " + res.statusText);
    err.status = res.status;
    err.data = data;   // callers may need the body (e.g. host-key approval)
    throw err;
  }
  return data;
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// ---- relay status + pairing ---------------------------------------------

async function refreshStatus() {
  const banner = document.getElementById("relay-banner");
  try {
    const s = await api("/api/status");
    if (!s.paired) {
      banner.className = "banner banner-off";
      banner.textContent = "Not paired with a relay. Enter a pairing token below.";
      return;
    }
    const kill = document.getElementById("killswitch");
    if (kill) {
      const stopped = s.tunnel_state === "stopped";
      tunnelStopped = stopped;
      kill.textContent = stopped ? "Reconnect" : "Kill switch";
      kill.className = stopped ? "primary" : "danger";
      kill.title = stopped
        ? "Clear the kill switch and reconnect to the relay"
        : "Close every session and take the tunnel offline until you reconnect";
    }
    switch (s.tunnel_state) {
      case "connected":
        banner.className = "banner banner-on";
        banner.textContent = "Connected to " + s.relay_url +
          (s.ssh_host ? " via SSH " + s.ssh_user + "@" + s.ssh_host : "");
        break;
      case "connecting":
        banner.className = "banner banner-mid";
        banner.textContent = "Connecting to " + s.relay_url + "…";
        break;
      case "stopped":
        banner.className = "banner banner-err";
        banner.textContent = "Stopped by the kill switch. Offline until you reconnect.";
        break;
      default:
        banner.className = "banner banner-err";
        banner.textContent = "Disconnected from " + s.relay_url +
          (s.tunnel_error ? ": " + s.tunnel_error : "");
    }
  } catch (e) {
    banner.className = "banner banner-err";
    banner.textContent = "agent unreachable: " + e.message;
  }
}

function pairMode() {
  const checked = document.querySelector('input[name="mode"]:checked');
  return checked ? checked.value : "direct";
}

for (const radio of document.querySelectorAll('input[name="mode"]')) {
  radio.addEventListener("change", () => {
    const ssh = pairMode() === "ssh";
    document.getElementById("mode-ssh").classList.toggle("hidden", !ssh);
    document.getElementById("mode-direct").classList.toggle("hidden", ssh);
  });
}

// Builds the /api/pair body from the form. acceptHostKey is set only after
// the human has seen and approved the SSH fingerprint.
function pairBody(acceptHostKey) {
  const val = (id) => document.getElementById(id).value.trim();
  const body = {
    token: val("pair-token"),
    name: val("pair-name"),
  };
  if (pairMode() === "ssh") {
    body.relay_url = val("pair-url-ssh");
    body.ssh = {
      host: val("ssh-host"),
      user: val("ssh-user"),
      key_path: val("ssh-key"),
      accept_new_host_key: !!acceptHostKey,
    };
    const secret = val("ssh-pass");
    // One field, two uses: a key file means it decrypts the key, otherwise
    // it is the login password.
    if (secret) {
      if (body.ssh.key_path) body.ssh.key_passphrase = secret;
      else body.ssh.password = secret;
    }
  } else {
    body.relay_url = val("pair-url");
  }
  return body;
}

async function doPair(acceptHostKey) {
  const msg = document.getElementById("pair-msg");
  const btn = document.getElementById("pair-btn");
  const hostkey = document.getElementById("hostkey");
  btn.disabled = true;
  msg.className = "small dim";
  msg.textContent = "Pairing…";
  try {
    await api("/api/pair", pairBody(acceptHostKey));
    document.getElementById("pair-token").value = "";
    document.getElementById("ssh-pass").value = "";
    hostkey.classList.add("hidden");
    msg.className = "small msg-ok";
    msg.textContent = "Paired. Connecting to relay.";
  } catch (e) {
    if (e.data && e.data.needs_host_key_approval) {
      // Not a failure: the human has to vouch for the server's identity.
      document.getElementById("hostkey-fp").textContent =
        e.data.host + ": " + e.data.fingerprint;
      hostkey.classList.remove("hidden");
      msg.className = "small dim";
      msg.textContent = "Verify the SSH host key below to continue.";
    } else {
      msg.className = "small msg-err";
      msg.textContent = "Pairing failed: " + e.message;
    }
  } finally {
    btn.disabled = false;
    refreshStatus();
  }
}

document.getElementById("pair-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  doPair(false);
});
document.getElementById("hostkey-accept").addEventListener("click", () => doPair(true));
document.getElementById("hostkey-cancel").addEventListener("click", () => {
  document.getElementById("hostkey").classList.add("hidden");
});

// ---- devices -------------------------------------------------------------

async function toggleDevice(uuid, field, value) {
  try { await api("/api/devices/" + encodeURIComponent(uuid), { [field]: value }); }
  finally { refreshDevices(); }
}

async function release(uuid) {
  try { await api("/api/devices/" + encodeURIComponent(uuid) + "/release", {}); }
  finally { refreshDevices(); refreshSessions(); }
}

async function regenerate(uuid) {
  try { await api("/api/devices/" + encodeURIComponent(uuid) + "/regenerate", {}); }
  finally { refreshDevices(); }
}

function deviceRow(d) {
  const tr = el("tr");
  tr.appendChild(el("td", "wordid", d.id));
  tr.appendChild(el("td", "path", d.meta.path || "n/a"));
  tr.appendChild(el("td", "", d.meta.transport || "n/a"));

  const conf = d.meta.fingerprint_confidence || "weak";
  const badge = el("span", "badge badge-" + conf, conf);
  if (conf === "weak") {
    badge.title = "Weak fingerprint: this port may be renumbered when devices are added or removed, so its identity can shift across replugs.";
  }
  const tdConf = el("td");
  tdConf.appendChild(badge);
  tr.appendChild(tdConf);

  tr.appendChild(el("td", "", d.meta.product || "n/a"));

  const state = !d.online ? ["offline", "state-offline"]
    : d.busy ? ["busy", "state-busy"] : ["online", "state-online"];
  tr.appendChild(el("td", state[1], state[0]));

  const mkToggle = (checked, field, title) => {
    const td = el("td");
    const cb = el("input", "toggle");
    cb.type = "checkbox";
    cb.checked = checked;
    cb.title = title;
    cb.addEventListener("change", () => toggleDevice(d.uuid, field, cb.checked));
    td.appendChild(cb);
    return td;
  };
  tr.appendChild(mkToggle(d.exposed, "exposed", "Expose this device to the relay"));
  tr.appendChild(mkToggle(d.meta.control_lines_allowed, "allow_control_lines",
    "Allow DTR/RTS and baud changes, which can reset boards or enter bootloaders"));

  const tdBtn = el("td", "actions");
  if (d.busy) {
    // The LLM is holding this port right now, so nothing local can open it.
    // Release hands it straight back without touching the tunnel.
    const rel = el("button", "danger", "Release");
    rel.title = "Force-close the session holding this device and hand the port " +
      "back for local use. The device stays exposed.";
    rel.addEventListener("click", () => release(d.uuid));
    tdBtn.appendChild(rel);
  }
  const btn = el("button", "ghost", "Regenerate");
  btn.title = "Assign a fresh word-ID";
  btn.addEventListener("click", () => regenerate(d.uuid));
  tdBtn.appendChild(btn);
  tr.appendChild(tdBtn);
  return tr;
}

async function refreshDevices() {
  try {
    const devs = await api("/api/devices");
    const tbody = document.querySelector("#devices tbody");
    tbody.replaceChildren(...devs.map(deviceRow));
    document.getElementById("no-devices").classList.toggle("hidden", devs.length > 0);
  } catch { /* transient; next poll retries */ }
}

// ---- sessions ------------------------------------------------------------

async function refreshSessions() {
  try {
    const sessions = await api("/api/sessions");
    const ul = document.getElementById("sessions");
    ul.replaceChildren(...sessions.map((s) => {
      const li = el("li", "mono");
      li.textContent = s.device_id + ": session " + s.session_id.slice(0, 8) +
        ", " + s.bytes_in + " B in / " + s.bytes_out + " B out, opened " +
        new Date(s.opened).toLocaleTimeString();
      return li;
    }));
    document.getElementById("no-sessions").classList.toggle("hidden", sessions.length > 0);
  } catch { /* transient */ }
}

// ---- activity ------------------------------------------------------------

let lastSeq = 0;

async function refreshActivity() {
  try {
    const entries = await api("/api/activity?after=" + lastSeq);
    if (!entries.length) return;
    const ul = document.getElementById("activity");
    for (const e of entries) {
      lastSeq = Math.max(lastSeq, e.seq);
      const li = el("li", "k-" + e.kind);
      const t = el("time", "", new Date(e.time).toLocaleTimeString());
      li.appendChild(t);
      li.appendChild(document.createTextNode("[" + e.kind + "] " + e.text));
      ul.prepend(li);
    }
    while (ul.children.length > 300) ul.removeChild(ul.lastChild);
  } catch { /* transient */ }
}

// ---- kill switch ---------------------------------------------------------

// The kill switch latches: it stays offline until explicitly reconnected, so
// the same button doubles as Reconnect once stopped.
let tunnelStopped = false;

document.getElementById("killswitch").addEventListener("click", async () => {
  if (tunnelStopped) {
    try { await api("/api/reconnect", {}); }
    catch (e) { alert("Reconnect failed: " + e.message); }
  } else {
    if (!confirm("Close every device session and take the tunnel offline?\n\nIt stays offline until you press Reconnect.")) return;
    try { await api("/api/disconnect", {}); }
    catch (e) { alert("Kill switch failed: " + e.message); }
  }
  refreshStatus();
  refreshSessions();
});

// ---- polling -------------------------------------------------------------

refreshStatus();
refreshDevices();
refreshSessions();
refreshActivity();
setInterval(() => { refreshStatus(); refreshDevices(); refreshSessions(); }, 3000);
setInterval(refreshActivity, 2000);
