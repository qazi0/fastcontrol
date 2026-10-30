const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),vm=require('node:vm');
function fixture(){const c=vm.createContext({SHEETS:{},shortModel:s=>String(s||'').replace(/^claude-/,'').replace(/(\d+)-(\d+)$/,'$1.$2').replace(/-/g,' ').replace(/\b\w/g,v=>v.toUpperCase())});vm.runInContext(fs.readFileSync('web/new-chat.js','utf8'),c);return c;}
test('new-chat choices use versioned native names and have one default choice',()=>{
 const c=fixture(),models=[{slug:'default',resolved:'claude-opus-5-5',name:'Default (recommended)'},{slug:'opus',resolved:'claude-opus-5-5',name:'Opus 5.5'},{slug:'fable',resolved:'claude-fable-5-1',name:'Fable 5.1'},{slug:'claude-fable-5',name:'Fable 5'}];
 const choices=c.newChatModelChoices({models:{models,default:{model:'default'}}});
 assert.equal(choices.length,4);assert.equal(choices[0].name,'Default (Opus 5.5)');assert.equal(choices[1].name,'Opus 5.5');assert.equal(choices[2].name,'Fable 5.1');assert.equal(choices[3].name,'Fable 5');
 assert.equal(c.sessionModelLabel({slug:'opus',name:'Opus',resolved:'claude-opus-5-5'}),'Opus 5.5');
});
test('Codex default and explicit models retain their full display names and effort capabilities',()=>{
 const c=fixture(),model={slug:'gpt-6.1-sol',name:'GPT-6.1 Sol',efforts:['low','high','xhigh'],defaultEffort:'high'},connection={models:{models:[model],default:{model:model.slug}}};
 assert.equal(c.newChatModelChoices(connection)[0].name,'Default (GPT-6.1 Sol)');assert.equal(c.newChatModelChoices(connection)[1].name,'GPT-6.1 Sol');assert.equal(c.newChatModel(connection,{model:''}),model);
});
