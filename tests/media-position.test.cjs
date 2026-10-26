const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');

test('preparing an image update never detaches decoded images from the live transcript',()=>{
 const app=fs.readFileSync('web/app.js','utf8'),context=vm.createContext({Map});
 vm.runInContext(app.slice(app.indexOf('function preserveImages('),app.indexOf('function enterTranscript(')),context);
 const root=()=>({images:[],querySelectorAll(selector){return selector==='.image-preview'?this.images:[];}});
 const live=root(),template=root();
 const button=(parent,path)=>{
  const node={parent,querySelector:()=>({dataset:{server:'host',relayImage:path}}),replaceWith(existing){
   existing.parent.images.splice(existing.parent.images.indexOf(existing),1);
   parent.images[parent.images.indexOf(node)]=existing;existing.parent=parent;
  }};
  parent.images.push(node);return node;
 };
 const first=button(live,'/image.png'),second=button(live,'/image.png');
 const nextFirst=button(template,'/image.png'),nextSecond=button(template,'/image.png');
 context.preserveImages(live,template);
 assert.equal(live.images.length,2);assert.equal(first.parent,live);assert.equal(second.parent,live);
 assert.equal(nextFirst.liveImage,first);assert.equal(nextSecond.liveImage,second);
});

test('detached transcript construction cannot bypass lazy image loading',()=>{
 const source=fs.readFileSync('web/app.js','utf8'),queued=[],unobserved=[];
 const context=vm.createContext({observedImages:new Set(),imageObserver:{observe:img=>queued.push(img),unobserve:img=>unobserved.push(img)}});
 vm.runInContext(source.slice(source.indexOf('function queueImages('),source.indexOf('async function loadRelayImage(')),context);
 const image={isConnected:false,dataset:{}},root={querySelectorAll:()=>image.dataset.queued ? [] : [image]};
 context.queueImages(root);assert.equal(queued.length,0);assert.equal(image.dataset.queued,undefined);
 image.isConnected=true;context.queueImages(root);assert.equal(queued.length,1);
 context.queueImages(root);assert.equal(queued.length,1,'one observer per image');
 image.isConnected=false;context.queueImages(root);assert.equal(unobserved.length,1);
 image.isConnected=true;context.queueImages(root);assert.equal(queued.length,2,'reattached pending image is observed again');
});
