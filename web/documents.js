"use strict";

let documentRequest = null;
window.disposeDocumentViewer = () => { documentRequest?.abort(); documentRequest=null; };
window.openMarkdownDocument = (server,path) => openSheet('document',{server,path});
window.resolveDocumentPath = (directory,path) => {
  path=path.trim().replace(/^<|>$/g,'');
  if (/^(?:[a-z][a-z0-9+.-]*:|[\\/]|#)/i.test(path)) return path;
  const parts=[];
  for(const part of (directory+'/'+path).replace(/\\/g,'/').split('/')) {
    if(part==='..') {if(parts.length>1)parts.pop();}
    else if(part!=='.')parts.push(part);
  }
  return parts.join('/');
};
async function readMarkdownDocument(response, signal) {
  if(!response.ok)throw Error('Could not open this file. Check the computer connection and try again.');
  const limit=2*1024*1024;
  if(Number(response.headers.get('Content-Length'))>limit)throw Error('This file is too large to preview. Use Download to save it.');
  const reader=response.body.getReader(),parts=[];let size=0;
  try {
    for(;;) {
      const {value,done}=await reader.read();if(done)break;
      size+=value.length;
      if(size>limit)throw Error('This file is too large to preview. Use Download to save it.');
      if(signal.aborted)throw Error('Cancelled');
      parts.push(value);
    }
  } finally {await reader.cancel().catch(()=>{});}
  const data=new Uint8Array(size);let offset=0;
  for(const part of parts){data.set(part,offset);offset+=part.length;}
  return new TextDecoder('utf-8',{fatal:true}).decode(data);
}
SHEETS.document = (sh,opts) => {
  const {server,path}=opts,connection=conns.get(server);
  const controller=new AbortController();documentRequest=controller;
  const directory=path.replace(/[\\/][^\\/]*$/,''),name=base(path);
  const download=fileHTML(path,name,true,server).replace('data-file-path=','data-file-download data-file-path=');
  sh.innerHTML=`<header class="document-head"><button class="icon-btn" data-document-close aria-label="Close document">${svg('<path d="M15 5l-7 7 7 7"/>')}</button><div class="head-text"><h2 class="h-title">${esc(name)}</h2><div class="h-sub">${esc(serverName(server))}</div></div>${download}</header><main class="document-body scroll"><div class="empty">${loadingHTML('Opening document')}</div></main>`;
  const body=sh.querySelector('.document-body');
  sh.onclick=event=>{
    if(event.target.closest('[data-document-close]'))closeSheet();
    else if(event.target.closest('[data-document-retry]'))openMarkdownDocument(server,path);
    else transcriptClick(event);
  };
  (async()=>{
    try {
      if(!connection)throw Error('Reconnect to this computer to open the file.');
      const local=/^(\/|[a-z]:[\\/])/i.test(path);
      if(!local && !/^https?:\/\//i.test(path))throw Error('This file address is not supported.');
      const response=await fetch(local ? connection.base+'/file?path='+encodeURIComponent(path) : path,{headers:local ? {Authorization:'Bearer '+connection.s.token} : {},signal:controller.signal});
      const text=await readMarkdownDocument(response,controller.signal);
      if(controller.signal.aborted || documentRequest!==controller)return;
      body.innerHTML=`<article class="md document-prose">${md(text,{server,directory:local ? directory : ''})}</article>`;
      queueImages(body);
    } catch(error) {
      if(controller.signal.aborted || documentRequest!==controller)return;
      body.innerHTML=`<div class="empty"><p>${esc(error instanceof TypeError ? 'Could not open this file. Check the computer connection and try again.' : error.message)}</p><button class="btn" data-document-retry>Try again</button></div>`;
    }
  })();
};
