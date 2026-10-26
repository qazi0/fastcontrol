const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),vm=require('node:vm');
const source=fs.readFileSync('web/app.js','utf8'),c=vm.createContext({URL});
vm.runInContext(source.slice(source.indexOf('function sameRelayPairing('),source.indexOf('function addServer(')),c);
test('secure pairings use certificate identity and known legacy addresses',()=>{
 const old={url:'http://192.168.1.102:8787',token:'shared',alts:['http://100.125.53.124:8787']};
 assert.equal(c.sameRelayPairing(old,'https://192.168.1.102:8787','shared','a'.repeat(64)),true);
 assert.equal(c.sameRelayPairing(old,'https://100.125.53.124:8787','shared','a'.repeat(64)),true);
 assert.equal(c.sameRelayPairing(old,'https://192.168.1.101:8787','shared','a'.repeat(64)),false,'a copied credential cannot merge another Mac');
 assert.equal(c.sameRelayPairing({...old,pin:'a'.repeat(64)},'https://another-address:8787','new-token','a'.repeat(64)),true);
 assert.equal(c.sameRelayPairing({...old,pin:'a'.repeat(64)},'https://another-address:8787','shared','b'.repeat(64)),false);
});
