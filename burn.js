// Stand-in for a gate (tests, typecheck, build): JavaScript work shaped like a test runner or type
// checker (JSON, sorting, maps, regex, lots of garbage) spread over threads and holding memory,
// plus what real gates also do on Windows: start child processes and read many small files.
// usage: node burn.js <units> <threads> <memMB> <ioKBPerUnit> <tmpdir> <spawnEveryNUnits> <filesPerUnit> [js|math]
const { Worker, isMainThread, workerData, parentPort } = require("node:worker_threads");
const fs = require("node:fs");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const FILES = 2000;

function unit(seed) {
  const arr = [];
  for (let i = 0; i < 2000; i++) arr.push({ id: (seed * 7919 + i) % 10007, name: "item" + i, tags: ["a" + (i % 13), "b" + (i % 7)] });
  const back = JSON.parse(JSON.stringify(arr));
  back.sort((a, b) => a.id - b.id || (a.name < b.name ? -1 : 1));
  const m = new Map();
  for (const o of back) m.set(o.name, o.tags.join(","));
  let n = 0;
  for (const v of m.values()) if (/a1[0-2]/.test(v)) n++;
  return n;
}

// mathUnit is shaped like an optimizer step (the plant-fitting jobs): dense Float64 matrix work in
// a tight loop with no garbage, so the thread never stops to wait.
const N = 64;
const ma = new Float64Array(N * N).map((_, i) => (i % 17) / 17);
const mb = new Float64Array(N * N).map((_, i) => (i % 13) / 13);
const mc = new Float64Array(N * N);
function mathUnit(seed) {
  let s = 0;
  for (let r = 0; r < 6; r++) {
    for (let i = 0; i < N; i++)
      for (let j = 0; j < N; j++) {
        let acc = 0;
        for (let k = 0; k < N; k++) acc += ma[i * N + k] * mb[k * N + j];
        mc[i * N + j] = acc * 0.5 + mc[i * N + j] * 0.5 + seed * 1e-9;
      }
    s += mc[r];
  }
  return s > 0 ? 1 : 0;
}

// tree makes the shared folder of small files once (like node_modules); every job reads it.
function tree(tmp) {
  const dir = path.join(tmp, "tree");
  const done = path.join(dir, ".done");
  if (fs.existsSync(done)) return dir;
  fs.mkdirSync(dir, { recursive: true });
  const body = "export const x = " + JSON.stringify("y".repeat(3000)) + ";\n";
  for (let i = 0; i < FILES; i++) {
    const sub = path.join(dir, String(i % 50));
    fs.mkdirSync(sub, { recursive: true });
    const f = path.join(sub, `m${i}.js`);
    if (!fs.existsSync(f)) fs.writeFileSync(f, body);
  }
  fs.writeFileSync(done, "");
  return dir;
}

function work({ units, ioKB, tmp, tag, spawnEvery, files, treeDir, kind }) {
  let n = 0;
  if (kind === "math") {
    for (let u = 0; u < units; u++) n += mathUnit(u);
    return n;
  }
  const file = ioKB > 0 ? path.join(tmp, `burn-${process.pid}-${tag}.tmp`) : "";
  const block = ioKB > 0 ? Buffer.alloc(ioKB * 1024, 7) : null;
  for (let u = 0; u < units; u++) {
    n += unit(u);
    for (let f = 0; f < files; f++) {
      const i = (u * 31 + f * 17 + tag * 7) % FILES;
      const p = path.join(treeDir, String(i % 50), `m${i}.js`);
      fs.statSync(p);
      n += fs.readFileSync(p).length & 1;
    }
    if (block) {
      fs.writeFileSync(file, block);
      n += fs.readFileSync(file).length & 1;
    }
    if (spawnEvery > 0 && u % spawnEvery === spawnEvery - 1) {
      n += spawnSync(process.execPath, ["-e", "0"], { windowsHide: true }).status ?? 1;
    }
  }
  if (file) fs.rmSync(file, { force: true });
  return n;
}

if (isMainThread) {
  const [units = "200", threads = "1", memMB = "0", ioKB = "0", tmp = require("node:os").tmpdir(), spawnEvery = "0", files = "0", kind = "js"] = process.argv.slice(2);
  const t0 = Date.now();
  const hold = [];
  // memory grows over the first half of the run, as a test run's does, not all at once
  const jobs = [
    new Promise((ok) => {
      const grow = setInterval(() => {
        for (let i = 0; i < 2 && hold.length < Number(memMB); i++) hold.push(Buffer.alloc(1 << 20, hold.length & 255));
        if (hold.length >= Number(memMB)) clearInterval(grow), ok(0);
      }, 10);
    }),
  ];
  const treeDir = Number(files) > 0 ? tree(tmp) : "";
  const per = Math.ceil(Number(units) / Number(threads));
  const opts = (tag) => ({ units: per, ioKB: Number(ioKB), tmp, tag, spawnEvery: Number(spawnEvery), files: Number(files), treeDir, kind });
  for (let t = 0; t < Number(threads); t++) // all work on worker threads, so the memory keeps growing meanwhile
    jobs.push(new Promise((ok, bad) => {
      const w = new Worker(__filename, { workerData: opts(t) });
      w.once("message", ok);
      w.once("error", bad);
    }));
  Promise.all(jobs).then((r) => {
    console.log(JSON.stringify({ ms: Date.now() - t0, result: r.reduce((a, b) => a + b, 0), held: hold.length }));
  });
} else {
  parentPort.postMessage(work(workerData));
}
