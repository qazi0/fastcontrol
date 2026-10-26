"use strict";
const models=new Map(),epochs=new Map(),timers=new Map();
const MAX_BYTES=20*1024*1024,MAX_WINDOWS=5;
let database;
function db(){
 if(!database)database=new Promise(resolve=>{
  const request=indexedDB.open('fastcontrol-transcripts',2);
  request.onupgradeneeded=()=>{const d=request.result;for(const name of ['windows','index']){if(d.objectStoreNames.contains(name))d.deleteObjectStore(name);d.createObjectStore(name,{keyPath:'key'});}};
  request.onsuccess=()=>resolve(request.result);request.onerror=()=>resolve(null);request.onblocked=()=>resolve(null);
 });return database;
}
const keyFor=(server,provider,id)=>server+':'+provider+':'+id;
function snapshot(model){return JSON.stringify({meta:{...model.meta,requests:[],queued:[]},turns:model.turns,at:model.at});}
async function persist(model){
 const epoch=epochs.get(model.server)||0,d=await db();
 if(!d || epoch!==(epochs.get(model.server)||0) || models.get(model.key)!==model)return;
 try{
  const payload=snapshot(model),bytes=payload.length*2;if(bytes>MAX_BYTES){models.delete(model.key);return;}
  const tx=d.transaction(['windows','index'],'readwrite');
  tx.objectStore('windows').put({key:model.key,server:model.server,at:model.at,payload});
  tx.objectStore('index').put({key:model.key,server:model.server,at:model.at,bytes});tx.oncomplete=()=>prune(d);
 }catch{}
}
function schedule(model){if(timers.has(model.key))return;timers.set(model.key,setTimeout(()=>{timers.delete(model.key);if(models.get(model.key)===model)persist(model);},1500));}
function prune(d){
 const tx=d.transaction(['windows','index'],'readwrite');
 const request=tx.objectStore('index').getAll();request.onsuccess=()=>{
  const rows=request.result.sort((a,b)=>b.at-a.at),remove=[];let bytes=0;
  for(let i=0;i<rows.length;i++){bytes+=rows[i].bytes;if(i>=MAX_WINDOWS || bytes>MAX_BYTES || Date.now()-rows[i].at>7*86400000)remove.push(rows[i].key);}
  if(!remove.length)return;
  for(const key of remove){tx.objectStore('windows').delete(key);tx.objectStore('index').delete(key);}
 };
}
function event(data){
 let m;try{m=JSON.parse(data.raw);}catch{return;}
 const key=keyFor(data.server,data.provider,data.id);let model=models.get(key);
 if(m.t==='open'){
  clearTimeout(timers.get(key));timers.delete(key);
  model={key,server:data.server,meta:m.meta,turns:(m.turns||[]).slice(-15),at:Date.now()};models.delete(key);models.set(key,model);
  while(models.size>3){const old=models.keys().next().value;clearTimeout(timers.get(old));timers.delete(old);models.delete(old);}
 }else{
  if(!model)return;
  if(m.t==='meta')model.meta=m.meta;
  if(m.t==='turn'){const i=model.turns.findIndex(t=>t.key===m.turn.key);if(i<0)model.turns.push(m.turn);else model.turns[i]=m.turn;model.turns=model.turns.slice(-15);}
  if(m.t==='head'){const t=model.turns.find(t=>t.key===m.key);if(t){t.status=m.status;t.durationMs=m.durationMs;t.error=m.error;}}
  if(m.t==='item'){const t=model.turns.find(t=>t.key===m.key);if(t)t.items[m.idx]=m.item;}
  model.at=Date.now();
 }
 schedule(model);
}
async function read(data){
 const key=keyFor(data.server,data.provider,data.thread),epoch=epochs.get(data.server)||0,model=models.get(key);
 if(model){const payload=snapshot(model);postMessage({id:data.id,payload:payload.length*2<=MAX_BYTES?payload:null});return;}
 const d=await db();if(!d){postMessage({id:data.id,payload:null});return;}
 try{const request=d.transaction('windows').objectStore('windows').get(key);request.onsuccess=()=>{const row=request.result;postMessage({id:data.id,payload:epoch===(epochs.get(data.server)||0) && row && Date.now()-row.at<7*86400000?row.payload:null});};request.onerror=()=>postMessage({id:data.id,payload:null});}catch{postMessage({id:data.id,payload:null});}
}
async function forget(server){
 epochs.set(server,(epochs.get(server)||0)+1);
 for(const [key,model] of models)if(model.server===server){clearTimeout(timers.get(key));timers.delete(key);models.delete(key);}
 const d=await db();if(!d)return;
 const tx=d.transaction(['windows','index'],'readwrite');
 const request=tx.objectStore('index').getAll();request.onsuccess=()=>{for(const row of request.result)if(row.server===server){tx.objectStore('windows').delete(row.key);tx.objectStore('index').delete(row.key);}};
}
onmessage=e=>{const data=e.data;if(data.type==='event')event(data);else if(data.type==='read')read(data);else if(data.type==='forget')forget(data.server);else if(data.type==='flush')for(const model of models.values())persist(model);};
