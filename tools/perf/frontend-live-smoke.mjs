// End-to-end UI smoke against a disposable server using tools/perf/fakecli.
// Refuses to send prompts unless Claude reports the deterministic fixture version.
// node tools/perf/frontend-live-smoke.mjs --url http://127.0.0.1:18788 --modules /path/to/node_modules --browser /path/to/chromium --out result.json --screenshots /tmp/roundtable-qa
import assert from "node:assert/strict";
import { mkdir, writeFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { resolve } from "node:path";

const args = process.argv.slice(2);
const option = (key, fallback) => args.includes(key) ? args[args.indexOf(key) + 1] : fallback;
assert(args.includes("--url"), "explicit disposable fixture URL required");
const url = new URL(option("--url"));
assert(["127.0.0.1", "localhost", "[::1]"].includes(url.hostname));
const require = createRequire(resolve(option("--modules", "node_modules"), "__perf__.cjs"));
const { chromium } = require("playwright");
const browser = await chromium.launch({ executablePath: option("--browser"), headless: true, args: ["--no-sandbox", "--disable-dev-shm-usage"] });
const errors = [];
const consoleErrors = [];
const checks = {};
const screenshots = [];
try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => { if (message.type() === "error") consoleErrors.push(message.text()); });
  await page.goto(url.href);
  const bindState = () => page.evaluate(async () => { window.liveSmokeState = (await import("/js/state.js")).state; });
  await bindState();
  await page.waitForFunction(() => window.liveSmokeState.providers.length > 0);
  const api = async (method, path, body) => page.evaluate(async ({ method, path, body }) => {
    const response = await fetch(path, { method, headers: { "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
    const data = await response.json();
    if (!response.ok) throw new Error(path + ": " + JSON.stringify(data));
    return data;
  }, { method, path, body });
  await api("POST", "/api/providers/versions");
  const providers = await api("GET", "/api/providers");
  assert(providers.providers.some((provider) => provider.id === "claude" && provider.version?.includes("roundtable deterministic perf fixture")), "real AI CLI calls are forbidden in this smoke test");
  const snapshot = (roomID) => api("GET", "/api/rooms/" + roomID + "?limit=200");
  const screenshot = async (name) => {
    if (!args.includes("--screenshots")) return;
    const folder = resolve(option("--screenshots"));
    await mkdir(folder, { recursive: true });
    await page.screenshot({ path: resolve(folder, name + ".png") });
    screenshots.push(name + ".png");
  };
  const waitForRoom = async (roomID) => page.waitForFunction((roomID) => {
    const state = window.liveSmokeState;
    return state.roomId === roomID && state.room?.id === roomID;
  }, roomID);
  const selectRoom = async (roomID) => {
    await page.locator('#room-list .room-item[data-room="' + roomID + '"]').click();
    await waitForRoom(roomID);
  };
  const suffix = Date.now();
  await page.locator("#btn-new-room").click();
  await page.locator('#form-room input[name="title"]').fill("성능 UI 검증 " + suffix);
  await page.locator('#form-room select[name="mode"]').selectOption("chat");
  await page.locator("#room-seat-specs .seat-provider").selectOption("claude");
  await page.locator("#room-seat-specs .seat-model").selectOption("__custom__");
  await page.locator("#room-seat-specs .seat-custom-model").fill("perf-fixture");
  await page.locator("#room-seat-specs .seat-count").fill("4");
  await page.locator("#dlg-room-ok").click();
  await page.waitForFunction(() => window.liveSmokeState.room?.participants.length === 4);
  const firstRoom = await page.evaluate(async () => (await import("/js/state.js")).state.room);
  checks.room_created_in_ui = firstRoom.participants.length;

  // Real persisted history, without running any participants in this room.
  const historyRoom = await api("POST", "/api/rooms", { title: "이전 기록 검증 " + suffix, mode: "chat", participant_specs: [{ provider: "claude", model: "perf-fixture", count: 1 }] });
  await api("PATCH", "/api/rooms/" + historyRoom.id, { participants: [{ id: historyRoom.participants[0].id, enabled: false }] });
  await page.evaluate(async (roomID) => {
    for (let batch = 0; batch < 1000; batch += 10) {
      await Promise.all(Array.from({ length: 10 }, async (_, offset) => {
        const response = await fetch("/api/rooms/" + roomID + "/messages", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ text: "보존된 합성 기록 " + (batch + offset) + "\n본문과 문맥은 그대로 남아 있어야 해." }) });
        if (!response.ok) throw new Error("history seed failed: " + await response.text());
      }));
    }
    await (await import("/js/rooms.js")).loadRooms();
  }, historyRoom.id);
  assert.equal((await snapshot(historyRoom.id)).history.total, 1000);

  const input = page.locator("#input");
  await input.fill('PERF_JSON {"tokens":256,"delay_us":25000,"tools":4}');
  await input.press("Enter");
  await page.waitForFunction(() => [...window.liveSmokeState.messages.values()].filter((message) => message.kind === "agent" && message.status === "streaming").length === 4);
  const streamIDs = await page.evaluate(async () => [...(await import("/js/state.js")).state.messages.values()].filter((message) => message.kind === "agent").map((message) => message.id));
  await input.fill("스트리밍 중 입력을 유지해");
  await input.evaluate((element) => element.setSelectionRange(3, 7));
  await page.waitForTimeout(250);
  assert(await input.evaluate((element) => element.value === "스트리밍 중 입력을 유지해" && element.selectionStart === 3 && element.selectionEnd === 7 && document.activeElement === element));
  checks.composer_during_stream = true;
  const traceButton = page.locator("#m-" + streamIDs.at(-1) + " .trace-btn");
  await traceButton.focus();
  await traceButton.press("Enter");
  const toolSummary = page.locator("#t-" + streamIDs.at(-1) + " details.item > summary").first();
  await toolSummary.waitFor();
  await toolSummary.focus();
  await toolSummary.press("Enter");
  await page.waitForTimeout(250);
  assert(await toolSummary.evaluate((element) => document.activeElement === element && element.parentElement.open));
  checks.trace_keyboard_during_stream = true;
  await screenshot("desktop-streaming");

  // This switches while tokens and previously scheduled renders are arriving.
  await selectRoom(historyRoom.id);
  await page.waitForTimeout(250);
  assert.equal(await page.locator("#timeline .msg").count(), 100);
  for (const id of streamIDs) assert.equal(await page.locator("#m-" + id).count(), 0);
  await page.locator("#timeline-more").click();
  await page.waitForFunction(() => document.querySelectorAll("#timeline .msg").length === 200);
  await page.locator("#timeline-more").click();
  await page.waitForFunction(() => !window.liveSmokeState.historyLoading);
  assert.equal(await page.locator("#timeline .msg").count(), 200);
  await page.locator("#btn-latest-history").click();
  await page.waitForFunction(() => {
    const state = window.liveSmokeState;
    return !state.browsingHistory && state.order.length === 100;
  });
  checks.persisted_history_paging = { stored: 1000, initial_dom: 100, older_dom_cap: 200, latest_dom: 100 };
  checks.room_switch_during_stream = true;

  await selectRoom(firstRoom.id);
  await page.waitForFunction(() => [...window.liveSmokeState.messages.values()].filter((message) => message.kind === "agent" && message.status === "done").length === 4, undefined, { timeout: 30000 });
  const completed = (await snapshot(firstRoom.id)).messages.filter((message) => message.kind === "agent");
  // Existing hub completion applies strings.TrimSpace to the final reply.
  const expected = Array.from({ length: 256 }, (_, index) => "piece-" + String(index).padStart(4, "0") + ": 검증용 **stream** data.\n").join("").trim();
  for (const message of completed) {
    assert.equal(message.text, expected);
    assert.equal(message.usage.output_tokens, 256);
    assert(await page.locator("#m-" + message.id + " .body").textContent().then((text) => text.includes("piece-0255")));
  }
  checks.four_streams_complete_exact_content = true;
  await page.reload();
  await bindState();
  await waitForRoom(firstRoom.id);
  await page.waitForFunction(() => window.liveSmokeState.es?.readyState === 1);
  assert.equal((await snapshot(firstRoom.id)).messages.filter((message) => message.kind === "agent").length, 4);
  checks.reload_sse_resync = true;

  await input.fill('PERF_JSON {"tokens":128,"delay_us":50000,"tools":2}');
  await input.press("Enter");
  await page.waitForFunction(() => [...window.liveSmokeState.messages.values()].filter((message) => message.status === "streaming").length === 4);
  const hiddenBefore = await page.evaluate(async () => {
    const { state } = await import("/js/state.js");
    const message = [...state.messages.values()].find((item) => item.status === "streaming");
    Object.defineProperty(document, "hidden", { configurable: true, value: true });
    document.dispatchEvent(new Event("visibilitychange"));
    return { id: message.id, text: document.querySelector("#m-" + message.id + " .body").textContent, length: message.text.length };
  });
  await page.waitForTimeout(350);
  assert(await page.evaluate(async (before) => {
    const { state } = await import("/js/state.js");
    return state.messages.get(before.id).text.length > before.length && document.querySelector("#m-" + before.id + " .body").textContent === before.text;
  }, hiddenBefore));
  await page.evaluate(() => { delete document.hidden; document.dispatchEvent(new Event("visibilitychange")); });
  await page.waitForFunction((before) => document.querySelector("#m-" + before.id + " .body").textContent !== before.text, hiddenBefore);
  checks.visibility_event_recovery = "synthetic document.hidden/visibilitychange; real SSE continues, rendering resumes without loss";
  await page.locator("#pcard-" + firstRoom.participants[0].id + " .stop-participant").click();
  await page.waitForFunction(() => {
    const messages = [...window.liveSmokeState.messages.values()].filter((message) => message.kind === "agent");
    return messages.length === 8 && !messages.some((message) => message.status === "streaming");
  }, undefined, { timeout: 30000 });
  const stopped = await snapshot(firstRoom.id);
  assert.equal(stopped.messages.filter((message) => message.status === "cancelled").length, 1);
  assert.equal(stopped.messages.filter((message) => message.kind === "agent" && message.status === "done").length, 7);
  assert.equal(stopped.room.participants[0].enabled, false);
  checks.individual_stop_other_seats_complete = true;

  const lastMessage = stopped.messages.filter((message) => message.kind === "agent").at(-1);
  const lastTrace = page.locator("#m-" + lastMessage.id + " .trace-btn");
  if (await lastTrace.getAttribute("aria-expanded") !== "true") await lastTrace.click();
  await page.locator("#t-" + lastMessage.id + " details.item").first().waitFor();
  for (const theme of ["dark", "light"]) {
    for (let i = 0; i < 2 && await page.evaluate(() => document.documentElement.dataset.theme) !== theme; i++) await page.locator("#btn-theme").click();
    assert.equal(await page.evaluate(() => document.documentElement.dataset.theme), theme);
    await screenshot("desktop-" + theme + "-trace");
  }
  await page.setViewportSize({ width: 390, height: 844 });
  await input.fill("좁은 화면에서도 입력 가능");
  assert(await input.isVisible());
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
  await screenshot("mobile-light");
  checks.themes_and_mobile = { desktop: [1440, 1000], mobile: [390, 844], themes: ["dark", "light"], horizontal_overflow: false };
  assert.deepEqual(errors, []);
  assert.deepEqual(consoleErrors, []);
  const result = { url: url.href, browser: await browser.version(), fixture_provider_verified: true, checks, page_errors: errors, console_errors: consoleErrors, screenshots };
  const json = JSON.stringify(result, null, 2) + "\n";
  if (args.includes("--out")) await writeFile(option("--out"), json);
  process.stdout.write(json);
} finally {
  await browser.close();
}
