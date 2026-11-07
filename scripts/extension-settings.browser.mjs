// Optional real-browser regression. Supply Playwright installed outside the repo
// via BROKER_PLAYWRIGHT_MODULE, or install it locally without saving a dependency.
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
const { chromium } = await import(process.env.BROKER_PLAYWRIGHT_MODULE || 'playwright');
const root = fileURLToPath(new URL('../', import.meta.url));
const temp = mkdtempSync(join(tmpdir(), 'broker-extension-browser-'));
let broker, browser;
try {
 const binary = join(temp, 'broker');
 execFileSync('go', ['build', '-o', binary, './cmd/codex-broker'], {cwd:root, stdio:'pipe'});
 const fields = [
  ['text','Example'], ['textarea','Long text'], ['password',''], ['email','example@example.com'],
  ['url','https://example.com'], ['tel','+10000000000'], ['path','/tmp/example'],
  ['number',1.5], ['integer',2], ['slider',50], ['percentage',75], ['ratio',0.5], ['duration',1000],
  ['boolean',true], ['select','a'], ['radio','a'], ['multiselect',['a']],
  ['color','#336699'], ['date','2026-11-07'], ['time','14:30:15'], ['datetime','2026-11-07T14:30:15'],
  ['string-list',['one']], ['key-value',{key:'value'}], ['json',{nested:[true,1]}],
 ].map(([type,value]) => ({key:type.replaceAll('-','_'), type, label:type, default:value,
  ...(['select','radio','multiselect'].includes(type) ? {options:[{value:'a',label:'A'}, {value:'b',label:'B'}]} : {}),
 }));
 fields.push({key:'emptyColor',type:'color',label:'Empty color',default:''});
 fields.push({key:'conditional',type:'text',label:'Conditional',default:'hidden',visibleWhen:{key:'boolean',equals:false},group:'Advanced'});
 fields.push({key:'safe',type:'text',label:'<img src=x onerror=alert(1)>',default:'safe'});
 mkdirSync(join(temp,'extensions'));
 writeFileSync(join(temp,'extensions','example.ts'),`export const settings=${JSON.stringify({title:'Example settings',fields})};export default p=>p.on('request',()=>({response:{status:200,headers:{},body:'healthy fallback'}}))`);
 writeFileSync(join(temp,'extensions','broken.ts'),`export default p=>p.on('request',()=>{p.log('info','received request',{responseId:'example'});throw Error('<img src=x onerror=alert(1)> example failure')})`);
 const listener=createServer();await new Promise(resolve=>listener.listen(0,'127.0.0.1',resolve));const port=listener.address().port;await new Promise(resolve=>listener.close(resolve));
 broker=spawn(binary,['serve','--port',String(port)], {cwd:temp,env:{
  PATH:process.env.PATH,
  WINDOWKEEPER_DATA_DIR:join(temp,'data'), WINDOWKEEPER_RUNTIME_DIR:join(temp,'run'), WINDOWKEEPER_LOG_DIR:join(temp,'logs'),
  WINDOWKEEPER_ADMIN_PASSWORD:'example browser fixture password', WINDOWKEEPER_VAULT_KEY:'wk1_'+randomBytes(32).toString('base64url'),
  WINDOWKEEPER_CODEX_EXECUTABLE:'/bin/false', WINDOWKEEPER_ROOT_PATH:'/broker', WINDOWKEEPER_COOKIE_SECURE:'false',
  WINDOWKEEPER_PROXY_EXTENSIONS_DIR:join(temp,'extensions'),
 },stdio:'pipe'});
 let logs='';broker.stderr.on('data',chunk=>{logs=(logs+chunk).slice(-4000)});
 const base=`http://127.0.0.1:${port}/broker`;
 let ready=false;
 for(let i=0;i<100;i++){try{if((await fetch(base+'/health/live')).ok){ready=true;break}}catch{}await new Promise(resolve=>setTimeout(resolve,100));}
 assert.ok(ready,`Fixture did not start: ${logs}`);
 browser=await chromium.launch({headless:true,args:['--no-sandbox']});
 const page=await browser.newPage();const errors=[];page.on('pageerror',error=>errors.push(error.message));
 await page.goto(base+'/login');await page.getByLabel('Password',{exact:true}).fill('example browser fixture password');await page.getByRole('button',{name:'Sign in',exact:true}).click();
 await page.goto(base+'/settings');
 const panel=page.locator('[data-extension-settings="example"]');
 assert.equal(await panel.getAttribute('open'),null);
 assert.equal(await panel.locator('input').count(),0,'settings loaded before opening');
 const open=async()=>{await panel.locator('summary').click();await panel.getByRole('button',{name:'Apply',exact:true}).waitFor();};
 const apply=async()=>{await panel.getByRole('button',{name:'Apply',exact:true}).click();await panel.locator('[data-extension-status]').filter({hasText:'Settings applied.'}).waitFor();};
 const endpoint=base+'/settings/proxy-extensions/example/settings';
 const read=async()=>{const response=await page.request.get(endpoint);assert.equal(response.status(),200);return response.json();};
 await open();
 assert.equal(await panel.getByLabel('Empty color',{exact:true}).inputValue(),'');
 assert.equal(await panel.locator('img').count(),0,'extension HTML executed');
 assert.equal(await panel.getByLabel('text',{exact:true}).inputValue(),'Example');
 assert.equal(await panel.getByLabel('Conditional',{exact:true}).isVisible(),false);
 await panel.getByLabel('text',{exact:true}).fill('discard me');await panel.getByRole('button',{name:'Cancel',exact:true}).click();
 await open();assert.equal(await panel.getByLabel('text',{exact:true}).inputValue(),'Example');
 await panel.getByLabel('boolean',{exact:true}).uncheck();assert.equal(await panel.getByLabel('Conditional',{exact:true}).isVisible(),true);
 await panel.getByLabel('text',{exact:true}).fill('Applied');
 await panel.getByLabel('password',{exact:true}).fill('example secret');
 await panel.getByLabel('select',{exact:true}).selectOption('b');
 await panel.getByRole('radiogroup',{name:'radio',exact:true}).getByLabel('B',{exact:true}).check();
 await panel.getByLabel('multiselect',{exact:true}).selectOption(['a','b']);
 await panel.getByLabel('json',{exact:true}).fill('{"changed":true}');
 await apply();
 const saved=await read();
 assert.equal(saved.values.text,'Applied');assert.equal(saved.values.boolean,false);assert.equal(saved.values.select,'b');assert.equal(saved.values.radio,'b');
 assert.deepEqual(saved.values.multiselect,['a','b']);assert.deepEqual(saved.values.json,{changed:true});
 assert.equal(saved.secretSet.password,true);assert.equal(saved.values.password,undefined);
 for(const field of fields.filter(field=>!['text','password','boolean','select','radio','multiselect','json'].includes(field.type)))assert.deepEqual(saved.values[field.key],field.default,`control ${field.type} changed`);
 await panel.getByLabel('json',{exact:true}).fill('{bad');await panel.getByRole('button',{name:'Apply',exact:true}).click();
 await panel.locator('[data-extension-status]').filter({hasText:'enter valid JSON'}).waitFor();
 assert.equal(await panel.getByLabel('json',{exact:true}).inputValue(),'{bad','failed save lost draft');
 assert.equal((await read()).revision,saved.revision,'invalid draft mutated persistence');
 await panel.getByRole('button',{name:'Reset to defaults',exact:true}).click();
 assert.equal(await panel.getByLabel('text',{exact:true}).inputValue(),'Example');
 assert.equal((await read()).values.text,'Applied','reset saved without Apply');
 await panel.getByLabel('text',{exact:true}).fill('Edited defaults');await apply();
 const reset=await read();assert.equal(reset.values.text,'Edited defaults');assert.equal(reset.secretSet.password,false);assert.equal(reset.values.boolean,true);
 // Saved passwords stay secret and blank drafts preserve them.
 await panel.getByLabel('password',{exact:true}).fill('second secret');await apply();
 await panel.getByLabel('text',{exact:true}).fill('Preserve password');await apply();assert.equal((await read()).secretSet.password,true);
 await panel.getByLabel('Clear saved value',{exact:true}).check();await apply();assert.equal((await read()).secretSet.password,false);
 // An external save must produce a visible conflict while preserving this draft.
 const current=await read();const csrf=await panel.getAttribute('data-csrf');
 const response=await page.request.post(endpoint,{headers:{'X-CSRF-Token':csrf},data:{values:{...current.values,text:'External'},revision:current.revision}});assert.equal(response.status(),200);
 await panel.getByLabel('text',{exact:true}).fill('Conflicting draft');await panel.getByRole('button',{name:'Apply',exact:true}).click();
 await panel.locator('[data-extension-status]').filter({hasText:'Settings changed elsewhere'}).waitFor();assert.equal(await panel.getByLabel('text',{exact:true}).inputValue(),'Conflicting draft');
 await page.setViewportSize({width:390,height:844});
 assert.equal(await panel.getByRole('button',{name:'Cancel',exact:true}).isVisible(),true);
 // Generate a proxy failure using a temporary client key; a later extension
 // handles locally, so this test never contacts an upstream server.
 await page.goto(base+'/settings');
 await page.getByLabel('Client name',{exact:true}).fill('Example device');
 await page.getByRole('button',{name:'Create key',exact:true}).click();
 const token=await page.locator('.service-alert code').textContent();
 const proxied=await page.request.post(base+'/v1/responses',{headers:{Authorization:'Bearer '+token.trim()},data:{input:[]}});
 assert.equal(proxied.status(),200);assert.equal(await proxied.text(),'healthy fallback');
 await page.goto(base+'/settings');
 const card=page.locator('.data-list article').filter({has:page.getByText('broken',{exact:true})});
 await card.getByText('Extension reported an error',{exact:false}).waitFor();
 const error=card.locator('.extension-error');
 assert.ok(await error.locator('time').getAttribute('datetime'));
 assert.equal(await error.locator('pre').isVisible(),false,'error expanded by default');
 await error.getByText('View last error',{exact:true}).click();
 assert.equal(await error.locator('pre').isVisible(),true);
 assert.ok((await error.locator('pre').textContent()).includes('example failure'));
 assert.equal(await card.locator('img').count(),0,'error HTML executed');
 await card.getByRole('link',{name:'View logs',exact:true}).click();
 await page.getByRole('heading',{name:'Service logs',exact:true}).waitFor();
 assert.ok((await page.locator('.log-list').textContent()).includes('received request'));
 assert.ok((await page.locator('.log-list').textContent()).includes('example failure'));
 assert.deepEqual(errors,[]);
 console.log('Extension settings browser regression passed: all controls, cancel, apply, reset, secrets, conflicts, validation, root path, narrow viewport, fail-open proxy and collapsed error/log presentation.');
} finally {
 await browser?.close();
 if(broker && broker.exitCode===null){broker.kill('SIGTERM');await new Promise(resolve=>broker.once('exit',resolve));}
 rmSync(temp,{recursive:true,force:true});
}
