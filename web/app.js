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
  if (!res.ok) throw new Error(data.error || res.status + " " + res.statusText);
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
      banner.textContent = "Not paired with a relay — enter a pairing token below.";
      return;
    }
    switch (s.tunnel_state) {
      case "connected":
        banner.className = "banner banner-on";
        banner.textContent = "Connected to " + s.relay_url;
        break;
      case "connecting":
        banner.className = "banner banner-mid";
        banner.textContent = "Connecting to " + s.relay_url + "…";
        break;
      default:
        banner.className = "banner banner-err";
        banner.textContent = "Disconnected from " + s.relay_url +
          (s.tunnel_error ? " — " + s.tunnel_error : "");
    }
  } catch (e) {
    banner.className = "banner banner-err";
    banner.textContent = "agent unreachable: " + e.message;
  }
}

document.getElementById("pair-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const msg = document.getElementById("pair-msg");
  const btn = document.getElementById("pair-btn");
  btn.disabled = true;
  msg.className = "small dim";
  msg.textContent = "Pairing…";
  try {
    await api("/api/pair", {
      relay_url: document.getElementById("pair-url").value.trim(),
      token: document.getElementById("pair-token").value.trim(),
      name: document.getElementById("pair-name").value.trim(),
    });
    document.getElementById("pair-token").value = "";
    msg.className = "small msg-ok";
    msg.textContent = "Paired — connecting to relay.";
  } catch (e) {
    msg.className = "small msg-err";
    msg.textContent = "Pairing failed: " + e.message;
  } finally {
    btn.disabled = false;
    refreshStatus();
  }
});

// ---- devices -------------------------------------------------------------

async function toggleDevice(uuid, field, value) {
  try { await api("/api/devices/" + encodeURIComponent(uuid), { [field]: value }); }
  finally { refreshDevices(); }
}

async function regenerate(uuid) {
  try { await api("/api/devices/" + encodeURIComponent(uuid) + "/regenerate", {}); }
  finally { refreshDevices(); }
}

function deviceRow(d) {
  const tr = el("tr");
  tr.appendChild(el("td", "wordid", d.id));
  tr.appendChild(el("td", "path", d.meta.path || "—"));
  tr.appendChild(el("td", "", d.meta.transport || "—"));

  const conf = d.meta.fingerprint_confidence || "weak";
  const badge = el("span", "badge badge-" + conf, conf);
  if (conf === "weak") {
    badge.title = "Weak fingerprint: this port may be renumbered when devices are added or removed, so its identity can shift across replugs.";
  }
  const tdConf = el("td");
  tdConf.appendChild(badge);
  tr.appendChild(tdConf);

  tr.appendChild(el("td", "", d.meta.product || "—"));

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
    "Allow DTR/RTS and baud changes — these can reset boards or enter bootloaders"));

  const tdBtn = el("td");
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
      li.textContent = s.device_id + " — session " + s.session_id.slice(0, 8) +
        " — " + s.bytes_in + " B in / " + s.bytes_out + " B out — opened " +
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

document.getElementById("killswitch").addEventListener("click", async () => {
  if (!confirm("Close every device session and sever the relay link?")) return;
  try { await api("/api/disconnect", {}); }
  catch (e) { alert("Kill switch failed: " + e.message); }
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
