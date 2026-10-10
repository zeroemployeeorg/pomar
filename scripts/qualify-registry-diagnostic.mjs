import { constants, openSync, fstatSync, readSync, closeSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import assert from 'node:assert/strict';

const digest = bytes => createHash('sha256').update(bytes).digest('hex');
const sha = (value, length) => typeof value === 'string' && new RegExp(`^[a-f0-9]{${length}}$`).test(value);
const routes = rows => rows.map(r => ({package:r.package,path:r.path,integrity:r.integrity})).sort((a,b)=>a.path.localeCompare(b.path));

function readBounded(path, cap) {
  const fd=openSync(path,constants.O_RDONLY|constants.O_NOFOLLOW|constants.O_NONBLOCK);
  try {
    const stat=fstatSync(fd); assert.ok(stat.isFile() && stat.size<=cap,'bounded regular file');
    const bytes=Buffer.alloc(cap+1); let size=0;
    while(size<bytes.length) { const n=readSync(fd,bytes,size,bytes.length-size,null); if(!n)break; size+=n; }
    assert.ok(size<=cap,'file grew beyond bound'); return bytes.subarray(0,size);
  } finally { closeSync(fd); }
}

export function selectedExpectation(expected) {
  assert.equal(expected.schema, 'pomar.registry-expectation/v1');
  assert.ok(sha(expected.source,40) && sha(expected.command,64) && sha(expected.lockSha256,64), 'frozen hashes');
  assert.ok(Number.isSafeInteger(expected.uid) && expected.uid >= 0 && Number.isSafeInteger(expected.guestUid) && expected.guestUid >= 0, 'frozen UIDs');
  assert.equal(expected.npmConfig.registry, 'expected-loopback');
  assert.ok(['npmjs','always','never'].includes(expected.npmConfig.replaceRegistryHost), 'registry replacement policy');
  assert.deepEqual(Object.keys(expected.npmConfig).sort(),['registry','replaceRegistryHost','audit','offline','ignore-scripts','update-notifier'].sort());
  for(const name of ['audit','offline','ignore-scripts','update-notifier'])assert.equal(typeof expected.npmConfig[name],'boolean');
  assert.equal(expected.selectedRoutes.length,32);
  const selected = expected.selectedRoutes.map(row => {
    const url = new URL(row.resolved);
    assert.ok(url.protocol === 'https:' && url.hostname === 'registry.npmjs.org' && !url.port && !url.username && !url.password && !url.search && !url.hash, 'public locked route');
    assert.match(url.pathname, /^\/(?:@[A-Za-z0-9][A-Za-z0-9._~-]*\/)?[A-Za-z0-9][A-Za-z0-9._~-]*\/-\/[A-Za-z0-9][A-Za-z0-9._~+-]*\.tgz$/);
    assert.ok(typeof row.package === 'string' && row.package.startsWith('node_modules/'), 'locked package');
    assert.match(row.integrity, /^sha512-[A-Za-z0-9+/]{86}==$/);
    const bytes=Buffer.from(row.integrity.slice(7),'base64');
    assert.ok(bytes.length===64 && 'sha512-'+bytes.toString('base64')===row.integrity, 'canonical SRI');
    return {package:row.package,path:url.pathname,integrity:row.integrity};
  });
  assert.equal(new Set(selected.map(r=>r.path)).size,32,'32 distinct expected routes');
  assert.equal(new Set(selected.map(r=>r.package)).size,32,'32 distinct expected packages');
  return routes(selected);
}

// The independently retained expectation bytes must be hashed before calling
// this function. Report hashes alone cannot establish the intended route set.
export function qualify(entry, reply, files, expected) {
  const selected=selectedExpectation(expected);
  const result=reply.result;
  assert.equal(entry.attempt,expected.attempt);
  assert.equal(entry.state,'exited'); assert.equal(entry.exit_code,0);
  assert.equal(entry.source.sha,expected.source); assert.equal(entry.source.base_sha,expected.source);
  assert.equal(entry.started_by_uid,expected.uid);
  assert.equal(result.attempt,expected.attempt); assert.equal(result.state,'exited'); assert.equal(result.exit_code,0);
  assert.equal(result.sha,expected.source); assert.equal(result.base_sha,expected.source);
  assert.equal(result.command_sha256,expected.command); assert.equal(result.started_by_uid,expected.uid);
  assert.equal(result.npm_lock.sha256,expected.lockSha256);
  const names=['probe.json','b1.json','npm-ci.log'];
  assert.equal(result.outputs.length,names.length);
  assert.equal(new Set(result.outputs.map(o=>o.name)).size,names.length);
  for (const name of names) {
    const bytes=files[name];
    assert.ok(Buffer.isBuffer(bytes) && bytes.length>0 && bytes.length<=4*1024*1024, 'required bounded output');
    const record=result.outputs.find(o=>o.name===name);
    assert.equal(record?.status,'ok'); assert.equal(record.bytes,bytes.length); assert.equal(record.sha256,digest(bytes));
  }
  const probe=JSON.parse(files['probe.json']), b1=JSON.parse(files['b1.json']);
  assert.equal(probe.verdict,'PASS_CONCURRENT_AND_NPM_INSTALL_ONLY');
  for (const report of [probe,b1]) {
    assert.equal(report.uid,expected.guestUid); assert.equal(report.lockSha256,expected.lockSha256);
    assert.deepEqual(report.npmConfig,expected.npmConfig);
    assert.equal(report.b1.pass,true); assert.equal(report.b1.requests.length,32);
    assert.equal(new Set(report.b1.requests.map(r=>r.path)).size,32,'distinct observed routes');
    assert.ok(report.b1.requests.every(r=>r.status===200 && r.integrityMatch===true && Number.isSafeInteger(r.bytes) && r.bytes>0 && r.bytes<=1024*1024));
    assert.deepEqual(routes(report.b1.requests),selected,'exact package/route/observed SRI set');
    assert.deepEqual([...report.b1.declaredWarmRoutes].sort(),selected.map(r=>r.path).sort());
  }
  assert.deepEqual(probe.b1,b1.b1,'same B1 snapshot');
  assert.equal(probe.b2.run,true); assert.equal(probe.b2.pass,true); assert.equal(probe.b2.process.exit,0);
  assert.equal(probe.b2.process.stoppedAtBound,false); assert.equal(probe.b2.process.logTruncated,false);
  return 'PASS_CONCURRENT_AND_NPM_INSTALL_ONLY';
}

if (process.argv[1] && import.meta.url===pathToFileURL(process.argv[1]).href) {
  try {
    const [dir,specPath,specSha]=process.argv.slice(2);
    assert.ok(dir && specPath && sha(specSha,64),'directory/frozen expectation/hash required');
    const specBytes=readBounded(specPath,256*1024); assert.equal(digest(specBytes),specSha);
    const expected=JSON.parse(specBytes);
    const json=name=>JSON.parse(readBounded(`${dir}/${name}`,1024*1024));
    const files=Object.fromEntries(['probe.json','b1.json','npm-ci.log'].map(n=>[n,readBounded(`${dir}/${n}`,4*1024*1024)]));
    console.log(qualify(json('terminal-entry.json'),json('unsigned-result.json'),files,expected));
  } catch { console.error('REFUSE: diagnostic evidence does not match the frozen expectation.'); process.exitCode=1; }
}
