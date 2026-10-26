/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// Line diff of two texts (Myers' O(ND) algorithm), for the preview step of
// the unified editor: what changes before a modification is submitted.

function splitLines(text) {
  if (text === undefined || text === null || text === '') return [];
  return String(text).replace(/\r\n/g, '\n').replace(/\n$/, '').split('\n');
}

// Returns the edit script as a list of { type: 'equal'|'add'|'del', text }.
export function diffLines(oldText, newText) {
  const a = splitLines(oldText);
  const b = splitLines(newText);
  const n = a.length;
  const m = b.length;
  const max = n + m;
  const offset = max + 1;
  const v = new Int32Array(2 * max + 3);
  const trace = [];

  let done = false;
  for (let d = 0; d <= max && !done; d++) {
    trace.push(v.slice());
    for (let k = -d; k <= d; k += 2) {
      let x;
      if (k === -d || (k !== d && v[offset + k - 1] < v[offset + k + 1])) {
        x = v[offset + k + 1];
      } else {
        x = v[offset + k - 1] + 1;
      }
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x++;
        y++;
      }
      v[offset + k] = x;
      if (x >= n && y >= m) {
        done = true;
        break;
      }
    }
  }

  // Backtrack through the saved frontiers.
  const ops = [];
  let x = n;
  let y = m;
  for (let d = trace.length - 1; d >= 0; d--) {
    const vd = trace[d];
    const k = x - y;
    let prevK;
    if (k === -d || (k !== d && vd[offset + k - 1] < vd[offset + k + 1])) {
      prevK = k + 1;
    } else {
      prevK = k - 1;
    }
    const prevX = vd[offset + prevK];
    const prevY = prevX - prevK;
    while (x > prevX && y > prevY) {
      ops.push({ type: 'equal', text: a[x - 1] });
      x--;
      y--;
    }
    if (d > 0) {
      if (x === prevX) ops.push({ type: 'add', text: b[y - 1] });
      else ops.push({ type: 'del', text: a[x - 1] });
    }
    x = prevX;
    y = prevY;
  }
  ops.reverse();

  // Line numbers in the old and the new text.
  let oldNo = 0;
  let newNo = 0;
  return ops.map((op) => {
    if (op.type !== 'add') oldNo++;
    if (op.type !== 'del') newNo++;
    return {
      ...op,
      oldNo: op.type === 'add' ? null : oldNo,
      newNo: op.type === 'del' ? null : newNo,
    };
  });
}

// Groups the edit script into hunks with `context` unchanged lines around
// the changes; unchanged runs in between are collapsed into a
// { type: 'skip', count } marker.
export function diffHunks(oldText, newText, context = 3) {
  const ops = diffLines(oldText, newText);
  const keep = new Array(ops.length).fill(false);
  ops.forEach((op, i) => {
    if (op.type === 'equal') return;
    for (
      let j = Math.max(0, i - context);
      j <= Math.min(ops.length - 1, i + context);
      j++
    ) {
      keep[j] = true;
    }
  });
  const lines = [];
  let skipped = 0;
  ops.forEach((op, i) => {
    if (keep[i]) {
      if (skipped) lines.push({ type: 'skip', count: skipped });
      skipped = 0;
      lines.push(op);
    } else {
      skipped++;
    }
  });
  if (skipped && lines.length) lines.push({ type: 'skip', count: skipped });
  return lines;
}

export function diffStats(oldText, newText) {
  const ops = diffLines(oldText, newText);
  return {
    added: ops.filter((op) => op.type === 'add').length,
    removed: ops.filter((op) => op.type === 'del').length,
  };
}
