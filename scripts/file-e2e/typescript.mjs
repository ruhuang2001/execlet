import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import { Client } from '../../sandbox-ts/dist/index.js';
const c = new Client(); const names = []; let cursor;
do {
  const page = await c.listFiles(process.argv[2], '/listing', { limit: 2, ...(cursor ? {cursor} : {}) });
  names.push(...page.entries.map(e => e.name)); cursor = page.next_cursor;
} while (cursor);
assert.deepEqual(names, ['.hidden', 'a space', 'empty', 'link', 'nested', 'z', 'é']);
console.log(JSON.stringify({sdk: 'typescript', names}));

if (process.argv[3] === 'wide') {
  const { Sandbox } = await import('../../sandbox-ts/dist/e2b.js');
  const sb = await Sandbox.connect(process.argv[2]);
  const written = await sb.files.write('/wide/typescript.txt', 'ok');
  assert.deepEqual(written, {name:'typescript.txt',path:'/wide/typescript.txt',type:'file'});
  console.log(JSON.stringify(written));
}

if (process.argv[3] === 'archive') {
  await c.writeArchive(process.argv[2], '/archives/typescript', readFileSync(process.argv[4]));
  const result = await c.readArchive(process.argv[2], '/archives/typescript/tree');
  assert.ok(result.byteLength >= 1024);
  assert.ok(Buffer.from(result).includes(Buffer.from([0,255,98,105,110,97,114,121,10])));
  console.log(JSON.stringify({archive: 'passed'}));
}
