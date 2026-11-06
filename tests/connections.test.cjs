const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),vm=require('node:vm');
function fixture(){
 const sockets=[],timers=new Map(),intervals=new Map();let next=0;
 class Socket {constructor(url){this.url=url;this.readyState=0;this.sent=[];sockets.push(this);}close(){this.closed=true;this.readyState=3;}send(text){this.sent.push(JSON.parse(text));}open(){this.readyState=1;this.onopen?.();}message(value){this.onmessage?.({data:JSON.stringify(value)});}}
 const c=vm.createContext({Native:null,WebSocket:Socket,Set,Map,Date,JSON,Math,AbortSignal,fetch:async()=>({status:204}),
  setTimeout:(fn,delay)=>{timers.set(++next,{fn,delay});return next;},clearTimeout:id=>timers.delete(id),setInterval:(fn,delay)=>{intervals.set(++next,{fn,delay});return next;},clearInterval:id=>intervals.delete(id),
  store:{get:(key,fallback)=>fallback,set(){}},S:{provider:'codex',view:'home',servers:[]},window:{},document:{querySelectorAll:()=>[]},scheduleRender(){},saveServers(){},mark(){},serverName:()=> 'Mac',
 });
 const source=fs.readFileSync('web/app.js','utf8');vm.runInContext(source.slice(source.indexOf('function computerStatus('),source.indexOf('function syncConns()'))+';globalThis.Conn=Conn',c);
 const connection=new c.Conn({id:'a',url:'http://a:8787',alts:['http://b:8787'],token:'private'});
 return {c,connection,sockets,timers,intervals,fire(delay){const timer=[...timers.entries()].find(([id,t])=>t.delay===delay);assert.ok(timer,'missing timer '+delay);timers.delete(timer[0]);timer[1].fn();}};
}
test('hung address races time out, close all candidates and retry',async()=>{
 const f=fixture();assert.equal(f.sockets.length,2);f.fire(7000);await new Promise(setImmediate);
 assert.ok(f.sockets.every(socket=>socket.closed));assert.equal(f.connection.state,'off');f.fire(300);assert.equal(f.sockets.length,4);
});
test('pause and removal close pending candidates and prevent stale reconnects',async()=>{
 const f=fixture(),oldOpen=f.sockets[0].onopen;f.connection.pause();oldOpen();assert.ok(f.sockets.every(s=>s.closed));assert.equal(f.timers.size,0);assert.equal(f.connection.ws,null);
 f.connection.paused=false;f.connection.connect();f.connection.close();assert.equal(f.timers.size,0);assert.ok(f.sockets.every(s=>s.closed));
});
test('late auth probes cannot overwrite a successful newer connection',async()=>{
 const f=fixture();let finish;f.c.fetch=()=>new Promise(resolve=>finish=resolve);f.fire(7000);f.connection.connect();f.sockets[2].open();f.sockets[2].message({t:'hello',desktop:true,heartbeat:true,addrs:[]});finish({status:401});await Promise.resolve();await Promise.resolve();
 assert.equal(f.connection.state,'on');assert.equal(f.connection.ok,true);assert.ok(f.connection.lastSeen);assert.equal(f.sockets[3].closed,true);
});
test('supported relays receive provider-independent heartbeat and stale sockets reconnect',()=>{
 const f=fixture();f.sockets[0].open();f.sockets[0].message({t:'hello',desktop:true,heartbeat:true,addrs:['http://new:8787']});
 const heartbeat=[...f.intervals.values()][0].fn;heartbeat();assert.equal(f.sockets[0].sent.at(-1).op,'ping');
 f.sockets[0].message({t:'network',addrs:['http://changed:8787']});assert.equal(f.connection.s.alts[0],'http://changed:8787');
 f.connection.lastMessage=Date.now()-31000;heartbeat();assert.equal(f.sockets[0].closed,true);assert.ok(f.sockets.some(s=>s.url.startsWith('ws://changed:8787')));
});
test('old relays are not sent unknown heartbeat operations',()=>{
 const f=fixture();f.sockets[0].open();f.sockets[0].message({t:'hello',desktop:false,addrs:[]});const count=f.sockets[0].sent.length;[...f.intervals.values()][0].fn();assert.equal(f.sockets[0].sent.length,count);assert.equal(f.c.computerStatus(f.connection),'Connected');
});
