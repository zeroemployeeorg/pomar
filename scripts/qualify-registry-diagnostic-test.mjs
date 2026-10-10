import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { qualify,selectedExpectation } from './qualify-registry-diagnostic.mjs';
const hash=b=>createHash('sha256').update(b).digest('hex');
const selectedRoutes=Array.from({length:32},(_,i)=>({package:`node_modules/package${i}`,resolved:`https://registry.npmjs.org/package${i}/-/package${i}-1.0.0.tgz`,integrity:'sha512-'+createHash('sha512').update(`synthetic${i}`).digest('base64')}));
const expected={schema:'pomar.registry-expectation/v1',attempt:'synthetic',source:'a'.repeat(40),command:'b'.repeat(64),uid:503,guestUid:1000,lockSha256:'e'.repeat(64),npmConfig:{registry:'expected-loopback',replaceRegistryHost:'npmjs',audit:true,offline:false,'ignore-scripts':false,'update-notifier':true},selectedRoutes};
function fixture(mutate=()=>{}) {
 const requests=selectedRoutes.map(r=>({package:r.package,path:new URL(r.resolved).pathname,integrity:r.integrity,status:200,integrityMatch:true,bytes:15}));
 const probe={uid:1000,lockSha256:expected.lockSha256,npmConfig:structuredClone(expected.npmConfig),verdict:'PASS_CONCURRENT_AND_NPM_INSTALL_ONLY',b1:{pass:true,requests,declaredWarmRoutes:requests.map(r=>r.path).sort()},b2:{run:true,pass:true,process:{exit:0,stoppedAtBound:false,logTruncated:false}}};
 const b1={uid:probe.uid,lockSha256:probe.lockSha256,npmConfig:structuredClone(probe.npmConfig),b1:structuredClone(probe.b1)};
 mutate(probe,b1);
 const files={'probe.json':Buffer.from(JSON.stringify(probe)),'b1.json':Buffer.from(JSON.stringify(b1)),'npm-ci.log':Buffer.from('synthetic install completed')};
 const entry={attempt:expected.attempt,state:'exited',exit_code:0,source:{sha:expected.source,base_sha:expected.source},started_by_uid:503};
 const result={attempt:expected.attempt,state:'exited',exit_code:0,sha:expected.source,base_sha:expected.source,started_by_uid:503,command_sha256:expected.command,npm_lock:{sha256:expected.lockSha256},outputs:Object.entries(files).map(([name,b])=>({name,status:'ok',bytes:b.length,sha256:hash(b)}))};
 return {entry,result,files};
}
const f=fixture();
assert.equal(qualify(f.entry,{result:f.result},f.files,expected),'PASS_CONCURRENT_AND_NPM_INSTALL_ONLY');
for(const state of ['timed-out','failed','lost','running','stopped'])assert.throws(()=>qualify({...f.entry,state},{result:f.result},f.files,expected));
for(const name of Object.keys(f.files))assert.throws(()=>qualify(f.entry,{result:f.result},{...f.files,[name]:undefined},expected));
assert.throws(()=>qualify(f.entry,{result:f.result},{...f.files,'probe.json':Buffer.from('tampered')},expected));
assert.throws(()=>qualify({...f.entry,exit_code:2},{result:f.result},f.files,expected));
for(const [field,value] of [['sha','c'.repeat(40)],['base_sha','c'.repeat(40)],['command_sha256','d'.repeat(64)],['started_by_uid',999]])assert.throws(()=>qualify(f.entry,{result:{...f.result,[field]:value}},f.files,expected));
const cases={
 duplicate:(p,b)=>{for(const r of [p,b])r.b1.requests=Array.from({length:32},()=>({...r.b1.requests[0]}));},
 wrongRoute:(p,b)=>{for(const r of [p,b])r.b1.requests[0].path='/not-locked/-/not-locked-1.tgz';},
 wrongPackage:(p,b)=>{for(const r of [p,b])r.b1.requests[0].package='node_modules/not-locked';},
 wrongSRI:(p,b)=>{for(const r of [p,b])r.b1.requests[0].integrity=selectedRoutes[1].integrity;},
 missingSRI:(p,b)=>{for(const r of [p,b])delete r.b1.requests[0].integrity;},
 wrongLock:(p,b)=>{for(const r of [p,b])r.lockSha256='0'.repeat(64);},
 publicRegistry:(p,b)=>{for(const r of [p,b])r.npmConfig.registry='public-npmjs';},
 wrongRewrite:(p,b)=>{for(const r of [p,b])r.npmConfig.replaceRegistryHost='never';},
 inconsistentSnapshot:(p,b)=>{b.b1.requests[0].bytes++;},
 warmMismatch:(p,b)=>{for(const r of [p,b])r.b1.declaredWarmRoutes.pop();},
 B2timeout:p=>{p.b2.process.stoppedAtBound=true;}, B2failed:p=>{p.b2.process.exit=1;},
 originalCounterexample:(p,b)=>{for(const r of [p,b]){r.b1.requests=Array.from({length:32},()=>({path:'/not-a-locked-route.tgz',status:200,integrityMatch:true,bytes:1}));r.lockSha256='0'.repeat(64);r.npmConfig.registry='public-npmjs';}},
};
for(const [name,mutate]of Object.entries(cases)) {const {entry,result,files}=fixture(mutate);assert.throws(()=>qualify(entry,{result},files,expected),name);}
assert.throws(()=>selectedExpectation({...expected,selectedRoutes:Array(32).fill(selectedRoutes[0])}));
assert.throws(()=>selectedExpectation({...expected,selectedRoutes:selectedRoutes.map((r,i)=>i?r:{...r,resolved:'https://user:canary@registry.npmjs.org/x.tgz'})}));
console.log('PASS frozen route/package/SRI, lock/config, consistent reports, original counterexample and terminal/hash/UID/watchdog refusals');
