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
 const context=vm.createContext({URL,Set,S:{servers:[old]},saveServers(){},syncConns(){},sid:()=> 'new',hostOf:()=> 'host'});
 vm.runInContext(source.slice(source.indexOf('function sameRelayPairing('),source.indexOf('const serverName =')),context);
 const paired=context.addServer({url:'http://192.168.1.20:8787',token:'shared',name:'Default host name',alts:{bad:true}});
 assert.equal(context.S.servers.length,1);assert.equal(paired.id,'keep');assert.equal(paired.name,'My M5P');assert.equal(paired.custom,true);
});
test('QR rescan deduplicates via incoming alts overlap and refreshes offline connections',()=>{
 const old={id:'win1',url:'http://100.64.0.9:8787',token:'tok-win',name:'My Windows PC',custom:true,alts:[]};
 let reconnectCalls=0;
 const fakeConn={s:old,ok:false,state:'off',paused:true,backoff:5000,connect(wait){reconnectCalls++;this.lastWait=wait;}};
 const conns=new Map([['win1',fakeConn]]);
 const context=vm.createContext({URL,Set,Map,JSON,S:{servers:[old]},conns,saveServers(){},syncConns(){},sid:()=> 'dup',hostOf:()=> 'host'});
 vm.runInContext(source.slice(source.indexOf('function sameRelayPairing('),source.indexOf('const serverName =')),context);
 const pin='c'.repeat(64);
 const paired=context.addServer({url:'https://192.168.1.50:8787',token:'tok-win',name:'DESKTOP-123',pin,alts:['https://100.64.0.9:8787','https://desktop-123.local:8787']});
 assert.equal(context.S.servers.length,1);
 assert.equal(paired.id,'win1');
 assert.equal(paired.url,'https://192.168.1.50:8787');
 assert.equal(paired.pin,pin);
 assert.equal(paired.name,'My Windows PC');
 assert.deepEqual(Array.from(paired.alts),['https://100.64.0.9:8787','https://desktop-123.local:8787']);
 assert.equal(reconnectCalls,1);
 assert.equal(fakeConn.paused,false);
 assert.equal(fakeConn.backoff,300);
 // DHCP IP change still deduplicates case-insensitively via shared .local hostname in alts.
 const oldHttp={id:'m1',url:'http://192.168.1.10:8787',token:'tok-m1',alts:['http://M5P.local:8787']};
 assert.equal(c.sameRelayPairing(oldHttp,'http://192.168.1.99:8787','tok-m1','',['http://m5p.local:8787']),true);
});
