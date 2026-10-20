"use strict";
const test=require("node:test"),assert=require("node:assert/strict");const {parseHours,buildSettings}=require("./settings.js");
test("four default slots and strict input",()=>{assert.deepEqual(parseHours("20,5,10,15"),[5,10,15,20]);for(const bad of ["","5,5","24","-1","5foo","5,"])assert.throws(()=>parseHours(bad));});
test("form preserves timings and rejects ambiguous empty selection",()=>{const previous={version:1,slot_grace_seconds:5400,check_seconds:20};const form={enabled:false,hours:"5,10,15,20",codex:"gpt-test",gemini:"gemini-test",third_party:"claude-test",allAccounts:true,accounts:[]};assert.deepEqual(buildSettings(previous,form).accounts,[]);assert.equal(buildSettings(previous,form).slot_grace_seconds,5400);assert.throws(()=>buildSettings(previous,{...form,allAccounts:false}));assert.throws(()=>buildSettings(previous,{...form,codex:"prefix/model"}));assert.deepEqual(buildSettings(previous,{...form,allAccounts:false,accounts:["account.json"]}).accounts,["account.json"]);});
test("status reasons explain unresolved, readiness, and cycle boundaries",()=>{const {reasonText}=require("./settings.js");assert.equal(reasonText("host-hooks-not-ready"),"宿主门控尚未就绪，等待注册完成");assert.equal(reasonText("unresolved-prior-request"),"此前请求仍未决，暂停跨槽重发");assert.equal(reasonText("active:1800000000000"),"等待现有周期截止");assert.equal(reasonText("active-beyond-grace:1800000000000"),"现有周期超过本槽期限，持续复核");assert.equal(reasonText("successful-request:cycle-unverified"),"请求已完成，周期尚未核实");assert.equal(reasonText("new-reason"),"new-reason");assert.equal(reasonText(""),"");});

const fs=require("node:fs");
test("design reflects the blue admin system",()=>{const html=fs.readFileSync(__dirname+"/settings.html","utf8");assert.match(html,/--accent:#265fae/);assert.match(html,/@media\(max-width:520px\)/);assert.match(html,/prefers-reduced-motion:reduce/);});


const vm = require("node:vm");
const source = fs.readFileSync(__dirname + "/settings.js", "utf8");
class Element {
  constructor(tag = "div") { this.tag = tag; this.children = []; this.value = ""; this.textContent = ""; this.hidden = false; this.disabled = false; }
  append(item) { this.children.push(item); }
  replaceChildren() { this.children = []; }
  setAttribute(name, value) { this[name] = value; }
  addEventListener(type, handler) { this["on" + type] = handler; }
  focus() {}
  set innerHTML(_) { throw new Error("unsafe HTML rendering"); }
}
function sessionApp(env = {}) {
  const elements = {}, requests = [], saved = new Map();
  const get = id => elements[id] || (elements[id] = new Element());
  const storage = {
    getItem: name => saved.has(name) ? saved.get(name) : null,
    setItem: (name, value) => saved.set(name, String(value)),
    removeItem: name => saved.delete(name)
  };
  const context = {
    document: { getElementById: get, createElement: tag => new Element(tag) },
    localStorage: storage,
    sessionStorage: storage,
    navigator: { userAgent: "test-agent" },
    window: { location: { host: "cpamp.example.invalid" } },
    TextEncoder, TextDecoder, Uint8Array, atob, fetch: async (url, options) => {
      requests.push({ url, options });
      if (url.endsWith("/settings")) return { ok: true, headers: { get: () => '"warmup-r1"' }, json: async () => ({ settings: { version: 1, enabled: true, slot_hours: [5,10,15,20], slot_grace_seconds: 5400, check_seconds: 20, accounts: [], models: { codex: "gpt-test", gemini: "gemini-test", third_party: "claude-test" } }, revision: "warmup-r1" }) };
      if (url.endsWith("/status")) return { ok: true, headers: { get: () => null }, json: async () => ({ version: "0.2.0", worker_alive: true, quiesced: false, observe: false, last_tick_ms: 1, next_slot: "2026-10-21T05:00:00+08:00", slots: {}, accounts: [] }) };
      throw new Error("unexpected request " + url);
    }, Date, JSON, Number, Object, console, setTimeout, clearTimeout
  };
  if (env.loggedIn) saved.set("isLoggedIn", "true");
  if (env.sessionKey) saved.set("cli-proxy-auth", JSON.stringify({ state: { managementKey: env.sessionKey } }));
  if (env.savedKey) saved.set("cpa_warmup_mgmt_key", env.savedKey);
  vm.runInNewContext(source, context);
  return { get, requests, saved, storage };
}
test("CPAMP session reuses automatically and hides the auth gate", async () => {
  const a = sessionApp({ loggedIn: true, sessionKey: "FAKE_SESSION" });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(a.get("authGate").hidden, true);
  assert.equal(a.get("app").hidden, false);
  assert.equal(a.requests[0].options.headers.Authorization, "Bearer FAKE_SESSION");
});
test("remembered manual key survives a fresh page load without showing the login gate", async () => {
  const first = sessionApp();
  first.get("manualKey").value = "FAKE_ADMIN";
  first.get("rememberKey").checked = true;
  first.get("manualConnect").onclick();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(first.saved.get("cpa_warmup_mgmt_key"), "FAKE_ADMIN");
  const second = sessionApp({ savedKey: "FAKE_ADMIN" });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(second.get("authGate").hidden, true);
  assert.equal(second.get("app").hidden, false);
});
test("logout clears the saved key and never renders a token field inside the app", async () => {
  const a = sessionApp({ savedKey: "FAKE_ADMIN" });
  await new Promise(resolve => setImmediate(resolve));
  a.get("logout").onclick();
  assert.equal(a.saved.has("cpa_warmup_mgmt_key"), false);
  assert.equal(a.get("authGate").hidden, false);
  const html = fs.readFileSync(__dirname + "/settings.html", "utf8");
  const app = html.slice(html.indexOf('id="app"'));
  assert.doesNotMatch(app, /type="password"|management|管理令牌|连接管理/);
});
test("status card uses a host-safe runtime component", () => {
  const html = fs.readFileSync(__dirname + "/settings.html", "utf8");
  assert.match(html, /class="runtime-rail"/);
  assert.doesNotMatch(html, /class="status-card"/);
  assert.match(html, /\.runtime-rail\{/);
});
