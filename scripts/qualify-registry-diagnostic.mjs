import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import assert from 'node:assert/strict';

export function qualify(entry, reply, files, expected) {
  const result = reply.result;
  assert.equal(entry.attempt, expected.attempt, 'entry attempt');
  assert.equal(entry.state, 'exited', 'terminal state must be exited');
  assert.equal(entry.exit_code, 0, 'terminal exit must be zero');
  assert.equal(entry.source.sha, expected.source, 'entry source');
  assert.equal(entry.started_by_uid, expected.uid, 'entry owner');
  assert.equal(result.attempt, expected.attempt, 'result attempt');
  assert.equal(result.state, 'exited', 'result state');
  assert.equal(result.exit_code, 0, 'result exit');
  assert.equal(result.sha, expected.source, 'result source');
  assert.equal(result.command_sha256, expected.command, 'result command');
  assert.equal(result.started_by_uid, expected.uid, 'result owner');
  for (const name of ['probe.json', 'b1.json', 'npm-ci.log']) {
    const bytes = files[name];
    assert.ok(Buffer.isBuffer(bytes) && bytes.length > 0 && bytes.length <= 4 * 1024 * 1024, `${name} required bounded bytes`);
    const record = result.outputs.find(o => o.name === name);
    assert.equal(record?.status, 'ok', `${name} result output status`);
    assert.equal(record.bytes, bytes.length, `${name} size`);
    assert.equal(record.sha256, createHash('sha256').update(bytes).digest('hex'), `${name} hash`);
  }
  const probe = JSON.parse(files['probe.json']);
  const b1 = JSON.parse(files['b1.json']);
  assert.equal(probe.verdict, 'PASS_CONCURRENT_AND_NPM_INSTALL_ONLY');
  for (const report of [probe.b1, b1.b1]) {
    assert.equal(report.pass, true, 'B1 pass');
    assert.equal(report.requests.length, 32, 'all32 B1 routes');
    assert.ok(report.requests.every(r => r.status === 200 && r.integrityMatch === true), 'every SRI verified');
  }
  assert.equal(probe.b2.run, true, 'B2 ran');
  assert.equal(probe.b2.pass, true, 'B2 pass');
  assert.equal(probe.b2.process.exit, 0, 'B2 exit');
  assert.equal(probe.b2.process.stoppedAtBound, false, 'B2 watchdog');
  assert.equal(probe.b2.process.logTruncated, false, 'B2 log complete');
  return 'PASS_CONCURRENT_AND_NPM_INSTALL_ONLY';
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const [dir, attempt, source, command, uid] = process.argv.slice(2);
    assert.ok(dir && attempt && source && command && /^\d+$/.test(uid ?? ''), 'exact directory/attempt/source/command/UID required');
    const json = name => JSON.parse(readFileSync(`${dir}/${name}`));
    const files = Object.fromEntries(['probe.json','b1.json','npm-ci.log'].map(n => [n, readFileSync(`${dir}/${n}`)]));
    const verdict = qualify(json('terminal-entry.json'), json('unsigned-result.json'), files,
      {attempt,source,command,uid:Number(uid)});
    console.log(verdict);
  } catch (e) { console.error(`REFUSE: ${e.message}`); process.exitCode = 1; }
}
