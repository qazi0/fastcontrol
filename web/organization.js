"use strict";

window.nativeGroupsFor = () => {
  const groups=[];
  for(const [server,c] of conns) {
    if(S.machine!=='all' && S.machine!==server)continue;
    for(const group of c.groups || [])groups.push({...group,server});
  }
  return groups.sort((a,b)=>(a.order||0)-(b.order||0) || a.name.localeCompare(b.name));
};
window.nativeGroupRow = group => `<button class="row proj" data-native-group="${esc(group.id)}" data-server="${esc(group.server)}">${I.folder}<span class="main"><span class="t">${esc(group.name)}</span><span class="s">${esc(serverName(group.server))}</span></span>${I.chev}</button>`;

// Connect indexed ancestors before filtering so a matching child keeps its
// parent context. Subagents use their separate read-only transcript viewer.
window.sessionTreeRows = (selected,universe,options={}) => {
  const key=t=>t.sid+':'+t.id,byKey=new Map();
  for(const t of [...selected,...universe])if(!byKey.has(key(t)))byKey.set(key(t),t);
  const wanted=new Set(selected.map(key));
  for(const t of selected) {
    let parent=t.parentId;const seen=new Set([key(t)]);
    for(let i=0;parent && i<32;i++) {
      const id=t.sid+':'+parent,node=byKey.get(id);
      if(!node || seen.has(id))break;
      seen.add(id);wanted.add(id);parent=node.parentId;
    }
  }
  const children=new Map();
  for(const id of wanted) {
    const t=byKey.get(id),parent=t.sid+':'+t.parentId;
    if(t.parentId && wanted.has(parent) && parent!==id) {
      if(!children.has(parent))children.set(parent,[]);children.get(parent).push(id);
    }
  }
  for(const list of children.values())list.sort((a,b)=>byKey.get(b).recency-byKey.get(a).recency);
  const emitted=new Set(),draw=(id,depth=0)=>{
    if(emitted.has(id))return '';emitted.add(id);
    const nested=depth<32 ? (children.get(id)||[]).map(child=>draw(child,depth+1)).join('') : '';
    return `<div class="session-tree-node">${threadRow(byKey.get(id),options)}${nested ? `<div class="${depth<6?'session-tree-children':'session-tree-flat'}" role="group" aria-label="Child chats">${nested}</div>` : ''}</div>`;
  };
  let html='';
  for(const t of selected) {
    let id=key(t);const seen=new Set();
    while(byKey.get(id)?.parentId && wanted.has(t.sid+':'+byKey.get(id).parentId)) {
      if(seen.has(id))break;seen.add(id);id=t.sid+':'+byKey.get(id).parentId;
    }
    html+=draw(id);
  }
  for(const id of wanted)html+=draw(id);
  return html;
};

let groupGeneration=0;
window.openNativeGroup = async(server,id,push) => {
  const c=conns.get(server),group=c?.groups?.find(group=>group.id===id);
  if(!c || !group){toast('This group is no longer available');return;}
  const selection={server,id,name:group.name,rows:null,error:''};
  S.group=selection;S.cwd=group.roots?.[0] || '';
  if(push)history.pushState({view:'project',group:{server,id},provider:S.provider},'');
  $('p-search').value='';show('project');renderNativeProject();$('project-list').scrollTop=0;
  const generation=++groupGeneration,provider=S.provider;
  const result=await c.call({op:'group',id,provider},15000);
  if(generation!==groupGeneration || S.group!==selection || S.provider!==provider)return;
  if(result.ok) {
    selection.rows=(result.threads || []).filter(t=>!t.subagent).map(t=>({...t,sid:server}));
    if(!S.cwd)S.cwd=selection.rows[0]?.cwd || '';
  } else selection.error=nice(result.msg);
  renderNativeProject();
};
window.renderNativeProject = () => {
  const group=S.group;if(!group || S.view!=='project')return;
  const source=allThreads(),latest=new Map(source.filter(t=>t.sid===group.server).map(t=>[t.id,t]));
  const indexed=group.rows ? group.rows.map(t=>latest.get(t.id) || t) : source.filter(t=>t.sid===group.server);
  const members=new Set(indexed.filter(t=>t.groupIds?.includes(group.id)).map(t=>t.id));
  for(let i=0;i<32;i++) {
    let changed=false;
    for(const t of indexed)if(!members.has(t.id) && members.has(t.parentId)){members.add(t.id);changed=true;}
    if(!changed)break;
  }
  const rows=indexed.filter(t=>members.has(t.id));
  const q=$('p-search').value.toLocaleLowerCase().trim();
  const selected=q ? rows.filter(t=>`${t.displayTitle||t.title} ${t.preview} ${t.lastMsg||''}`.toLocaleLowerCase().includes(q)) : rows;
  $('p-title').textContent=group.name;$('p-sub').textContent=serverName(group.server);
  const list=$('project-list'),top=list.scrollTop;
  list.innerHTML=`${group.rows===null && !group.error ? `<div class="group-loading">${loadingHTML('Loading chats')}</div>` : ''}${selected.length ? `<div class="sec">Chats<span class="n">${selected.length}</span></div><div class="group">${sessionTreeRows(selected,[...indexed,...source],{project:false})}</div>` : group.rows!==null ? '<div class="empty"><b>No local chats in this group</b></div>' : ''}${group.error ? `<div class="empty"><p>${esc(group.error)}</p><button class="btn" data-group-retry>Try again</button></div>` : ''}`;
  list.querySelector('[data-group-retry]')?.addEventListener('click',()=>openNativeGroup(group.server,group.id,false));
  list.scrollTop=top;
};
scheduleRender();
