const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),vm=require('node:vm');
function fixture(){
 const target={id:'target',url:'https://100.64.0.2:8787',alts:['http://192.168.1.20:8787']};
 const requests=[],helper={s:{id:'helper'},ok:true,state:'on',recoveryPeers:[{id:'m5p',name:'Other Mac',urls:['http://100.64.0.2:8787']}],call:(request,timeout)=>new Promise(resolve=>requests.push({request,timeout,resolve}))};
 let connects=0;
 const c=vm.createContext({URL,Map,Set,S:{servers:[target],sheet:'connection',connectionID:'target',redraw(){}},conns:new Map([['helper',helper],['target',{s:target,ok:false,state:'off',connect(){connects++}}]]),serverName:()=> 'Helper Mac',toast(){}});
 const source=fs.readFileSync('web/app.js','utf8');vm.runInContext(source.slice(source.indexOf('const recoveryJobs='),source.indexOf('function computerStatus(')),c);
 return {c,target,helper,requests,connects:()=>connects};
}
test('recovery helpers match the paired computer across HTTP/TLS and ignore offline helpers',()=>{
 const f=fixture();assert.equal(f.c.recoveryChoices(f.target).length,1);
 f.helper.ok=false;assert.equal(f.c.recoveryChoices(f.target).length,0);f.helper.ok=true;
 assert.equal(f.c.recoveryChoices({...f.target,url:'https://other:8787',alts:[]}).length,0);
});
test('recovery submits only the allowlisted ID once and reconnects after success',async()=>{
 const f=fixture();const pending=f.c.recoverComputer('target','helper','m5p');await f.c.recoverComputer('target','helper','m5p');
 assert.equal(f.requests.length,1);assert.deepEqual(JSON.parse(JSON.stringify(f.requests[0].request)),{op:'recover',id:'m5p'});assert.equal(f.requests[0].timeout,55000);
 f.requests[0].resolve({ok:true});await pending;assert.equal(f.connects(),1);
});
test('a removed or re-paired computer cannot be reconnected by a late recovery result',async()=>{
 const f=fixture();const pending=f.c.recoverComputer('target','helper','m5p');f.c.S.servers=[{...f.target,token:'changed'}];f.requests[0].resolve({ok:true});await pending;assert.equal(f.connects(),0);
 assert.equal(vm.runInContext('recoveryJobs.size',f.c),0);
});
