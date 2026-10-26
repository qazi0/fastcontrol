const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),vm=require('node:vm');
function fixture(){
 const requests=[],intervals=[];
 const c=vm.createContext({Map,Date,JSON,base:s=>s.split('/').pop(),S:{provider:'claude',view:'home'},SHEETS:{},$:()=>({}),document:{hidden:false,addEventListener(){}},setInterval:fn=>intervals.push(fn),requestAnimationFrame:()=>1,
  conns:new Map([['mac',{ok:true,s:{id:'mac'},call:async r=>{requests.push(r);return {ok:true,agents:[{id:r.provider+'-worker'}]};}}]])});c.window=c;
 vm.runInContext(fs.readFileSync('web/agents.js','utf8'),c);return {c,requests,intervals};
}
test('agent caches and requests remain scoped to the selected provider',async()=>{
 const {c,requests,intervals}=fixture();
 const claude=await c.readAgents('mac','same-parent',false,'claude'),codex=await c.readAgents('mac','same-parent',false,'codex');
 assert.equal(claude.agents[0].id,'claude-worker');assert.equal(codex.agents[0].id,'codex-worker');assert.deepEqual(requests.map(r=>r.provider),['claude','codex']);
 await c.readAgents('mac','same-parent',false,'claude');assert.equal(requests.length,2);
 intervals.forEach(fn=>fn());
});
test('a late message from another provider cannot replace the open agent transcript',()=>{
 const {c}=fixture();vm.runInContext('agentView={server:"mac",id:"worker",provider:"claude",meta:null}',c);
 assert.equal(c.onAgentMessage({s:{id:'mac'}},{id:'worker',t:'meta',meta:{title:'Wrong'}},'codex'),false);
 assert.equal(c.onAgentMessage({s:{id:'mac'}},{id:'worker',t:'meta',meta:{title:'Right'}},'claude'),true);
 assert.equal(vm.runInContext('agentView.meta.title',c),'Right');
 assert.equal(vm.runInContext('agentStatus({agentState:"saved"})',c),'Saved');assert.equal(vm.runInContext('agentStatus({agentState:"failed"})',c),'Failed');
});
