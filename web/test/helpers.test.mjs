import test from 'node:test';
import assert from 'node:assert/strict';
import { lockLabel, buildRows, filterRows, duration, isZeroTime, readConfig, splitName, countLong } from '../helpers.js';

const st = {
  inputs: ['Camera 1', 'Camera 2'],
  outputs: ['Monitor A', 'Monitor B', 'Record'],
  route: [1, -1, 0],
  lock: ['U', 'O', 'L'],
};

test('lockLabel', () => {
  assert.equal(lockLabel('U'), 'Unlocked');
  assert.equal(lockLabel('O'), 'Locked (this client)');
  assert.equal(lockLabel('L'), 'Locked (other)');
  assert.equal(lockLabel(undefined), 'Unlocked');
});

test('buildRows numbers from 1 and handles unrouted', () => {
  const r = buildRows(st);
  assert.deepEqual(r[0], { out: 1, name: 'Monitor A', inNum: 2, inName: 'Camera 2', lock: 'Unlocked' });
  assert.equal(r[1].inNum, null);
  assert.equal(r[1].lock, 'Locked (this client)');
  assert.equal(buildRows({}).length, 0);
});

test('filterRows', () => {
  const r = buildRows(st);
  assert.equal(filterRows(r, '').length, 3);
  assert.deepEqual(filterRows(r, 'MONITOR').map((x) => x.out), [1, 2]);
  assert.deepEqual(filterRows(r, 'camera 1 rec').map((x) => x.out), [3]);
  assert.deepEqual(filterRows(r, 'monitor b').map((x) => x.out), [2]);
  assert.deepEqual(filterRows(r, 'other').map((x) => x.out), [3]);
  assert.deepEqual(filterRows(r, '3').map((x) => x.out), [3]);
  assert.equal(filterRows(r, 'zzz').length, 0);
});

test('duration', () => {
  assert.equal(duration(5), '5s');
  assert.equal(duration(65), '1m 05s');
  assert.equal(duration(3700), '1h 01m');
  assert.equal(duration(90000), '1d 1h');
  assert.equal(duration(-3), '0s');
});

test('isZeroTime', () => {
  assert.ok(isZeroTime('0001-01-01T00:00:00Z'));
  assert.ok(isZeroTime(''));
  assert.ok(!isZeroTime('2026-10-28T10:00:00Z'));
});

test('readConfig', () => {
  const ok = readConfig({ hub: ' 10.0.0.5 ', protect: '1', resync: '60' });
  assert.deepEqual(ok.cfg, { hub: '10.0.0.5', protectDevice: 1, resyncSeconds: 60 });
  assert.match(readConfig({ hub: '', protect: '1', resync: '4' }).error, /Resync/);
  assert.match(readConfig({ hub: '', protect: 'x', resync: '60' }).error, /Protect/);
  assert.match(readConfig({ hub: '', protect: '1024', resync: '60' }).error, /Protect/);
});

test('splitName cuts at 16 characters and counts characters not UTF-16 units', () => {
  assert.deepEqual(splitName('SWR ME1 PGM'), { head: 'SWR ME1 PGM', tail: '' });
  assert.deepEqual(splitName('ABCDEFGHIJKLMNOPQR'), { head: 'ABCDEFGHIJKLMNOP', tail: 'QR' });
  assert.deepEqual(splitName('1234567890123456'), { head: '1234567890123456', tail: '' });
  assert.equal(splitName('😀'.repeat(17)).tail, '😀');
  assert.deepEqual(splitName(null), { head: '', tail: '' });
});

test('countLong counts names past 16 characters', () => {
  assert.equal(countLong(['short', 'x'.repeat(17), 'y'.repeat(16), 'z'.repeat(40)]), 2);
});
