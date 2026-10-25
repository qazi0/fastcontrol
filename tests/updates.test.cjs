const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');

function fixture() {
 const calls=[], context=vm.createContext({
  Native:{checkAppUpdates:manual=>calls.push(['check',manual]),appUpdateState:()=>JSON.stringify({phase:'current',installed:'0.2.6'}),installAppUpdate:()=>calls.push(['install'])},
  document:{hidden:false,addEventListener(){}},S:{sheet:null,meta:{}},voiceState:{phase:'idle'},input:{value:''},
  openSheet:name=>calls.push(['sheet',name]),pendingAsk:()=>null,setTimeout(){},setInterval(){},window:{},
 });
 const app=fs.readFileSync('web/app.js','utf8');
 vm.runInContext(app.slice(app.indexOf('let appUpdate ='),app.indexOf('adoptPairingLink();',app.indexOf('let appUpdate ='))),context);
 context.event=state=>context.window.onAppUpdate(state);
 return {context,calls};
}
const available={phase:'available',version:'0.2.7',automatic:true,offer:true};
test('automatic update offers wait for recording, typing, active turns and other dialogs',()=>{
 for(const blocked of ['voice','typing','turn','dialog','hidden']){
  const {context:c,calls}=fixture();
  if(blocked==='voice')c.voiceState.phase='recording';
  if(blocked==='typing')c.input.value='Unsent message';
  if(blocked==='turn')c.S.meta.running=true;
  if(blocked==='dialog')c.S.sheet='model';
  if(blocked==='hidden')c.document.hidden=true;
  c.event(available);assert.deepEqual(calls,[],blocked);
  c.voiceState.phase='idle';c.input.value='';c.S.meta.running=false;c.S.sheet=null;c.document.hidden=false;
  c.considerAppUpdate();c.considerAppUpdate();
  assert.deepEqual(calls,[['sheet','update']],blocked);
 }
});
test('manual results and deferred updates do not trigger an unsolicited modal',()=>{
 for(const state of [{...available,automatic:false},{...available,offer:false},{...available,phase:'error'},{...available,phase:'current'}]){
  const {context,calls}=fixture();context.event(state);assert.deepEqual(calls,[]);
 }
});
test('only a requested download completion can request the Android installer',()=>{
 const {context:c,calls}=fixture();
 c.event({phase:'ready'});assert.deepEqual(calls,[]);
 vm.runInContext('appUpdateInstallWanted=true',c);c.document.hidden=true;c.event({phase:'ready'});assert.deepEqual(calls,[]);
 c.document.hidden=false;c.event({phase:'ready'});c.event({phase:'ready'});assert.deepEqual(calls,[['install']]);
 vm.runInContext('appUpdateInstallWanted=true',c);c.event({phase:'error'});c.event({phase:'ready'});assert.deepEqual(calls,[['install']]);
});
test('unsent chat text is saved before handing off to the installer',()=>{
 const {context:c,calls}=fixture();c.S.id='chat';c.input.value='Keep this unsent message';c.voiceDraftKey=()=> 'chat-draft';
 c.store={get:()=>null,set:(key,draft)=>calls.push(['saved',key,draft.text])};
 c.requestAppUpdateInstall();assert.deepEqual(calls,[['saved','chat-draft','Keep this unsent message'],['install']]);
});
