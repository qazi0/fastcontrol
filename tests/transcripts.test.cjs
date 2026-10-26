const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),vm=require('node:vm');

function fixture(){
 const replies=[],timers=new Map();let next=0,open;
 const c=vm.createContext({onmessage:null,Map,Date,JSON,setTimeout:fn=>{timers.set(++next,fn);return next;},clearTimeout:id=>timers.delete(id),postMessage:r=>replies.push(r),indexedDB:{open:()=>open={}}});
 vm.runInContext(fs.readFileSync('web/transcript-worker.js','utf8'),c);
 const send=(type,extra={})=>c.onmessage({data:{type,server:'mac',provider:'codex',id:'chat',...extra}});
 const event=m=>send('event',{raw:JSON.stringify({id:'chat',...m})});
 return {c,replies,timers,send,event,get open(){return open;}};
}
test('cached windows retain live edits but never replay pending approvals or queued prompts',async()=>{
 const f=fixture();
 f.event({t:'open',meta:{title:'Chat',requests:[{id:'approval'}],queued:[{id:'prompt'}]},turns:[{key:'one',items:[{text:'old'}]}]});
 f.event({t:'item',key:'one',idx:0,item:{text:'new'}});
 f.event({t:'head',key:'one',status:'completed',durationMs:12});
 f.send('read',{id:9,thread:'chat'});
 const saved=JSON.parse(f.replies[0].payload);
 assert.equal(saved.turns[0].items[0].text,'new');assert.equal(saved.turns[0].status,'completed');
 assert.equal(saved.meta.requests.length,0);assert.equal(saved.meta.queued.length,0);
 f.send('forget');f.send('read',{id:10,thread:'chat'});
 assert.equal(f.replies.length,1,'forgotten in-memory data cannot be returned');
 f.open.onerror();await new Promise(setImmediate);
 assert.equal(f.replies[1].payload,null);
});
test('late storage initialization cannot publish a forgotten computer snapshot',async()=>{
 const f=fixture();let writes=0;
 f.event({t:'open',meta:{},turns:[]});f.send('flush');f.send('forget');
 const tx={objectStore:()=>({getAll:()=>({}),put:()=>writes++})};
 f.open.result={transaction:()=>tx};f.open.onsuccess();await new Promise(setImmediate);
 assert.equal(writes,0);
});
test('cache eviction reads and removes records in the same write transaction',()=>{
 const f=fixture(),transactions=[],deleted=[];
 const request={result:[{key:'expired',bytes:1,at:0}]};
 const tx={objectStore:()=>({getAll:()=>request,delete:key=>deleted.push(key)})};
 f.c.target={transaction:(names,mode)=>{transactions.push({names,mode});return tx;}};
 vm.runInContext('prune(target)',f.c);request.onsuccess();
 assert.equal(transactions.length,1);assert.equal(transactions[0].mode,'readwrite');
 assert.deepEqual(deleted,['expired','expired']);
});
