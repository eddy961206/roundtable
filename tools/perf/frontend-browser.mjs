// Production timeline/CSS browser benchmark with deterministic messages and no CLI calls.
// Development-only Playwright is required; it is not an application dependency.
// Example: node tools/perf/frontend-browser.mjs --root . --modules /path/to/node_modules --browser /path/to/chromium
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile, writeFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { resolve, extname, sep } from "node:path";
import { cpus, platform } from "node:os";

const args = process.argv.slice(2);
const option = (key, fallback) => args.includes(key) ? args[args.indexOf(key) + 1] : fallback;
const root = resolve(option("--root", "."));
const samples = Number(option("--samples", "5"));
const ticks = Number(option("--ticks", "8"));
const require = createRequire(resolve(option("--modules", "node_modules"), "__perf__.cjs"));
const { chromium } = require("playwright");
const web = resolve(root, "web");
const server = createServer(async (req, res) => {
  try {
    const pathname = new URL(req.url, "http://localhost").pathname;
    const file = resolve(web, "." + (pathname === "/" ? "/index.html" : pathname));
    if (!file.startsWith(web + sep)) { res.writeHead(403).end(); return; }
    const type = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml" }[extname(file)] || "application/octet-stream";
    res.setHeader("Content-Type", type);
    // Keep the real HTML/CSS/marked and imported modules; the fixture drives state
    // instead of boot's HTTP calls, background polling, or real provider discovery.
    res.end(pathname === "/js/main.js" ? "// driven by the deterministic benchmark" : await readFile(file));
  } catch { res.writeHead(404).end(); }
});
await new Promise((done) => server.listen(0, "127.0.0.1", done));
let browser;
try {
  browser = await chromium.launch({ executablePath: option("--browser"), headless: true, args: ["--no-sandbox", "--disable-dev-shm-usage"] });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto("http://127.0.0.1:" + server.address().port);
  assert.equal(await page.title(), "Roundtable");
  await page.evaluate(async () => {
    const { state } = await import("/js/state.js");
    const timeline = await import("/js/timeline.js");
    const { hooks } = await import("/js/hooks.js");
    Object.assign(hooks, { scheduleRender: timeline.scheduleRender, syncPendingScrollRestore: timeline.syncPendingScrollRestore });
    window.perfFixture = { state, timeline, serial: 0 };
  });
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Performance.enable");
  const metrics = async () => Object.fromEntries((await cdp.send("Performance.getMetrics")).metrics.map((item) => [item.name, item.value]));
  const initialize = async (count) => {
    await page.evaluate((count) => {
      const { state, timeline } = window.perfFixture;
      state.roomId = "synthetic";
      state.room = { id: state.roomId, title: "동시 스트리밍 성능 검증", mode: "chat", participants: Array.from({ length: count }, (_, i) => ({ id: "p" + i, provider: "claude", label: "참가자 " + i, enabled: true, status: "running" })) };
      state.messages.clear(); state.order = []; state.expanded.clear(); state.traces.clear(); state.traceMeta.clear();
      state.browsingHistory = false; state.stickToBottom = true;
      state.history = { has_more: true, total: 2000 };
      const text = "## 합성 응답\n\n문맥, 도구 결과, 사용자 입력을 유지하는 성능 검증. ".repeat(8) + "\n\n```js\nconst result = 42;\n```\n\n- 첫 번째 항목\n- 두 번째 항목\n";
      for (let i = 0; i < 200; i++) {
        const streaming = i >= 200 - count;
        const id = streaming ? "stream" + (i - 200 + count) : "history" + i;
        state.messages.set(id, { id, seq: i + 1, kind: "agent", author: "p" + (i % count), status: streaming ? "streaming" : "done", model: "fixture", text, ts: 1800000000000 + i });
        state.order.push(id);
      }
      state.winStart = 0; state.winEnd = 200;
      document.querySelector("#empty").hidden = true;
      document.querySelector("#room").hidden = false;
      timeline.renderTimeline(true);
    }, count);
    await page.evaluate(() => new Promise((done) => requestAnimationFrame(done)));
  };
  const run = async (count, ticks) => page.evaluate(async ({ count, ticks }) => {
    const { state, timeline } = window.perfFixture;
    let marker;
    for (let tick = 0; tick < ticks; tick++) {
      marker = "render-pulse-" + (++window.perfFixture.serial);
      for (let i = 0; i < count; i++) {
        state.messages.get("stream" + i).text += "\n\n" + marker + ": **정확한 스트리밍 본문**";
        timeline.scheduleRender("stream" + i);
      }
      await new Promise((done, reject) => {
        const deadline = performance.now() + 10000;
        const poll = () => {
          if (performance.now() > deadline) { reject(new Error("render timeout")); return; }
          if (state.renderTimers.size) setTimeout(poll, 10);
          else requestAnimationFrame(done);
        };
        poll();
      });
    }
    for (let i = 0; i < count; i++) {
      if (!document.querySelector("#m-stream" + i + " .body").textContent.includes(marker)) throw new Error("lost streamed text for " + i);
    }
    return { message_nodes: document.querySelectorAll("#timeline .msg").length, dom_nodes: document.querySelectorAll("*").length };
  }, { count, ticks });
  const results = [];
  for (const count of args.includes("--checks-only") ? [] : [1, 4, 16, 32]) {
    await initialize(count);
    await run(count, 2);
    const values = [];
    for (let sample = 0; sample < samples; sample++) {
      await initialize(count);
      const before = await metrics();
      const shape = await run(count, ticks);
      const after = await metrics();
      assert.equal(shape.message_nodes, 200);
      values.push({
        layout_count: after.LayoutCount - before.LayoutCount,
        layout_ms: (after.LayoutDuration - before.LayoutDuration) * 1000,
        style_recalc_count: after.RecalcStyleCount - before.RecalcStyleCount,
        script_ms: (after.ScriptDuration - before.ScriptDuration) * 1000,
        task_ms: (after.TaskDuration - before.TaskDuration) * 1000,
        ...shape,
      });
    }
    results.push({ participants: count, ticks, samples: values });
  }
  // Exercise state transitions that batching must preserve, after all timed runs.
  await initialize(32);
  await page.evaluate(async () => {
    const { state } = window.perfFixture;
    const { cacheTrace, ensureAndRenderTrace } = await import("/js/trace.js");
    const events = [
      { kind: "tool_call", id: "tool", name: "Read", input: "file.txt" },
      { kind: "tool_result", id: "tool", output: "complete output" },
      { kind: "subagent_start", parent: "child", name: "review", input: "check everything" },
      { kind: "subagent_meta", parent: "child", meta: { model: "fixture", effort: "high" } },
      { kind: "subagent_msg", parent: "child", role: "assistant", text: "## Preserved report\nAll context retained." },
      { kind: "subagent_end", parent: "child", output: "complete report" },
    ];
    state.messages.get("stream31").trace = events.length;
    cacheTrace("stream31", events, events.length);
    state.expanded.add("stream31");
    await ensureAndRenderTrace("stream31");
  });
  const summary = page.locator("#t-stream31 details.item:not(.sub) > summary");
  await summary.focus();
  await summary.press("Enter");
  await run(32, 1);
  assert(await page.evaluate(() => {
    const details = document.querySelector("#t-stream31 details.item:not(.sub)");
    return details.open && document.activeElement === details.querySelector("summary") && document.querySelector("#t-stream31 .sub-report").textContent.includes("complete report");
  }), "nested trace state and keyboard focus must survive concurrent body updates");
  await page.locator("#input").fill("사용자 입력과 선택 범위 유지");
  await page.locator("#input").focus();
  await page.evaluate(() => document.querySelector("#input").setSelectionRange(3, 7));
  await run(32, 1);
  assert(await page.evaluate(() => {
    const input = document.querySelector("#input");
    return input.value === "사용자 입력과 선택 범위 유지" && input.selectionStart === 3 && input.selectionEnd === 7 && document.activeElement === input;
  }), "typing focus and selection must survive concurrent body updates");
  const readingTop = await page.evaluate(() => {
    const { state } = window.perfFixture;
    state.stickToBottom = false;
    const tl = document.querySelector("#timeline");
    tl.scrollTop = tl.scrollHeight / 2;
    return tl.scrollTop;
  });
  await run(32, 1);
  assert(Math.abs(await page.locator("#timeline").evaluate((node) => node.scrollTop) - readingTop) <= 1, "reading older content must not jump to the bottom");
  await page.evaluate(() => {
    const { state, timeline } = window.perfFixture;
    timeline.scheduleRender("stream0");
    const message = state.messages.get("stream0");
    message.status = "cancelled";
    message.text = "취소 전의 최종 본문은 남아 있어야 함";
    timeline.renderMessage(message.id, false);
  });
  await page.waitForFunction(() => window.perfFixture.state.renderTimers.size === 0);
  assert(await page.locator("#m-stream0 .body").textContent() === "취소 전의 최종 본문은 남아 있어야 함\n");
  assert.equal(await page.locator("#m-stream0 .body.cursor").count(), 0);
  // Existing room-resync cancellation must still cancel the shared timeout.
  await page.evaluate(() => {
    const { state, timeline } = window.perfFixture;
    timeline.scheduleRender("stream1");
    for (const timer of state.renderTimers.values()) clearTimeout(timer);
    state.renderTimers.clear();
    state.messages.clear(); state.order = []; state.expanded.clear();
    timeline.renderTimeline(false);
  });
  await page.waitForTimeout(180);
  assert.equal(await page.locator("#timeline .msg").count(), 0);
  await initialize(4);
  const themes = [];
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 1000 });
    for (const theme of ["dark", "light"]) {
      await page.evaluate((theme) => { document.documentElement.dataset.theme = theme; }, theme);
      await run(4, 1);
      assert(await page.locator("#input").isVisible());
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), "viewport must not acquire horizontal overflow");
      themes.push({ width, theme });
    }
  }
  const checks = { nested_trace_keyboard_focus: true, composer_selection: true, scroll_position: true, cancellation_final_text: true, resync_timer_cancellation: true, responsive_themes: themes };
  await page.evaluate(() => {
    const { state, timeline } = window.perfFixture;
    state.messages.set("outside", { id: "outside", kind: "agent", author: "p0", status: "streaming", text: "outside the visible page" });
    state.order.unshift("outside");
    state.winStart = 1; state.winEnd = 201;
    timeline.scheduleRender("outside");
    timeline.scheduleRender("stream0");
  });
  await page.waitForFunction(() => window.perfFixture.state.renderTimers.size === 0);
  assert.equal(await page.locator("#timeline .msg").count(), 200);
  assert.equal(await page.locator("#m-outside").count(), 0);
  checks.scheduled_off_window_dom_cap = true;
  assert.deepEqual(errors, []);
  const result = {
    workload: "Production timeline scheduler and renderMessage, 200 mounted messages/2000 history count, 1/4/16/32 concurrent streaming bodies, 125ms production throttle. Each sample starts with identical content; two warmup ticks excluded. HTTP/API/CLI excluded. DOM event/content assertions included.",
    environment: { browser: await browser.version(), viewport: { width: 1440, height: 1000 }, os: platform(), cpu: cpus()[0]?.model, headless: true },
    samples, results, checks, page_errors: errors,
  };
  const json = JSON.stringify(result, null, 2) + "\n";
  if (args.includes("--out")) await writeFile(option("--out"), json);
  process.stdout.write(json);
} finally {
  await browser?.close();
  await new Promise((done) => server.close(done));
}
