const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),vm=require('node:vm');

function fixture(){
 const c=vm.createContext({Map,Set,S:{},scheduleRender(){},threadRow:t=>`<button data-id="${t.sid}:${t.id}">${t.title}</button>`});c.window=c;
 vm.runInContext(fs.readFileSync('web/organization.js','utf8'),c);return c;
}
test('child search results retain their parent and never attach across computers',()=>{
 const c=fixture(),parent={sid:'one',id:'parent',title:'Parent',recency:1},child={sid:'one',id:'child',title:'Child',parentId:'parent',recency:2},other={sid:'two',id:'child',title:'Other Mac',parentId:'parent',recency:3};
 const html=c.sessionTreeRows([child,other],[parent,child,other]);
 assert.equal((html.match(/data-id="one:parent"/g)||[]).length,1);
 assert.equal((html.match(/data-id="one:child"/g)||[]).length,1);
 assert.ok(html.indexOf('one:parent')<html.indexOf('one:child'));
 assert.equal((html.match(/session-tree-children/g)||[]).length,1);
 assert.match(html,/two:child/);
});
test('malformed cyclic parentage cannot duplicate or hide chats',()=>{
 const c=fixture(),a={sid:'one',id:'a',title:'A',parentId:'b',recency:1},b={sid:'one',id:'b',title:'B',parentId:'a',recency:2};
 const html=c.sessionTreeRows([a,b],[a,b]);
 assert.equal((html.match(/data-id="one:a"/g)||[]).length,1);
 assert.equal((html.match(/data-id="one:b"/g)||[]).length,1);
});
