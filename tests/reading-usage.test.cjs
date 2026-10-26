const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');

test('only reader gestures change following, including a drag close to the bottom',()=>{
 const source=fs.readFileSync('web/app.js','utf8'),context=vm.createContext({Date});
 vm.runInContext(source.slice(source.indexOf('function bindTranscriptScroll('),source.indexOf('function captureReadingPlace(')),context);
 const handlers={},root={scrollHeight:1000,scrollTop:700,clientHeight:300,addEventListener:(event,handler)=>handlers[event]=handler};
 const state={stick:true};context.bindTranscriptScroll(root,state);
 const event={clientY:100,target:{closest:()=>null}};
 handlers.pointerdown(event);assert.equal(state.stick,false);
 handlers.pointermove({...event,clientY:105});root.scrollTop=695;handlers.scroll();handlers.pointercancel();
 assert.equal(state.stick,false,'even five pixels away is a deliberate reading position');
 root.scrollTop=700;handlers.scroll();assert.equal(state.stick,false,"layout reaching the bottom cannot override an upward reading gesture");
 // A resize during the same gesture may reach the end. After the gesture's
 // intent expires, layout scrolls must never resume following.
 state.stick=false;context.Date={now:()=>Date.now()+5000};handlers.scroll();assert.equal(state.stick,false);
 context.Date=Date;handlers.wheel({...event,deltaY:100});handlers.scroll();assert.equal(state.stick,true);
 handlers.pointerdown({...event,target:{closest:()=>({})}});handlers.pointerup();handlers.scroll();assert.equal(state.stick,false,'reading a nested command stays put');
});
function usageFixture(){
 const requests=[],context=vm.createContext({Map,Date,Number,Math,Intl,AbortSignal,Error,
  conns:new Map([['a',{ok:true,base:'http://a',s:{token:'a'}}],['b',{ok:true,base:'http://b',s:{token:'b'}}]]),
  S:{sheet:null},SHEETS:{},$:()=>({}),setInterval(){},document:{hidden:false},
  fetch:(url)=>new Promise(resolve=>requests.push({url,resolve})),
 });
 vm.runInContext(fs.readFileSync('web/usage.js','utf8'),context);
 return {context,requests};
}
test('weekly meter selects the overall provider quota and strict requested color thresholds',()=>{
 const {context:c}=usageFixture();
 for(const [used,tone] of [[0,''],[75,''],[75.1,'caution'],[90,'caution'],[90.1,'critical'],[100,'critical']]){
  const result=c.weeklyQuota({providers:[{provider:'claude',windows:[{label:'Opus Weekly',used:99},{label:'Weekly',used}]},{provider:'codex',windows:[{label:'Weekly',used:13}]}]},'claude');
  assert.equal(result.used,used);assert.equal(result.tone,tone);assert.equal(result.remaining,100-used);
 }
 for(const used of [null,undefined,'80',NaN,Infinity])assert.equal(c.weeklyQuota({providers:[{provider:'codex',windows:[{label:'Weekly',used}]}]},'codex').window,undefined);
 assert.equal(c.weeklyQuota({providers:[{provider:'claude',windows:[{label:'Opus Weekly',used:80}]}]},'claude').window,undefined);
});
test('context and full usage share an in-flight request and keep computers isolated',async()=>{
 const {context:c,requests}=usageFixture();
 const first=c.readUsage('a'),same=c.readUsage('a'),other=c.readUsage('b');
 assert.equal(requests.length,2);
 requests[1].resolve({ok:true,json:async()=>({providers:[{provider:'claude'}]})});
 assert.equal((await other).report.providers[0].provider,'claude');
 requests[0].resolve({ok:true,json:async()=>({providers:[{provider:'codex'}]})});
 assert.equal((await first).report.providers[0].provider,'codex');assert.equal((await same).report.providers[0].provider,'codex');
 await c.readUsage('a');assert.equal(requests.length,2,'fresh cache is shared');
});
test('closing or switching a context panel cannot paint an old quota response into a new panel',async()=>{
 const {context:c,requests}=usageFixture();
 let button={},old={innerHTML:'',querySelector:()=>button},current=old;
 const sheet={querySelector:()=>current};Object.assign(c,{esc:String,serverName:()=> 'Computer',I:{right:'>'},openUsage(){}});
 c.S={sheet:'context',sid:'a',provider:'codex'};
 const pending=c.showContextWeekly(sheet,'a','codex');
 current={innerHTML:'New panel'};
 requests[0].resolve({ok:true,json:async()=>({providers:[{provider:'codex',windows:[{label:'Weekly',used:80}]}]})});
 await pending;assert.equal(current.innerHTML,'New panel');assert.match(old.innerHTML,/Reading Weekly Usage/);
});

test('weekly context requests only its provider while a full reading is pending',async()=>{
 const {context:c,requests}=usageFixture();
 const full=c.readUsage('a'),weekly=c.readUsage('a',false,'codex');
 assert.equal(requests.length,2);
 assert.match(requests[1].url,/provider=codex/);
 requests[1].resolve({ok:true,json:async()=>({providers:[{provider:'codex',windows:[{label:'Weekly',used:30}]}]})});
 assert.equal((await weekly).report.providers[0].windows[0].used,30);
 requests[0].resolve({ok:true,json:async()=>({providers:[{provider:'codex',localLoading:true,windows:[]},{provider:'claude',quotaLoading:true,windows:[]}]})});
 await full;
 assert.equal(c.contextUsage('a','codex').report.providers[0].windows[0].used,30);
});
