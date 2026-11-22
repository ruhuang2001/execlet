import assert from 'node:assert/strict';
import { test } from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { Client, SandboxError as NativeError } from '@tiny-sandbox/sdk';
import { Sandbox, AdapterError, AuthenticationError, InvalidArgumentError,
  TimeoutError, NotFoundError, SandboxNotFoundError, CommandExitError } from '@tiny-sandbox/sdk/e2b';

const sid = 'sandbox-one', eid = 'execution-one', pid = 42;
const base = '/v1/sandboxes/' + sid, exec = base + '/executions/' + eid;
const flags = ['e2b.background.v1', 'execution.env-cwd.v1', 'execution.stdin.v1', 'execution.concurrent.v1', 'files.list.v1', 'files.archive.v1'];
const image = 'sha256:' + 'a'.repeat(64);
const sandbox = { id: sid, state: 'running', expires_at: '2027-01-01T00:00:00Z' };
const running = { id: eid, sandbox_id: sid, pid, command: ['true'], state: 'running',
  exit_code: null, termination_reason: null, stdout: '', stderr: '', output_truncated: false,
  started_at: '2026-11-18T00:00:00Z', finished_at: null };
const done = (extra = {}) => ({ ...running, state: 'completed', exit_code: 0,
  termination_reason: 'exited', finished_at: '2026-11-18T00:00:01Z', ...extra });
const json = (value, status = 200) => new Response(JSON.stringify(value), { status,
  headers: { 'Content-Type': 'application/json' } });
const failure = (code, status) => json({ code, message: code }, status);
const newline = String.fromCharCode(10);
const frame = (id, type, data) => [`id: ${id}`, `event: ${type}`, `data: ${JSON.stringify(data)}`, '', ''].join(newline);
const output = (id, data, type = 'stdout') => frame(id, type, { data });
const complete = (id, extra) => frame(id, 'complete', done(extra));
const sse = text => new Response(text, { headers: { 'Content-Type': 'text/event-stream' } });
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};
async function prompt(promise) {
  const controller = new AbortController();
  try {
    return await Promise.race([promise, delay(500, null, { signal: controller.signal }).then(() => {
      throw new Error('observation operation did not finish promptly');
    })]);
  } finally { controller.abort(); }
}
async function rejected(promise) {
  const value = await promise.then(value => ({ ok: true, value }), error => ({ ok: false, error }));
  assert.equal(value.ok, false, 'expected rejection, got success');
  return value.error;
}
function live(signal) {
  let controller, closed = false;
  const stop = () => {
    if (closed) return;
    closed = true;
    controller.error(new Error('stream aborted'));
  };
  const body = new ReadableStream({
    start(value) { controller = value; },
    cancel() { closed = true; signal?.removeEventListener('abort', stop); },
  });
  signal?.addEventListener('abort', stop, { once: true });
  if (signal?.aborted) stop();
  return {
    response: new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }),
    send(text) { if (!closed) controller.enqueue(new TextEncoder().encode(text)); },
    get closed() { return closed; },
  };
}
function fixture(t, route = () => undefined) {
  const calls = [];
  t.mock.method(globalThis, 'fetch', async (url, init = {}) => {
    const call = { url: String(url), path: new URL(url).pathname, method: init.method ?? 'GET',
      headers: new Headers(init.headers), signal: init.signal,
      body: typeof init.body === 'string' ? JSON.parse(init.body) : init.body };
    calls.push(call);
    const response = await route(call, calls);
    if (response !== undefined) return response;
    if (call.path === '/v1/sandboxes' && call.method === 'POST') return json(sandbox, 201);
    if (call.path === base && ['GET', 'PATCH'].includes(call.method)) return json(sandbox);
    if (call.path === '/v1/capabilities') return json({ capabilities: flags });
    if (call.path === base + '/executions' && call.method === 'POST') return json(running, 201);
    if (call.path === base + '/commands/' + pid) return json({ execution: running, last_event_id: 0 });
    if (call.path === exec + '/events') return sse(complete(1));
    if (call.path === exec + '/kill') return json({ execution: done({ termination_reason: 'canceled', exit_code: 137 }), signaled: true });
    throw new Error('unexpected request ' + call.method + ' ' + call.path);
  });
  return { calls, create: (options = {}) => Sandbox.create({ image, endpoint: 'http://sdk.test', apiKey: 'key-one', ...options }) };
}
const commandCalls = calls => calls.filter(c => c.path.includes('/executions') || c.path.includes('/commands/'));
const signals = calls => calls.filter(c => c.path.endsWith('/cancel') || c.path.endsWith('/kill'));

test('E2B writes return observed symlink metadata without listing', async t => {
  const { calls, create } = fixture(t, c => {
    if (c.path === '/v1/capabilities') return json({ capabilities: [] });
    if (c.method === 'PUT') return json({ name: 'file', type: 'symlink', size: 4, mode: 0o777,
      mtime: '2026-11-18T00:00:00Z', is_dir: false, uid: 0, gid: 0, symlink_target: 'dest' });
  });
  const sb = await create();
  calls.length = 0;
  assert.deepEqual(await sb.files.write('/work/file', 'replacement'), {name: 'file', path: '/work/file', type: 'symlink'});
  assert.deepEqual(calls.map(c => c.method), ['PUT']);
});

test('native additive fields fail before mutation on old servers; basic execution still works', async t => {
  const { calls } = fixture(t, c => c.path === '/v1/capabilities' ? failure('not_found', 404) : undefined);
  const native = new Client({ domain: 'http://sdk.test', maxRetries: -1 });
  const operations = [
    () => native.createSandbox({ env: { TOKEN: 'private' } }),
    () => native.createSandbox({ max_concurrent_executions: 2 }),
    ...[{ env: { X: 'Y' } }, { cwd: '/tmp' }, { stdin: 'payload' }, { timeout_ms: 0 }].map(
      extra => () => native.startExecution(sid, { command: ['true'], ...extra })),
    () => native.resolveCommand(sid, pid),
    () => native.killExecution(sid, eid),
    () => native.listFiles(sid, '/'),
    () => native.readArchive(sid, '/'),
    () => native.writeArchive(sid, '/', new Uint8Array()),
  ];
  for (const operation of operations) {
    const before = calls.length;
    assert.equal((await rejected(operation())).code, 'incompatible_server');
    assert(calls.slice(before).every(c => c.path === '/v1/capabilities'));
  }
  const before = calls.length;
  await native.startExecution(sid, { command: ['true'] });
  assert.deepEqual(calls.slice(before).map(c => c.path), [base + '/executions']);
});

test('background consumes immediately, live fields and simultaneous waits cache one serial result', async t => {
  const entered = deferred(), release = deferred(), received = [];
  let active = 0;
  const { create, calls } = fixture(t, c => c.path.endsWith('/events')
    ? sse(output(1, '世界🌍') + output(2, 'err🙂', 'stderr') + output(3, 'end') +
      complete(4, { stdout: '世界🌍end', stderr: 'err🙂' })) : undefined);
  const sb = await create();
  const callback = async (kind, chunk) => {
    assert.equal(active++, 0); received.push([kind, chunk]); entered.resolve();
    await release.promise; active--;
  };
  const handle = await sb.commands.run('printf ready', { background: true, timeoutMs: 1250,
    cwd: '/tmp', envs: { TEST: 'yes' }, onStdout: c => callback('out', c), onStderr: c => callback('err', c) });
  t.after(() => handle.disconnect());
  await prompt(entered.promise);
  assert.equal(handle.stdout, '世界🌍'); assert.equal(handle.stderr, '');
  assert.equal(handle.exitCode, undefined); assert.equal(handle.error, undefined);
  assert.equal(handle.pid, pid); assert.equal(handle.executionId, eid);
  assert.throws(() => { handle.executionId = 'replacement'; }, TypeError);
  const a = handle.wait(), b = handle.wait();
  assert.equal(a, b); release.resolve();
  const result = await a;
  assert.deepEqual(result, { stdout: '世界🌍end', stderr: 'err🙂', exitCode: 0 });
  assert.equal(await b, result);
  assert.deepEqual(received, [['out', '世界🌍'], ['err', 'err🙂'], ['out', 'end']]);
  assert.equal(handle.exitCode, 0);
  const before = calls.length;
  await handle.disconnect(); assert.equal(await handle.wait(), result);
  assert.equal(calls.length, before);
  const start = calls.find(c => c.method === 'POST' && c.path.endsWith('/executions'));
  assert.deepEqual(start.body.command, ['/bin/bash', '-l', '-c', 'printf ready']);
  assert.deepEqual(start.body.env, { TEST: 'yes' });
  assert.equal(start.body.cwd, '/tmp'); assert.equal(start.body.timeout_ms, 1250);
  assert.equal('timeout_seconds' in start.body, false);
});

test('foreground, false, dynamic and default timeout start managed commands', async t => {
  const { create, calls } = fixture(t);
  const sb = await create();
  for (const options of [{}, { background: false }, { timeoutMs: 0 }]) {
    const result = await sb.commands.run('true', options);
    assert.deepEqual(result, { stdout: '', stderr: '', exitCode: 0 });
    assert.equal(result.wait, undefined);
  }
  const handle = await sb.commands.run('true', { background: Boolean(1) });
  await handle.wait();
  assert.deepEqual(calls.filter(c => c.method === 'POST' && c.path.endsWith('/executions')).map(c => c.body.timeout_ms),
    [60000, 60000, 0, 60000]);
  assert.equal(calls.filter(c => c.path === '/v1/capabilities').length, 1);
});

test('fast completed start still returns a handle, replays output and caches ordinary nonzero error', async t => {
  const { create, calls } = fixture(t, c => {
    if (c.method === 'POST' && c.path.endsWith('/executions')) return json(done({ exit_code: 37, stdout: 'fast' }), 201);
    if (c.path.endsWith('/events')) return sse(output(1, 'fast') + complete(2, { exit_code: 37, stdout: 'fast' }));
  });
  const sb = await create();
  const handle = await sb.commands.run('exit 37', { background: true });
  const error = await rejected(handle.wait());
  assert(error instanceof CommandExitError);
  assert.equal(error.exitCode, 37); assert.equal(error.stdout, 'fast');
  assert.equal(error.executionId, eid); assert.equal(handle.exitCode, 37);
  const before = calls.length;
  await handle.disconnect();
  assert.equal(await rejected(handle.wait()), error); assert.equal(calls.length, before);
});

test('fresh attach uses atomic cursor and suffix, not the terminal snapshot prefix', async t => {
  const received = [];
  const { create, calls } = fixture(t, c => {
    if (c.path.includes('/commands/')) return json({ execution: { ...running, stdout: 'PRE', stderr: 'OLD' }, last_event_id: 8 });
    if (c.path.endsWith('/events')) return sse(output(7, 'PRE') + output(8, 'OLD', 'stderr') +
      output(9, 'POST') + output(10, 'NEW', 'stderr') + complete(11, { stdout: 'PREPOST', stderr: 'OLDNEW' }));
  });
  const sb = await create(), before = calls.length;
  const handle = await sb.commands.connect(pid, { timeoutMs: 0, onStdout: c => { received.push(c); } });
  assert.deepEqual(await handle.wait(), { stdout: 'POST', stderr: 'NEW', exitCode: 0 });
  assert.deepEqual(received, ['POST']);
  assert.equal(calls.find(c => c.path.endsWith('/events')).headers.get('Last-Event-ID'), '8');
  assert(calls.slice(before).every(c => c.method === 'GET'));
});

test('completed attach uses snapshot outcome without SSE or prefix', async t => {
  let code = 0;
  const { create, calls } = fixture(t, c => c.path.includes('/commands/')
    ? json({ execution: done({ exit_code: code, stdout: 'PRE', stderr: 'OLD' }), last_event_id: 12 }) : undefined);
  const sb = await create();
  for (code of [0, 37]) {
    const handle = await sb.commands.connect(pid, { onStdout: () => assert.fail('replayed prefix') });
    const result = code ? await rejected(handle.wait()) : await handle.wait();
    assert.equal(result.exitCode, code); assert.equal(result.stdout, ''); assert.equal(result.stderr, '');
    if (code) assert(result instanceof CommandExitError);
  }
  assert.equal(calls.filter(c => c.path.endsWith('/events')).length, 0);
});

test('same-handle recovery resumes once by cursor without PID re-resolution', async t => {
  let attempt = 0;
  const chunks = [];
  const { create, calls } = fixture(t, c => {
    if (c.path.includes('/commands/')) return json({ execution: { ...running, stdout: 'PREFIX' }, last_event_id: 4 });
    if (c.path.endsWith('/events')) {
      attempt++;
      return attempt === 1 ? sse(output(5, 'one') + ['id: 6', 'event: stdout', 'data: {'].join(newline))
        : sse(output(5, 'one') + output(6, 'two') + complete(7, { stdout: 'PREFIXonetwo' }));
    }
  });
  const sb = await create();
  const handle = await sb.commands.connect(pid, { timeoutMs: 0, onStdout: c => { chunks.push(c); } });
  assert.equal((await handle.wait()).stdout, 'onetwo'); assert.deepEqual(chunks, ['one', 'two']);
  assert.deepEqual(calls.filter(c => c.path.endsWith('/events')).map(c => c.headers.get('Last-Event-ID')), ['4', '5']);
  assert.equal(calls.filter(c => c.path.includes('/commands/')).length, 1);
});

test('callback rejections retain arbitrary values without unhandled rejection before wait', async t => {
  const unhandled = [], listener = error => unhandled.push(error);
  process.on('unhandledRejection', listener); t.after(() => process.off('unhandledRejection', listener));
  const { create, calls } = fixture(t, c => c.path.endsWith('/events')
    ? sse(output(1, 'first') + output(2, 'never') + complete(3)) : undefined);
  const sb = await create();
  for (const value of [undefined, null, 0, 'rejected value', { sentinel: true },
    new NativeError('canceled', 'sentinel'), new TimeoutError('user timeout')]) {
    let count = 0;
    const handle = await sb.commands.run('true', { background: true, timeoutMs: 0,
      onStdout: async () => { count++; throw value; } });
    await delay(5); assert.equal(count, 1); assert.equal(await rejected(handle.wait()), value);
    const before = calls.length;
    await handle.disconnect();
    assert.equal(await rejected(handle.wait()), value); assert.equal(calls.length, before);
    assert.equal(handle.exitCode, undefined);
  }
  assert.deepEqual(unhandled, []); assert.equal(signals(calls).length, 0);
});

test('disconnect promptly aborts silent and pending-callback streams with no new callbacks', async t => {
  const streams = [];
  const { create, calls } = fixture(t, c => {
    if (!c.path.endsWith('/events')) return;
    const stream = live(c.signal); streams.push(stream); return stream.response;
  });
  const sb = await create();
  const silent = await sb.commands.run('silent', { background: true, timeoutMs: 0 });
  const silentWait = silent.wait();
  await prompt(silent.disconnect()); await prompt(silent.disconnect());
  assert.equal((await rejected(silentWait)).code, 'disconnected');
  const entered = deferred(), release = deferred();
  let count = 0;
  const pending = await sb.commands.run('pending', { background: true, timeoutMs: 0,
    onStdout: async () => { count++; entered.resolve(); await release.promise; } });
  streams[1].send(output(1, 'first') + output(2, 'never'));
  await prompt(entered.promise); await prompt(pending.disconnect());
  assert.equal((await rejected(pending.wait())).code, 'disconnected');
  assert(streams.every(stream => stream.closed));
  release.reject(new Error('late callback rejection'));
  await delay(5); assert.equal(count, 1); assert.equal(signals(calls).length, 0);
});

test('callback can await its own disconnect without deadlock', async t => {
  let stream, count = 0;
  const finished = deferred();
  const { create } = fixture(t, c => {
    if (c.path.endsWith('/events')) { stream = live(c.signal); return stream.response; }
  });
  const sb = await create();
  const handle = await sb.commands.run('true', { background: true, timeoutMs: 0,
    onStdout: async () => { count++; await handle.disconnect(); finished.resolve(); } });
  stream.send(output(1, 'one') + output(2, 'two'));
  await prompt(finished.promise); assert.equal(count, 1);
  assert.equal((await rejected(handle.wait())).code, 'disconnected'); assert(stream.closed);
});

test('observation timeout starts at connect, never wait, and never cancels work', async t => {
  const { create, calls } = fixture(t, c => c.path.endsWith('/events') ? live(c.signal).response : undefined);
  const sb = await create();
  const handle = await sb.commands.connect(pid, { timeoutMs: 20 });
  await delay(40);
  const before = calls.length, error = await prompt(rejected(handle.wait()));
  assert(error instanceof TimeoutError); assert.equal(error.executionId, eid);
  assert.equal(error.code, 'wait_timeout'); assert.equal(handle.exitCode, undefined);
  await handle.disconnect(); assert.equal(await rejected(handle.wait()), error);
  assert.equal(calls.length, before); assert(calls.slice(1).every(c => c.method === 'GET'));
});

test('timeout settles wait even if callback is pending', async t => {
  const entered = deferred(), release = deferred();
  const { create } = fixture(t, c => c.path.endsWith('/events') ? sse(output(1, 'pending') + complete(2)) : undefined);
  const sb = await create();
  const handle = await sb.commands.run('true', { background: true, timeoutMs: 30,
    onStdout: async () => { entered.resolve(); await release.promise; } });
  await prompt(entered.promise);
  const error = await prompt(rejected(handle.wait())); assert(error instanceof TimeoutError);
  release.resolve(); await delay(5); assert.equal(await rejected(handle.wait()), error);
});

test('foreground timeout is precise on wire and preserves execution identity', async t => {
  const { create, calls } = fixture(t, c => c.path.endsWith('/events') ? live(c.signal).response : undefined);
  const sb = await create();
  const error = await rejected(sb.commands.run('sleep 1', { timeoutMs: 30 }));
  assert(error instanceof TimeoutError); assert.equal(error.executionId, eid);
  assert.equal(calls.find(c => c.path.endsWith('/executions')).body.timeout_ms, 30);
  assert.equal(signals(calls).length, 0);
});

test('accepted-response loss retries exactly one invocation despite caller mutation', async t => {
  let attempts = 0;
  const { create, calls } = fixture(t, c => {
    if (c.method === 'POST' && c.path.endsWith('/executions') && ++attempts === 1) {
      return new Response(new ReadableStream({ start(controller) { controller.error(new Error('lost accepted body')); } }), { status: 201 });
    }
  });
  const sb = await create();
  const options = { background: true, timeoutMs: 1250, cwd: '/tmp', envs: { TEST: 'first' } };
  const pending = sb.commands.run('echo once', options);
  options.cwd = '/other'; options.envs.TEST = 'changed'; options.background = false;
  const handle = await pending; assert.equal(handle.executionId, eid); await handle.wait();
  const starts = calls.filter(c => c.method === 'POST' && c.path.endsWith('/executions'));
  assert.equal(starts.length, 2); assert.deepEqual(starts[0].body, starts[1].body);
  assert.equal(starts[0].body.env.TEST, 'first'); assert.equal(starts[0].body.cwd, '/tmp');
  assert.equal(starts[0].body.timeout_ms, 1250); assert(starts[0].body.idempotency_key);
});

test('unknown accepted start retains key and never cancels or fabricates a handle', async t => {
  const { create, calls } = fixture(t, c => {
    if (c.method === 'POST' && c.path.endsWith('/executions')) {
      return new Promise((resolve, reject) => c.signal.addEventListener('abort', () => reject(new Error('held accepted reply')), { once: true }));
    }
  });
  const sb = await create();
  const error = await rejected(sb.commands.run('true', { background: true, timeoutMs: 30 }));
  assert(error instanceof TimeoutError);
  const starts = calls.filter(c => c.path.endsWith('/executions'));
  assert.equal(starts.length, 1); assert.equal(error.idempotencyKey, starts[0].body.idempotency_key);
  assert.equal(starts[0].body.timeout_ms, 30); assert.equal(error.executionId, undefined);
  assert.equal(signals(calls).length, 0);
});

test('missing capability fails closed for all commands; authentication remains distinct', async t => {
  let status = 200;
  const { create, calls } = fixture(t, c => c.path === '/v1/capabilities'
    ? status === 200 ? json({ capabilities: ['unrelated'] }) : failure(status === 401 || status === 403 ? 'decode' : 'not_found', status)
    : undefined);
  for (status of [200, 404, 401, 403]) {
    const sb = await create(), before = calls.length;
    for (const operation of [() => sb.commands.run('true'), () => sb.commands.run('true', { background: true }),
      () => sb.commands.connect(pid), () => sb.commands.kill(pid)]) {
      const error = await rejected(operation());
      if ([401, 403].includes(status)) assert(error instanceof AuthenticationError);
      else assert.equal(error.code, 'incompatible_server');
    }
    assert.deepEqual(commandCalls(calls.slice(before)), []);
  }
});

test('capability cache is scoped to client endpoint and credentials', async t => {
  const { create, calls } = fixture(t, c => {
    if (c.path !== '/v1/capabilities') return;
    if (c.headers.get('Authorization') === 'Bearer wrong') return failure('unauthorized', 401);
    if (new URL(c.url).host === 'old.test') return json({ capabilities: [] });
  });
  const sb = await create(); await sb.commands.run('true'); await sb.commands.run('true');
  assert.equal(calls.filter(c => c.path === '/v1/capabilities').length, 1);
  const wrong = await create({ apiKey: 'wrong' });
  assert(await rejected(wrong.commands.run('true')) instanceof AuthenticationError);
  const old = await create({ endpoint: 'http://old.test' });
  assert.equal((await rejected(old.commands.run('true'))).code, 'incompatible_server');
  assert.equal(calls.filter(c => c.path === '/v1/capabilities').length, 3);
});

test('invalid PID, duration and unsupported options reject before capability I/O', async t => {
  const { create, calls } = fixture(t);
  const sb = await create(), before = calls.length;
  for (const value of [0, -1, true, 1.5, '42', NaN, Infinity, null, undefined, 9007199254740992]) {
    assert(await rejected(sb.commands.connect(value)) instanceof InvalidArgumentError);
    assert(await rejected(sb.commands.kill(value)) instanceof InvalidArgumentError);
  }
  for (const timeoutMs of [-1, 1.5, true, '42', NaN, Infinity, -Infinity, null, 9223372036855, Number.MAX_SAFE_INTEGER]) {
    assert(await rejected(sb.commands.run('true', { background: true, timeoutMs })) instanceof InvalidArgumentError);
    assert(await rejected(sb.commands.connect(pid, { timeoutMs })) instanceof InvalidArgumentError);
  }
  for (const options of [{ background: 'true' }, { background: null }, { stdin: true }, { stdin: 'text' },
    { retainOutput: false }, { onStdout: true }, { onStderr: null }, { unexpected: true },
    { requestTimeoutMs: 100 }, { signal: new AbortController().signal }, { cwd: 'relative' },
    { cwd: '/nul' + String.fromCharCode(0) }, { envs: { BAD: true } }, { envs: { 'A=B': 'invalid' } }, { envs: null }]) {
    assert(await rejected(sb.commands.run('true', options)) instanceof InvalidArgumentError);
  }
  for (const options of [{ background: true }, { stdin: false }, { retainOutput: true }, { requestTimeoutMs: 10 },
    { signal: new AbortController().signal }, { onStdout: 0 }, null, []]) {
    assert(await rejected(sb.commands.connect(pid, options)) instanceof InvalidArgumentError);
  }
  for (const options of [{ timeoutMs: 0 }, { requestTimeoutMs: 10 }, { signal: new AbortController().signal }, null, []]) {
    assert(await rejected(sb.commands.kill(pid, options)) instanceof InvalidArgumentError);
  }
  for (const command of [null, true, 123, 'nul' + String.fromCharCode(0)]) {
    assert(await rejected(sb.commands.run(command)) instanceof InvalidArgumentError);
  }
  assert.equal(calls.length, before);
});

test('unconfirmed start/attach identity and watermark never produce fake handles', async t => {
  let invalid = 'pid';
  const { create, calls } = fixture(t, c => {
    if (c.path.endsWith('/executions')) return json({ ...running, pid: null }, 201);
    if (c.path.includes('/commands/')) return json({
      execution: { ...running, ...(invalid === 'pid' ? { pid: 43 } : {}) }, last_event_id: invalid === 'cursor' ? -1 : 0,
    });
  });
  const sb = await create();
  const error = await rejected(sb.commands.run('true', { background: true }));
  assert.equal(error.code, 'start_unconfirmed'); assert.equal(error.executionId, eid);
  assert.equal((await rejected(sb.commands.connect(pid))).code, 'start_unconfirmed');
  invalid = 'cursor'; assert.equal((await rejected(sb.commands.connect(pid))).code, 'incompatible_server');
  assert.equal(calls.filter(c => c.path.endsWith('/events')).length, 0);
});

test('kill uses immutable execution identity even for disconnected stale handles', async t => {
  let incarnation = eid, signaled = true;
  const { create, calls } = fixture(t, c => {
    if (c.path.endsWith('/events')) return live(c.signal).response;
    if (c.path.includes('/commands/')) return json({ execution: { ...running, id: incarnation }, last_event_id: 0 });
    if (c.path.endsWith('/kill')) return json({ execution: done({
      id: c.path.split('/').at(-2), termination_reason: 'canceled', exit_code: 137 }), signaled });
  });
  const sb = await create();
  const handle = await sb.commands.run('true', { background: true, timeoutMs: 0 });
  await handle.disconnect(); incarnation = 'replacement';
  assert.equal(await handle.kill(), true); assert.equal(await sb.commands.kill(pid), true);
  signaled = false; assert.equal(await handle.kill(), false);
  assert.deepEqual(calls.filter(c => c.path.endsWith('/kill')).map(c => c.path),
    [exec + '/kill', base + '/executions/replacement/kill', exec + '/kill']);
  assert.equal(calls.filter(c => c.path.includes('/commands/')).length, 1);
  assert.equal(calls.filter(c => c.path.endsWith('/cancel')).length, 0);
});

test('failed signal, fallback, nonterminal and lost acknowledgement remain explicit errors', async t => {
  let fault = 'backend';
  const { create, calls } = fixture(t, c => {
    if (c.path.endsWith('/events')) return live(c.signal).response;
    if (!c.path.endsWith('/kill')) return;
    if (fault === 'backend') return failure('backend_error', 502);
    if (fault === 'lost') throw new TypeError('lost kill acknowledgement');
    if (fault === 'decode') return new Response('{');
    if (fault === 'nonterminal') return json({ execution: running, signaled: true });
    if (fault === 'wrong') return json({ execution: done({ id: 'replacement' }), signaled: true });
    if (fault === 'malformed') return json({ execution: done(), signaled: 'true' });
    if (fault === 'missing-evidence') return json({ execution: { id: eid, sandbox_id: sid, state: 'completed' }, signaled: true });
  });
  const sb = await create();
  const handle = await sb.commands.run('true', { background: true, timeoutMs: 0 });
  await handle.disconnect();
  for (fault of ['backend', 'lost', 'decode', 'nonterminal', 'wrong', 'malformed', 'missing-evidence']) {
    const before = calls.length, error = await rejected(handle.kill());
    assert(error instanceof AdapterError); assert.equal(error.executionId, eid);
    assert.equal(error.code, 'backend_error');
    assert.equal(calls.slice(before).filter(c => c.path.endsWith('/kill')).length, 1);
    assert.equal(calls.slice(before).filter(c => c.path.includes('/commands/')).length, 0);
  }
});

test('missing commands differ from missing sandboxes; kill false means known absence', async t => {
  let missingSandbox = false;
  const { create } = fixture(t, c => {
    if (c.path.includes('/commands/')) return failure('not_found', 404);
    if (c.path === base && missingSandbox) return failure('not_found', 404);
  });
  const sb = await create(), error = await rejected(sb.commands.connect(pid));
  assert(error instanceof NotFoundError); assert(!(error instanceof SandboxNotFoundError));
  assert.equal(await sb.commands.kill(pid), false); missingSandbox = true;
  assert(await rejected(sb.commands.connect(pid)) instanceof SandboxNotFoundError);
  assert(await rejected(sb.commands.kill(pid)) instanceof SandboxNotFoundError);
});

test('truncation and abnormal outcomes retain diagnostics, never ordinary success', async t => {
  let reason = 'unknown';
  const { create } = fixture(t, c => {
    if (c.path.endsWith('/events')) return sse(output(1, 'partial') +
      complete(2, { termination_reason: reason, exit_code: reason === 'unknown' ? null : 137 }));
    if (c.path.includes('/commands/')) return json({ execution: done({ output_truncated: true, stdout: 'PRE' }), last_event_id: 2 });
  });
  const sb = await create();
  for (reason of ['oom', 'evicted', 'unknown', 'canceled', 'timeout']) {
    const handle = await sb.commands.run('true', { background: true, timeoutMs: 0 });
    const error = await rejected(handle.wait());
    assert(!(error instanceof CommandExitError)); assert.equal(error.code, reason);
    assert.equal(error.executionId, eid); assert.equal(error.terminationReason, reason); assert.equal(error.stdout, 'partial');
    if (reason === 'unknown') assert.equal(handle.exitCode, undefined);
  }
  const attached = await sb.commands.connect(pid), error = await rejected(attached.wait());
  assert.equal(error.code, 'output_truncated'); assert.equal(error.outputTruncated, true);
  assert.equal(error.executionId, eid); assert.equal(error.stdout, '');
});

test('premature EOF exhausts bounded recovery, not a successful empty result', async t => {
  const { create, calls } = fixture(t, c => c.path.endsWith('/events') ? sse('') : undefined);
  const sb = await create(), handle = await sb.commands.run('true', { background: true, timeoutMs: 0 });
  const error = await rejected(handle.wait());
  assert(error instanceof AdapterError); assert.equal(error.executionId, eid);
  assert.equal(handle.exitCode, undefined); assert.equal(calls.filter(c => c.path.endsWith('/events')).length, 4);
});

test('native capability/attach/kill APIs validate new fields and retain existing defaults', async t => {
  const { calls } = fixture(t);
  const native = new Client({ domain: 'http://sdk.test', apiKey: 'native-key' });
  assert.deepEqual(await native.getCapabilities(), { capabilities: flags });
  assert.equal(calls[0].headers.get('Authorization'), 'Bearer native-key');
  assert.equal((await native.resolveCommand(sid, pid)).execution.pid, pid);
  assert.equal((await native.killExecution(sid, eid)).signaled, true);
  await native.startExecution(sid, { command: ['true'] });
  await native.startExecution(sid, { command: ['true'], timeout_ms: 0 });
  const starts = calls.filter(c => c.path.endsWith('/executions'));
  assert.equal('timeout_ms' in starts[0].body, false); assert.equal('timeout_seconds' in starts[0].body, false);
  assert.equal(starts[1].body.timeout_ms, 0);
  const before = calls.length;
  for (const timeout_ms of [-1, 1.5, true, NaN, Infinity, 9223372036855, null]) {
    assert.equal((await rejected(native.startExecution(sid, { command: ['true'], timeout_ms }))).code, 'invalid_request');
  }
  assert.equal((await rejected(native.startExecution(sid, { command: ['true'], timeout_ms: 0, timeout_seconds: 0 }))).code, 'invalid_request');
  for (const value of [0, -1, true, '42', 1.5, Number.MAX_SAFE_INTEGER + 1]) {
    assert.equal((await rejected(native.resolveCommand(sid, value))).code, 'invalid_request');
  }
  assert.throws(() => native.streamExecution(sid, eid, { lastEventId: -1 }), { code: 'invalid_request' });
  assert.equal(calls.length, before);
});

test('native abort still uses graceful cancel; iterator return never does', async t => {
  const { calls } = fixture(t, c => {
    if (c.path.endsWith('/events')) return live(c.signal).response;
    if (c.path.endsWith('/cancel')) return json(done({ termination_reason: 'canceled', exit_code: 143 }));
  });
  const native = new Client({ domain: 'http://sdk.test' }), controller = new AbortController();
  const iterator = native.streamExecution(sid, eid, { signal: controller.signal }), next = iterator.next();
  controller.abort(); assert.equal((await rejected(next)).code, 'canceled');
  assert.equal(calls.filter(c => c.path.endsWith('/cancel')).length, 1);
  const local = native.streamExecution(sid, eid), waiting = local.next();
  await local.return(); assert.equal((await waiting).done, true);
  assert.equal(calls.filter(c => c.path.endsWith('/cancel')).length, 1);
  assert.equal(calls.filter(c => c.path.endsWith('/kill')).length, 0);
});

test('start stream recovery replays by delivered event ID and foreground preserves callback identity', async t => {
  let attempt = 0;
  const chunks = [];
  const { create, calls } = fixture(t, c => {
    if (c.path.endsWith('/events')) return ++attempt === 1 ? sse(output(1, 'first'))
      : sse(output(1, 'first') + output(2, 'second') + complete(3, { stdout: 'firstsecond' }));
  });
  const sb = await create();
  const handle = await sb.commands.run('true', { background: true, timeoutMs: 0, onStdout: c => { chunks.push(c); } });
  assert.equal((await handle.wait()).stdout, 'firstsecond');
  assert.deepEqual(chunks, ['first', 'second']);
  assert.deepEqual(calls.filter(c => c.path.endsWith('/events')).map(c => c.headers.get('Last-Event-ID')), [null, '1']);
  assert.equal(calls.filter(c => c.path.includes('/commands/')).length, 0);
  const sentinel = new NativeError('canceled', 'user callback, not a native abort');
  assert.equal(await rejected(sb.commands.run('true', { onStdout: () => { throw sentinel; } })), sentinel);
  assert.equal(signals(calls).length, 0);
});

test('pruned observation is command-not-found; terminal identity mismatches never succeed', async t => {
  let mode = 'pruned';
  const { create } = fixture(t, c => {
    if (!c.path.endsWith('/events')) return;
    if (mode === 'pruned') return failure('not_found', 404);
    return sse(complete(1, { id: 'different-execution' }));
  });
  const sb = await create();
  const pruned = await sb.commands.run('true', { background: true, timeoutMs: 0 });
  const error = await rejected(pruned.wait());
  assert(error instanceof NotFoundError); assert(!(error instanceof SandboxNotFoundError));
  assert.equal(error.executionId, eid);
  mode = 'mismatch';
  const wrong = await sb.commands.run('true', { background: true, timeoutMs: 0 });
  assert.equal((await rejected(wrong.wait())).code, 'incomplete'); assert.equal(wrong.exitCode, undefined);
});

test('truncated event stops observation with partial evidence and no remote cleanup', async t => {
  const { create, calls } = fixture(t, c => c.path.endsWith('/events') ? sse(output(1, 'partial') +
    frame(2, 'truncated', { limit_bytes: 1048576 }) + complete(3)) : undefined);
  const sb = await create(), handle = await sb.commands.run('true', { background: true, timeoutMs: 0 });
  const error = await rejected(handle.wait());
  assert.equal(error.code, 'output_truncated'); assert.equal(error.outputTruncated, true);
  assert.equal(error.stdout, 'partial'); assert.equal(error.executionId, eid);
  assert.equal(handle.exitCode, undefined); assert.equal(signals(calls).length, 0);
});

test('malformed capability fails closed and a fresh client can discover support', async t => {
  let malformed = true;
  const { create, calls } = fixture(t, c => {
    if (c.path === '/v1/capabilities' && malformed) return new Response('{');
  });
  const sb = await create();
  assert.equal((await rejected(sb.commands.run('true'))).code, 'incompatible_server');
  assert.equal(commandCalls(calls).length, 0);
  malformed = false;
  const fresh = await create(); await fresh.commands.run('true');
});

test('kill request budget is independent of a timed-out handle; auth remains auth', async t => {
  let mode = 'held';
  const { create, calls } = fixture(t, c => {
    if (c.path.endsWith('/events')) return live(c.signal).response;
    if (c.path.endsWith('/kill')) {
      if (mode === 'held') return new Promise((resolve, reject) =>
        c.signal.addEventListener('abort', () => reject(new Error('lost confirmation')), { once: true }));
      return failure('not_found', 401);
    }
  });
  const sb = await create({ requestTimeoutMs: 25 });
  const handle = await sb.commands.run('true', { background: true, timeoutMs: 10 });
  assert(await rejected(handle.wait()) instanceof TimeoutError);
  const error = await prompt(rejected(handle.kill()));
  assert(error instanceof AdapterError); assert.equal(error.code, 'backend_error'); assert.equal(error.executionId, eid);
  mode = 'auth';
  assert(await rejected(handle.kill()) instanceof AuthenticationError);
  assert.equal(calls.filter(c => c.path.endsWith('/kill')).length, 2);
  assert.equal(calls.filter(c => c.path.includes('/commands/')).length, 0);
});

test('supported maximum deadline never overflows timers and no-option handles reject deferred arguments', async t => {
  const { create, calls } = fixture(t, c => c.path.endsWith('/events') ? live(c.signal).response : undefined);
  const sb = await create();
  const handle = await sb.commands.run('true', { background: true, timeoutMs: 9223372036854, stdin: false, retainOutput: true });
  assert.equal(calls.find(c => c.path.endsWith('/executions')).body.timeout_ms, 9223372036854);
  assert.equal(handle.error, undefined);
  const before = calls.length;
  assert.throws(() => handle.wait({ timeoutMs: 0 }), InvalidArgumentError);
  assert.throws(() => handle.kill({ requestTimeoutMs: 10 }), InvalidArgumentError);
  assert(await rejected(handle.disconnect({ signal: new AbortController().signal })) instanceof InvalidArgumentError);
  assert.equal(calls.length, before); await handle.disconnect();
});

test('native abort rejects unknown terminal cancellation outcome as cancel_unconfirmed', async t => {
  let cancelReason = 'unknown';
  fixture(t, c => {
    if (c.path.endsWith('/events')) return live(c.signal).response;
    if (c.path.endsWith('/cancel')) return json(done({ termination_reason: cancelReason, exit_code: null }));
    if (c.path.endsWith('/kill')) return json({ execution: done({ termination_reason: 'exited', exit_code: 0 }), signaled: true });
  });
  const native = new Client({ domain: 'http://sdk.test', apiKey: 'key-one', maxRetries: -1 });
  for (const reason of ['unknown', 'future_reason']) {
    cancelReason = reason;
    const controller = new AbortController();
    controller.abort();
    const error = await rejected(native.streamExecution(sid, eid, () => {}, { signal: controller.signal }));
    assert.equal(error.code, 'cancel_unconfirmed');
  }
  const killErr = await rejected(native.killExecution(sid, eid));
  assert.equal(killErr.code, 'backend_error');
});

test('automatic cancellation polls only matching running snapshots, without resending a signal', async t => {
  let ack, polls;
  const { calls } = fixture(t, c => {
    if (c.path === exec + '/cancel') return ack;
    if (c.path === exec && c.method === 'GET') return json(polls.shift());
  });
  const native = new Client({ domain: 'http://sdk.test' });
  for (const [reply, observed, code, gets] of [
    [json(running), [running, done()], 'canceled', 2],
    [json({ ...running, id: 'other' }), [done()], 'cancel_unconfirmed', 0],
    [json(running), [{ ...running, sandbox_id: 'other' }, done()], 'cancel_unconfirmed', 1],
    [failure('unavailable', 503), [], 'cancel_unconfirmed', 0],
  ]) {
    ack = reply; polls = observed;
    const before = calls.length;
    const error = await rejected(native.streamExecution(sid, eid, () => {}, { signal: AbortSignal.abort() }));
    assert.equal(error.code, code);
    assert.equal(error.execution.id, eid); assert.equal(error.execution.sandbox_id, sid);
    assert.deepEqual(calls.slice(before).map(c => [c.method, c.path]),
      [['POST', exec + '/cancel'], ...Array(gets).fill(['GET', exec])]);
  }
});

test('an ambiguous start never signals an execution recovered from another sandbox', async t => {
  const controller = new AbortController();
  const { calls } = fixture(t, c => {
    if (c.path !== base + '/executions') return;
    if (!controller.signal.aborted) {
      controller.abort();
      throw new Error('lost start');
    }
    return json({ ...running, sandbox_id: 'other' });
  });
  const native = new Client({ domain: 'http://sdk.test' });
  const error = await rejected(native.startExecution(sid, { command: ['true'], idempotency_key: 'original' }, { signal: controller.signal }));
  assert.equal(error.code, 'cancel_unconfirmed');
  assert.equal(error.idempotencyKey, 'original');
  assert.deepEqual(calls.map(c => [c.path, c.body.idempotency_key]), Array(2).fill([base + '/executions', 'original']));
});

test('file list requires typed metadata, retains unknown fields and accepts zero IDs', async t => {
  const entry = { name: 'child', size: 1, mode: 420, mtime: '2026-11-18T00:00:00Z',
    is_dir: false, type: 'future_kind', uid: 0, gid: 0, future: { x: 1 } };
  let page = { entries: [entry], revision: 3 };
  fixture(t, c => c.path.endsWith('/files/list') ? json(page) : undefined);
  const native = new Client({ domain: 'http://sdk.test' });
  assert.deepEqual(await native.listFiles(sid, '/'), page);
  const malformed = [{ entries: null }, { entries: [], next_cursor: 1 }];
  for (const [key, wrong] of [['type', 0], ['uid', true], ['gid', null], ['owner', 0]]) {
    malformed.push({ entries: [{ ...entry, [key]: wrong }] });
    if (key !== 'owner') malformed.push({ entries: [{ ...entry, [key]: undefined }] });
  }
  for (page of malformed) assert.equal((await rejected(native.listFiles(sid, '/'))).code, 'decode');
});
