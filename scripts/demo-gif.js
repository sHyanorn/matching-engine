// Records docs/img/demo.gif: the order book page while loadgen trades.
//
//   node scripts/demo-gif.js [OUT.gif]
//
// Needs Go, Node with the playwright package and its Chromium, and ffmpeg
// (or FFMPEG=path). It starts a fresh engine and loadgen on a free port,
// captures PNG frames of the page, and encodes them with one palette:
// frames from a video recording carry compression noise that makes the
// GIF several times larger.
"use strict";
const { chromium } = require("playwright");
const { execFileSync, spawn } = require("child_process");
const fs = require("fs");
const os = require("os");
const path = require("path");

const out = process.argv[2] || path.join("docs", "img", "demo.gif");
const ffmpeg = process.env.FFMPEG || "ffmpeg";
const addr = "127.0.0.1:18090";
const size = { width: 800, height: 840 };
const seconds = 8, fps = 10;

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "demo-gif-"));
const exe = name => path.join(tmp, name + (process.platform === "win32" ? ".exe" : ""));
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  for (const cmd of ["engine", "loadgen"]) {
    execFileSync("go", ["build", "-o", exe(cmd), "./cmd/" + cmd], { stdio: "inherit" });
  }
  const engine = spawn(exe("engine"), ["-addr", addr, "-data", path.join(tmp, "data"), "-symbols", "BTC-USD"], { stdio: "ignore" });
  let loadgen;
  const browser = await chromium.launch();
  try {
    await waitHealthy();
    loadgen = spawn(exe("loadgen"), ["-addr", addr, "-rate", "2000", "-warmup", "0s", "-duration", "10m"], { stdio: "ignore" });
    const page = await browser.newPage({ viewport: size, deviceScaleFactor: 1, colorScheme: "dark" });
    await page.goto(`http://${addr}/`);
    await page.waitForFunction(() => document.getElementById("status").dataset.state === "live");
    await sleep(3000); // let the book fill, so the first frame, shown to readers who stop motion, is a full book

    const frames = path.join(tmp, "frames");
    fs.mkdirSync(frames);
    const list = ["ffconcat version 1.0"];
    let prev, file;
    for (let i = 0, end = Date.now() + seconds * 1000; Date.now() < end; i++) {
      const t = Date.now();
      if (file) list.push(`duration ${((t - prev) / 1000).toFixed(3)}`);
      file = `f${String(i).padStart(5, "0")}.png`;
      await page.screenshot({ path: path.join(frames, file) });
      list.push(`file ${file}`);
      prev = t;
    }
    list.push("duration 0.1", `file ${file}`); // the last frame needs a duration of its own
    fs.writeFileSync(path.join(frames, "list.txt"), list.join("\n") + "\n");

    execFileSync(ffmpeg, [
      "-y", "-loglevel", "error", "-f", "concat", "-i", path.join(frames, "list.txt"),
      "-filter_complex", `fps=${fps},split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle`,
      "-loop", "0", out,
    ], { stdio: "inherit" });
    const mb = fs.statSync(out).size / 1e6;
    console.log(`${out}: ${mb.toFixed(2)} MB${mb > 3 ? " (over 3 MB: shorten it or lower the rate)" : ""}`);
  } finally {
    await browser.close();
    loadgen?.kill();
    engine.kill();
    await sleep(500);
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

async function waitHealthy() {
  for (let i = 0; i < 100; i++) {
    try {
      if ((await fetch(`http://${addr}/healthz`)).ok) return;
    } catch {}
    await sleep(100);
  }
  throw new Error("the engine did not become healthy");
}

main().catch(err => {
  console.error(err);
  process.exitCode = 1;
});
