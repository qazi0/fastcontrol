"use strict";

const agentLists = new Map();
let agentView = null, agentListView = null, agentFrame = 0;
const agentListKey = (server, parent) => server + ":" + parent;
const agentLabel = agent => base(agent.agentPath) || agent.agentName || agent.title || "Subagent";
const agentStatus = agent => agent.lastErr ? "Stopped" : agent.running ? "Working" : "Done";

async function readAgents(server, parent, force = false) {
  const key = agentListKey(server, parent), c = conns.get(server);
  let entry = agentLists.get(key);
  if (!entry) { entry = { agents:[], at:0, error:"", pending:null }; agentLists.set(key,entry); }
  if (!c?.ok || entry.pending || (!force && Date.now() - entry.at < 4000)) return entry.pending || entry;
  entry.pending = (async () => {
    try {
      const result = await c.call({ op:"agents", id:parent },15000);
      if (!result.ok) throw Error(result.msg || "Could Not Load Subagents");
      entry.agents = result.agents || []; entry.error = "";
    } catch { entry.error = "Could Not Load Subagents. Reconnect To This Computer And Try Again."; }
    finally { entry.at = Date.now(); entry.pending = null; }
    return entry;
  })();
  return entry.pending;
}

window.refreshThreadAgents = async () => {
  const button = $("agents-btn");
  if (S.provider !== "codex" || S.view !== "thread" || !S.id) { button.classList.add("hidden"); return; }
  const server = S.sid, parent = S.id, entry = await readAgents(server,parent);
  if (S.provider !== "codex" || S.sid !== server || S.id !== parent) return;
  button.innerHTML = I.agent;
  button.classList.toggle("hidden", !entry.agents.length);
  button.setAttribute("aria-label", `Subagents, ${entry.agents.filter(a=>a.running).length} Working`);
};
$("agents-btn").onclick = () => openSheet("agents", { server:S.sid, parent:S.id });

function agentRow(agent) {
  const status = agentStatus(agent), detail = agent.activity || agent.lastMsg || agent.title || "";
  return `<button type="button" class="agent-row" data-open-agent="${esc(agent.id)}">${I.agent}<span class="agent-copy"><span class="agent-row-head"><b>${esc(agentLabel(agent))}</b><span class="agent-state${agent.running ? " working" : ""}">${esc(status)}</span></span>${agent.agentPath ? `<span class="agent-path">${esc(agent.agentPath)}</span>` : ""}<span class="agent-preview">${esc(detail)}</span></span>${I.chev}</button>`;
}
function paintAgentList(opts, entry) {
  if (S.sheet !== "agents" || agentListView !== opts) return;
  const list = $("agent-list"), top = list.scrollTop;
  const focused = document.activeElement?.dataset.openAgent;
  const active = entry.agents.filter(a=>a.running), done = entry.agents.filter(a=>!a.running);
  const section = (label,agents) => agents.length ? `<section><h3 class="agent-section">${label} <span>${agents.length}</span></h3>${agents.map(agentRow).join("")}</section>` : "";
  list.innerHTML = entry.error ? `<div class="empty"><p>${esc(entry.error)}</p><button class="btn" data-agent-refresh>Try Again</button></div>` : entry.agents.length ? section("Working",active) + section("Done",done) : '<div class="empty"><b>No Subagents Yet</b>Agents Delegated From This Chat Will Appear Here.</div>';
  list.scrollTop = top;
  if (focused) list.querySelector(`[data-open-agent="${CSS.escape(focused)}"]`)?.focus({preventScroll:true});
}
async function refreshAgentList(opts, force = false) { paintAgentList(opts, await readAgents(opts.server,opts.parent,force)); }
SHEETS.agents = (sh, opts) => {
  agentListView = opts;
  sh.innerHTML = `<header class="diff-head"><button class="icon-btn" data-agent-back aria-label="Back">${svg('<path d="M15 5l-7 7 7 7"/>')}</button><h2>Subagents</h2><button class="icon-btn" data-agent-refresh aria-label="Refresh Subagents">${svg('<path d="M20 8a8 8 0 10.2 7M20 3v5h-5"/>')}</button></header><main id="agent-list" class="scroll agent-list">${loadingHTML("Loading Subagents")}</main>`;
  sh.onclick = event => {
    if (event.target.closest("[data-agent-back]")) { if (opts.backAgent) openSheet("agent",opts.backAgent); else closeSheet(); return; }
    if (event.target.closest("[data-agent-refresh]")) { refreshAgentList(opts,true); return; }
    const row = event.target.closest("[data-open-agent]");
    const agent = agentLists.get(agentListKey(opts.server,opts.parent))?.agents.find(a=>a.id===row?.dataset.openAgent);
    if (agent) openSheet("agent", { server:opts.server, parent:opts.parent, agent, listOpts:opts });
  };
  const cached = agentLists.get(agentListKey(opts.server,opts.parent));
  if (cached?.at) paintAgentList(opts,cached);
  refreshAgentList(opts);
};

window.agentStepHTML = (item, turn, key) => `<div class="step" data-entry="${esc(key)}"><button type="button" class="step-h agent-link" data-agent-id="${esc(item.agentId || "")}" data-agent-path="${esc(item.agentPath || "")}" data-agent-parent="${esc(turn.agentParent || S.id)}">${I.agent}<span class="lbl">Subagent ${esc(item.title || item.agentPath || "")}</span>${I.chev}</button></div>`;
document.addEventListener("click", async event => {
  const button = event.target.closest("[data-agent-parent]");
  if (!button || S.provider !== "codex") return;
  const server = S.sid, parent = button.dataset.agentParent;
  const entry = await readAgents(server,parent);
  if (!button.isConnected || S.sid !== server) return;
  const matches = entry.agents.filter(a=>button.dataset.agentId ? a.id===button.dataset.agentId : a.agentPath===button.dataset.agentPath);
  const listOpts = { server,parent };
  if (agentView) listOpts.backAgent = { server:agentView.server,parent:agentView.parent,agent:agentView.info,listOpts:agentView.listOpts };
  if (matches.length === 1) openSheet("agent", { server,parent,agent:matches[0],listOpts });
  else openSheet("agents",listOpts);
});

function paintAgentViewer(more = false) {
  const view = agentView, root = $("agent-feed");
  if (!view || !root || S.sheet !== "agent") return;
  const top = root.scrollTop, fromBottom = root.scrollHeight - top;
  rememberTranscriptScroll(root);
  const existing = new Map([...root.querySelectorAll(":scope > .turn")].map(node=>[node.dataset.key,node]));
  const nodes = [];
  if (view.meta?.hasOlder) {
    const older = document.createElement("button"); older.className = "load-earlier"; older.dataset.agentMore = "1"; older.textContent = "Load Earlier"; nodes.push(older);
  }
  for (const turn of view.turns) {
    const el = existing.get(turn.key) || document.createElement("div"); el.className = "turn"; el.dataset.key = turn.key;
    const items = (turn.items || []).map(item=>item?.asks?.length ? {...item,text:[item.text,...item.asks.map(ask=>`**${ask.title}**\n\n${(ask.options || []).map((option,i)=>`- ${option}${ask.descriptions?.[i] ? ": "+ask.descriptions[i] : ""}`).join("\n")}`)].filter(Boolean).join("\n\n")} : item);
    renderTurn(el,{...turn,items,key:"agent:"+view.id+":"+turn.key,originalKey:turn.key,agentParent:view.id},!!view.loaded && !more); nodes.push(el);
  }
  for (const request of view.meta?.requests || []) {
    const block = document.createElement("aside");block.className="agent-request";block.dataset.entry="agent:"+view.id+":request:"+request.id;
    block.innerHTML=`<b>${esc(request.title || "Waiting For Input In Parent Chat")}</b>${request.body ? `<pre>${esc(request.body)}</pre>` : ""}${(request.questions || []).map(question=>`<div class="md">${md(question.question || "")}<ul>${(question.options || []).map(option=>`<li>${esc(option.label || option)}</li>`).join("")}</ul></div>`).join("")}`;
    nodes.push(block);
  }
  if (!nodes.length) { const empty = document.createElement("div"); empty.className = "empty"; empty.textContent = "No Messages Yet"; nodes.push(empty); }
  reconcileChildren(root,nodes);
  root.scrollTop = more ? root.scrollHeight - fromBottom : view.stick ? root.scrollHeight : top;
  restoreTranscriptScroll(root);
  queueImages(root);
  root.dataset.cwd = view.meta?.cwd || "";
  const connected = conns.get(view.server)?.ok;
  const waiting = !!view.meta?.requests?.length;
  $("agent-status").textContent = [view.meta?.model && shortModel(view.meta.model), view.meta?.effort, connected ? waiting ? "Waiting For Input In Parent Chat" : view.meta?.running ? "Working" : "Done" : "Offline"].filter(Boolean).join(" · ");
  $("agent-working").classList.toggle("hidden", !connected || waiting || !view.meta?.running);
  view.loaded = true;
}
window.followAgentResponses = () => { const root=$("agent-feed");if(root && agentView?.stick)root.scrollTop=root.scrollHeight; };
function dirtyAgentViewer() {
  if (!agentFrame) agentFrame = requestAnimationFrame(()=>{ agentFrame=0;paintAgentViewer(); });
}
window.onAgentMessage = (connection, message, provider) => {
  const view = agentView;
  if (!view || provider !== "codex" || connection.s.id !== view.server || message.id !== view.id) return false;
  switch (message.t) {
    case "open": view.meta=message.meta;view.turns=message.turns || [];paintAgentViewer(message.more);return true;
    case "meta": view.meta=message.meta;dirtyAgentViewer();return true;
    case "turn": { const index=view.turns.findIndex(t=>t.key===message.turn.key);if(index>=0)view.turns[index]=message.turn;else view.turns.push(message.turn);dirtyAgentViewer();return true; }
    case "head": { const turn=view.turns.find(t=>t.key===message.key);if(turn){turn.status=message.status;turn.durationMs=message.durationMs;turn.error=message.error;}dirtyAgentViewer();return true; }
    case "item": { const turn=view.turns.find(t=>t.key===message.key);if(turn)turn.items[message.idx]=message.item;dirtyAgentViewer();return true; }
    default:return false;
  }
};
window.disposeAgentViewer = () => {
  if (!agentView) return;
  conns.get(agentView.server)?.send({ op:"close",id:agentView.id });
  const root = $("agent-feed"); if (root) clearAudio(root);
  for (const key of responses.keys()) if (key.startsWith("agent:"+agentView.id+":")) responses.delete(key);
  agentView = null; cancelAnimationFrame(agentFrame);agentFrame=0;
};
window.reconnectAgentViewer = connection => {
  const view = agentView;
  if (view?.server === connection.s.id) connection.call({op:"agentOpen",id:view.id,parent:view.parent},15000);
};
SHEETS.agent = async (sh, opts) => {
  window.disposeAgentViewer();
  const view = agentView = { server:opts.server,parent:opts.parent,id:opts.agent.id,info:opts.agent,listOpts:opts.listOpts,meta:null,turns:[],stick:true };
  sh.innerHTML = `<header class="diff-head agent-head"><button class="icon-btn" data-agent-back aria-label="Back To Subagents">${svg('<path d="M15 5l-7 7 7 7"/>')}</button><div class="head-text"><h2>${esc(agentLabel(opts.agent))}</h2><div class="h-sub" id="agent-status">Read Only</div></div><button class="icon-btn" data-agent-children aria-label="View This Agent's Subagents">${I.agent}</button></header><main id="agent-feed" class="scroll feed agent-feed"><div class="empty">${loadingHTML("Loading Subagent")}</div></main><div id="agent-working" class="agent-working hidden" role="status"><span class="thinking">Thinking</span></div><footer class="agent-footer">Read Only<span>Managed By The Parent Chat</span></footer>`;
  const root = $("agent-feed");
  root.dataset.server = view.server;root.dataset.thread = view.id;
  root.addEventListener("scroll",()=>{view.stick=!view.readingOutput&&root.scrollHeight-root.scrollTop-root.clientHeight<90;},{passive:true});
  for (const event of ["pointerdown","wheel"]) root.addEventListener(event,e=>{view.readingOutput=!!e.target.closest("pre,.md-table");if(view.readingOutput)view.stick=false;},{passive:true});
  root.addEventListener("load",()=>{if(view.stick)root.scrollTop=root.scrollHeight;},true);
  root.addEventListener("click",event=>{
    if (event.target.closest("[data-agent-more]")) { event.target.closest("[data-agent-more]").innerHTML=loadingHTML("Loading Earlier Messages");conns.get(view.server)?.send({op:"more",id:view.id});return; }
    transcriptClick(event);
  });
  sh.onclick = event => {
    if (event.target.closest("[data-agent-back]")) openSheet("agents",opts.listOpts || {server:view.server,parent:view.parent});
    else if (event.target.closest("[data-agent-children]")) openSheet("agents", {server:view.server,parent:view.id,backAgent:opts});
  };
  const result = await conns.get(view.server)?.call({op:"agentOpen",id:view.id,parent:view.parent},15000);
  if (agentView !== view || S.sheet !== "agent") return;
  if (!result?.ok) root.innerHTML=`<div class="empty"><b>Subagent Is Unavailable</b>${esc(result?.msg || "Reconnect To This Computer And Try Again")}</div>`;
};

setInterval(()=>{
  if (document.hidden || S.provider !== "codex" || S.view !== "thread") return;
  window.refreshThreadAgents();
  if (agentView) dirtyAgentViewer();
  if (S.sheet === "agents" && agentListView) refreshAgentList(agentListView);
},4000);
