// Real Codex + CLI + browser regression. Starts and stops ONLY its own server
// on an ephemeral loopback port, with a fresh data folder under --out.
// Requires explicit --binary --modules --browser --out --screenshots.
// Uses gpt-6-luna / low (real account tokens); no existing room is addressed.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { mkdir, writeFile } from "node:fs/promises";
import { resolve as pathResolve, join } from "node:path";
import { once } from "node:events";

const args = process.argv.slice(2);
const option = (name) => {
  const index = args.indexOf(name);
  assert(index >= 0 && args[index + 1], "required option " + name);
  return pathResolve(args[index + 1]);
};
const binary = option("--binary");
const output = option("--out");
const screenshots = option("--screenshots");
const require = createRequire(join(option("--modules"), "__user_request__.cjs"));
const { chromium } = require("playwright");
const executablePath = option("--browser");
await mkdir(output, { recursive: true });
await mkdir(screenshots, { recursive: true });
const data = join(output, "data");
const workspace = join(output, "workspace");
// Non-recursive creation refuses a reused run folder and its stored rooms.
await mkdir(data);
await mkdir(workspace);
const evidence = { started_at: new Date().toISOString(), binary, data, workspace, commands: [], screenshots: [], checks: {} };
const checks = evidence.checks;
let server;
let browser;
let room;
let url;
let serverLog = "";

async function startServer() {
  const argv = ["-addr", "127.0.0.1:0", "-data", data, "-claude", "__missing_user_request_test__", "-agy", "__missing_user_request_test__", "-opencode", "__missing_user_request_test__", "-update-check", "off"];
  server = spawn(binary, argv, { windowsHide: true, stdio: ["ignore", "pipe", "pipe"] });
  evidence.commands.push({ argv: [binary, ...argv], pid: server.pid });
  server.stderr.on("data", (chunk) => { serverLog += chunk.toString(); });
  url = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("server startup timed out")), 60000);
    server.once("error", (error) => { clearTimeout(timer); reject(error); });
    server.once("exit", (code) => { clearTimeout(timer); reject(new Error("server exited during startup: " + code)); });
    server.stdout.on("data", (chunk) => {
      serverLog += chunk.toString();
      const matched = /roundtable listening on (http:\/\/127\.0\.0\.1:\d+)/.exec(chunk.toString());
      if (matched) { clearTimeout(timer); resolve(matched[1]); }
    });
  });
  assert.notEqual(new URL(url).port, "8787");
  console.log("ISOLATED_SERVER", url, "PID", server.pid);
}

async function stopServer() {
  if (server && server.pid !== undefined && server.exitCode === null && server.signalCode === null) {
    const ended = once(server, "exit");
    server.kill();
    await ended;
  }
}

async function rt(...argv) {
  const command = ["rt", "--url", url, ...argv];
  const started = performance.now();
  const child = spawn(binary, command, { windowsHide: true, stdio: ["ignore", "pipe", "pipe"] });
  let stdout = "", stderr = "";
  child.stdout.on("data", (chunk) => { stdout += chunk.toString(); });
  child.stderr.on("data", (chunk) => { stderr += chunk.toString(); });
  const [code] = await once(child, "exit");
  const record = { argv: [binary, ...command], elapsed_ms: Math.round(performance.now() - started), code, stderr };
  evidence.commands.push(record);
  assert.equal(code, 0, stderr);
  record.result = JSON.parse(stdout);
  return record.result;
}

async function snapshot() {
  const response = await fetch(url + "/api/rooms/" + room + "?limit=200");
  assert(response.ok, "snapshot HTTP " + response.status);
  return response.json();
}

try {
  await startServer();
  const created = await rt("create_room", "--title", "사용자 요청 전달 · 실제 Codex 검증", "--workspace", workspace,
    "--mode", "write", "--participants", JSON.stringify([{ provider: "codex", model: "gpt-6-luna", effort: "low", label: "검증 좌석" }]));
  room = created.room;
  evidence.room = room;
  evidence.seat = created.participants[0].id;
  const prompt = "이 방은 Roundtable 사용자 요청 전달 검증 전용이야. 실제 업무와 파일 수정은 하지 마. " +
    "이번 턴은 PowerShell Start-Sleep -Seconds 8을 한 번 실행하고, 최종 답은 아래 세 줄만 글자 그대로 출력해. " +
    "각 줄 맨 앞의 [[REQUEST:@User]]는 실제 기능을 실행하는 마커라 반드시 포함해야 해. 대괄호를 생략하거나 백틱·코드 블록으로 감싸면 검증 실패야. " +
    "후속 턴에서 사용자 요청에 대한 답을 받으면 전달받은 값을 한 줄로 확인하고 새 요청 마커는 만들지 마.\n" +
    "[[REQUEST:@User]] CLI 답변 검증 값을 알려 줘\n[[REQUEST:@User]] 화면 답변 검증 값을 알려 줘\n[[REQUEST:@User]] 화면에서 이 요청을 확인 표시해 줘";
  const sent = await rt("post", "--room", room, "--text", prompt);
  await rt("post", "--room", room, "--text", "이번 후속 턴은 PowerShell Start-Sleep -Seconds 50을 한 번 실행해. 끝나면 '후속 턴 종료' 한 줄만 답해. 요청 마커는 다시 쓰지 마.");
  const queued = await snapshot();
  checks.queued_before_first_finishes = queued.queue.some((turn) => turn.status === "running") && queued.queue.some((turn) => turn.status === "queued");
  assert(checks.queued_before_first_finishes, "follow-up was not queued behind the first real Codex turn");
  const first = await rt("wait", "--rooms", room + ":" + sent.seq, "--timeout_seconds", "180", "--max_chars", "1");
  evidence.first_wait = first;
  const active = await snapshot();
  evidence.active_snapshot = active;
  assert.equal(active.user_requests.length, 3, "real Codex did not emit the three required user-request markers; probe precondition failed");
  checks.woke_while_active = first.user_requested === true && first.ready.length === 0 && !first.timed_out && active.active;
  assert(checks.woke_while_active, "user request did not interrupt an active room");
  assert.equal(first.user_requests[0].requests.length, 3);
  const continued = await rt("wait", "--rooms", first.waiting, "--timeout_seconds", "2");
  checks.receipt_prevents_repeat = continued.timed_out && !continued.user_requested && continued.user_requests[0].requests.length === 3;
  assert(checks.receipt_prevents_repeat);
  const seats = await rt("seats", "--rooms", room);
  assert.equal(seats.rooms[0].user_requests.length, 3);
  const cliRequest = first.user_requests[0].requests.find((request) => request.text.startsWith("CLI"));
  const uiRequest = first.user_requests[0].requests.find((request) => request.text.startsWith("화면 답변"));
  const ackRequest = first.user_requests[0].requests.find((request) => request.text.startsWith("화면에서"));
  const cliAnswer = "CLI_ANSWER_20261106";
  const uiAnswer = "UI_ANSWER_20261106";
  const answered = await rt("resolve_user_request", "--room", room, "--id", cliRequest.id, "--text", cliAnswer);
  assert.equal(answered.user_request.status, "answered");
  checks.cli_answer_accepted = true;

  browser = await chromium.launch({ executablePath, headless: false });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  const pageErrors = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  const navigation = await page.goto(url, { waitUntil: "domcontentloaded" });
  evidence.browser_http_status = navigation.status();
  assert.equal(navigation.status(), 200);
  await page.locator('#room-list .room-item[data-room="' + room + '"]').click();
  await page.locator("#btn-user-requests").waitFor({ state: "visible" });
  assert.equal(await page.locator("#user-requests .user-request-card").count(), 2);
  await page.locator("#btn-collapse-right").click();
  await page.locator("#btn-user-requests").click();
  assert(await page.locator("#user-requests-title").evaluate((element) => element === document.activeElement));
  const capture = async (name) => {
    const path = join(screenshots, name + ".png");
    await page.screenshot({ path });
    evidence.screenshots.push(path);
  };
  await capture("user-request-pending-desktop");
  // The theme control lives in the desktop sidebar, which is hidden on mobile.
  await page.locator("#btn-theme").click();
  await page.setViewportSize({ width: 420, height: 900 });
  await page.locator("#btn-user-requests").click();
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), "mobile horizontal overflow");
  await capture("user-request-pending-mobile");
  await page.locator("#btn-close-right").click();
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.locator("#btn-theme").click();
  const answerCard = page.locator('[data-request-id="' + uiRequest.id + '"]');
  await answerCard.getByRole("button", { name: "원문 보기" }).click();
  assert(await page.locator("#m-" + uiRequest.source_msg).isVisible());
  await answerCard.locator("summary").click();
  await answerCard.locator("textarea").fill(uiAnswer);
  const ackCard = page.locator('[data-request-id="' + ackRequest.id + '"]');
  await ackCard.getByRole("button", { name: "확인 완료" }).click();
  await page.waitForFunction(() => document.querySelector("#user-requests-count").textContent === "1");
  assert.equal(await answerCard.locator("textarea").inputValue(), uiAnswer, "other request's SSE resolution lost the draft");
  checks.ui_ack_and_draft_survives = true;
  // Whitespace is validated by the server. Its error must keep the request open.
  await answerCard.locator("textarea").fill("   ");
  await answerCard.getByRole("button", { name: "답변 보내기" }).click();
  await page.locator(".dlg-message").waitFor({ state: "visible" });
  assert((await page.locator(".dlg-message").innerText()).includes("text와 acknowledge"));
  await page.locator(".dlg-message").getByRole("button", { name: "확인", exact: true }).click();
  assert.equal(await page.locator("#user-requests-count").textContent(), "1");
  await answerCard.locator("textarea").fill(uiAnswer);
  await answerCard.getByRole("button", { name: "답변 보내기" }).click();
  await page.locator("#btn-user-requests").waitFor({ state: "hidden" });
  await page.locator("#resolved-user-requests-title").click();
  assert.equal(await page.locator("#resolved-user-requests .user-request-card").count(), 3);
  assert((await answerCard.innerText()).includes(uiAnswer));
  checks.ui_answer_and_history = true;
  // The room-list SSE refresh is debounced separately from the open room.
  await page.locator('#room-list .room-item[data-room="' + room + '"] .badge.warn').waitFor({ state: "hidden" });
  await capture("user-request-resolved-desktop");
  assert.deepEqual(pageErrors, []);
  checks.browser_errors = pageErrors;
  const resolvedWait = await rt("wait", "--rooms", first.waiting, "--timeout_seconds", "2");
  assert(!resolvedWait.user_requested && !resolvedWait.user_requests);
  checks.resolution_prevents_repeat = true;
  const finished = await rt("wait", "--rooms", room + ":" + sent.seq, "--timeout_seconds", "240");
  assert.equal(finished.ready.length, 1);
  assert(!finished.user_requested);
  const final = await snapshot();
  evidence.final_snapshot = final;
  const replies = final.messages.filter((message) => message.kind === "agent");
  assert(replies.some((message) => message.text.includes(cliAnswer)), "real seat did not acknowledge the CLI answer");
  assert(replies.some((message) => message.text.includes(uiAnswer)), "real seat did not acknowledge the browser answer");
  assert(replies.every((message) => message.model === "gpt-6-luna" && message.effort === "low"));
  assert.equal(final.user_requests.length, 3);
  assert(final.user_requests.every((request) => request.status !== "pending"));
  checks.real_codex_received_answers = true;
  await stopServer();
  await startServer();
  const restored = await snapshot();
  assert.deepEqual(restored.user_requests, final.user_requests);
  const noRepeat = await rt("wait", "--rooms", room, "--timeout_seconds", "1");
  assert(noRepeat.timed_out && !noRepeat.user_requested);
  checks.restart_restores_resolutions = true;
  evidence.result = "PASS";
  console.log("USER_REQUEST_LIVE_PASS", JSON.stringify(checks));
} catch (error) {
  evidence.result = "FAIL";
  evidence.error = String(error.stack);
  throw error;
} finally {
  try {
    if (browser) await browser.close();
    // On failure, cancel only this probe's room before stopping its server.
    if (room && server && server.exitCode === null && server.signalCode === null) {
      const response = await fetch(url + "/api/rooms/" + room + "/stop", { method: "POST" });
      assert(response.ok, "failed to stop probe room before server cleanup");
    }
  } finally {
    try {
      await stopServer();
    } finally {
      evidence.finished_at = new Date().toISOString();
      await writeFile(join(output, "result.json"), JSON.stringify(evidence, null, 2));
      await writeFile(join(output, "server.log"), serverLog);
    }
  }
}
