// End-to-end test of the /ui/ console: drives it in headless Chrome over the
// DevTools protocol (no dependencies — Node >= 22 for the built-in WebSocket).
// Run it through run.sh (or `make console-e2e`), which starts a fresh,
// uninitialized server for it.
//
// usage: node console-e2e.mjs <vault-base-url> <screenshot-dir>
import { spawn } from "node:child_process";
import { writeFileSync, mkdtempSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const BASE = process.argv[2];
const OUT = process.argv[3];
const CHROME = process.env.CHROME_BIN || [
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  "/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium", "/usr/bin/chromium-browser",
].find((p) => existsSync(p));
if (!CHROME) { console.error("no Chrome found; set CHROME_BIN"); process.exit(2); }
const PORT = 9333;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const check = (name, ok, detail = "") => {
  console.log(`${ok ? "ok  " : "FAIL"} ${name}${detail ? "  — " + detail : ""}`);
  if (!ok) failures++;
};

const profile = mkdtempSync(join(tmpdir(), "cdp-"));
const chromeArgs = ["--headless=new", `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`,
  "--window-size=1100,900", "--no-first-run", "about:blank"];
if (process.env.CI) chromeArgs.push("--no-sandbox"); // CI containers lack the sandbox's kernel features
const chrome = spawn(CHROME, chromeArgs, { stdio: "ignore" });

async function target() {
  for (let i = 0; i < 50; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${PORT}/json/new?${encodeURIComponent(BASE + "/ui/")}`, { method: "PUT" });
      if (r.ok) return await r.json();
    } catch (_) { /* not up yet */ }
    await sleep(200);
  }
  throw new Error("chrome devtools did not come up");
}

const t = await target();
const ws = new WebSocket(t.webSocketDebuggerUrl);
await new Promise((res) => ws.addEventListener("open", res, { once: true }));
let seq = 0;
const pending = new Map();
ws.addEventListener("message", (ev) => {
  const m = JSON.parse(ev.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
});
const send = (method, params = {}) => new Promise((res) => { const id = ++seq; pending.set(id, res); ws.send(JSON.stringify({ id, method, params })); });
const js = async (expr) => {
  const r = await send("Runtime.evaluate", { expression: `(async () => { ${expr} })()`, awaitPromise: true, returnByValue: true });
  if (r.result && r.result.exceptionDetails) throw new Error("page error: " + JSON.stringify(r.result.exceptionDetails.exception?.description || r.result.exceptionDetails.text));
  return r.result?.result?.value;
};
const shot = async (name) => {
  const r = await send("Page.captureScreenshot", { format: "png" });
  writeFileSync(join(OUT, name + ".png"), Buffer.from(r.result.data, "base64"));
};
const visible = (id) => js(`const e = document.getElementById(${JSON.stringify(id)}); return !!e && !e.hidden && e.offsetParent !== null;`);
const text = (id) => js(`return document.getElementById(${JSON.stringify(id)}).textContent;`);
const settle = () => sleep(700);
const submit = (formId) => js(`document.getElementById(${JSON.stringify(formId)}).requestSubmit(); return true;`);
const setVal = (id, v) => js(`const e = document.getElementById(${JSON.stringify(id)}); e.value = ${JSON.stringify(v)}; e.dispatchEvent(new Event("change")); return true;`);

await send("Page.enable");
await send("Runtime.enable");
await sleep(1500);

// ---- 1. uninitialized: only Initialize shows
check("uninitialized: Initialize panel shown", await visible("init-panel"));
check("uninitialized: Unseal panel hidden", !(await visible("unseal-panel")));
await shot("1-uninitialized");

// ---- 2. initialize 3 shares / threshold 2
await setVal("init-shares", "3");
await setVal("init-threshold", "2");
await submit("init-form");
await sleep(1500);
const keys = await js(`return [...document.querySelectorAll("#init-out .keys")].map(l => [...l.querySelectorAll("code")].map(c => c.textContent));`);
check("init: 3 key shares shown", keys && keys[0] && keys[0].length === 3, keys ? `${keys[0]?.length} shares` : "none");
check("init: root token shown", keys && keys[1] && /^uv\./.test(keys[1][0] || ""));
check("init: keys stay visible after init (panel not auto-hidden)", await visible("init-panel"));
check("init: Unseal panel now shown (sealed)", await visible("unseal-panel"));
check("init: progress starts at 0 of 2", (await text("unseal-progress")) === "0 of 2", await text("unseal-progress"));
await shot("2-initialized-keys");
const [shares, [root]] = keys;

// ---- 3. hide the secrets
await js(`[...document.querySelectorAll("#init-out button")].find(b => b.textContent.startsWith("I've stored")).click(); return true;`);
await settle();
check("hide: Initialize panel gone", !(await visible("init-panel")));
check("hide: no key material left in the page", !(await js(`return document.body.innerText.includes(${JSON.stringify(shares[0])});`)));

// ---- 4. unseal with two shares
await setVal("unseal-key", shares[0]);
await submit("unseal-form");
await settle();
check("unseal: share 1 accepted, 1 of 2", (await text("unseal-progress")) === "1 of 2", await text("unseal-progress"));
check("unseal: input cleared after submit", (await js(`return document.getElementById("unseal-key").value;`)) === "");
await shot("3-unseal-progress");
await setVal("unseal-key", "not-a-share");
await submit("unseal-form");
await settle();
check("unseal: a bad share is reported", /Request failed|key|invalid/i.test(await text("unseal-out")), await text("unseal-out"));
await setVal("unseal-key", shares[1]);
await submit("unseal-form");
await sleep(1200);
check("unseal: vault unsealed", (await text("seal-word")) === "Unsealed", await text("seal-word"));
check("unseal: Unseal panel hidden", !(await visible("unseal-panel")));
await shot("4-unsealed");

// ---- 5. sign in with the root token (phase 1, never browser-tested before)
await setVal("login-method", "token");
await js(`document.getElementById("login-method").dispatchEvent(new Event("change")); return true;`);
check("sign-in: token field shown for Token method", await visible("login-token"));
check("sign-in: username hidden for Token method", !(await visible("login-user")));
await setVal("login-token", root);
await submit("login-form");
await sleep(1000);
check("sign-in (token): who-am-I shown", await visible("whoami"));
check("sign-in (token): login form hidden", !(await visible("login-form")));
check("sign-in (token): button says Forget token", (await text("logout")) === "Forget token", await text("logout"));
check("sign-in (token): policies show root", (await text("whoami-table")).includes("root"));
await shot("5-signed-in-token");

// ---- 6. userpass: create a user with the root token, then sign in with the form
const mk = await fetch(BASE + "/v1/auth/userpass/users/alice", { method: "POST", headers: { "X-Vault-Token": root },
  body: JSON.stringify({ password: "pw-console-e2e", policies: ["readers"] }) });
check("setup: userpass user created", mk.status === 204, String(mk.status));
await js(`document.getElementById("logout").click(); return true;`);
await settle();
check("forget: pasted root token only forgotten", /not revoked/i.test(await text("session-out")), await text("session-out"));
const rootStill = await fetch(BASE + "/v1/auth/token/lookup-self", { headers: { "X-Vault-Token": root } });
check("forget: root token still valid on the server", rootStill.status === 200, String(rootStill.status));

await setVal("login-method", "userpass");
await js(`document.getElementById("login-method").dispatchEvent(new Event("change")); return true;`);
await setVal("login-user", "alice");
await setVal("login-pass", "wrong");
await submit("login-form");
await settle();
check("sign-in (userpass): wrong password gets the uniform message", /Sign-in failed/.test(await text("session-out")), await text("session-out"));
check("sign-in (userpass): password field cleared", (await js(`return document.getElementById("login-pass").value;`)) === "");
await setVal("login-user", "alice");
await setVal("login-pass", "pw-console-e2e");
await submit("login-form");
await sleep(1000);
const who = await text("whoami-table");
check("sign-in (userpass): who-am-I shows the user", who.includes("alice") && who.includes("readers"), who.replace(/\s+/g, " ").slice(0, 120));
check("sign-in (userpass): button says Sign out", (await text("logout")) === "Sign out");
const aliceToken = await js(`return sessionStorage.getItem("ubixvault.token");`);
await shot("6-signed-in-userpass");
await js(`document.getElementById("logout").click(); return true;`);
await settle();
check("sign out: says the token was revoked", /revoked/i.test(await text("session-out")), await text("session-out"));
const dead = await fetch(BASE + "/v1/auth/token/lookup-self", { headers: { "X-Vault-Token": aliceToken } });
check("sign out: the token is really revoked on the server", dead.status === 403, String(dead.status));

ws.close();
chrome.kill();
console.log(failures ? `\n${failures} FAILED` : "\nALL PASSED");
process.exit(failures ? 1 : 0);
