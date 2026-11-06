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
test('QR alternate routes reject credentials and HTTP downgrades',()=>{
 vm.runInContext(source.slice(source.indexOf('function pairingAlternates('),source.indexOf('function addServer(')),c);
 const result=c.pairingAlternates('https://192.168.1.20:8787',['https://100.64.0.2:8787','http://192.168.1.20:8787','https://user:password@example.com','https://example.com?t=secret','https://100.64.0.2:8787']);
 assert.deepEqual(Array.from(result),['https://100.64.0.2:8787']);
});

test('legacy QR rescans match known routes without merging an unrelated computer',()=>{
 const old={url:'http://100.64.0.2:8787',token:'shared',alts:['http://192.168.1.20:8787']};
 assert.equal(c.sameRelayPairing(old,'http://192.168.1.20:8787','shared',''),true);
 assert.equal(c.sameRelayPairing(old,'http://192.168.1.21:8787','shared',''),false);
 assert.equal(c.sameRelayPairing({...old,pin:'a'.repeat(64)},'http://192.168.1.20:8787','shared',''),false);
});
test('QR rescans retain a user-defined computer name and ID',()=>{
 const old={id:'keep',url:'http://100.64.0.2:8787',token:'shared',name:'My M5P',custom:true,alts:['http://192.168.1.20:8787']};
 const context=vm.createContext({URL,S:{servers:[old]},saveServers(){},syncConns(){},sid:()=> 'new',hostOf:()=> 'host'});
 vm.runInContext(source.slice(source.indexOf('function sameRelayPairing('),source.indexOf('const serverName =')),context);
 const paired=context.addServer({url:'http://192.168.1.20:8787',token:'shared',name:'Default host name',alts:{bad:true}});
 assert.equal(context.S.servers.length,1);assert.equal(paired.id,'keep');assert.equal(paired.name,'My M5P');assert.equal(paired.custom,true);
});
