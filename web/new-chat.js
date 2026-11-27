"use strict";

function sessionModelLabel(model) {
  if (!model) return "Model";
  const name = model.name || "", resolved = model.resolved || model.slug;
  const alias = !!(model.resolved && model.resolved !== model.slug);
  return alias && !/\d/.test(name) && /\d/.test(resolved || "") && model.slug !== "default" ? shortModel(resolved) : name || shortModel(resolved);
}
function sortNewChatEfforts(list) {
  if (typeof sortEffortsAscending === "function") return sortEffortsAscending(list);
  const rank = { minimal: 0, low: 1, medium: 2, high: 3, xhigh: 4, max: 5, ultra: 6 };
  const seen = new Set(), items = [];
  for (const raw of list || []) {
    const value = String(raw || "").trim().toLowerCase();
    if (!value || seen.has(value)) continue;
    seen.add(value);
    items.push({ value, rank: Object.prototype.hasOwnProperty.call(rank, value) ? rank[value] : 999, idx: items.length });
  }
  items.sort((a, b) => a.rank - b.rank || a.idx - b.idx);
  return items.map(x => x.value);
}
function newChatModel(connection, draft) {
  const models = connection?.models?.models || [], value = draft.model || connection?.models?.default?.model;
  return models.find(model => model.slug === value || model.resolved === value) || (!draft.model ? models[0] : undefined);
}
function newChatModelChoices(connection) {
  const models = connection?.models?.models || [], selectedDefault = newChatModel(connection, {model:""});
  const resolvedDefault = selectedDefault?.resolved && models.find(model => model.slug !== "default" && model.resolved === selectedDefault.resolved);
  const label = resolvedDefault ? sessionModelLabel(resolvedDefault) : selectedDefault?.resolved ? shortModel(selectedDefault.resolved) : selectedDefault ? sessionModelLabel(selectedDefault) : "";
  return [{slug:"",name:label && label !== "Default" ? `Default (${label})` : "Default",description:""}, ...models.filter(model => model.slug !== "default").map(model => ({...model,name:sessionModelLabel(model)}))];
}
function newChatModes(connection, provider) {
  if (provider === "antigravity") {
    if (typeof antigravityPermsFor === "function") return antigravityPermsFor(null, connection?.models);
    const modes = Array.isArray(connection?.models?.modes) ? connection.models.modes : [];
    const modeIds = new Set(modes.map(x => typeof x === "string" ? x : x?.id).filter(Boolean));
    const allowFull = connection?.models?.allowFullAccess !== false || modeIds.has("full-access");
    const baseModes = [
      { id: "default", name: "Default", d: "Asks Before Edits And Commands" },
      { id: "accept-edits", name: "Accept Edits", d: "Approves File Edits Automatically" },
      { id: "auto", name: "Auto", d: "Runs Approved Actions Automatically", warn: true },
      { id: "plan", name: "Plan", d: "Plans Without Making Edits" },
    ];
    if (allowFull) baseModes.push({ id: "full-access", name: "Turbo", d: "Auto-Approves All Edits And Commands", warn: true });
    return baseModes.map(mode => {
      const live = modes.find(x => x && typeof x === "object" && x.id === mode.id);
      return live?.description ? { ...mode, d: live.description } : mode;
    });
  }
  if (provider === "grok") {
    if (typeof grokPermsFor === "function") return grokPermsFor(null, connection?.models);
    const modes = Array.isArray(connection?.models?.modes) ? connection.models.modes : [];
    const modeIds = new Set(modes.map(x => typeof x === "string" ? x : x?.id).filter(Boolean));
    const allowFull = Boolean(connection?.models?.allowFullAccess || modeIds.has("bypassPermissions"));
    const baseModes = [
      { id: "default", name: "Default", d: "Asks Before Edits And Commands" },
      { id: "acceptEdits", name: "Accept Edits", d: "Approves File Edits Automatically" },
      { id: "auto", name: "Auto", d: "Runs Approved Actions Automatically", warn: true },
      { id: "plan", name: "Plan", d: "Plans Without Making Edits" },
    ];
    if (allowFull) baseModes.push({ id: "bypassPermissions", name: "Full Access", d: "Skips All Permission Prompts", warn: true });
    return baseModes.map(mode => {
      const live = modes.find(x => x && typeof x === "object" && x.id === mode.id);
      return live?.description ? { ...mode, d: live.description } : mode;
    });
  }
  return [];
}

SHEETS.new = (sh, opts = {}) => {
  const provider = S.provider;
  const agentTitle = provider === "claude" ? "Claude" : provider === "antigravity" ? "Antigravity" : provider === "grok" ? "Grok" : "Codex";
  const d = S.newDraft || (S.newDraft = {cwd:"",text:"",model:"",effort:"",mode:"",sid:""});
  if(d.model === "default")d.model="";
  if (opts.cwd) d.cwd = opts.cwd;
  if (opts.sid) d.sid = opts.sid;
  if (!d.sid || !conns.has(d.sid)) {
    const supported = S.servers.find(server => conns.get(server.id)?.capabilities?.[provider]);
    d.sid = S.machine !== "all" && conns.has(S.machine) ? S.machine : supported?.id || S.servers[0]?.id;
  }
  const initial = conns.get(d.sid);
  if (!initial) {closeSheet(true);openSheet("macs");return;}
  if (!d.cwd) d.cwd = initial.threads.find(thread => !thread.subagent)?.cwd || "";
  let modelOpen = false, projectOpen = false;
  let sliderDragging = false, redrawPending = false;
  const toggleMenu = (kind,open) => {
    const button=kind === "model" ? sh.querySelector("#nt-model") : sh.querySelector("[data-project-toggle]");
    const menu=button?.closest(".field").querySelector(".setup-disclosure");
    if(!menu)return;button.setAttribute("aria-expanded",String(open));menu.inert=!open;menu.classList.toggle("open",open);
  };
  sh.classList.add("new-chat-sheet");
  const draw = () => {
    if (S.sheet !== "new" || S.newDraft !== d || S.provider !== provider) return;
    if(sliderDragging){redrawPending=true;return;}
    const c = conns.get(d.sid);if (!c) return;
    const focused = sh.contains(document.activeElement) ? document.activeElement : null;
    const focus = focused?.id, selection = focused && /^(TEXTAREA|INPUT)$/.test(focused.tagName) ? [focused.selectionStart,focused.selectionEnd] : null;
    const scroll = sh.scrollTop;
    const projects = (c.projects || projectsOf(c.threads,false)).slice(0,30);
    const models = c.models?.models || [], choices = newChatModelChoices(c), model = newChatModel(c,d);
    const selected = choices.find(choice => choice.slug === d.model);
    const rawEfforts = model?.efforts || (provider === "antigravity" && models.length ? ["low","medium","high","xhigh"] : provider === "grok" && models.length ? ["minimal","low","medium","high","xhigh"] : []);
    const efforts = sortNewChatEfforts(rawEfforts), levels = ["",...efforts];
    if (d.effort && model && !efforts.includes(d.effort)) d.effort = "";
    const level = Math.max(0,levels.indexOf(d.effort));
    const label = value => value ? effortName(value) : "Default";
    const modes = newChatModes(c, provider);
    const defaultMode = c.models?.default?.mode || "default";
    if (modes.length && !modes.some(mode => mode.id === d.mode)) d.mode = modes.some(mode => mode.id === defaultMode) ? defaultMode : modes[0].id;
    const activeMode = modes.find(mode => mode.id === d.mode);
    const error = c.models?.error;
    const emptyResponse = !models.length && !error && c.models && Array.isArray(c.models.models);
    const modelFailed = Boolean(error || emptyResponse);
    sh.innerHTML = `<div class="grip"></div><header class="new-chat-head"><h2>New ${agentTitle} Chat</h2><button class="icon-btn" data-new-close aria-label="Close New Chat">${I.close}</button></header><fieldset class="new-chat-fields"${d.starting ? " disabled" : ""}>` +
      (S.servers.length > 1 ? `<div class="field"><label class="lab" for="nt-computer">Computer</label><div class="setup-select"><select id="nt-computer">${S.servers.map(server => `<option value="${esc(server.id)}"${server.id === d.sid ? " selected" : ""}>${esc(server.name || hostOf(server.url))}</option>`).join("")}</select>${I.down}</div></div>` : "") +
      `<div class="field"><label class="lab" for="nt-cwd">Project</label><div class="setup-folder"><input id="nt-cwd" placeholder="Folder Path" value="${esc(d.cwd)}" autocomplete="off" autocapitalize="off" spellcheck="false"><button class="icon-btn" data-project-toggle aria-expanded="${projectOpen}" aria-controls="nt-projects" aria-label="Choose Project">${I.down}</button></div><div class="setup-disclosure${projectOpen ? " open" : ""}"${projectOpen ? "" : " inert"}><div class="setup-menu-clip"><div class="proj-pick" id="nt-projects">${projects.map(project => `<button data-cwd="${esc(project.cwd)}" class="${project.cwd === d.cwd ? "on" : ""}"><span class="nm">${esc(base(project.cwd))}</span><span class="pa">${esc(c.home ? project.cwd.replace(c.home,"~") : project.cwd)}</span></button>`).join("")}</div></div></div></div>` +
      `<div class="field"><label class="lab" id="nt-model-label">Model</label><button class="setup-model" id="nt-model" aria-labelledby="nt-model-label nt-model-name" aria-haspopup="listbox" aria-expanded="${modelOpen}" aria-controls="nt-models"><span id="nt-model-name">${models.length ? esc(selected?.name || sessionModelLabel({slug:d.model})) : modelFailed ? "Choose Model" : loadingHTML("Loading Models")}</span>${I.down}</button><div class="setup-disclosure${modelOpen ? " open" : ""}"${modelOpen ? "" : " inert"}><div class="setup-menu-clip"><div class="setup-model-list" id="nt-models" role="listbox" aria-labelledby="nt-model-label">${models.length ? choices.map((choice,i) => `<button role="option" id="nt-model-option-${i}" aria-selected="${choice.slug === d.model}" data-new-model="${esc(choice.slug)}"><span><b>${esc(choice.name)}</b>${choice.description ? `<small>${esc(choice.description)}</small>` : ""}</span>${choice.slug === d.model ? I.check : ""}</button>`).join("") : modelFailed ? "" : loadingHTML("Loading Models")}${modelFailed ? `<p class="hint">${error ? esc(error) : "No models available yet."}</p><button class="btn" data-model-retry>Try Again</button>` : ""}</div></div></div></div>` +
      (model ? `<div class="field"><div class="setup-effort-head">${efforts.length ? '<label class="lab" for="nt-effort">Intelligence</label>' : '<span class="lab">Intelligence</span>'}<output id="nt-level"${efforts.length ? ' for="nt-effort"' : ''}>${efforts.length ? esc(label(d.effort)) : "Automatic"}</output></div>${efforts.length ? `<div class="setup-effort"><input id="nt-effort" type="range" min="0" max="${levels.length-1}" step="1" value="${level}" aria-valuetext="${esc(label(d.effort))}" style="--level:${level/(levels.length-1)*100}%"><div class="setup-effort-ends"><span>Default</span><span>${esc(effortName(efforts.at(-1)))}</span></div></div>` : ""}</div>` : "") +
      (modes.length ? `<div class="field"><span class="lab" id="nt-mode-label">Execution Mode</span><div class="seg" role="group" aria-labelledby="nt-mode-label">${modes.map(mode => `<button type="button" data-new-mode="${esc(mode.id)}" class="${mode.id === d.mode ? "on" : ""}${mode.warn ? " warn" : ""}" aria-pressed="${mode.id === d.mode}">${esc(mode.name)}</button>`).join("")}</div>${activeMode?.d ? `<p class="setup-mode-hint">${esc(activeMode.d)}</p>` : ""}</div>` : "") +
      `<div class="field"><label class="lab" for="nt-text">Prompt</label><textarea id="nt-text" placeholder="What Should ${agentTitle} Do?">${esc(d.text)}</textarea></div><div class="attachments" id="nt-att">${attHTML(S.newAttach)}</div></fieldset><footer class="sheet-actions new-chat-actions"><button class="btn" data-x="img"${d.starting ? " disabled" : ""}>Attach File</button><span class="grow"></span><button class="btn primary" data-x="start"${d.starting || !models.length ? " disabled" : ""}>${d.starting ? loadingHTML("Starting") : "Start Chat"}</button></footer>`;
    const text = sh.querySelector("#nt-text"), cwd = sh.querySelector("#nt-cwd"), computer = sh.querySelector("#nt-computer"), slider = sh.querySelector("#nt-effort");
    text.oninput = () => d.text = text.value;
    cwd.oninput = () => d.cwd = cwd.value.trim();
    if (computer) computer.onchange = () => {
      for(const [id,batch] of attachmentBatches)if(batch.list===S.newAttach)attachmentBatches.delete(id);
      persistAttachmentBatches();store.set("attachment-return",null);
      d.sid = computer.value;d.model = "";d.effort = "";d.mode = "";S.newAttach = [];modelOpen = projectOpen = false;
      const next = conns.get(d.sid);if (!next) return;
      d.cwd = next.threads.find(thread => !thread.subagent)?.cwd || "";
      if (!next.models?.models?.length) next.models = null;
      next.send({op:"models",provider});next.send({op:"projects",provider});draw();
    };
    if (slider) slider.oninput = () => {
      d.effort = levels[Number(slider.value)];
      slider.style.setProperty("--level",`${Number(slider.value)/(levels.length-1)*100}%`);
      slider.setAttribute("aria-valuetext",label(d.effort));sh.querySelector("#nt-level").textContent = label(d.effort);
    };
    if(slider){slider.onpointerdown=()=>{sliderDragging=true;};const finish=()=>{sliderDragging=false;if(redrawPending){redrawPending=false;draw();}};slider.onpointerup=finish;slider.onpointercancel=finish;slider.onlostpointercapture=finish;}
    sh.scrollTop = scroll;
    const escapedFocus = focus && (typeof CSS !== "undefined" && CSS.escape ? CSS.escape(focus) : focus);
    const restore = escapedFocus && sh.querySelector("#"+escapedFocus);
    if (restore) {restore.focus({preventScroll:true});if (selection && selection[0] != null) restore.setSelectionRange(...selection);}
  };
  S.redraw = draw;
  if (!initial.models?.models?.length) initial.models = null;
  initial.send({op:"models",provider});initial.send({op:"projects",provider});
  draw();sh.scrollTop = 0;
  sh.onkeydown = event => {
    if (event.key === "Escape" && (modelOpen || projectOpen)) {event.preventDefault();event.stopPropagation();modelOpen = projectOpen = false;toggleMenu("model",false);toggleMenu("project",false);sh.querySelector("#nt-model")?.focus({preventScroll:true});}
    if (modelOpen && event.target.closest("#nt-models") && ["ArrowDown","ArrowUp","Home","End"].includes(event.key)) {
      event.preventDefault();const buttons = [...sh.querySelectorAll("[data-new-model]")], current = buttons.indexOf(document.activeElement);
      const next = event.key === "Home" ? 0 : event.key === "End" ? buttons.length-1 : Math.max(0,Math.min(buttons.length-1,current+(event.key === "ArrowDown" ? 1 : -1)));buttons[next]?.focus({preventScroll:true});buttons[next]?.scrollIntoView({block:"nearest"});
    }
  };
  sh.onclick = async event => {
    if (event.target.closest("[data-new-close]")) {closeSheet();return;}
    if (d.starting) return;
    if (event.target.closest("#nt-model")) {modelOpen = !modelOpen;projectOpen = false;toggleMenu("model",modelOpen);toggleMenu("project",false);if (modelOpen) sh.querySelector('[role="option"][aria-selected="true"]')?.focus({preventScroll:true});return;}
    if (event.target.closest("[data-project-toggle]")) {projectOpen = !projectOpen;modelOpen = false;toggleMenu("project",projectOpen);toggleMenu("model",false);return;}
    if (event.target.closest("[data-model-retry]")) {const target = conns.get(d.sid);if (target) {if (!target.models?.models?.length) target.models = null;target.send({op:"models",provider});draw();}return;}
    const model = event.target.closest("[data-new-model]");
    if (model) {d.model = model.dataset.newModel;modelOpen = false;draw();sh.querySelector("#nt-model").focus({preventScroll:true});return;}
    const modeBtn = event.target.closest("[data-new-mode]");
    if (modeBtn) {d.mode = modeBtn.dataset.newMode;draw();return;}
    const project = event.target.closest("[data-cwd]");
    if (project) {d.cwd = project.dataset.cwd;projectOpen = false;draw();return;}
    const remove = event.target.closest("[data-rm]");
    if (remove) {removeAttachment(S.newAttach,+remove.dataset.rm);redrawAttachments("new");return;}
    const action = event.target.closest("[data-x]")?.dataset.x;
    if (action === "img") {pickAttachments("new");return;}
    if (action !== "start") return;
    if (!d.text.trim()) {toast("Write A Prompt First");sh.querySelector("#nt-text").focus();return;}
    if (S.newAttach.some(attachment => attachment.uploading)) {toast("Waiting For Files To Upload");return;}
    const target = {server:d.sid,provider,list:S.newAttach};
    const c = conns.get(target.server);
    d.starting = true;draw();buzz();
    const payload = {
      op:"new",provider,cwd:d.cwd,text:d.text.trim(),
      model:d.model || undefined,
      effort:d.effort || undefined,
      images:target.list.filter(file => file.kind !== "file").map(file => file.path),
      files:target.list.filter(file => file.kind === "file").map(file => file.path)
    };
    if (provider === "antigravity" || provider === "grok") payload.permissionMode = d.mode || c?.models?.default?.mode || "default";
    const result = await c.call(payload,60000);
    d.starting = false;
    if (!result.ok) {toast(nice(result.msg));if(S.newDraft===d && S.sheet==='new' && S.provider===provider)S.redraw?.();return;}
    for (const [id,batch] of attachmentBatches) if (batch.list === target.list) attachmentBatches.delete(id);
    persistAttachmentBatches();store.set("attachment-return",null);
    const current = S.newDraft === d && S.provider === provider && S.sheet === "new";
    if (S.newDraft === d) {S.newDraft = null;if (S.newAttach === target.list) S.newAttach = [];}
    if (!current) {toast("Chat Created",() => {selectProvider(provider,false);openThread(target.server,result.thread,true);});return;}
    closeSheet(true);history.replaceState({view:"thread",sid:target.server,id:result.thread,provider},"");openThread(target.server,result.thread,false);
  };
};
