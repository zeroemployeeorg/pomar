import assert from 'node:assert/strict';
import { startJobPressureSampling } from './sample-job-pressure.mjs';
const rows = [];
let tick, cancelled = 0;
const stop = startJobPressureSampling({samples: 2,
  emit: line => rows.push(JSON.parse(line)), read: name => ({outcome:'read',value:name}),
  memory: () => ({rss:123,heapUsed:45}),
  interval: (fn, ms) => { assert.equal(ms,5000); tick=fn; return {unref(){}}; },
  cancel: () => { cancelled++; }});
assert.equal(rows.length,1); tick();
assert.equal(rows.length,2); assert.equal(cancelled,1); stop();
assert.equal(rows[0].scope,'job-cgroup-and-sampler-process');
assert.equal(rows[0].samplerRss,123);
assert.deepEqual(Object.keys(rows[0].cgroup),['memory.current','memory.max','memory.events','memory.pressure','cpu.pressure']);
for (const samples of [0,157,NaN,1.5]) assert.throws(()=>startJobPressureSampling({samples}));
console.log('PASS five-second cadence, finite sample bound, fixed metric scope and explicit stop');
