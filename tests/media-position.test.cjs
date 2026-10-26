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
