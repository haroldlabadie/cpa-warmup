"use strict";
const reasons={"unresolved-prior-request":"此前请求仍未决，暂停跨槽重发","probe-unknown":"额度未知，等待可靠采样","family-quota-unknown":"模型族额度不完整","disabled":"凭证已禁用","model-excluded":"目标模型被排除","quota-exhausted":"额度耗尽","quota-boundary-unknown":"额度周期边界未知","activity-history-unknown":"持久活动历史不可验证","prefix-missing-conflicting-or-invalid":"请配置唯一有效的凭证 prefix","observe-or-paused":"观察模式或计划已暂停","routing-changed":"凭证路由发生变化，等待复核","inventory-unknown":"凭证清单暂不可用","host-hooks-not-ready":"宿主门控尚未就绪，等待注册完成","sealed-generation-floor":"新世代仅允许未来槽位，等待时间下限","sealed-history-immutable":"封存历史不可改写","identity-not-bound":"身份尚未绑定，请停止 worker 后运行恢复 helper","migration-identity-unproven":"迁移身份未证明，需离线恢复","credential-identity-mismatch":"凭证身份已变化，历史保留","identity-generation-mismatch":"凭证与当前世代不符","historical-model-family-mismatch":"历史模型族不匹配","unresolved-history-without-identity":"存在无身份的历史未决记录","reset-buffer":"等待周期结束后的 15 秒缓冲","reset-buffer-or-retry-delay":"等待缓冲或重试间隔","policy-blocked":"模型访问策略拒绝","verified":"事件与周期已核实","event-query-unknown":"持久事件查询不可用","missing-event-or-new-cycle":"缺少完整请求或周期证据","no-five-hour-window":"此账号没有五小时窗口","already-active":"已有周期仍活跃","attempt-limit":"已到尝试次数上限","upstream-failed":"上游明确拒绝，等待有限重试"};
function reasonText(reason){const [base,...detail]=String(reason||"").split(":");if(base==="active")return "等待现有周期截止";if(base==="active-beyond-grace")return "现有周期超过本槽期限，持续复核";if(base==="slot-expired")return "本槽期限已过";if(base==="successful-request")return "请求已完成，周期尚未核实";if(base==="old-request")return "后续周期已核实，历史未决退休";return reasons[base]||reason||"";}
const labels={pending:"等待",retry:"等待重试",sending:"请求发送中",sent:"等待持久证据",unconfirmed:"结果未决",confirmed:"周期已核实",request_completed:"请求完成，周期未核实",uncertainty_expired:"历史未决已退休",skipped:"已跳过",error:"停止此槽"};
function parseHours(value){const parts=value.split(",").map(x=>x.trim());if(!parts.length||parts.some(x=>!/^\d{1,2}$/.test(x)))throw Error("请输入逗号分隔的 0–23 小时");const hours=parts.map(Number).sort((a,b)=>a-b);if(hours.some(x=>x>23)||new Set(hours).size!==hours.length)throw Error("小时不得重复，范围为 0–23");return hours;}
function modelValue(value){value=value.trim();if(!value||/[\/\s]/.test(value)||value.length>128)throw Error("模型名不能为空，且不能含 prefix、斜线或空格");return value;}
function buildSettings(previous,form){const accounts=form.allAccounts?[]:form.accounts;if(!form.allAccounts&&!accounts.length)throw Error("请选择至少一个账号，或勾选全部账号");return {...previous,enabled:form.enabled,slot_hours:parseHours(form.hours),accounts,models:{codex:modelValue(form.codex),gemini:modelValue(form.gemini),third_party:modelValue(form.third_party)}};}
if(typeof module!=="undefined")module.exports={parseHours,modelValue,buildSettings,labels,reasonText};
if(typeof document!=="undefined"){
 const $=id=>document.getElementById(id),base="/v0/management/plugins/cpa-warmup";let revision="",settings=null;
 async function call(path,method="GET",body){const headers={Authorization:"Bearer "+$("token").value.trim()};if(body!==undefined){headers["Content-Type"]="application/json";headers["If-Match"]=revision;}const response=await fetch(base+path,{method,headers,body:body===undefined?undefined:JSON.stringify(body),cache:"no-store"});const data=await response.json();if(!response.ok)throw Error(data.error||"HTTP "+response.status);return{data,etag:response.headers.get("ETag")};}
 function setText(id,text){$(id).textContent=text==null?"":String(text);}
 function setStatusTone(healthy){$("statusDot").className="dot"+(healthy?"":" danger");}
 function renderStatus(status){
  setText("version",status.version?"v"+status.version:"");
  const healthy=status.worker_alive&&!status.quiesced&&!status.last_error&&!status.observe;
  setStatusTone(healthy);
  setText("statusLabel",status.observe?"观察模式，不发送请求":status.quiesced?"已停止":status.worker_alive?"自动预热运行中":"已停用");
  setText("modeLabel",status.observe?"Observe · 观察模式":status.quiesced?"已停止":"Warmup · 自动预热");
  setText("lastCheck",status.last_tick_ms?new Date(status.last_tick_ms).toLocaleString():"尚未检查");
  setText("next",status.next_slot?(new Date(status.next_slot)).toLocaleString():"暂无计划");
  setText("error",status.last_error||"");
  const tbody=$("rows");tbody.replaceChildren();
  const rows=[];for(const [slot,records]of Object.entries(status.slots||{}).sort((a,b)=>b[0].localeCompare(a[0]))){for(const [name,record]of Object.entries(records))rows.push({slot,name,record});}
  if(!rows.length){const row=document.createElement("tr"),cell=document.createElement("td");cell.className="empty";cell.colSpan=6;cell.textContent="暂无槽位记录。保存计划后，调度器会在下一个计划时点开始检查。";row.append(cell);tbody.append(row);return;}
  for(const {slot,name,record} of rows){
   const row=document.createElement("tr");
   const tableLabels=Array.from(tbody.closest("table").querySelectorAll("thead th"),th=>th.textContent.trim());for(const [index,value] of [slot,name+(record.scope?" / "+record.scope:""),labels[record.status]||record.status,reasonText(record.last_reason),record.attempts||0,record.cycle_end_ms?new Date(record.cycle_end_ms).toLocaleString():"—"].entries()){const cell=document.createElement("td");cell.textContent=String(value);cell.setAttribute("data-label",tableLabels[index]||"");row.append(cell);}
   tbody.append(row);
  }
 }
 async function load(){
  try{
   $("message").textContent="正在读取计划与运行状态…";$("message").className="message";
   const [config,status]=await Promise.all([call("/settings"),call("/status")]);
   settings=config.data.settings;revision=config.etag||'"'+config.data.revision+'"';
   $("enabled").checked=settings.enabled;$("hours").value=settings.slot_hours.join(",");
   for(const scope of ["codex","gemini","third_party"])$(scope).value=settings.models[scope];
   $("allAccounts").checked=settings.accounts.length===0;
   const names=new Set(settings.accounts);for(const name of status.data.accounts||[])names.add(name);
   const select=$("accounts");select.replaceChildren();
   for(const name of [...names].sort()){const option=document.createElement("option");option.value=name;option.textContent=name;option.selected=settings.accounts.includes(name);select.append(option);}
   select.disabled=$("allAccounts").checked;$("form").disabled=false;
   setText("hoursSummary",settings.slot_hours.map(x=>String(x).padStart(2,"0")+":00").join(" / "));
   setText("accountSummary",settings.accounts.length===0?"全部支持的账号":settings.accounts.length+" 个指定账号");
   renderStatus(status.data);$("message").textContent="已加载最新计划";$("message").className="message success";
  }catch(error){$("message").textContent=error.message;$("message").className="message error";$("authLabel").hidden=false;}
 }
 function readSession(){try{let raw=localStorage.getItem("cli-proxy-auth");if(!raw)return "";if(raw.startsWith("enc::v1::")){const bytes=Uint8Array.from(atob(raw.slice(9)),c=>c.charCodeAt(0));const salt=new TextEncoder().encode(`cli-proxy-api-webui::secure-storage|${window.location.host}|${navigator.userAgent}`);raw=new TextDecoder().decode(bytes.map((b,i)=>b^salt[i%salt.length]));}const auth=JSON.parse(raw),state=auth.state||auth;return typeof state.managementKey==="string"?state.managementKey:"";}catch(_){return "";}}
 const session=readSession();if(session){$("token").value=session;$("authLabel").hidden=true;load();}
}
