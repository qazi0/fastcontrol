"use strict";

const usageCache = new Map();
const usageRequests = new Map();
let usageServer = "", usageRenderedServer = "";
const usageViewState = new Map();
const usageNumber = (value) => new Intl.NumberFormat(undefined, {notation: "compact", maximumFractionDigits: 1}).format(value || 0);
const usageMoney = (value) => value == null ? "Unavailable" : new Intl.NumberFormat(undefined, {style: "currency", currency: "USD"}).format(value);
const usageLogo = (provider) => `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="${AGENT_LOGOS[provider === 'claude' ? 'claude' : 'openai']}"/></svg>`;
function usageReset(timestamp) {
  const minutes = Math.max(0, Math.ceil((timestamp * 1000 - Date.now()) / 60000));
  if (!minutes) return "Reset Due";
  const days = Math.floor(minutes / 1440), hours = Math.floor(minutes % 1440 / 60), rest = minutes % 60;
  const parts = [];
  if (days) parts.push(`${days} ${days === 1 ? 'day' : 'days'}`);
  if (hours) parts.push(`${hours} ${hours === 1 ? 'hour' : 'hours'}`);
  if (!days && rest) parts.push(`${rest} ${rest === 1 ? 'minute' : 'minutes'}`);
  return "Resets in " + parts.join(", ");
}
function usageWindow(window) {
  const remaining = Math.max(0,Math.min(100,Number(window.remaining)));
  const percentage = new Intl.NumberFormat(undefined,{maximumFractionDigits:1}).format(remaining);
  return `<div class="usage-window${remaining < 10 ? ' near-limit' : ''}"><div class="usage-window-head"><span>${esc(window.label || 'Usage Window')}</span><strong>${percentage}<small>% Left</small></strong></div><div class="usage-track" role="meter" aria-label="${esc(window.label)} Remaining" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${remaining}"><span style="width:${remaining}%"></span></div><div class="usage-reset" ${window.resetAt ? `data-reset-at="${window.resetAt}" title="${esc(new Date(window.resetAt*1000).toLocaleString())}"` : ''}>${window.resetAt ? esc(usageReset(window.resetAt)) : 'Reset Time Unavailable'}</div></div>`;
}
function usageCost(total) {
  const money = usageMoney(total.costUsd);
  return money + (total.unpricedTokens && total.costUsd != null ? '+' : '');
}
function usageProvider(provider) {
  const name = provider.provider === 'claude' ? 'Claude Code' : 'Codex';
  const plan = provider.plan ? provider.plan.replace(/_/g,' ').replace(/\b\w/g,c=>c.toUpperCase()) : '';
  const today = provider.today, month = provider.days30;
  const tokens = (value) => `<span title="${Number(value || 0).toLocaleString()} Tokens" aria-label="${Number(value || 0).toLocaleString()} Tokens">${usageNumber(value)}</span>`;
  const extras = [];
  if (provider.resetCredits != null) extras.push(`<div class="usage-extra"><span>Available Resets</span><b>${provider.resetCredits}</b></div>`);
  if (provider.credits != null && provider.credits > 0) extras.push(`<div class="usage-extra"><span>Credits Remaining</span><b>${Number(provider.credits).toLocaleString()}</b></div>`);
  return `<section class="usage-provider" aria-label="${name} Usage"><header class="usage-provider-head">${usageLogo(provider.provider)}<h2>${name}</h2>${plan ? `<span class="usage-plan">${esc(plan)}</span>` : ''}</header>`+
    `<div class="usage-limits">${provider.windows.length ? provider.windows.map(usageWindow).join('') : `<p class="usage-unavailable">${esc(provider.quotaError || 'Subscription Limits Unavailable')}</p>`}</div>${extras.join('')}`+
    `<table class="usage-totals"><caption>Local Activity</caption><thead><tr><td></td><th scope="col">Today</th><th scope="col">30 Days</th></tr></thead><tbody><tr><th scope="row">Tokens</th><td>${tokens(today.tokens)}</td><td>${tokens(month.tokens)}</td></tr><tr><th scope="row">Est. API Cost</th><td>${esc(usageCost(today))}</td><td>${esc(usageCost(month))}</td></tr></tbody></table><p class="usage-cost-note">API-equivalent estimate. Subscription charges are separate.</p>`+
    (provider.localError || provider.skipped || month.unpricedTokens ? `<p class="usage-coverage">${esc(provider.localError || (provider.skipped ? 'Some large log entries were skipped.' : ''))}${month.unpricedTokens ? `${provider.localError || provider.skipped ? ' ' : ''}${usageNumber(month.unpricedTokens)} tokens have no published rate. Their cost is excluded.` : ''}</p>` : '')+
    (provider.models.length ? `<details class="usage-models" data-usage-provider="${esc(provider.provider)}"><summary>30-Day Breakdown ${I.down}</summary><table><thead><tr><th scope="col">Model</th><th scope="col">Tokens</th><th scope="col">Cost</th></tr></thead><tbody>${provider.models.map(model=>`<tr><th scope="row">${esc(model.model || 'Model Unrecorded')}</th><td>${tokens(model.tokens)}</td><td>${esc(usageCost(model))}</td></tr>`).join('')}</tbody></table><div class="usage-token-types"><span>Input <b>${usageNumber(month.input)}</b></span><span>Cached <b>${usageNumber(month.cached)}</b></span><span>Cache Writes <b>${usageNumber(month.writes)}</b></span><span>Output <b>${usageNumber(month.output)}</b></span></div></details>` : '')+
    `</section>`;
}
function renderUsage() {
  if (S.sheet !== 'usage') return;
  const sh = $('sheet'), connection = conns.get(usageServer), cached = usageCache.get(usageServer);
  const previousBody=sh.querySelector('.usage-body');
  if(previousBody && usageRenderedServer) usageViewState.set(usageRenderedServer,{scroll:previousBody.scrollTop,expanded:[...sh.querySelectorAll('.usage-models[open]')].map(node=>node.dataset.usageProvider)});
  usageRenderedServer=usageServer;

  const loading = usageRequests.has(usageServer);
  sh.innerHTML = `<header class="usage-head"><button class="icon-btn" data-usage="close" aria-label="Back">${svg('<path d="M15 5l-7 7 7 7"/>')}</button><div class="head-text"><h1>Usage</h1><div class="h-sub">${esc(serverName(usageServer))}</div></div><button class="icon-btn${loading ? ' loading' : ''}" data-usage="refresh" aria-label="Refresh Usage" ${loading ? 'disabled' : ''}>${loading ? '<span class="voice-spinner"></span>' : svg('<path d="M20 8a8 8 0 10.2 7M20 3v5h-5"/>')}</button></header>`+
    `<nav class="chips usage-machines" aria-label="Usage Computer">${S.servers.map(server=>`<button class="chip${server.id === usageServer ? ' on' : ''}" data-usage-server="${esc(server.id)}" aria-pressed="${server.id === usageServer}"><span class="dot${conns.get(server.id)?.ok ? ' on' : ' off'}"></span>${esc(serverName(server.id))}</button>`).join('')}</nav>`+
    `<main class="usage-body scroll">${cached?.report ? `<p class="usage-scope">Subscription limits follow the account on this computer.</p><div class="usage-providers">${cached.report.providers.map(usageProvider).join('')}</div><footer class="usage-foot"><p>Tokens and costs reflect local coding activity. Costs estimate the API value of that activity; subscription charges are separate.</p><div><span>Today · ${esc(cached.report.day)} ${esc(cached.report.zone)}</span><span>Updated ${esc(new Date(cached.report.updatedAt*1000).toLocaleTimeString([], {hour:'numeric',minute:'2-digit'}))}</span></div></footer>` : loading ? '<div class="usage-loading" role="status"><span class="voice-spinner"></span><p>Reading Usage</p><span>Checking limits and local activity.</span></div>' : ''}${cached?.error ? `<div class="usage-error" role="status"><p>${esc(cached.error)}</p><button class="btn" data-usage="refresh">Try Again</button></div>` : ''}${!connection?.ok ? '<p class="usage-offline">This computer is offline. Showing the last available reading.</p>' : ''}</main>`;
  const state=usageViewState.get(usageServer);
  if(state){for(const details of sh.querySelectorAll('.usage-models'))details.open=state.expanded.includes(details.dataset.usageProvider);sh.querySelector('.usage-body').scrollTop=state.scroll;}
}
async function readUsage(sid, refresh = false) {
  const connection = conns.get(sid);
  if (!connection?.ok) return {...usageCache.get(sid),error:'Connect to this computer to read its usage.'};
  if (usageRequests.has(sid)) return usageRequests.get(sid);
  const cached = usageCache.get(sid);
  if (!refresh && cached?.report && Date.now() - cached.at < 60000) return cached;
  const request = (async()=>{
    try {
      const response = await fetch(connection.base+'/usage'+(refresh?'?refresh=1':''),{headers:{Authorization:'Bearer '+connection.s.token},signal:AbortSignal.timeout(40000)});
      if (!response.ok) throw Error(response.status===404 ? 'Update fastcontrol on this computer to enable usage.' : 'Could not read usage. Check the computer connection and try again.');
      const report = await response.json();
      if (!Array.isArray(report.providers)) throw Error('This computer returned an invalid usage reading.');
      usageCache.set(sid,{report,at:Date.now()});
    } catch(error) {usageCache.set(sid,{...usageCache.get(sid),error:error.name==='TimeoutError'?'Usage took too long to refresh. Try again.':error.message});}
    return usageCache.get(sid);
  })();
  usageRequests.set(sid,request);
  try {return await request;} finally {if(usageRequests.get(sid)===request)usageRequests.delete(sid);}
}
async function loadUsage(refresh = false) {
  const sid=usageServer, request=readUsage(sid,refresh);
  renderUsage();
  const result=await request;
  if(result && !conns.get(sid)?.ok) usageCache.set(sid,result);
  if(sid===usageServer)renderUsage();
}
function weeklyQuota(report, provider) {
  const account = report?.providers?.find(value => value.provider === provider);
  const window = account?.windows?.find(value => /^weekly$/i.test(value.label || ''));
  if (!window || typeof window.used !== 'number' || !Number.isFinite(window.used)) return {account};
  const used = Math.max(0,Math.min(100,window.used));
  return {account,window,used,remaining:100-used,tone:used>90?'critical':used>75?'caution':''};
}
async function showContextWeekly(sheet, sid, provider) {
  const target=sheet.querySelector('#context-weekly');
  if(!target)return;
  const valid=()=>S.sheet==='context' && S.sid===sid && S.provider===provider && sheet.querySelector('#context-weekly')===target;
  const paint=cached=>{
    if(!valid())return;
    const quota=weeklyQuota(cached?.report,provider);
    target.className='context-weekly'+(quota.tone?' '+quota.tone:'');
    target.innerHTML=`<div class="context-weekly-head"><h3>Weekly Usage</h3>${quota.window ? `<strong>${new Intl.NumberFormat(undefined,{maximumFractionDigits:1}).format(quota.remaining)}% Left</strong>` : ''}</div>`+
      (quota.window ? `<div class="usage-track" role="meter" aria-label="Weekly Usage Used" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${quota.used}" aria-valuetext="${quota.remaining}% remaining"><span style="width:${quota.used}%"></span></div><p ${quota.window.resetAt ? `data-reset-at="${quota.window.resetAt}"` : ''}>${quota.window.resetAt ? esc(usageReset(quota.window.resetAt)):'Reset Time Unavailable'}</p>` : `<p>${esc(cached?.error || quota.account?.quotaError || 'Weekly usage is unavailable for this account.')}</p>`)+
      (cached?.error && quota.window ? `<p>${conns.get(sid)?.ok ? "Couldn't refresh usage" : 'Offline'}${quota.account?.quotaAt || cached.at ? ` · Updated ${esc(new Date(quota.account?.quotaAt ? quota.account.quotaAt*1000 : cached.at).toLocaleTimeString([], {hour:'numeric',minute:'2-digit'}))}` : ''}</p>` : '') +
      `<button class="context-weekly-link">Usage And Limits ${I.chev}</button>`;
    target.querySelector('button').onclick=()=>openUsage(sid);
  };
  const cached=usageCache.get(sid);
  if(cached?.report)paint(cached);
  else target.innerHTML='<div class="context-weekly-loading" role="status"><span class="voice-spinner"></span><span>Reading Weekly Usage</span></div>';
  paint(await readUsage(sid));
}

function openUsage(sid) {
  usageServer = sid || (S.machine!=='all'&&conns.has(S.machine)?S.machine:S.sid&&conns.has(S.sid)?S.sid:S.servers.find(server=>conns.get(server.id)?.ok)?.id||S.servers[0]?.id);
  if (!usageServer) {openSheet('macs');return;}
  openSheet('usage');
}
SHEETS.usage = (sh) => {
  renderUsage();loadUsage();
  sh.onclick = event => {
    const server = event.target.closest('[data-usage-server]');
    if(server){usageServer=server.dataset.usageServer;renderUsage();loadUsage();return;}
    const action = event.target.closest('[data-usage]')?.dataset.usage;
    if(action==='close')closeSheet();if(action==='refresh')loadUsage(true);
  };
};
$('usage-open').onclick = () => openUsage();
setInterval(()=>{
  if(!['usage','context'].includes(S.sheet)||document.hidden)return;
  for(const label of $('sheet').querySelectorAll('[data-reset-at]'))label.textContent=usageReset(Number(label.dataset.resetAt));
  if(S.sheet==='context'){if(Date.now()-(usageCache.get(S.sid)?.at||0)>=60000)showContextWeekly($('sheet'),S.sid,S.provider);}
  else if(Date.now() - (usageCache.get(usageServer)?.at || 0) >= 60000)loadUsage();
},30000);
