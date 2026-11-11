// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0
//
// Reads the pairs.csv files of the Dolos runs and writes only the file pairs
// across the two sides (Norca, HashiCorp) with their scores: /out/pairs.csv
// and /out/summary.md. No source code leaves the container.
//
// For the busl set it also computes, per BUSL file, the MPL baseline: the
// highest similarity of HashiCorp's own MPL files (imported into Norca) to
// it. A file Norca added is a candidate for the auditor only if it is at or
// above the threshold and above that baseline; same-path pairs also get a
// character-level similarity (set busl-char), which counts names and
// literals as well. Files of kind "restored" (HashiCorp's pre-switch MPL
// code that v1.6.5 replaced with a BUSL file, brought back nearly unchanged)
// are never candidates; they are listed on their own.
//
// For the full set (Norca's new and changed files against every file of the
// BSL release) a changed file is a candidate only if it is at or above the
// threshold and more similar to the BSL file than its own v1.6.5 file (the
// base side) is; a new file if it is at or above the threshold.
//
// usage: node report.mjs <result dir> <out dir> <threshold>  (metadata on stdin)
import { createReadStream, existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createInterface } from "node:readline";

const [resultDir, outDir, thresholdArg] = process.argv.slice(2);
const threshold = Number(thresholdArg);
const meta = readFileSync(0, "utf8").trim().split("\n").map((l) => l.split(/=(.*)/s).slice(0, 2));

// parseLine splits one CSV line (Dolos quotes the paths; no field holds a
// line break).
function parseLine(line) {
  const out = [];
  let field = "", quoted = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (quoted) {
      if (c === '"' && line[i + 1] === '"') { field += '"'; i++; }
      else if (c === '"') quoted = false;
      else field += c;
    } else if (c === '"') quoted = true;
    else if (c === ",") { out.push(field); field = ""; }
    else if (c !== "\r") field += c;
  }
  out.push(field);
  return out;
}

// csvRows reads a CSV file line by line: the pairs.csv of the full set is too
// large to read at once.
async function* csvRows(file) {
  let head;
  for await (const line of createInterface({ input: createReadStream(file), crlfDelay: Infinity })) {
    if (!line) continue;
    const r = parseLine(line);
    if (!head) { head = r; continue; }
    if (r.length === head.length) yield Object.fromEntries(head.map((h, i) => [h, r[i]]));
  }
}

// The full set keeps only pairs of at least half the threshold.
const fullFloor = threshold / 2;

// side splits /work/<set>/<norca|upstream|base>/<path>.<lang>.src.
function side(p) {
  const m = p.match(/\/work\/([^/]+)\/(norca|upstream|base)\/(.*)\.[a-z]+\.src$/);
  return m ? { set: m[1], side: m[2], path: m[3] } : null;
}

// full-kinds.tsv: the v1.6.5 path of a Norca file of the full set, "new" or
// "modified".
const fullKinds = new Map();
const fullKindsFile = join(resultDir, "full-kinds.tsv");
if (existsSync(fullKindsFile)) {
  for (const l of readFileSync(fullKindsFile, "utf8").split("\n")) {
    const [p, k] = l.split("\t");
    if (p) fullKinds.set(p, k);
  }
}
// baseSim: the similarity of a v1.6.5 file (base side) to a BSL file.
const baseSim = new Map();

const kinds = new Map();
const kindsFile = join(resultDir, "busl-kinds.tsv");
if (existsSync(kindsFile)) {
  for (const l of readFileSync(kindsFile, "utf8").split("\n")) {
    const [p, k] = l.split("\t");
    if (p) kinds.set(p, k);
  }
}

// busl-restored.tsv: upstream path, lines (the longer of the two), changed lines.
const restored = new Map();
const restoredFile = join(resultDir, "busl-restored.tsv");
if (existsSync(restoredFile)) {
  for (const l of readFileSync(restoredFile, "utf8").split("\n")) {
    const [p, lines, changed] = l.split("\t");
    if (p) restored.set(p, { lines: Number(lines), changed: Number(changed) });
  }
}

const all = [];
for (const f of readdirSync(resultDir).filter((n) => n.endsWith(".pairs.csv"))) {
  const name = f.replace(/\.pairs\.csv$/, "");
  const lang = name.slice(name.lastIndexOf("-") + 1);
  for await (const r of csvRows(join(resultDir, f))) {
    const a = side(r.leftFilePath), b = side(r.rightFilePath);
    if (!a || !b || a.side === b.side) continue;
    if (a.set === "full" && Number(r.similarity) < fullFloor) continue;
    if (a.side === "base" || b.side === "base") {
      // Only base against upstream counts: the baseline of a changed file.
      const [x, u] = a.side === "base" ? [a, b] : [b, a];
      if (u.side === "upstream") baseSim.set(`${x.path}\t${u.path}`, Number(r.similarity));
      continue;
    }
    const [n, u] = a.side === "norca" ? [a, b] : [b, a];
    // bugfix paths start with the 12-character commit prefix.
    const norcaPath = n.set === "bugfix" ? n.path.replace(/^([0-9a-f]{12})\//, "$1 ") : n.path;
    const samePath = n.set === "bugfix" ? n.path.slice(13) === u.path : n.path === u.path;
    all.push({
      set: n.set, language: lang, norca: norcaPath, upstream: u.path, samePath,
      similarity: Number(r.similarity), longest: Number(r.longestFragment), overlap: Number(r.totalOverlap),
      kind: n.set === "busl" ? kinds.get(n.path) ?? "" : n.set === "full" ? fullKinds.get(n.path) ?? "" : "",
    });
  }
}

// Character-level similarity of the same-path busl pairs.
const charSim = new Map(all.filter((p) => p.set === "busl-char" && p.samePath).map((p) => [p.upstream, p.similarity]));
const pairs = all.filter((p) => p.set !== "busl-char");
pairs.sort((x, y) => y.similarity - x.similarity);

const baseline = new Map();
for (const p of pairs) {
  if (p.set === "busl" && p.kind === "mpl") baseline.set(p.upstream, Math.max(baseline.get(p.upstream) ?? 0, p.similarity));
}
for (const p of pairs) {
  if (p.set !== "busl") continue;
  p.baseline = baseline.get(p.upstream) ?? 0;
  p.charSimilarity = p.samePath ? charSim.get(p.upstream) : undefined;
  p.candidate = p.kind === "new" && p.similarity >= threshold && p.similarity > p.baseline;
}

for (const p of pairs) {
  if (p.set !== "full") continue;
  if (p.kind === "modified") p.baseline = baseSim.get(`${p.norca}\t${p.upstream}`) ?? 0;
  p.candidate = p.similarity >= threshold && (p.kind === "new" || p.similarity > p.baseline);
}

const q = (s) => `"${String(s).replaceAll('"', '""')}"`;
const num = (v) => (v === undefined ? "" : v.toFixed(4));
writeFileSync(join(outDir, "pairs.csv"),
  "set,language,norca_file,upstream_file,similarity,longest_fragment,total_overlap,same_path,norca_kind,mpl_baseline,char_similarity,candidate\n" +
  pairs.map((p) => [p.set, p.language, q(p.norca), q(p.upstream), p.similarity.toFixed(4), p.longest, p.overlap, p.samePath,
    p.kind, num(p.baseline), num(p.charSimilarity), p.candidate ?? ""].join(",")).join("\n") + "\n");

const md = ["# Clean room similarity audit", "", "| Property | Value |", "| --- | --- |",
  ...meta.map(([k, v]) => `| ${k} | \`${v}\` |`), ""];
const fmt = (v) => (v === undefined ? "–" : v.toFixed(3));

const busl = pairs.filter((p) => p.set === "busl");
if (busl.length) {
  const cands = busl.filter((p) => p.candidate);
  md.push("## busl", "",
    `${busl.length} cross pairs, ${busl.filter((p) => p.similarity >= threshold).length} at or above ${threshold}; ` +
    `${cands.length} candidates for the auditor: Norca-added files at or above the threshold and above the MPL baseline ` +
    "(the highest similarity of HashiCorp's own MPL files to the same BUSL file).", "",
    "### Candidates", "",
    "| Norca file | BUSL file | Similarity | MPL baseline | Character level | Longest fragment | Total overlap |",
    "| --- | --- | --- | --- | --- | --- | --- |",
    ...cands.map((p) => `| ${p.norca} | ${p.upstream} | ${fmt(p.similarity)} | ${fmt(p.baseline)} | ${fmt(p.charSimilarity)} | ${p.longest} | ${p.overlap} |`),
    ...(cands.length ? [] : ["| – | – | – | – | – | – | – |"]), "");
  // The highest pair of every restored file.
  const best = new Map();
  for (const p of busl.filter((p) => p.kind === "restored")) {
    if (!best.has(p.norca) || p.similarity > best.get(p.norca).similarity) best.set(p.norca, p);
  }
  md.push("### Restored MPL files", "",
    "Not in the v1.6.5 tree, but the MPL anchor (the import's second parent) has the same path, and the Norca " +
    "file differs from it in at most 10% of the lines once the module path and the Norca header lines are " +
    "removed. A BUSL file that grew out of the MPL file resembles it; these are not candidates.", "",
    "| Norca file | Highest BUSL pair | Similarity | Lines | Changed lines |", "| --- | --- | --- | --- | --- |",
    ...[...best.values()].map((p) => {
      const r = restored.get(p.norca) ?? {};
      return `| ${p.norca} | ${p.upstream} | ${fmt(p.similarity)} | ${r.lines ?? "–"} | ${r.changed ?? "–"} |`;
    }),
    ...(best.size ? [] : ["| – | – | – | – | – |"]), "",
    "### Same-path pairs (clean room replacements)", "",
    "| File | Similarity | MPL baseline | Character level | Longest fragment |", "| --- | --- | --- | --- | --- |",
    ...busl.filter((p) => p.samePath).map((p) => `| ${p.norca} | ${fmt(p.similarity)} | ${fmt(p.baseline)} | ${fmt(p.charSimilarity)} | ${p.longest} |`), "");
}

const bugfix = pairs.filter((p) => p.set === "bugfix");
if (bugfix.length) {
  md.push("## bugfix", "",
    `${bugfix.length} cross pairs, ${bugfix.filter((p) => p.similarity >= threshold).length} at or above ${threshold}; same-path pairs: ${bugfix.filter((p) => p.samePath).length}.`, "",
    "| Norca commit and file | HashiCorp file | Similarity | Longest fragment | Total overlap |", "| --- | --- | --- | --- | --- |",
    ...bugfix.filter((p) => p.similarity > 0).slice(0, 25).map((p) => `| ${p.norca} | ${p.upstream} | ${fmt(p.similarity)} | ${p.longest} | ${p.overlap} |`), "");
}
const full = pairs.filter((p) => p.set === "full");
if (full.length || fullKinds.size) {
  const cands = full.filter((p) => p.candidate);
  const kindCount = (k) => [...fullKinds.values()].filter((v) => v === k).length;
  md.push("## full", "",
    `${kindCount("new")} new and ${kindCount("modified")} changed Norca files against every file of the BSL release ` +
    `with the same language, whatever its path; pairs below ${threshold / 2} are not reported. ` +
    `${full.length} cross pairs, ${full.filter((p) => p.similarity >= threshold).length} at or above ${threshold}; ` +
    `${cands.length} candidates for the auditor: a new file at or above the threshold, or a changed file at or above ` +
    "the threshold and more similar to the BSL file than its own v1.6.5 file is.", "",
    "### Candidates", "",
    "| Norca file | Kind | BSL file | Similarity | v1.6.5 base | Longest fragment | Total overlap |",
    "| --- | --- | --- | --- | --- | --- | --- |",
    ...cands.map((p) => `| ${p.norca} | ${p.kind} | ${p.upstream} | ${fmt(p.similarity)} | ${fmt(p.baseline)} | ${p.longest} | ${p.overlap} |`),
    ...(cands.length ? [] : ["| – | – | – | – | – | – | – |"]), "",
    "### Highest pairs", "",
    "| Norca file | Kind | BSL file | Similarity | v1.6.5 base | Same path |", "| --- | --- | --- | --- | --- | --- |",
    ...full.slice(0, 25).map((p) => `| ${p.norca} | ${p.kind} | ${p.upstream} | ${fmt(p.similarity)} | ${fmt(p.baseline)} | ${p.samePath} |`),
    ...(full.length ? [] : ["| – | – | – | – | – | – |"]), "");
}
if (!pairs.length) md.push("No cross pairs were found.", "");
writeFileSync(join(outDir, "summary.md"), md.join("\n"));
console.log(`${pairs.length} cross pairs, ${pairs.filter((p) => p.similarity >= threshold).length} at or above ${threshold}, ` +
  `${pairs.filter((p) => p.candidate && p.set === "busl").length} busl candidates, ` +
  `${pairs.filter((p) => p.candidate && p.set === "full").length} full candidates`);
