"use strict";

// Parsing, snapshot assembly and persistence run off the transcript UI thread.
let transcriptWorker,transcriptWorkerStart,cacheSequence=0;
const cacheReads=new Map();
function startTranscriptWorker(){
 if(transcriptWorkerStart)return transcriptWorkerStart;
 transcriptWorkerStart=(async()=>{
  if(!window.Worker || !window.indexedDB)return null;
  try{
   const response=await fetch('transcript-worker.js');if(!response.ok)return null;
   const url=URL.createObjectURL(new Blob([await response.text()],{type:'text/javascript'}));
   const worker=new Worker(url);URL.revokeObjectURL(url);
   worker.onmessage=event=>{const result=event.data,pending=cacheReads.get(result.id);if(!pending)return;cacheReads.delete(result.id);clearTimeout(pending.timer);try{pending.resolve(result.payload ? JSON.parse(result.payload) : null);}catch{pending.resolve(null);}};
   worker.onerror=()=>{for(const pending of cacheReads.values()){clearTimeout(pending.timer);pending.resolve(null);}cacheReads.clear();worker.terminate();transcriptWorker=null;transcriptWorkerStart=null;};
   transcriptWorker=worker;return worker;
  }catch{return null;}
 })();return transcriptWorkerStart;
}
window.cacheTranscriptMessage=(server,provider,message,raw)=>{
 if(!['open','meta','turn','head','item'].includes(message.t))return;
 const post=worker=>worker?.postMessage({type:'event',server,provider,id:message.id,raw:raw || JSON.stringify(message)});
 if(transcriptWorker)post(transcriptWorker);else startTranscriptWorker().then(post);
};
window.cachedChatTranscript=async(server,provider,thread)=>{
 const worker=await startTranscriptWorker();if(!worker)return null;
 return new Promise(resolve=>{const id=++cacheSequence,timer=setTimeout(()=>{cacheReads.delete(id);resolve(null);},2000);cacheReads.set(id,{resolve,timer});worker.postMessage({type:'read',id,server,provider,thread});});
};
window.rememberChatTranscript=()=>{transcriptWorker?.postMessage({type:'flush'});};
window.forgetComputerTranscripts=async server=>{const worker=await startTranscriptWorker();worker?.postMessage({type:'forget',server});};
document.addEventListener('visibilitychange',()=>{if(document.hidden)rememberChatTranscript();});
startTranscriptWorker();
