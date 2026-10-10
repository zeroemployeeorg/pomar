import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { qualify } from './qualify-registry-diagnostic.mjs';
const expected = {attempt:'synthetic',source:'a'.repeat(40),command:'b'.repeat(64),uid:503};
const requests = Array.from({length:32},()=>({status:200,integrityMatch:true}));
const probe = {verdict:'PASS_CONCURRENT_AND_NPM_INSTALL_ONLY',b1:{pass:true,requests},
 b2:{run:true,pass:true,process:{exit:0,stoppedAtBound:false,logTruncated:false}}};
const files = {'probe.json':Buffer.from(JSON.stringify(probe)),'b1.json':Buffer.from(JSON.stringify({b1:probe.b1})),'npm-ci.log':Buffer.from('synthetic install completed')};
const entry={attempt:expected.attempt,state:'exited',exit_code:0,source:{sha:expected.source},started_by_uid:503};
const result={attempt:expected.attempt,state:'exited',exit_code:0,sha:expected.source,started_by_uid:503,command_sha256:expected.command,
 outputs:Object.entries(files).map(([name,b])=>({name,status:'ok',bytes:b.length,sha256:createHash('sha256').update(b).digest('hex')}))};
assert.equal(qualify(entry,{result},files,expected),'PASS_CONCURRENT_AND_NPM_INSTALL_ONLY');
for (const state of ['timed-out','failed','lost','running','stopped']) assert.throws(()=>qualify({...entry,state},{result},files,expected));
for (const name of Object.keys(files)) assert.throws(()=>qualify(entry,{result},{...files,[name]:undefined},expected));
assert.throws(()=>qualify(entry,{result},{...files,'probe.json':Buffer.from('tampered')},expected));
assert.throws(()=>qualify({...entry,exit_code:2},{result},files,expected));
assert.throws(()=>qualify(entry,{result:{...result,sha:'c'.repeat(40)}},files,expected));
assert.throws(()=>qualify(entry,{result:{...result,command_sha256:'d'.repeat(64)}},files,expected));
console.log('PASS synthetic success; timeout/failure/missing/tampered/nonzero/wrong source/command refusals');
