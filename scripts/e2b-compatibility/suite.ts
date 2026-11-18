/** Real SDK bridge. All scenario assertions live in scenarios.py for both providers. */
import assert from 'node:assert/strict';
import { appendFileSync, closeSync, fsyncSync, openSync, readFileSync } from 'node:fs';
import { createInterface } from 'node:readline';
import { createRequire } from 'node:module';
import type * as Reference from 'e2b';

type SDK = typeof Reference;
type Sandbox = InstanceType<SDK['Sandbox']>;
type Query = { op: string; id: string; duration?: number; options?: Record<string, unknown>;
  command: string; callbacks?: boolean; callback_error?: boolean | 'sdk'; path: string; text?: string;
  bytes: string; bytearray?: boolean; raise_error?: boolean; method: string; wait?: number; millis?: number };
const emit = (value: unknown) => process.stdout.write(JSON.stringify(value) + '\n');
const options = JSON.parse(process.env.CONTRACT_OPTIONS || '{}');
// Template/image selection is create-only, so connect never receives it.
const connectOptions = Object.fromEntries(Object.entries(options).filter(([k]) => k !== 'image' && k !== 'template'));
const provider = process.env.CONTRACT_PROVIDER;
let sdk: SDK;
try {
  // A variable import keeps the future adapter optional, but calls below are
  // statically checked against the frozen official declarations.
  const name = provider === 'e2b' ? 'e2b'
    : (process.env.CONTRACT_TS_MODULE || process.env.CONTRACT_MODULE || '@tiny-sandbox/sdk/e2b');
  sdk = await import(name) as SDK;
  const require = createRequire(import.meta.url);
  // The E2B reference is pinned; the future adapter has no published version
  // to pin yet, so its version is reported when resolvable and otherwise null.
  const version = provider === 'e2b' ? JSON.parse(readFileSync(require.resolve('e2b/package.json'), 'utf8')).version : null;
  if (provider === 'e2b') assert.equal(version, '2.64.0');
  emit({ready: true, version, module: name, runtime: process.version});
} catch (error) {
  const e = error as NodeJS.ErrnoException;
  const missing = e.code === 'ERR_MODULE_NOT_FOUND' && /Cannot find package '(e2b|@tiny-sandbox\/sdk)'/.test(e.message);
  emit({ready: false, status: missing ? 'not_run' : 'failed', reason: e.message});
  process.exit(2);
}
const sandboxes = new Map<string, Sandbox>();
function own(s: Sandbox) {
  assert.equal(typeof s.sandboxId, 'string');
  assert(s.sandboxId);
  const fd = openSync(process.env.CONTRACT_OWNED!, 'a');
  try { appendFileSync(fd, JSON.stringify({id: s.sandboxId}) + '\n'); fsyncSync(fd); }
  finally { closeSync(fd); }
  sandboxes.set(s.sandboxId, s);
  return s.sandboxId;
}
function result(r: Reference.CommandResult) {
  assert(Number.isInteger(r.exitCode));
  assert.equal(typeof r.stdout, 'string'); assert.equal(typeof r.stderr, 'string');
  assert(r.error === undefined || typeof r.error === 'string');
  return {exit: r.exitCode, stdout: r.stdout, stderr: r.stderr, has_error: !!r.error};
}
function entry(r: Reference.WriteInfo, full = false) {
  assert.equal(typeof r.name, 'string'); assert.equal(typeof r.path, 'string');
  assert(r.type === undefined || Object.values(sdk.FileType).includes(r.type));
  const data: Record<string, unknown> = {name: r.name, path: r.path, type: r.type ?? null};
  if (full) {
    const e = r as Reference.EntryInfo;
    assert(Number.isInteger(e.size) && e.size >= 0); assert(Number.isInteger(e.mode));
    for (const k of ['permissions', 'owner', 'group'] as const) assert.equal(typeof e[k], 'string');
    assert(e.modifiedTime instanceof Date && Number.isFinite(e.modifiedTime.getTime()));
    Object.assign(data, {size:e.size, mode:e.mode, permissions:e.permissions, owner:e.owner, group:e.group,
      modified:e.modifiedTime.getTime()/1000, symlink:e.symlinkTarget ?? null});
  }
  return data;
}
function failure(e: unknown) {
  // A future adapter need not export every reference error class, so map by
  // name through a tolerant instanceof and never assume the export exists.
  const is = (name: string) => {
    const cls = (sdk as unknown as Record<string, unknown>)[name];
    return typeof cls === 'function' && e instanceof (cls as new (...a: never[]) => object);
  };
  const family = is('CommandExitError') ? 'exit' : is('SandboxNotFoundError') ? 'sandbox_not_found'
    : is('FileNotFoundError') ? 'file_not_found' : is('AuthenticationError') ? 'auth'
    : is('InvalidArgumentError') ? 'invalid' : is('TimeoutError') ? 'timeout' : 'other';
  const data: Record<string, unknown> = {family, class:e instanceof Error ? e.name : typeof e, message:String(e)};
  if (family === 'exit') Object.assign(data, result(e as unknown as Reference.CommandResult));
  const fields = {code:'code', executionId:'execution_id', idempotencyKey:'idempotency_key',
    terminationReason:'termination_reason', outputTruncated:'output_truncated', stdout:'stdout', stderr:'stderr'};
  for (const [key, target] of Object.entries(fields)) {
    const value = (e as Record<string, unknown>)?.[key];
    if (['string','number','boolean'].includes(typeof value)) data[target] = value;
  }
  return data;
}
type Callbacks = {stdout: string[]; stderr: string[]; times: number[]};
async function dispatch(q: Query, callbacks: Callbacks) {
  const opts: Record<string, unknown> = {...q.options};
  // This is spelling/unit translation only. The application scenarios never
  // branch by provider and do not emulate missing SDK behavior.
  const names: Record<string, string> = {retain_output:'retainOutput', request_timeout:'requestTimeoutMs'};
  for (const [python, ts] of Object.entries(names)) if (python in opts) { opts[ts] = opts[python]; delete opts[python]; }
  if (q.duration !== undefined) opts.timeoutMs = q.duration * 1000;
  if (q.millis !== undefined) opts.timeoutMs = q.millis;
  if (q.op === 'create' || q.op === 'context') {
    const template = opts.template; delete opts.template;
    const s = template === undefined ? await sdk.Sandbox.create({...options, ...opts})
      : await sdk.Sandbox.create(template as string, {...options, ...opts});
    const id = own(s);
    if (q.op === 'context') {
      const sentinel = new Error('application-sentinel');
      let caught = false;
      try {
        try { if (q.raise_error) throw sentinel; } finally { assert.equal(await s.kill(), true); }
      } catch (e) { assert.equal(e, sentinel); caught = true; }
      assert.equal(caught, !!q.raise_error);
      return {id, application_error:caught};
    }
    return {id};
  }
  if (q.op === 'connect') {
    const s = await sdk.Sandbox.connect(q.id, {...connectOptions, ...opts});
    assert.equal(s.sandboxId, q.id); sandboxes.set(q.id, s); return {id:s.sandboxId};
  }
  const s = sandboxes.get(q.id)!;
  if (q.op === 'kill') { const v = await s.kill(opts); assert.equal(typeof v, 'boolean'); return v; }
  if (q.op === 'timeout') {
    const { timeoutMs, ...requestOptions } = opts;
    await s.setTimeout(timeoutMs as number, requestOptions); return null;
  }
  if (q.op === 'info') {
    const i = await sdk.Sandbox.getInfo(q.id);
    assert.equal(i.sandboxId, q.id);
    return {id:i.sandboxId, state:String(i.state), template:i.templateId, end:i.endAt.getTime()/1000};
  }
  if (q.op === 'run') {
    if (q.wait !== undefined) opts.timeoutMs = q.wait * 1000;
    const sentinel = q.callback_error === 'sdk' ? new sdk.TimeoutError('callback-sentinel') : new Error('callback-sentinel');
    let active = false;
    const callback = async (stream: 'stdout'|'stderr', chunk: string) => {
      assert.equal(typeof chunk, 'string');
      assert(!active, 'TypeScript callbacks must be awaited in sequence');
      active = true;
      await new Promise(resolve => setTimeout(resolve, 1));
      callbacks[stream].push(chunk); callbacks.times.push(performance.now()/1000); active = false;
      if (q.callback_error) throw sentinel;
    };
    if (q.callbacks || q.callback_error) Object.assign(opts, {onStdout:(c:string) => callback('stdout',c), onStderr:(c:string) => callback('stderr',c)});
    try {
      const value = result(await s.commands.run(q.command, opts));
      assert(!active, 'run returned before its callback finished'); return value;
    } catch (e) { if (e === sentinel) return {callback_error_preserved:true}; throw e; }
  }
  if (q.op === 'write') return entry(await s.files.write(q.path, q.text === undefined
    ? (q.bytearray ? new Uint8Array(Buffer.from(q.bytes, 'base64')) : Buffer.from(q.bytes, 'base64')) : q.text, opts));
  if (q.op === 'read') {
    if (opts.format === 'bytes') {
      const v = await s.files.read(q.path, {...opts, format:'bytes'});
      assert(v instanceof Uint8Array); return {bytes:Buffer.from(v).toString('base64')};
    }
    const v = await s.files.read(q.path, opts); assert.equal(typeof v, 'string'); return v;
  }
  if (q.op === 'list') { const v = await s.files.list(q.path, opts); assert(Array.isArray(v)); return v.map(e => entry(e,true)); }
  if (q.op === 'unsupported') return (s as unknown as Record<string, () => unknown>)[q.method]();
  throw new Error('unknown harness operation');
}
for await (const line of createInterface({input:process.stdin})) {
  const callbacks: Callbacks = {stdout:[], stderr:[], times:[]};
  const start = performance.now();
  let response;
  try { response = {ok:true, value:await dispatch(JSON.parse(line), callbacks)}; }
  catch (e) { response = {ok:false, error:failure(e)}; }
  emit({...response, callbacks, elapsed:(performance.now()-start)/1000});
}
