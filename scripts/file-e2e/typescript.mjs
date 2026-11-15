import assert from 'node:assert/strict';
import { Client } from '../../sandbox-ts/dist/index.js';
const c = new Client(); const names = []; let cursor;
do {
  const page = await c.listFiles(process.argv[2], '/listing', { limit: 2, ...(cursor ? {cursor} : {}) });
  names.push(...page.entries.map(e => e.name)); cursor = page.next_cursor;
} while (cursor);
assert.deepEqual(names, ['.hidden', 'a space', 'empty', 'link', 'nested', 'z', 'é']);
console.log(JSON.stringify({sdk: 'typescript', names}));
