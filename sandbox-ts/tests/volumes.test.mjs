import assert from 'node:assert/strict';
import { test } from 'node:test';
import { Client, SandboxError } from '@execlet/sdk';

const caps = 'GET /v1/capabilities';
const createSandbox = 'POST /v1/sandboxes';
const createVolume = 'POST /v1/volumes';
const listVolumes = 'GET /v1/volumes';
const size = 2 ** 34;
const volume = { id: 'vol_a/b?c', size_bytes: size, created_at: '2026-11-24T12:00:00Z' };
const mounts = [
  { volume_id: 'vol_a', path: '/mnt/data' },
  { volume_id: 'vol_b', path: '/mnt/input', read_only: true },
  { volume_id: 'vol_c', path: '/mnt/output', read_only: false },
];

function fixture(replies, maxRetries = -1) {
  const calls = [];
  const client = new Client({ domain: 'http://sdk.test', maxRetries, fetch: async (url, init) => {
    const key = `${init.method} ${new URL(url).pathname}`;
    calls.push([key, init.body === undefined ? undefined : JSON.parse(init.body)]);
    const reply = replies[key].shift();
    if (reply instanceof Error) throw reply;
    const [status, data] = reply;
    return status === 204 ? new Response(null, { status }) : Response.json(data, { status });
  } });
  return { client, calls };
}

test('native volume methods discover capability and use the volume wire models', async () => {
  const get = 'GET /v1/volumes/vol_a%2Fb%3Fc', del = 'DELETE /v1/volumes/vol_a%2Fb%3Fc';
  const { client, calls } = fixture({
    [caps]: [[200, { capabilities: ['volumes.v1'] }]],
    [createVolume]: [[201, volume]],
    [listVolumes]: [[200, { volumes: [volume] }]],
    [get]: [[200, volume]],
    [del]: [[204]],
  });
  assert.deepEqual(await client.createVolume({ size_bytes: size }), volume);
  assert.deepEqual(await client.listVolumes(), [volume]);
  assert.deepEqual(await client.getVolume(volume.id), volume);
  assert.equal(await client.deleteVolume(volume.id), undefined);
  assert.deepEqual(calls, [
    [caps, undefined], [createVolume, { size_bytes: size }], [listVolumes, undefined],
    [get, undefined], [del, undefined],
  ]);
});

test('mounted sandbox creation snapshots mount payloads and batches capability discovery', async () => {
  const snapshot = { id: 'sbx_a', state: 'running', mounts };
  const get = 'GET /v1/sandboxes/sbx_a';
  const { client, calls } = fixture({
    [caps]: [[200, { capabilities: ['volumes.v1', 'sandbox.runtime.v1', 'execution.env-cwd.v1', 'execution.concurrent.v1'] }]],
    [createSandbox]: [[201, snapshot]],
    [get]: [[200, snapshot]],
  });
  const request = { mounts: mounts.map(mount => ({ ...mount })), runtime: 'runc', env: { A: 'B' }, max_concurrent_executions: 2 };
  const created = client.createSandbox(request);
  request.mounts[0].path = '/mnt/changed';
  request.mounts.pop();
  assert.deepEqual((await created).mounts, mounts);
  assert.deepEqual((await client.getSandbox('sbx_a')).mounts, mounts);
  assert.deepEqual(calls, [
    [caps, undefined], [createSandbox, { ...request, mounts }], [get, undefined],
  ]);
});

test('unsupported volume operations fail closed before any volume request or sandbox POST', async () => {
  for (const reply of [[404, { code: 'not_found' }], [200, { capabilities: ['execution.env-cwd.v1'] }]]) {
    const { client, calls } = fixture({
      [caps]: Array(5).fill(reply),
      [createSandbox]: [[201, { id: 'sbx_a', state: 'running' }]],
    });
    for (const operation of [
      () => client.createVolume({ size_bytes: size }),
      () => client.listVolumes(),
      () => client.getVolume(volume.id),
      () => client.deleteVolume(volume.id),
      () => client.createSandbox({ mounts, env: { A: 'B' } }),
    ]) {
      await assert.rejects(operation, { name: 'SandboxError', code: 'incompatible_server' });
    }
    assert.deepEqual(calls, Array(reply[0] === 404 ? 5 : 1).fill([caps, undefined]));
    assert.equal((await client.createSandbox({ mounts: [] })).mounts, undefined);
    assert.deepEqual(calls.at(-1), [createSandbox, {}]);
  }
});

test('invalid volume arguments are rejected without sending an HTTP request', async () => {
  const { client, calls } = fixture({});
  for (const operation of [
    () => client.createVolume({ size_bytes: Number.MAX_SAFE_INTEGER + 1 }),
    () => client.createVolume({ size_bytes: size + 1 }),
    () => client.createSandbox({ mounts: [{ ...mounts[0], read_only: 'yes' }] }),
    () => client.createSandbox({ mounts: [mounts[0], { volume_id: 'vol_child', path: '/mnt/data/child' }] }),
    () => client.deleteVolume('..'),
  ]) {
    await assert.rejects(operation, { name: 'SandboxError', code: 'invalid_request' });
  }
  assert.deepEqual(calls, []);
});

test('mount paths accept 4096 bytes and reject 4097 before discovery', async () => {
  const path = '/mnt/' + 'x/'.repeat(2045) + 'x';
  const create = path => ({ mounts: [{ volume_id: 'vol_a', path }] });
  const { client, calls } = fixture({
    [caps]: [[200, { capabilities: ['volumes.v1'] }]],
    [createSandbox]: [[201, { id: 'sbx_a', state: 'running' }]],
  });
  await assert.rejects(client.createSandbox(create(path + 'x')), { code: 'invalid_request' });
  assert.deepEqual(calls, []);
  await client.createSandbox(create(path));
  assert.deepEqual(calls.at(-1), [createSandbox, create(path)]);
});

test('delete retries explicit 503 but not a lost response or unknown ID', async () => {
  const del = 'DELETE /v1/volumes/vol_a', missing = [404, { code: 'not_found' }];
  for (const [responses, code, count] of [
    [[new TypeError('response lost'), missing], 'transport', 1],
    [[missing], 'not_found', 1],
    [[[504, { code: 'gateway_timeout' }], missing], 'gateway_timeout', 1],
    [[[503, { code: 'unavailable' }], [204]], null, 2],
  ]) {
    const { client, calls } = fixture({ [caps]: [[200, { capabilities: ['volumes.v1'] }]], [del]: responses }, 1);
    if (code) await assert.rejects(client.deleteVolume('vol_a'), { code });
    else await client.deleteVolume('vol_a');
    assert.equal(calls.filter(([key]) => key === del).length, count);
  }
});

test('volume creation is not retried, and open error codes survive the transport', async () => {
  const del = 'DELETE /v1/volumes/vol_a';
  const { client, calls } = fixture({
    [caps]: [[200, { capabilities: ['volumes.v1'] }]],
    [createVolume]: [[503, { code: 'unavailable' }]],
    [listVolumes]: [[503, { code: 'unavailable' }], [200, { volumes: [] }]],
    [del]: [[409, { code: 'volume_in_use' }], [409, { code: 'future_volume_error' }]],
  }, 1);
  await assert.rejects(client.createVolume({ size_bytes: size }), { code: 'unavailable', statusCode: 503 });
  assert.deepEqual(await client.listVolumes(), []);
  for (const code of ['volume_in_use', 'future_volume_error']) {
    await assert.rejects(client.deleteVolume('vol_a'), error =>
      error instanceof SandboxError && error.code === code && error.statusCode === 409);
  }
  assert.deepEqual(calls.map(([key]) => key), [caps, createVolume, listVolumes, listVolumes, del, del]);
});
