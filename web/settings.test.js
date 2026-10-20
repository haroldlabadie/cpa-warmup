"use strict";
const test=require("node:test"),assert=require("node:assert/strict");let helpers={};const fs=require("node:fs");const vm=require("node:vm");const source=fs.readFileSync(__dirname+"/settings.js","utf8");vm.runInNewContext(source,{module:{exports:helpers},document:undefined,window:undefined,localStorage:undefined,sessionStorage:undefined,navigator:undefined,TextEncoder,TextDecoder,Uint8Array,atob,fetch:undefined,Date,JSON,Number,Object,console,setTimeout,clearTimeout});const parseHours=(...a)=>helpers.parseHours(...a),buildSettings=(...a)=>helpers.buildSettings(...a);
test("four default slots and strict input",()=>{assert.equal(JSON.stringify(parseHours("20,5,10,15")),JSON.stringify([5,10,15,20]));for(const bad of ["","5,5","24","-1","5foo","5,"])assert.throws(()=>parseHours(bad));});
test("form preserves timings and keeps channels independent",()=>{const previous={version:2,slot_grace_seconds:5400,check_seconds:20};const form={enabled:false,hours:"5,10,15,20",targets:[{scope:"codex",accounts:["codex.json"],model:"gpt-test"},{scope:"gemini",accounts:["gem.json"],model:"gemini-test"},{scope:"third_party",accounts:[],model:"claude-test"}]};const built=buildSettings(previous,form);assert.equal(built.version,2);assert.equal(JSON.stringify(built.targets[0].accounts),JSON.stringify(["codex.json"]));assert.equal(JSON.stringify(built.targets[1].accounts),JSON.stringify(["gem.json"]));assert.equal(JSON.stringify(built.targets[2].accounts),JSON.stringify([]));assert.equal(built.slot_grace_seconds,5400);assert.throws(()=>buildSettings(previous,{...form,targets:[...form.targets.slice(0,2),{...form.targets[2],model:"prefix/model"}]}));});
test("status reasons explain unresolved, readiness, and cycle boundaries",()=>{const {reasonText}=require("./settings.js");assert.equal(reasonText("host-hooks-not-ready"),"宿主门控尚未就绪，等待注册完成");assert.equal(reasonText("unresolved-prior-request"),"此前请求仍未决，暂停跨槽重发");assert.equal(reasonText("active:1800000000000"),"等待现有周期截止");assert.equal(reasonText("active-beyond-grace:1800000000000"),"现有周期超过本槽期限，持续复核");assert.equal(reasonText("successful-request:cycle-unverified"),"请求已完成，周期尚未核实");assert.equal(reasonText("new-reason"),"new-reason");assert.equal(reasonText(""),"");});
class Element {
  constructor(tag = "div") { this.tag = tag; this.children = []; this.value = ""; this.textContent = ""; this.hidden = false; this.disabled = false; this.checked = false; }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this.children = items; }
  setAttribute(name, value) { this[name] = value; }
  addEventListener(type, handler) { this["on" + type] = handler; }
  focus() {}
  contains() { return false; }
  set innerHTML(_) { throw new Error("unsafe HTML rendering"); }
}
function sessionApp(env = {}) {
  const elements = {}, requests = [], saved = new Map();
  const get = id => elements[id] || (elements[id] = new Element());
  const storage = { getItem: name => saved.has(name) ? saved.get(name) : null, setItem: (name, value) => saved.set(name, String(value)), removeItem: name => saved.delete(name) };
  const context = {
    document: { getElementById: get, createElement: tag => new Element(tag), createElementNS: (_ns, tag) => new Element(tag), addEventListener: () => {} },
    localStorage: storage, sessionStorage: storage, navigator: { userAgent: "test-agent" },
    window: { location: { host: "cpamp.example.invalid" } },
    TextEncoder, TextDecoder, Uint8Array, atob, fetch: async (url, options) => {
      requests.push({ url, options });
      if (url.endsWith("/settings")) return { ok: true, headers: { get: () => '"warmup-r1"' }, json: async () => ({ settings: { version: 2, enabled: true, slot_hours: [5,10,15,20], slot_grace_seconds: 5400, check_seconds: 20, targets: [{scope:"codex",accounts:[],model:"gpt-test"},{scope:"gemini",accounts:[],model:"gemini-test"},{scope:"third_party",accounts:[],model:"claude-test"}] }, revision: "warmup-r1" }) };
      if (url.endsWith("/status")) return { ok: true, headers: { get: () => null }, json: async () => ({ version: "0.2.0", worker_alive: true, quiesced: false, observe: false, last_tick_ms: 1, next_slot: "2026-10-21T05:00:00+08:00", slots: {}, account_options: [{name:"codex-long-file.json",provider:"codex",email:"person@example.invalid",disabled:false},{name:"antigravity-person@example.invalid.json",provider:"antigravity",email:"person@example.invalid",disabled:false,excluded_models:[]}] }) };
      if (url.endsWith("/catalog")) return { ok: true, headers: { get: () => null }, json: async () => ({ models: [{name:"gpt-test",provider:"openai"},{name:"gemini-test",provider:"antigravity"},{name:"claude-test",provider:"antigravity"}] }) };
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

test("long account names stay on one line in account pickers and records", () => {
  const html = fs.readFileSync(__dirname + "/settings.html", "utf8");
  assert.match(html, /\.multi-option small\{[^}]*text-overflow:ellipsis[^}]*white-space:nowrap/);
  assert.match(html, /\.sample-file\{[^}]*text-overflow:ellipsis[^}]*white-space:nowrap/);
  assert.match(html, /\.record-title strong\{[^}]*text-overflow:ellipsis/);
});
test("channels are ordered Codex, Gemini, Claude / GPT and use custom pickers", () => {
  const html = fs.readFileSync(__dirname + "/settings.html", "utf8");
  const codex = html.indexOf("Codex</strong>"), gemini = html.indexOf("Gemini</strong>"), third = html.indexOf("Claude / GPT</strong>");
  assert.ok(codex > 0 && codex < gemini && gemini < third);
  assert.doesNotMatch(html, /<select[\s>]/);
  assert.doesNotMatch(html, /最近槽位/);
  assert.match(html, /预热执行记录/);
});