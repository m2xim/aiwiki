import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const data = path.join(root, '.cache', `qa-data-${Date.now()}`);
mkdirSync(data, {recursive:true});
const exe = path.join(root, '.cache', process.platform==='win32'?'qa-wiki.exe':'qa-wiki');
const build = spawnSync('go', ['build','-o',exe,'./cmd/aiwiki'], {cwd:root, env:{...process.env,GOCACHE:path.join(root,'.cache','qa-go')},encoding:'utf8',windowsHide:true});
assert.equal(build.status,0,build.stderr);
const port=18091, base=`http://127.0.0.1:${port}`;
let logs='';
const start=()=>{ const s=spawn(exe,['-addr',`127.0.0.1:${port}`,'-data',data,'-web',path.join(root,'web','dist')],{cwd:root,windowsHide:true,stdio:['ignore','pipe','pipe']});s.stdout.on('data',d=>logs+=d);s.stderr.on('data',d=>logs+=d);return s; };
let server=start();
const stop=()=>new Promise(resolve=>{if(server.exitCode!==null)return resolve();server.once('exit',resolve);server.kill();});
async function ready(){for(let i=0;i<100;i++){if(server.exitCode!==null)throw new Error(logs);try{const r=await fetch(`${base}/api/health`,{signal:AbortSignal.timeout(500)});if(r.ok)return;}catch{}await new Promise(r=>setTimeout(r,100));}throw new Error('Server failed to start: '+logs);}
const results=[];
async function request(route,method='GET',value,expected=200,headers={}){const r=await fetch(base+route,{method,headers:{...(value===undefined?{}:{'Content-Type':'application/json'}),...headers},body:value===undefined?undefined:JSON.stringify(value),signal:AbortSignal.timeout(5000)});const raw=await r.text();assert.equal(r.status,expected,`${method} ${route}: ${raw}`);return raw?JSON.parse(raw):null;}
async function test(name,fn){await fn();results.push(name);console.log('PASS '+name);}
const input=(extra={})=>({space_id:'engineering',parent_id:'',title:'Проверка знаний',summary:'Описание',body:'## Проверка\nУникальноесодержимое\n',kind:'research',status:'active',pinned:false,tags:['пилот','дисплей'],properties:{'repo.url':'https://example.org/a:b',project:'qa'},author:'qa-agent',message:'Начальное исследование',...extra});
let a, child, file;
async function tool(name,args){const r=await request('/mcp','POST',{jsonrpc:'2.0',id:3,method:'tools/call',params:{name,arguments:args}});assert.equal(r.result.isError,false,JSON.stringify(r));return JSON.parse(r.result.content[0].text);}
try {
 await ready();
 await test('UI assets and bootstrap seed',async()=>{const r=await fetch(base);assert.ok((await r.text()).includes('/assets/'));const b=await request('/api/bootstrap?space_id=engineering');assert.equal(b.spaces.length,3);assert.ok(b.instructions.some(x=>x.id==='agent-rules'));});
 await test('Create parent and child with arbitrary metadata',async()=>{a=await request('/api/articles','POST',input(),201);assert.equal(a.revision,1);child=await request('/api/articles','POST',input({title:'Потомок',parent_id:a.id,body:`[Источник](/?article=${a.id}#проверка)`}),201);});
 await test('FTS body-only Unicode search and literal metadata keys',async()=>{let found=await request('/api/articles?q='+encodeURIComponent('Уникальноесодержимое'));assert.ok(found.some(x=>x.id===a.id));found=await request('/api/articles?tag='+encodeURIComponent('пилот')+'&tag='+encodeURIComponent('дисплей')+'&property='+encodeURIComponent('repo.url:https://example.org/a:b'));assert.ok(found.some(x=>x.id===a.id));});
 await test('Incoming links respond without exhausting SQLite pool',async()=>{const l=await request(`/api/articles/${a.id}/links`);assert.ok(l.incoming.some(x=>x.id===child.id));assert.equal((await request('/api/health')).ok,true);});
 await test('Cycle rejected and subtree moves with revision history',async()=>{
   await request(`/api/articles/${a.id}`,'PUT',input({parent_id:child.id,base_revision:1}),400);
   const parent=await request('/api/articles','POST',input({title:'Move root'}),201);
   const leaf=await request('/api/articles','POST',input({title:'Move child',parent_id:parent.id}),201);
   const grandchild=await request('/api/articles','POST',input({title:'Move grandchild',parent_id:leaf.id}),201);
   await request(`/api/articles/${parent.id}`,'PUT',{...parent,space_id:'genealogy',base_revision:1});
   for(const original of [parent,leaf,grandchild]) {
     const moved=await request(`/api/articles/${original.id}`);
     assert.equal(moved.space_id,'genealogy');assert.equal(moved.parent_id,original.parent_id);assert.equal(moved.revision,2);
     const versions=await request(`/api/articles/${original.id}/history`);
     assert.equal(versions[0].snapshot.space_id,'genealogy');assert.equal(versions[1].snapshot.space_id,'engineering');
   }
   await request(`/api/articles/${leaf.id}`,'PUT',{...leaf,base_revision:1},409);
   await request(`/api/articles/${parent.id}/restore`,'POST',{revision:1,base_revision:2});
   assert.equal((await request(`/api/articles/${grandchild.id}`)).space_id,'engineering');
 });
 await test('Space rename, archive and restore preserve materials',async()=>{
   const space=await request('/api/spaces','POST',{name:'Temporary space',description:'QA'},201);
   const material=await request('/api/articles','POST',input({space_id:space.id,title:'Archived material',pinned:true}),201);
   const renamed=await request(`/api/spaces/${space.id}`,'PATCH',{name:'Renamed space'});assert.equal(renamed.name,'Renamed space');
   const archived=await request(`/api/spaces/${space.id}`,'PATCH',{archived:true});assert.ok(archived.archived_at);
   assert.ok(!(await request('/api/spaces')).some(x=>x.id===space.id));
   assert.ok((await request('/api/spaces?include_archived=true')).some(x=>x.id===space.id&&x.archived_at));
   assert.ok(!(await request('/api/articles')).some(x=>x.id===material.id));
   assert.ok(!(await request(`/api/bootstrap?space_id=${space.id}`)).instructions.some(x=>x.id===material.id));
   assert.deepEqual(await request(`/api/articles/${material.id}`),material);
   assert.ok((await request('/api/export')).spaces.some(x=>x.id===space.id&&x.archived_at));
   const restored=await request(`/api/spaces/${space.id}`,'PATCH',{archived:false});assert.ok(!restored.archived_at);
   assert.ok((await request('/api/spaces')).some(x=>x.id===space.id));
   assert.ok((await request(`/api/articles?space_id=${space.id}`)).some(x=>x.id===material.id));
 });
 await test('Concurrent writes preserve exactly one winner',async()=>{const responses=await Promise.all(['A','B'].map(body=>fetch(`${base}/api/articles/${a.id}`,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(input({body,base_revision:1})),signal:AbortSignal.timeout(5000)})));assert.deepEqual(responses.map(r=>r.status).sort(),[200,409]);a=await request(`/api/articles/${a.id}`);assert.equal(a.revision,2);});
 await test('Restore appends version and rebuilds FTS',async()=>{a=await request(`/api/articles/${a.id}/restore`,'POST',{revision:1,base_revision:2,author:'qa-human',message:'Возврат'});assert.equal(a.revision,3);assert.ok(a.body.includes('Уникальноесодержимое'));const h=await request(`/api/articles/${a.id}/history`);assert.deepEqual(h.map(x=>x.revision),[3,2,1]);assert.equal(h[0].author,'qa-human');});
 await test('Pinned research is delivered to matching bootstrap',async()=>{a=await request(`/api/articles/${a.id}`,'PUT',{...a,pinned:true,base_revision:a.revision});assert.ok((await request('/api/bootstrap?space_id=engineering')).instructions.some(x=>x.id===a.id));assert.ok(!(await request('/api/bootstrap?space_id=genealogy')).instructions.some(x=>x.id===a.id));});
 await test('Comments preserve anchor and resolve',async()=>{let c=await request(`/api/articles/${a.id}/comments`,'POST',{body:'Уточнить источник',anchor:'проверка',author:'qa-human'},201);c=await request('/api/comments/'+c.id,'PATCH',{resolved:true});assert.equal(c.resolved,true);assert.equal(c.anchor,'проверка');});
 await test('Upload and download preserve original image bytes',async()=>{const bytes=Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aDioAAAAASUVORK5CYII=','base64');const form=new FormData();form.append('file',new Blob([bytes],{type:'image/png'}),'страница.png');const r=await fetch(base+`/api/articles/${a.id}/attachments`,{method:'POST',body:form,signal:AbortSignal.timeout(5000)});assert.equal(r.status,201);file=await r.json();const down=await fetch(base+file.url);assert.equal(down.headers.get('content-type'),'image/png');assert.deepEqual(Buffer.from(await down.arrayBuffer()),bytes);});
 await test('Foreign Origin cannot perform MCP mutation',async()=>{const r=await fetch(base+'/mcp',{method:'POST',headers:{Origin:'https://foreign.example','Content-Type':'text/plain'},body:JSON.stringify({jsonrpc:'2.0',id:1,method:'tools/call',params:{name:'wiki_write',arguments:{article:input({title:'Не должно сохраниться'})}}})});assert.equal(r.status,403);});
 await test('MCP initialize, discovery, scoped search, write and history',async()=>{const init=await request('/mcp','POST',{jsonrpc:'2.0',id:1,method:'initialize',params:{protocolVersion:'2025-03-26',capabilities:{},clientInfo:{name:'qa',version:'1'}}});assert.ok(init.result.capabilities.tools);const list=await request('/mcp','POST',{jsonrpc:'2.0',id:2,method:'tools/list'});assert.ok(list.result.tools.some(x=>x.name==='wiki_write'));const found=await tool('wiki_search',{space_id:'genealogy'});assert.ok(found.results.length>0&&found.results.every(x=>x.space_id==='genealogy'));const m=await tool('wiki_write',{article:input({title:'MCP создано'})});assert.equal(m.revision,1);const h=await tool('wiki_history',{id:m.id});assert.equal(h.length,1);const b=await tool('wiki_bootstrap',{space_id:'engineering'});assert.ok(b.instructions.some(x=>x.id==='agent-rules'));});
 await test('Export includes historical snapshots, comments and file metadata',async()=>{const e=await request('/api/export');assert.equal(e.format,'aiwiki-v1');assert.ok(e.revisions.length>=6);assert.ok(e.comments.length>=1);assert.ok(e.attachments.some(x=>x.id===file.id));});
 await test('Restart persists articles and attachment metadata',async()=>{await stop();server=start();await ready();const saved=await request(`/api/articles/${a.id}`);assert.equal(saved.revision,a.revision);assert.ok((await request(`/api/articles/${a.id}/attachments`)).some(x=>x.id===file.id));});
 console.log(`${results.length} integration checks passed. Test data: ${data}`);
} finally {await stop();mkdirSync(path.join(root,'qa-artifacts'),{recursive:true});writeFileSync(path.join(root,'qa-artifacts','integration.json'),JSON.stringify({passed:results,date:new Date().toISOString(),data,logs},null,2));}
