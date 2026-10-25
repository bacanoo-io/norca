"use strict";
// uBixVault console. Vanilla JS, no dependencies. Secret values are rendered via
// textContent only (never innerHTML), so a value can't inject markup.

const $ = (id) => document.getElementById(id);
const TOKEN_KEY = "ubixvault.token";
// How the current token was obtained: "userpass" / "ldap" when this console
// logged in (so signing out revokes it), "token" when it was pasted in (so
// signing out only forgets it — a pasted token may be root or shared).
const SOURCE_KEY = "ubixvault.token-source";
const USER_KEY = "ubixvault.token-user";

const getToken = () => sessionStorage.getItem(TOKEN_KEY) || "";
function setToken(t, source, user) {
  if (t) {
    sessionStorage.setItem(TOKEN_KEY, t);
    sessionStorage.setItem(SOURCE_KEY, source || "token");
    if (user) sessionStorage.setItem(USER_KEY, user);
    else sessionStorage.removeItem(USER_KEY);
  } else {
    for (const k of [TOKEN_KEY, SOURCE_KEY, USER_KEY]) sessionStorage.removeItem(k);
  }
}

// api performs a same-origin request, attaching the token and JSON body if given.
async function api(method, path, body) {
  const headers = {};
  const tok = getToken();
  if (tok) headers["X-Vault-Token"] = tok;
  const opts = { method, headers };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  let payload = null;
  try { payload = await res.json(); } catch (_) { /* 204 / empty */ }
  return { status: res.status, ok: res.ok, body: payload };
}

function friendlyError(status, body) {
  const detail = body && body.errors && body.errors.length ? " — " + body.errors.join("; ") : "";
  switch (status) {
    case 0: return "Can't reach the vault.";
    case 401:
    case 403: return "Not authorized — check your token." + detail;
    case 404: return "Not found." + detail;
    case 501: return "Vault is not initialized.";
    case 503: return "Vault is sealed." + detail;
    default: return "Request failed (" + status + ")" + detail;
  }
}

// ---- seal state ----
function fmtUptime(s) {
  s = Math.floor(s || 0);
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600),
    m = Math.floor((s % 3600) / 60), sec = s % 60;
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${sec}s`;
  return `${sec}s`;
}

async function refreshStatus() {
  const el = $("seal");
  try {
    const [health, seal] = await Promise.all([
      api("GET", "/v1/sys/health"),
      api("GET", "/v1/sys/seal-status"),
    ]);
    const st = seal.body || {};
    const h = health.body || {};
    $("version").textContent = h.version ? "v" + h.version : "";
    $("m-init").textContent = st.initialized ? "yes" : "no";
    $("m-type").textContent = st.type || "—";
    $("m-uptime").textContent = h.uptime_seconds != null ? fmtUptime(h.uptime_seconds) : "—";

    let state, word, tag;
    if (!st.initialized) {
      state = "uninitialized"; word = "Uninitialized"; tag = "Run operator init to set up the vault";
    } else if (st.sealed) {
      state = "sealed"; word = "Sealed"; tag = "The vault is locked; unseal to serve secrets";
    } else {
      state = "unsealed"; word = "Unsealed"; tag = "The vault is unlocked and serving";
    }
    el.dataset.state = state;
    $("seal-word").textContent = word;
    $("seal-tagline").textContent = tag;
    reflectLifecycle(st);
  } catch (_) {
    el.dataset.state = "error";
    $("seal-word").textContent = "Unreachable";
    $("seal-tagline").textContent = "Could not reach the vault";
  }
}

// ---- lifecycle: initialize and unseal ----
// While freshly generated keys are on screen, the Initialize panel stays up even
// though the vault is now initialized: hiding it would hide the keys with it.
let showingInitSecrets = false;

function reflectLifecycle(st) {
  $("init-panel").hidden = !(!st.initialized || showingInitSecrets);
  const sealed = !!st.initialized && !!st.sealed;
  $("unseal-panel").hidden = !sealed;
  if (!sealed) return;

  const auto = st.type === "auto";
  $("unseal-form").hidden = auto;
  $("unseal-hint").textContent = auto
    ? "This vault unseals itself with its seal (auto-unseal). If it stays sealed, the seal " +
      "(KMS or key) is unreachable — check the server log; key shares cannot unseal it."
    : "Enter key shares one at a time; the vault unseals when enough have been given. " +
      "A share is sent only to this vault and is not kept in the browser.";
  const tag = $("unseal-progress");
  tag.textContent = (st.progress || 0) + " of " + (st.t || "?");
  tag.dataset.set = String((st.progress || 0) > 0);
}

// secretList renders labelled values, each with a Copy button, into out.
function secretList(out, title, values) {
  const head = document.createElement("h3"); head.className = "sub"; head.textContent = title;
  const list = document.createElement("div"); list.className = "keys";
  values.forEach((v, i) => {
    const row = document.createElement("div"); row.className = "keyrow";
    const n = document.createElement("span"); n.className = "n"; n.textContent = values.length > 1 ? String(i + 1) : "";
    const code = document.createElement("code"); code.textContent = v;
    const copy = actionButton("Copy", "ghost", () => { if (navigator.clipboard) navigator.clipboard.writeText(v); });
    row.append(n, code, copy);
    list.appendChild(row);
  });
  out.append(head, list);
}

async function initVault() {
  const shares = parseInt($("init-shares").value, 10);
  const threshold = parseInt($("init-threshold").value, 10);
  if (!(shares >= 2 && shares <= 255 && threshold >= 2 && threshold <= shares)) {
    panelMsg("init-out", "error", "Use 2–255 shares, and a threshold from 2 up to the number of shares.");
    return;
  }
  const r = await api("POST", "/v1/sys/init", { secret_shares: shares, secret_threshold: threshold });
  if (!r.ok) { panelMsg("init-out", "error", friendlyError(r.status, r.body)); return; }
  const b = r.body || {};
  const keys = (b.recovery_keys && b.recovery_keys.length) ? b.recovery_keys : (b.keys || []);
  const recovery = !!(b.recovery_keys && b.recovery_keys.length);

  showingInitSecrets = true;
  $("init-form").hidden = true;
  const out = $("init-out"); out.replaceChildren();
  const warn = document.createElement("div"); warn.className = "warn";
  warn.textContent = "Shown once. Copy every " + (recovery ? "recovery key" : "key share") +
    " and the root token somewhere safe and offline now — they cannot be shown again.";
  out.appendChild(warn);
  secretList(out, (recovery ? "Recovery keys" : "Key shares") + " — any " + threshold + " of " + keys.length +
    (recovery ? " regenerate a root token" : " unseal the vault"), keys);
  secretList(out, "Root token", [b.root_token || ""]);
  const acts = document.createElement("div"); acts.className = "kv-actions";
  acts.appendChild(actionButton("Use root token in this tab", "", async () => {
    setToken(b.root_token || "", "token");
    await refreshSession();
  }));
  acts.appendChild(actionButton("I've stored them — hide", "ghost", async () => {
    showingInitSecrets = false;
    out.replaceChildren();
    $("init-form").hidden = false;
    await refreshStatus();
  }));
  out.appendChild(acts);
  await refreshStatus();
}

async function submitUnsealShare() {
  const key = $("unseal-key").value.trim();
  $("unseal-key").value = ""; // never keep a share in the page
  if (!key) { panelMsg("unseal-out", "error", "Enter a key share."); return; }
  const r = await api("POST", "/v1/sys/unseal", { key });
  if (!r.ok) { panelMsg("unseal-out", "error", friendlyError(r.status, r.body)); return; }
  const st = r.body || {};
  if (st.sealed === false) {
    panelMsg("unseal-out", "ok", "Unsealed.");
    await refreshStatus();
    await refreshSession();
    return;
  }
  panelMsg("unseal-out", "ok", "Share accepted — " + (st.progress || 0) + " of " + (st.t || "?") + ".");
  await refreshStatus();
}

// ---- session: sign in, who am I, renew, sign out ----
const LOGIN_METHODS = { userpass: "Username & password", ldap: "LDAP", token: "Token" };

function fmtDuration(s) {
  s = Math.max(0, Math.floor(s || 0));
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m`;
  return `${s}s`;
}

function reflectLoginMethod() {
  const tokenMode = $("login-method").value === "token";
  $("login-user").hidden = tokenMode;
  $("login-pass").hidden = tokenMode;
  $("login-token").hidden = !tokenMode;
}

function showSignedOut(message, cls) {
  $("login-form").hidden = false;
  $("whoami").hidden = true;
  $("whoami-table").replaceChildren();
  if (message) panelMsg("session-out", cls || "ok", message);
  else $("session-out").replaceChildren();
}

// refreshSession looks up the stored token and shows who it is, or the sign-in
// form when there is none (or it has expired or been revoked).
async function refreshSession() {
  if (!getToken()) { showSignedOut(); return; }
  const r = await api("GET", "/v1/auth/token/lookup-self");
  if (r.status === 403) {
    setToken("");
    showSignedOut("Your session has expired or was revoked. Sign in again.", "error");
    return;
  }
  if (!r.ok) {
    // Sealed or unreachable: keep the token, say why we can't describe it.
    $("login-form").hidden = true;
    $("whoami").hidden = false;
    $("whoami-table").replaceChildren();
    panelMsg("session-out", "error", friendlyError(r.status, r.body));
    return;
  }
  const d = (r.body && r.body.data) || {};
  const source = sessionStorage.getItem(SOURCE_KEY) || "token";
  const user = sessionStorage.getItem(USER_KEY);
  const rows = [
    ["signed in", (LOGIN_METHODS[source] || source) + (user ? " · " + user : "")],
    ["policies", (d.policies || []).join(", ") || "(none)"],
  ];
  if ((d.identity_policies || []).length) rows.push(["identity policies", d.identity_policies.join(", ")]);
  rows.push(["expires", d.expire_time ? "in " + fmtDuration(d.ttl) + " (" + d.expire_time + ")" : "never"]);

  const t = $("whoami-table"); t.replaceChildren();
  for (const [k, v] of rows) {
    const tr = document.createElement("tr");
    const th = document.createElement("th"); th.textContent = k;
    const td = document.createElement("td"); td.textContent = v;
    tr.append(th, td); t.appendChild(tr);
  }
  $("renew").hidden = !d.renewable;
  // Pasted tokens are only forgotten, never revoked — say so on the button.
  $("logout").textContent = source === "token" ? "Forget token" : "Sign out";
  $("login-form").hidden = true;
  $("whoami").hidden = false;
  $("session-out").replaceChildren();
}

async function login() {
  const method = $("login-method").value;
  if (method === "token") {
    const tok = $("login-token").value.trim();
    $("login-token").value = "";
    if (!tok) { panelMsg("session-out", "error", "Paste a token."); return; }
    setToken(tok, "token");
    await refreshSession();
    return;
  }
  const user = $("login-user").value.trim();
  const password = $("login-pass").value;
  $("login-pass").value = ""; // never keep the password around, whatever happens
  if (!user || !password) { panelMsg("session-out", "error", "Enter a username and password."); return; }
  const r = await api("POST", "/v1/auth/" + method + "/login/" + encodeURIComponent(user), { password });
  if (!r.ok) {
    // Login failures are deliberately uniform server-side; keep them uniform here.
    const msg = r.status === 400 || r.status === 401 || r.status === 403
      ? "Sign-in failed — check the username and password."
      : friendlyError(r.status, r.body);
    panelMsg("session-out", "error", msg);
    return;
  }
  const tok = r.body && r.body.auth && r.body.auth.client_token;
  if (!tok) { panelMsg("session-out", "error", "The vault returned no token."); return; }
  setToken(tok, method, user);
  await refreshSession();
}

async function renew() {
  const r = await api("POST", "/v1/auth/token/renew-self", {});
  if (r.status === 403) {
    panelMsg("session-out", "error", "Your policies don't allow renewing this token (auth/token/renew-self).");
    return;
  }
  if (!r.ok) { panelMsg("session-out", "error", friendlyError(r.status, r.body)); return; }
  await refreshSession();
  panelMsg("session-out", "ok", "Renewed.");
}

async function logout() {
  const source = sessionStorage.getItem(SOURCE_KEY) || "token";
  if (source !== "token") {
    // Revoke the token this console created. If that fails (e.g. sealed), still
    // forget it locally, and say it may remain valid until it expires.
    const r = await api("POST", "/v1/auth/token/revoke-self", {});
    setToken("");
    showSignedOut(r.ok ? "Signed out; the token was revoked."
      : "Signed out here, but the vault could not revoke the token (" + r.status + "); it stays valid until it expires.",
      r.ok ? "ok" : "error");
    return;
  }
  setToken("");
  showSignedOut("Token forgotten in this tab. It was not revoked.");
}

// ---- KV paths ----
const KV_DATA = (p) => "/v1/secret/data/" + p;
const KV_META = (p) => "/v1/secret/metadata/" + p;
const KV_UNDELETE = (p) => "/v1/secret/undelete/" + p;
const KV_DELETE_V = (p) => "/v1/secret/delete/" + p;
const KV_DESTROY = (p) => "/v1/secret/destroy/" + p;

function cleanPath(raw) {
  let p = (raw || "").trim().replace(/^\/+/, "");
  p = p.replace(/^secret\/(data|metadata)\//, ""); // tolerate a pasted full path
  return p;
}

function outMsg(cls, text) {
  const out = $("kv-out");
  out.replaceChildren();
  const d = document.createElement("div");
  d.className = "msg " + cls;
  d.textContent = text;
  out.appendChild(d);
}

function actionButton(label, cls, onClick) {
  const b = document.createElement("button");
  b.type = "button"; b.textContent = label;
  if (cls) b.className = cls;
  b.addEventListener("click", onClick);
  return b;
}

// ---- read + lifecycle ----
// Reading a soft-deleted current version returns 404 on the data endpoint but
// the metadata endpoint still describes it, so fall back to metadata to show the
// deleted state (and the Undelete action).
async function kvRead(path) {
  const p = cleanPath(path);
  if (!p) { outMsg("error", "Enter a secret path."); return; }
  const r = await api("GET", KV_DATA(p));
  if (r.ok) {
    const data = renderSecret(p, r.body);
    const actions = document.createElement("div");
    actions.className = "kv-actions";
    actions.appendChild(actionButton("Edit", "", () => openEditor(p, data)));
    actions.appendChild(actionButton("Delete (soft)", "btn-danger", () => kvDelete(p)));
    actions.appendChild(actionButton("History", "", () => kvHistory(p)));
    $("kv-out").appendChild(actions);
    return;
  }
  if (r.status === 404) {
    const meta = await api("GET", KV_META(p));
    if (meta.ok) { renderDeleted(p, meta.body); return; }
  }
  outMsg("error", friendlyError(r.status, r.body));
}

function renderSecret(p, body) {
  const data = (body && body.data && body.data.data) || {};
  const meta = (body && body.data && body.data.metadata) || {};
  const out = $("kv-out");
  out.replaceChildren();

  const keys = Object.keys(data);
  if (!keys.length) {
    const m = document.createElement("div");
    m.className = "msg ok"; m.textContent = "Secret at “" + p + "” has no fields.";
    out.appendChild(m);
  } else {
    const table = document.createElement("table");
    table.className = "kv-table";
    for (const k of keys) {
      const tr = document.createElement("tr");
      const th = document.createElement("th"); th.textContent = k;
      const td = document.createElement("td");
      td.textContent = typeof data[k] === "string" ? data[k] : JSON.stringify(data[k]);
      tr.append(th, td);
      table.appendChild(tr);
    }
    out.appendChild(table);
  }

  if (meta.version != null) {
    const line = document.createElement("div");
    line.className = "kv-metaline";
    line.textContent = "version " + meta.version + (meta.created_time ? " · created " + meta.created_time : "");
    out.appendChild(line);
  }
  return data;
}

function renderDeleted(p, body) {
  const md = (body && body.data) || {};
  const cv = md.current_version;
  const vinfo = (md.versions && md.versions[String(cv)]) || {};
  const out = $("kv-out");
  out.replaceChildren();

  const m = document.createElement("div");
  m.className = "msg";
  m.textContent = vinfo.destroyed
    ? "Version " + cv + " of “" + p + "” is destroyed (permanently gone)."
    : "Version " + cv + " of “" + p + "” is soft-deleted.";
  out.appendChild(m);

  const actions = document.createElement("div");
  actions.className = "kv-actions";
  if (!vinfo.destroyed) {
    actions.appendChild(actionButton("Undelete", "", () => kvUndelete(p, cv)));
    actions.appendChild(actionButton("New version", "", () => openEditor(p, null)));
  }
  if (actions.childElementCount) out.appendChild(actions);
}

async function kvDelete(path) {
  const r = await api("DELETE", KV_DATA(path));
  if (!r.ok) { outMsg("error", friendlyError(r.status, r.body)); return; }
  kvRead(path); // re-read; renderDeleted shows the Undelete action
}

async function kvUndelete(path, version) {
  const r = await api("POST", KV_UNDELETE(path), { versions: [version] });
  if (!r.ok) { outMsg("error", friendlyError(r.status, r.body)); return; }
  kvRead(path);
}

async function kvList(path) {
  const p = cleanPath(path);
  const r = await api("LIST", KV_META(p));
  if (!r.ok) { outMsg("error", friendlyError(r.status, r.body)); return; }
  const keys = (r.body && r.body.data && r.body.data.keys) || [];
  const out = $("kv-out");
  out.replaceChildren();
  if (!keys.length) { outMsg("ok", "No keys under “" + (p || "/") + "”."); return; }

  const ul = document.createElement("ul");
  ul.className = "kv-keys";
  for (const k of keys) {
    const li = document.createElement("li");
    const child = (p ? p.replace(/\/?$/, "/") : "") + k;
    const b = actionButton(k, "linklike", () => {
      $("kv-path").value = child;
      if (k.endsWith("/")) kvList(child); else kvRead(child);
    });
    li.appendChild(b);
    ul.appendChild(li);
  }
  out.appendChild(ul);
}

// ---- version history ----
// confirmThen swaps btn for an inline "question Yes/No", running onYes on Yes.
function confirmThen(btn, question, onYes) {
  const holder = document.createElement("span");
  holder.className = "confirm";
  const q = document.createElement("span"); q.className = "q"; q.textContent = question + " ";
  const yes = actionButton("Yes", "btn-danger", onYes);
  const no = actionButton("No", "", () => holder.replaceWith(btn));
  holder.append(q, yes, no);
  btn.replaceWith(holder);
}

async function kvHistory(path) {
  const p = cleanPath(path);
  if (!p) { outMsg("error", "Enter a secret path."); return; }
  const r = await api("GET", KV_META(p));
  if (!r.ok) { outMsg("error", friendlyError(r.status, r.body)); return; }
  renderHistory(p, r.body);
}

function renderHistory(p, body) {
  const md = (body && body.data) || {};
  const versions = md.versions || {};
  const nums = Object.keys(versions).map(Number).sort((a, b) => b - a); // newest first
  const out = $("kv-out");
  out.replaceChildren();

  const head = document.createElement("div");
  head.className = "kv-metaline";
  head.textContent = "“" + p + "” · current v" + md.current_version + " · " + nums.length + " version(s)";
  out.appendChild(head);

  const table = document.createElement("table");
  table.className = "kv-table history";
  for (const n of nums) {
    const v = versions[String(n)] || {};
    const state = v.destroyed ? "destroyed" : v.deleted ? "deleted" : "active";
    const tr = document.createElement("tr");
    const th = document.createElement("th"); th.textContent = "v" + n;
    const td = document.createElement("td");
    const meta = document.createElement("div"); meta.className = "vmeta";
    meta.textContent = state + (v.created_time ? " · " + v.created_time : "");
    const acts = document.createElement("div"); acts.className = "kv-actions";
    if (!v.destroyed) acts.appendChild(actionButton("Read", "", () => kvReadVersion(p, n)));
    if (!v.destroyed && !v.deleted) acts.appendChild(actionButton("Delete", "btn-danger", () => kvVersionOp(KV_DELETE_V(p), p, n)));
    if (v.deleted && !v.destroyed) acts.appendChild(actionButton("Undelete", "", () => kvVersionOp(KV_UNDELETE(p), p, n)));
    if (!v.destroyed) {
      const d = actionButton("Destroy", "btn-danger", null);
      d.addEventListener("click", () => confirmThen(d, "Destroy v" + n + " permanently?", () => kvVersionOp(KV_DESTROY(p), p, n)));
      acts.appendChild(d);
    }
    td.append(meta, acts);
    tr.append(th, td);
    table.appendChild(tr);
  }
  out.appendChild(table);
}

async function kvVersionOp(url, p, n) {
  const r = await api("POST", url, { versions: [n] });
  if (!r.ok) { outMsg("error", friendlyError(r.status, r.body)); return; }
  kvHistory(p); // refresh the history view
}

async function kvReadVersion(p, n) {
  const r = await api("GET", KV_DATA(p) + "?version=" + n);
  if (!r.ok) { outMsg("error", friendlyError(r.status, r.body)); return; }
  renderSecret(p, r.body);
  const wrap = document.createElement("div"); wrap.className = "kv-actions";
  wrap.appendChild(actionButton("← Back to history", "", () => kvHistory(p)));
  $("kv-out").appendChild(wrap);
}

// ---- editor (write) ----
function addFieldRow(key = "", value = "") {
  const row = document.createElement("div");
  row.className = "editor-row";
  const k = document.createElement("input");
  k.type = "text"; k.className = "k"; k.placeholder = "key"; k.value = key; k.spellcheck = false;
  const v = document.createElement("input");
  v.type = "text"; v.placeholder = "value"; v.value = value; v.spellcheck = false;
  const del = actionButton("✕", "del", () => row.remove());
  del.setAttribute("aria-label", "remove field");
  row.append(k, v, del);
  $("editor-fields").appendChild(row);
  return row;
}

function openEditor(path, existing) {
  const p = cleanPath(path);
  if (!p) { outMsg("error", "Enter a path first, then New / edit."); $("kv-path").focus(); return; }
  const ed = $("kv-editor");
  ed.dataset.path = p;
  $("editor-title").textContent = existing ? "Edit secret" : "New secret";
  $("editor-path").textContent = p;
  $("editor-fields").replaceChildren();
  const entries = existing ? Object.entries(existing) : [];
  if (entries.length) {
    for (const [k, v] of entries) addFieldRow(k, typeof v === "string" ? v : JSON.stringify(v));
  } else {
    addFieldRow();
  }
  ed.hidden = false;
  ed.scrollIntoView({ block: "nearest" });
}

function closeEditor() {
  const ed = $("kv-editor");
  ed.hidden = true;
  ed.dataset.path = "";
  $("editor-fields").replaceChildren();
}

function collectFields() {
  const data = {};
  for (const row of $("editor-fields").querySelectorAll(".editor-row")) {
    const inputs = row.querySelectorAll("input");
    const k = inputs[0].value.trim();
    if (k) data[k] = inputs[1].value;
  }
  return data;
}

async function saveSecret() {
  const p = $("kv-editor").dataset.path;
  if (!p) return;
  const data = collectFields();
  if (!Object.keys(data).length) { outMsg("error", "Add at least one field with a key."); return; }
  const r = await api("POST", KV_DATA(p), { data });
  if (!r.ok) { outMsg("error", friendlyError(r.status, r.body)); return; }
  closeEditor();
  $("kv-path").value = p;
  kvRead(p); // show the new version
}

// ---- policies ----
const POL = (n) => "/v1/sys/policies/acl/" + n;
const POL_LIST = "/v1/sys/policies/acl";

// apiRaw sends a raw (non-JSON-wrapped) body — policy documents are posted verbatim.
async function apiRaw(method, path, text) {
  const headers = {};
  const tok = getToken();
  if (tok) headers["X-Vault-Token"] = tok;
  const res = await fetch(path, { method, headers, body: text });
  let payload = null;
  try { payload = await res.json(); } catch (_) { /* 204 / empty */ }
  return { status: res.status, ok: res.ok, body: payload };
}

function panelMsg(id, cls, text) {
  const out = $(id);
  out.replaceChildren();
  const d = document.createElement("div");
  d.className = "msg " + cls;
  d.textContent = text;
  out.appendChild(d);
}

async function polList() {
  const r = await api("LIST", POL_LIST);
  if (!r.ok) { panelMsg("pol-out", "error", friendlyError(r.status, r.body)); return; }
  const keys = (r.body && r.body.data && r.body.data.keys) || [];
  if (!keys.length) { panelMsg("pol-out", "ok", "No policies defined."); return; }
  const out = $("pol-out"); out.replaceChildren();
  const ul = document.createElement("ul"); ul.className = "kv-keys";
  for (const k of keys) {
    const li = document.createElement("li");
    li.appendChild(actionButton(k, "linklike", () => { $("pol-name").value = k; polRead(k); }));
    ul.appendChild(li);
  }
  out.appendChild(ul);
}

async function polRead(name) {
  const n = (name || "").trim();
  if (!n) { panelMsg("pol-out", "error", "Enter a policy name."); return; }
  const r = await api("GET", POL(n));
  if (!r.ok) { panelMsg("pol-out", "error", friendlyError(r.status, r.body)); return; }
  const doc = r.body && r.body.data && r.body.data.policy;
  // The policy arrives as text (as in Vault); pretty-print it when it is JSON.
  let text = typeof doc === "string" ? doc : JSON.stringify(doc, null, 2);
  try { text = JSON.stringify(JSON.parse(text), null, 2); } catch (_) { /* HCL: show as-is */ }
  const out = $("pol-out"); out.replaceChildren();
  const pre = document.createElement("pre"); pre.className = "policy-doc"; pre.textContent = text;
  out.appendChild(pre);
  const acts = document.createElement("div"); acts.className = "kv-actions";
  acts.appendChild(actionButton("Edit", "", () => openPolEditor(n, text)));
  out.appendChild(acts);
}

function openPolEditor(name, text) {
  const n = (name || $("pol-name").value || "").trim();
  if (!n) { panelMsg("pol-out", "error", "Enter a policy name first."); $("pol-name").focus(); return; }
  $("pol-editor-name").textContent = n;
  $("pol-editor").dataset.name = n;
  $("pol-body").value = text || "";
  $("pol-editor").hidden = false;
}

function closePolEditor() {
  const e = $("pol-editor"); e.hidden = true; e.dataset.name = ""; $("pol-body").value = "";
}

async function polSave() {
  const n = $("pol-editor").dataset.name;
  const body = $("pol-body").value.trim();
  if (!body) { panelMsg("pol-out", "error", "Policy document is empty."); return; }
  const r = await apiRaw("PUT", POL(n), body);
  if (!r.ok) { panelMsg("pol-out", "error", friendlyError(r.status, r.body)); return; }
  closePolEditor(); $("pol-name").value = n; polRead(n);
}

async function polDelete(name) {
  const n = (name || "").trim();
  if (!n) { panelMsg("pol-out", "error", "Enter a policy name."); return; }
  const r = await api("DELETE", POL(n));
  if (!r.ok) { panelMsg("pol-out", "error", friendlyError(r.status, r.body)); return; }
  panelMsg("pol-out", "ok", "Deleted policy “" + n + "”.");
}

// ---- create token ----
async function tokCreate() {
  const policies = $("tok-policies").value.split(",").map((s) => s.trim()).filter(Boolean);
  const ttl = $("tok-ttl").value.trim();
  const body = { policies };
  if (ttl) body.ttl = ttl;
  const r = await api("POST", "/v1/auth/token/create", body);
  if (!r.ok) { panelMsg("tok-out", "error", friendlyError(r.status, r.body)); return; }
  const auth = (r.body && r.body.auth) || {};
  const out = $("tok-out"); out.replaceChildren();
  const reveal = document.createElement("div"); reveal.className = "token-reveal"; reveal.textContent = auth.client_token || "";
  const meta = document.createElement("div"); meta.className = "kv-metaline";
  meta.textContent = "policies: " + ((auth.policies || []).join(", ") || "(none)") +
    (auth.lease_duration ? " · expires in ~" + auth.lease_duration + "s" : " · no expiry");
  const note = document.createElement("div"); note.className = "kv-metaline"; note.textContent = "Copy it now — it is shown once.";
  out.append(reveal, meta, note);
}

// ---- PKI ----
const PKI_ROOT = "/v1/pki/root/generate/internal";
const PKI_CA = "/v1/pki/ca";
const PKI_ROLES = "/v1/pki/roles";
const PKI_ROLE = (n) => "/v1/pki/roles/" + n;
const PKI_ISSUE = (r) => "/v1/pki/issue/" + r;

// pemBlock renders a labeled PEM with a Copy button into out.
function pemBlock(out, label, pem) {
  const wrap = document.createElement("div"); wrap.className = "pem";
  const head = document.createElement("div"); head.className = "pem-head";
  const name = document.createElement("span"); name.textContent = label;
  const copy = actionButton("Copy", "ghost", () => { if (navigator.clipboard) navigator.clipboard.writeText(pem); });
  head.append(name, copy);
  const pre = document.createElement("pre"); pre.className = "policy-doc"; pre.textContent = pem;
  wrap.append(head, pre);
  out.appendChild(wrap);
}

async function pkiGenerateRoot() {
  const cn = $("pki-ca-cn").value.trim();
  if (!cn) { panelMsg("pki-ca-out", "error", "Enter a common name for the root CA."); return; }
  const r = await api("POST", PKI_ROOT, { common_name: cn, key_type: "ec" });
  if (!r.ok) { panelMsg("pki-ca-out", "error", friendlyError(r.status, r.body)); return; }
  const out = $("pki-ca-out"); out.replaceChildren();
  pemBlock(out, "root CA certificate", (r.body && r.body.data && r.body.data.certificate) || "");
}

async function pkiViewCA() {
  const r = await api("GET", PKI_CA);
  if (!r.ok) { panelMsg("pki-ca-out", "error", friendlyError(r.status, r.body)); return; }
  const out = $("pki-ca-out"); out.replaceChildren();
  pemBlock(out, "CA certificate", (r.body && r.body.data && r.body.data.certificate) || "");
}

async function pkiRoleList() {
  const r = await api("LIST", PKI_ROLES);
  if (!r.ok) { panelMsg("pki-role-out", "error", friendlyError(r.status, r.body)); return; }
  const keys = (r.body && r.body.data && r.body.data.keys) || [];
  if (!keys.length) { panelMsg("pki-role-out", "ok", "No roles defined."); return; }
  const out = $("pki-role-out"); out.replaceChildren();
  const ul = document.createElement("ul"); ul.className = "kv-keys";
  for (const k of keys) {
    const li = document.createElement("li");
    li.appendChild(actionButton(k, "linklike", () => { $("pki-role-name").value = k; pkiRoleRead(k); }));
    ul.appendChild(li);
  }
  out.appendChild(ul);
}

async function pkiRoleRead(name) {
  const n = (name || "").trim();
  if (!n) { panelMsg("pki-role-out", "error", "Enter a role name."); return; }
  const r = await api("GET", PKI_ROLE(n));
  if (!r.ok) { panelMsg("pki-role-out", "error", friendlyError(r.status, r.body)); return; }
  const d = (r.body && r.body.data) || {};
  const out = $("pki-role-out"); out.replaceChildren();
  const t = document.createElement("table"); t.className = "kv-table";
  const rows = [
    ["allowed_domains", (d.allowed_domains || []).join(", ")],
    ["allow_subdomains", String(!!d.allow_subdomains)],
    ["max_ttl", d.max_ttl || "—"],
    ["key_type", d.key_type || "ec"],
  ];
  for (const [k, v] of rows) {
    const tr = document.createElement("tr");
    const th = document.createElement("th"); th.textContent = k;
    const td = document.createElement("td"); td.textContent = v;
    tr.append(th, td); t.appendChild(tr);
  }
  out.appendChild(t);
  const acts = document.createElement("div"); acts.className = "kv-actions";
  acts.appendChild(actionButton("Edit", "", () => pkiOpenRoleEditor(n, d)));
  out.appendChild(acts);
}

function pkiOpenRoleEditor(name, existing) {
  const n = (name || $("pki-role-name").value || "").trim();
  if (!n) { panelMsg("pki-role-out", "error", "Enter a role name first."); $("pki-role-name").focus(); return; }
  const ed = $("pki-role-editor");
  ed.dataset.name = n;
  $("pki-role-editor-name").textContent = n;
  const d = existing || {};
  $("pki-role-domains").value = (d.allowed_domains || []).join(", ");
  $("pki-role-subdomains").checked = !!d.allow_subdomains;
  $("pki-role-ttl").value = d.max_ttl && d.max_ttl !== "0s" ? d.max_ttl : "";
  $("pki-role-keytype").value = d.key_type === "rsa" ? "rsa" : "ec";
  ed.hidden = false;
}

async function pkiRoleSave() {
  const n = $("pki-role-editor").dataset.name;
  if (!n) return;
  const domains = $("pki-role-domains").value.split(",").map((s) => s.trim()).filter(Boolean);
  const body = {
    allowed_domains: domains,
    allow_subdomains: $("pki-role-subdomains").checked,
    key_type: $("pki-role-keytype").value,
  };
  const ttl = $("pki-role-ttl").value.trim();
  if (ttl) body.max_ttl = ttl;
  const r = await api("POST", PKI_ROLE(n), body);
  if (!r.ok) { panelMsg("pki-role-out", "error", friendlyError(r.status, r.body)); return; }
  $("pki-role-editor").hidden = true;
  $("pki-role-name").value = n;
  pkiRoleRead(n);
}

async function pkiIssue() {
  const role = $("pki-issue-role").value.trim();
  const cn = $("pki-issue-cn").value.trim();
  if (!role || !cn) { panelMsg("pki-issue-out", "error", "Role and common name are required."); return; }
  const alt = $("pki-issue-alt").value.split(",").map((s) => s.trim()).filter(Boolean);
  const ttl = $("pki-issue-ttl").value.trim();
  const body = { common_name: cn };
  if (alt.length) body.alt_names = alt;
  if (ttl) body.ttl = ttl;
  const r = await api("POST", PKI_ISSUE(role), body);
  if (!r.ok) { panelMsg("pki-issue-out", "error", friendlyError(r.status, r.body)); return; }
  const d = (r.body && r.body.data) || {};
  const out = $("pki-issue-out"); out.replaceChildren();
  const meta = document.createElement("div"); meta.className = "kv-metaline";
  meta.textContent = "serial " + (d.serial_number || "") + (d.expiration ? " · expires " + d.expiration : "");
  out.appendChild(meta);
  pemBlock(out, "certificate", d.certificate || "");
  pemBlock(out, "private key", d.private_key || "");
  pemBlock(out, "issuing CA", d.issuing_ca || "");
  const warn = document.createElement("div"); warn.className = "warn";
  warn.textContent = "Copy the private key now — it is shown only once.";
  out.appendChild(warn);
}

// ---- wire up ----
document.addEventListener("DOMContentLoaded", () => {
  refreshStatus();
  reflectLoginMethod();
  refreshSession();

  $("refresh").addEventListener("click", () => { refreshStatus(); refreshSession(); });
  $("login-method").addEventListener("change", reflectLoginMethod);
  $("login-form").addEventListener("submit", (e) => { e.preventDefault(); login(); });
  $("init-form").addEventListener("submit", (e) => { e.preventDefault(); initVault(); });
  $("unseal-form").addEventListener("submit", (e) => { e.preventDefault(); submitUnsealShare(); });
  $("renew").addEventListener("click", renew);
  $("logout").addEventListener("click", logout);

  $("kv-form").addEventListener("submit", (e) => { e.preventDefault(); kvRead($("kv-path").value); });
  $("kv-list").addEventListener("click", () => kvList($("kv-path").value));
  $("kv-history").addEventListener("click", () => kvHistory($("kv-path").value));
  $("kv-new").addEventListener("click", () => openEditor($("kv-path").value, null));

  $("editor-add").addEventListener("click", () => addFieldRow());
  $("editor-cancel").addEventListener("click", closeEditor);
  $("kv-editor").addEventListener("submit", (e) => { e.preventDefault(); saveSecret(); });

  // Policies
  $("pol-form").addEventListener("submit", (e) => { e.preventDefault(); polRead($("pol-name").value); });
  $("pol-list").addEventListener("click", polList);
  $("pol-new").addEventListener("click", () => openPolEditor($("pol-name").value, ""));
  $("pol-delete").addEventListener("click", () => polDelete($("pol-name").value));
  $("pol-cancel").addEventListener("click", closePolEditor);
  $("pol-editor").addEventListener("submit", (e) => { e.preventDefault(); polSave(); });

  // Tokens
  $("tok-form").addEventListener("submit", (e) => { e.preventDefault(); tokCreate(); });

  // PKI
  $("pki-ca-form").addEventListener("submit", (e) => { e.preventDefault(); pkiGenerateRoot(); });
  $("pki-ca-view").addEventListener("click", pkiViewCA);
  $("pki-role-form").addEventListener("submit", (e) => { e.preventDefault(); pkiRoleRead($("pki-role-name").value); });
  $("pki-role-list").addEventListener("click", pkiRoleList);
  $("pki-role-new").addEventListener("click", () => pkiOpenRoleEditor($("pki-role-name").value, null));
  $("pki-role-cancel").addEventListener("click", () => { $("pki-role-editor").hidden = true; });
  $("pki-role-editor").addEventListener("submit", (e) => { e.preventDefault(); pkiRoleSave(); });
  $("pki-issue-form").addEventListener("submit", (e) => { e.preventDefault(); pkiIssue(); });
});
