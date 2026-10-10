import { openSync, readSync, closeSync } from 'node:fs';

const names = ['memory.current', 'memory.max', 'memory.events', 'memory.pressure', 'cpu.pressure'];

function boundedRead(name) {
  let fd;
  try {
    fd = openSync(`/sys/fs/cgroup/${name}`, 'r');
    const buffer = Buffer.alloc(4097);
    const length = readSync(fd, buffer, 0, buffer.length, null);
    if (length > 4096) return { outcome: 'oversized' };
    return { outcome: 'read', value: buffer.subarray(0, length).toString('utf8') };
  } catch (error) { return { outcome: 'unavailable', code: error.code ?? 'unknown' }; }
  finally { if (fd !== undefined) closeSync(fd); }
}

// This measures the job's namespace, never the vminitd agent's cgroup.
// The finite sampler streams records so VM loss cannot erase every sample.
export function startJobPressureSampling({ emit = line => console.log(line), samples = 156,
  read = boundedRead, memory = () => process.memoryUsage(), interval = setInterval,
  cancel = clearInterval } = {}) {
  if (!Number.isInteger(samples) || samples < 1 || samples > 156) throw Error('sample bound');
  let count = 0;
  let timer;
  const sample = () => {
    const usage = memory();
    emit(JSON.stringify({ schema: 'pomar.job-pressure/v1', time: new Date().toISOString(),
      sample: ++count, scope: 'job-cgroup-and-sampler-process', samplerRss: usage.rss,
      samplerHeapUsed: usage.heapUsed, cgroup: Object.fromEntries(names.map(name => [name, read(name)])) }));
    if (count >= samples && timer !== undefined) cancel(timer);
  };
  sample();
  if (count < samples) {
    timer = interval(sample, 5000);
    timer.unref?.();
  }
  return () => { if (timer !== undefined) cancel(timer); };
}
