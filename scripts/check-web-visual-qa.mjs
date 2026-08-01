#!/usr/bin/env node

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { createRequire } from "node:module";
import { execFileSync } from "node:child_process";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const webRoot = path.join(root, "web");
const screenshotRoot = path.join(root, "qa", "screenshots");
const reportPath = path.join(root, "qa", "task10-browser-matrix.json");
const require = createRequire(import.meta.url);

function loadPlaywright() {
  const candidates = [process.env.CONNECTMAC_PLAYWRIGHT_MODULE, "playwright"];
  try {
    const globalRoot = execFileSync("npm", ["root", "-g"], { encoding: "utf8" }).trim();
    candidates.push(path.join(globalRoot, "@playwright", "cli", "node_modules", "playwright"));
  } catch (error) {
    // The explicit module path or a project dependency can still satisfy the run.
  }
  for (const candidate of candidates.filter(Boolean)) {
    try {
      return require(candidate);
    } catch (error) {
      // Try the next portable resolution source.
    }
  }
  throw new Error("Playwright is required. Install playwright or set CONNECTMAC_PLAYWRIGHT_MODULE.");
}

const { chromium, firefox, webkit } = loadPlaywright();

const desktopViewports = [
  { width: 1280, height: 800 },
  { width: 1440, height: 900 },
  { width: 1920, height: 1080 },
];
const mobileViewport = { width: 390, height: 844 };
const engines = [
  { name: "chromium", launcher: chromium },
  { name: "firefox", launcher: firefox },
  { name: "webkit", launcher: webkit },
];
const states = ["stopped", "creating", "ready", "releasing", "blocked", "unknown", "error"];

function contentType(file) {
  const ext = path.extname(file).toLowerCase();
  return {
    ".html": "text/html; charset=utf-8",
    ".js": "text/javascript; charset=utf-8",
    ".css": "text/css; charset=utf-8",
    ".svg": "image/svg+xml",
    ".png": "image/png",
    ".woff2": "font/woff2",
  }[ext] || "application/octet-stream";
}

async function startStaticServer() {
  const server = createServer(async (request, response) => {
    try {
      const url = new URL(request.url || "/", "http://127.0.0.1");
      const relative = url.pathname === "/" ? "index.html" : decodeURIComponent(url.pathname.slice(1));
      const file = path.resolve(webRoot, relative);
      if (!file.startsWith(webRoot + path.sep)) throw new Error("invalid path");
      const bytes = await readFile(file);
      response.writeHead(200, { "Content-Type": contentType(file), "Cache-Control": "no-store" });
      response.end(bytes);
    } catch (error) {
      response.writeHead(404, { "Content-Type": "text/plain; charset=utf-8" });
      response.end("not found");
    }
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  return {
    url: `http://127.0.0.1:${address.port}/`,
    close: () => new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve())),
  };
}

function profileFor(stateName) {
  return {
    name: `qa-${stateName}-usw2`,
    apple_email: `${stateName}@example.test`,
    region: "us-west-2",
    description: `QA ${stateName}`,
    profile_yaml: `profiles:\n  qa-${stateName}-usw2:\n    user: ec2-user\n`,
    owners: [{ name: "QA Operator", email: "operator@example.test", enabled: true }],
  };
}

function statusFor(stateName) {
  const base = { decision: "unknown", ready: false, next: "refresh", instances: [], hosts: [] };
  if (stateName === "stopped") return { ...base, decision: "create", next: "preview create" };
  if (stateName === "creating") return { ...base, decision: "wait-ready", next: "wait for status checks" };
  if (stateName === "ready") return {
    ...base,
    decision: "ready",
    ready: true,
    next: "connect",
    elastic_ip: { allocation_id: "eipalloc-0123456789abcdef0", public_ip: "203.0.113.10" },
    instances: [{ instance_id: "i-0123456789abcdef0", host_id: "h-0123456789abcdef0", public_ip: "203.0.113.10" }],
    hosts: [{ host_id: "h-0123456789abcdef0", state: "available" }],
  };
  if (stateName === "releasing") return { ...base, decision: "blocked", next: "wait for host release" };
  if (stateName === "blocked") return { ...base, decision: "blocked", detail: "Dedicated Host is pending", next: "stop and inspect" };
  return base;
}

function jobsFor(stateName, profile) {
  if (stateName === "creating") return [{
    id: "qa-open-job",
    type: "aws-open",
    profile: profile.name,
    status: "running",
    lifecycle_state: "pending",
    started_at: "2026-08-01T01:00:00Z",
    request_id: "req-open-qa",
    actor_name: "QA Operator",
  }];
  if (stateName === "releasing") return [{
    id: "qa-destroy-job",
    type: "aws-destroy",
    profile: profile.name,
    status: "success",
    lifecycle_state: "waiting",
    started_at: "2026-08-01T01:00:00Z",
    request_id: "req-release-qa",
    actor_name: "QA Operator",
  }];
  return [];
}

function json(route, value, status = 200) {
  return route.fulfill({
    status,
    contentType: "application/json",
    headers: { "X-Request-ID": "req-browser-qa" },
    body: JSON.stringify(value),
  });
}

async function installMocks(page, stateName, options = {}) {
  const empty = stateName === "empty";
  const profile = profileFor(stateName === "empty" ? "stopped" : stateName);
  const member = {
    id: 1,
    name: "QA Admin",
    email: "admin@example.test",
    role: "admin",
    enabled: true,
    profiles: empty ? [] : [profile.name],
  };
  let assignedProfiles = member.profiles.slice();
  let challengeRequests = 0;
  const consoleErrors = [];
  const assetFailures = [];
  const requests = [];
  page.on("console", (message) => {
    if (message.type() === "error") consoleErrors.push(message.text());
  });
  page.on("pageerror", (error) => consoleErrors.push(error.message));
  page.on("response", (response) => {
    const kind = response.request().resourceType();
    if (["document", "stylesheet", "script", "image", "font"].includes(kind) && response.status() >= 400) {
      assetFailures.push({ url: response.url(), status: response.status(), kind });
    }
  });

  await page.route("https://127.0.0.1:18765/**", async (route) => {
    if (options.localAgentOnline) return json(route, { ok: true, data: { status: "ok" } });
    return json(route, { ok: false, error: "local agent offline" });
  });
  await page.route("http://127.0.0.1:18765/**", async (route) => {
    if (options.localAgentOnline) return json(route, { ok: true, data: { status: "ok" } });
    return json(route, { ok: false, error: "local agent offline" });
  });
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    const method = request.method();
    requests.push({ method, pathname, postData: request.postData() || "" });
    if (pathname === "/api/config") return json(route, { ok: true, data: { config: { user_api: "" } } });
    if (pathname === "/api/auth/me") return json(route, {
      ok: true,
      data: options.authenticated === false
        ? { authenticated: false, setup_required: false }
        : { authenticated: true, setup_required: false, member },
    });
    if (pathname === "/api/profiles") return json(route, { ok: true, data: { profiles: empty ? [] : [profile] } });
    if (pathname === "/api/members") return json(route, { ok: true, data: { members: [{ ...member, profiles: assignedProfiles }] } });
    if (pathname === "/api/managed-profiles") return json(route, { ok: true, data: { profiles: empty ? [] : [{ ...profile, enabled: true, members: [{ email: member.email }] }] } });
    if (pathname === "/api/profile-owners") return json(route, { ok: true, data: { owners: empty ? [] : [{ profile_name: profile.name, owner: profile.owners[0] }] } });
    if (pathname === "/api/release-reminders") return json(route, { ok: true, data: { reminders: empty ? [] : [{ profile_name: profile.name, status: "active", auto_release_enabled: false }] } });
    if (pathname === "/api/jobs") return json(route, { ok: true, data: { jobs: jobsFor(stateName, profile) } });
    if (pathname === "/api/settings") return json(route, { ok: true, data: { settings: { background_confirm: true } } });
    if (pathname === "/api/events") return json(route, { ok: true, data: { events: [], next_cursor: "" } });
    if (pathname === "/api/auth/challenge") {
      challengeRequests += 1;
      return json(route, { ok: true, data: { token: `qa-token-${challengeRequests}`, question: `QA challenge ${challengeRequests}: 1 + 1 = ?` } });
    }
    if (pathname === "/api/member/profiles" && method === "POST") {
      const payload = JSON.parse(request.postData() || "{}");
      assignedProfiles = Array.isArray(payload.profiles) ? payload.profiles : [];
      return json(route, { ok: true, data: {} });
    }
    if (pathname === "/api/local-intent") return json(route, { ok: true, data: {} });
    if (pathname === "/api/aws/status") {
      if (stateName === "error") return json(route, { ok: false, error: "Mock status failure", error_code: "mock_status" });
      return json(route, { ok: true, data: statusFor(stateName) });
    }
    if ((pathname === "/api/aws/open" || pathname === "/api/aws/destroy") && method === "POST") {
      const payload = JSON.parse(request.postData() || "{}");
      assert.equal(payload.confirm, false, "visual QA must never confirm an AWS mutation");
      const output = [
        `Profile: ${profile.name}`,
        `Apple account: ${profile.apple_email}`,
        "Instance: i-0123456789abcdef0",
        "Host: h-0123456789abcdef0",
        "Elastic IP: eipalloc-0123456789abcdef0",
      ].join("\n");
      return json(route, { ok: true, output, data: { preview: true } });
    }
    return json(route, { ok: true, data: {} });
  });
  return {
    profile,
    member,
    requests,
    consoleErrors,
    assetFailures,
    get challengeRequests() { return challengeRequests; },
  };
}

async function waitForApp(page) {
  await page.locator("#appShell:not(.hidden)").waitFor({ state: "visible" });
  await page.waitForFunction(() => document.querySelector("#statusLine")?.textContent !== "加载中...");
}

async function openWorkbench(page, profileName) {
  await page.locator(`[data-workbench-entry="${profileName}"]`).click();
  await page.locator("#operationsView:not(.hidden)").waitFor({ state: "visible" });
}

async function layoutMetrics(page) {
  return page.evaluate(() => ({
    viewport: { width: window.innerWidth, height: window.innerHeight },
    document: { width: document.documentElement.scrollWidth, height: document.documentElement.scrollHeight },
    horizontalOverflow: document.documentElement.scrollWidth > window.innerWidth + 1,
    visibleButtons: [...document.querySelectorAll("button")]
      .filter((button) => !button.closest(".hidden") && getComputedStyle(button).display !== "none")
      .map((button) => (button.textContent || button.getAttribute("aria-label") || "").trim())
      .filter(Boolean),
  }));
}

async function captureState(browser, engineName, serverURL, viewport, stateName) {
  const context = await browser.newContext({ viewport, ignoreHTTPSErrors: true });
  const page = await context.newPage();
  const online = stateName === "ready";
  const mock = await installMocks(page, stateName, { localAgentOnline: online });
  await page.goto(serverURL, { waitUntil: "networkidle" });
  await waitForApp(page);
  if (stateName !== "empty") {
    await page.waitForFunction((name) => {
      const row = document.querySelector(`[data-workbench="${name}"]`);
      return row && !row.textContent.includes("状态未知") || document.querySelector(".profile-status-summary");
    }, mock.profile.name);
    await openWorkbench(page, mock.profile.name);
    const expected = {
      stopped: "已停止",
      creating: "正在打开",
      ready: "已就绪",
      releasing: "正在释放",
      blocked: "受阻",
      unknown: "状态未知",
      error: "状态未知",
    }[stateName];
    await page.locator("#workbenchStateBadge").filter({ hasText: expected }).waitFor();
  } else {
    await page.locator("#profileEmptyState:not(.hidden)").waitFor({ state: "visible" });
  }
  const metrics = await layoutMetrics(page);
  assert.equal(metrics.viewport.width, viewport.width);
  assert.equal(metrics.viewport.height, viewport.height);
  assert.equal(metrics.horizontalOverflow, false, `${engineName} ${stateName} ${viewport.width} has horizontal overflow`);
  if (stateName !== "empty") {
    assert.equal(await page.locator("#selectedProfileRegion").innerText(), "Region：us-west-2");
    if (viewport.width <= 640) {
      for (const id of ["terminalBtn", "vncBtn", "syncBtn"]) {
        await expectHidden(page, `#${id}`);
      }
    }
  }
  const filename = `${engineName}-${viewport.width}x${viewport.height}-${stateName}.png`;
  await page.screenshot({ path: path.join(screenshotRoot, filename), fullPage: true });
  const result = {
    engine: engineName,
    cssViewport: viewport,
    state: stateName,
    screenshot: `qa/screenshots/${filename}`,
    horizontalOverflow: metrics.horizontalOverflow,
    consoleErrors: mock.consoleErrors,
    assetFailures: mock.assetFailures,
    visibleButtons: metrics.visibleButtons,
  };
  assert.deepEqual(mock.consoleErrors, [], `${engineName} ${stateName} console errors`);
  assert.deepEqual(mock.assetFailures, [], `${engineName} ${stateName} asset failures`);
  await context.close();
  return result;
}

async function expectHidden(page, selector) {
  const locator = page.locator(selector);
  assert.equal(await locator.count(), 1, `missing ${selector}`);
  assert.equal(await locator.evaluate((node) => getComputedStyle(node).display === "none"), true, `${selector} must be hidden`);
}

async function captureSurface(browser, engineName, serverURL, viewport, surface) {
  const stateName = surface.includes("release") ? "ready" : "stopped";
  const context = await browser.newContext({ viewport, ignoreHTTPSErrors: true });
  const page = await context.newPage();
  const mock = await installMocks(page, stateName, { localAgentOnline: stateName === "ready" });
  await page.goto(serverURL, { waitUntil: "networkidle" });
  await waitForApp(page);

  if (surface === "profiles-admin") {
    await page.locator('[data-view="profilesAdminView"]').click();
    await page.locator("#profilesAdminView:not(.hidden)").waitFor({ state: "visible" });
  } else if (surface === "members") {
    await page.locator('[data-view="userManagementView"]').click();
    await page.locator("#userManagementView:not(.hidden)").waitFor({ state: "visible" });
  } else if (surface === "mobile-open-confirm" || surface === "mobile-release-confirm") {
    await openWorkbench(page, mock.profile.name);
    if (surface === "mobile-open-confirm") {
      await page.locator("#technicalDetails").evaluate((node) => { node.open = true; });
      await page.locator("#assignMemberSelect").selectOption(mock.member.email);
      await page.locator("#openMacBtn").click();
    } else {
      await page.locator("#releaseMacBtn").click();
    }
    await page.locator("#awsConfirmLayer:not(.hidden)").waitFor({ state: "visible" });
    const dialog = page.locator("#awsConfirmLayer .picker-card");
    const box = await dialog.boundingBox();
    assert.ok(box && box.x >= 0 && box.x + box.width <= viewport.width + 1, `${engineName} mobile dialog width`);
    assert.equal(await page.evaluate(() => !!document.activeElement?.closest("#awsConfirmLayer")), true, "dialog focus must stay inside modal");
    if (surface === "mobile-release-confirm") {
      assert.equal(await page.locator("#awsConfirmEIPNotice").isVisible(), true);
    }
  } else {
    await page.locator("#profilesView:not(.hidden)").waitFor({ state: "visible" });
  }

  const metrics = await layoutMetrics(page);
  assert.equal(metrics.viewport.width, viewport.width);
  assert.equal(metrics.viewport.height, viewport.height);
  assert.equal(metrics.horizontalOverflow, false, `${engineName} ${surface} ${viewport.width} has horizontal overflow`);
  assert.deepEqual(mock.consoleErrors, [], `${engineName} ${surface} console errors`);
  assert.deepEqual(mock.assetFailures, [], `${engineName} ${surface} asset failures`);
  assert.equal(mock.requests.some((item) => item.postData.includes('"confirm":true')), false);
  const filename = `${engineName}-${viewport.width}x${viewport.height}-surface-${surface}.png`;
  await page.screenshot({ path: path.join(screenshotRoot, filename), fullPage: true });
  await context.close();
  return {
    engine: engineName,
    cssViewport: viewport,
    surface,
    screenshot: `qa/screenshots/${filename}`,
    horizontalOverflow: metrics.horizontalOverflow,
    consoleErrors: mock.consoleErrors,
    assetFailures: mock.assetFailures,
  };
}

async function verifyLoginChallenge(browser, engineName, serverURL) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await context.newPage();
  const mock = await installMocks(page, "stopped", { authenticated: false, localAgentOnline: false });
  await page.goto(serverURL, { waitUntil: "networkidle" });
  await page.locator("#authScreen:not(.hidden)").waitFor({ state: "visible" });
  await page.locator("#challengeQuestion").filter({ hasText: "QA challenge 1" }).waitFor();
  await page.locator("#loginUsername").fill("qa-user@example.test");
  await page.locator("#loginPassword").fill("not-a-real-password");
  await page.locator("#refreshChallengeBtn").click();
  await page.locator("#challengeQuestion").filter({ hasText: "QA challenge 2" }).waitFor();
  assert.equal(await page.locator("#loginUsername").inputValue(), "qa-user@example.test");
  assert.equal(await page.locator("#loginPassword").inputValue(), "not-a-real-password");
  assert.ok(mock.challengeRequests >= 2);
  assert.deepEqual(mock.consoleErrors, []);
  assert.deepEqual(mock.assetFailures, []);
  await context.close();
  return { engine: engineName, passed: true, retries: mock.challengeRequests };
}

async function verifyWorkflows(browser, engineName, serverURL) {
  const screenshots = [];
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, ignoreHTTPSErrors: true });
  const page = await context.newPage();
  const mock = await installMocks(page, "stopped", { localAgentOnline: false });
  await page.goto(serverURL, { waitUntil: "networkidle" });
  await waitForApp(page);
  await page.locator("#search").fill("does-not-exist");
  await page.locator("#profiles").filter({ hasText: "没有匹配的 Profile" }).waitFor();
  await page.locator("#search").fill(mock.profile.name);
  await page.locator(`[data-workbench-entry="${mock.profile.name}"]`).waitFor({ state: "visible" });
  const statusRequestsBefore = mock.requests.filter((item) => item.pathname === "/api/aws/status").length;
  await page.evaluate(() => refreshVisibleStatuses({ background: false }));
  assert.ok(mock.requests.filter((item) => item.pathname === "/api/aws/status").length > statusRequestsBefore);
  await openWorkbench(page, mock.profile.name);

  await page.locator("#technicalDetails").evaluate((node) => { node.open = true; });
  await page.locator("#assignMemberSelect").selectOption(mock.member.email);
  await page.locator("#openMacBtn").click();
  await page.locator("#awsConfirmLayer:not(.hidden)").waitFor({ state: "visible" });
  assert.equal(await page.locator("#awsConfirmTitle").innerText(), "打开预览");
  assert.equal(await page.locator("#runAWSConfirmBtn").isDisabled(), false);
  const openPreviewShot = `${engineName}-workflow-open-preview.png`;
  await page.screenshot({ path: path.join(screenshotRoot, openPreviewShot), fullPage: true });
  screenshots.push(`qa/screenshots/${openPreviewShot}`);
  await page.locator("#cancelAWSConfirmBtn").click();

  await page.locator("[data-view=\"profilesView\"]").click();
  await page.goBack();
  await page.locator("#operationsView:not(.hidden)").waitFor({ state: "visible" });
  await page.goForward();
  await page.locator("#profilesView:not(.hidden)").waitFor({ state: "visible" });

  await page.locator("[data-view=\"userManagementView\"]").click();
  await page.locator(`[data-member-profiles="${mock.member.email}"]`).click();
  await page.locator("#memberProfileLayer:not(.hidden)").waitFor({ state: "visible" });
  await page.locator("#saveMemberProfilesBtn").click();
  await page.locator("#memberProfileLayer.hidden").waitFor({ state: "hidden" });
  await page.locator(`[data-member-edit="${mock.member.email}"]`).click();
  await page.locator("#memberFormLayer:not(.hidden)").waitFor({ state: "visible" });
  await page.locator("#memberName").fill("QA Admin Updated");
  await page.locator("#addMemberBtn").click();
  await page.locator("#memberFormLayer.hidden").waitFor({ state: "hidden" });
  assert.ok(mock.requests.some((item) => item.pathname === "/api/member/update" && item.method === "POST"));

  await page.locator("#localAgentRepairBtn").click();
  await page.locator("#localAgentRepairLayer:not(.hidden)").waitFor({ state: "visible" });
  assert.match(await page.locator("#localAgentRepairCommands").innerText(), /cm local-agent install/);
  const repairShot = `${engineName}-workflow-agent-repair.png`;
  await page.screenshot({ path: path.join(screenshotRoot, repairShot), fullPage: true });
  screenshots.push(`qa/screenshots/${repairShot}`);
  await page.locator("#localAgentRepairCloseBtn").click();

  assert.equal(mock.requests.some((item) => item.pathname.startsWith("/api/aws/") && item.postData.includes('"confirm":true')), false);
  assert.deepEqual(mock.consoleErrors, []);
  assert.deepEqual(mock.assetFailures, []);
  await context.close();

  const releaseContext = await browser.newContext({ viewport: { width: 1440, height: 900 }, ignoreHTTPSErrors: true });
  const releasePage = await releaseContext.newPage();
  const readyMock = await installMocks(releasePage, "ready", { localAgentOnline: true });
  await releasePage.goto(serverURL, { waitUntil: "networkidle" });
  await waitForApp(releasePage);
  await openWorkbench(releasePage, readyMock.profile.name);
  await releasePage.locator("#releaseMacBtn").click();
  await releasePage.locator("#awsConfirmLayer:not(.hidden)").waitFor({ state: "visible" });
  assert.equal(await releasePage.locator("#awsConfirmTitle").innerText(), "释放预览");
  assert.equal(await releasePage.locator("#awsConfirmEIPNotice").isVisible(), true);
  const releasePreviewShot = `${engineName}-workflow-release-preview.png`;
  await releasePage.screenshot({ path: path.join(screenshotRoot, releasePreviewShot), fullPage: true });
  screenshots.push(`qa/screenshots/${releasePreviewShot}`);
  await releasePage.locator("#cancelAWSConfirmBtn").click();
  assert.equal(readyMock.requests.some((item) => item.postData.includes('"confirm":true')), false);
  await releaseContext.close();

  const taskContext = await browser.newContext({ viewport: { width: 1440, height: 900 }, ignoreHTTPSErrors: true });
  const taskPage = await taskContext.newPage();
  const taskMock = await installMocks(taskPage, "releasing", { localAgentOnline: false });
  await taskPage.goto(serverURL, { waitUntil: "networkidle" });
  await waitForApp(taskPage);
  await openWorkbench(taskPage, taskMock.profile.name);
  await taskPage.reload({ waitUntil: "networkidle" });
  await waitForApp(taskPage);
  await openWorkbench(taskPage, taskMock.profile.name);
  await taskPage.locator("#workbenchActiveTask:not(.hidden)").waitFor({ state: "visible" });
  assert.match(await taskPage.locator("#workbenchTaskLabel").innerText(), /等待 Dedicated Host 可释放/);
  const restoredTaskShot = `${engineName}-workflow-restored-task.png`;
  await taskPage.screenshot({ path: path.join(screenshotRoot, restoredTaskShot), fullPage: true });
  screenshots.push(`qa/screenshots/${restoredTaskShot}`);
  await taskContext.close();

  return { engine: engineName, passed: true, awsConfirmMutations: 0, screenshots };
}

async function main() {
  await mkdir(screenshotRoot, { recursive: true });
  const server = await startStaticServer();
  const report = {
    generatedAt: new Date().toISOString(),
    server: "isolated ephemeral static server with intercepted APIs",
    awsMutationsConfirmed: 0,
    matrix: [],
    surfaces: [],
    loginChallenges: [],
    workflows: [],
  };
  try {
    for (const engine of engines) {
      const browser = await engine.launcher.launch({ headless: true });
      try {
        for (const viewport of desktopViewports) {
          for (const stateName of states) {
            report.matrix.push(await captureState(browser, engine.name, server.url, viewport, stateName));
          }
          report.matrix.push(await captureState(browser, engine.name, server.url, viewport, "empty"));
          for (const surface of ["home", "profiles-admin", "members"]) {
            report.surfaces.push(await captureSurface(browser, engine.name, server.url, viewport, surface));
          }
        }
        if (engine.name !== "firefox") {
          for (const stateName of states) {
            report.matrix.push(await captureState(browser, engine.name, server.url, mobileViewport, stateName));
          }
          report.matrix.push(await captureState(browser, engine.name, server.url, mobileViewport, "empty"));
          for (const surface of ["home", "mobile-open-confirm", "mobile-release-confirm"]) {
            report.surfaces.push(await captureSurface(browser, engine.name, server.url, mobileViewport, surface));
          }
        }
        report.loginChallenges.push(await verifyLoginChallenge(browser, engine.name, server.url));
        report.workflows.push(await verifyWorkflows(browser, engine.name, server.url));
      } finally {
        await browser.close();
      }
    }
  } finally {
    await server.close();
  }
  await writeFile(reportPath, JSON.stringify(report, null, 2) + "\n");
  console.log(`ConnectMac visual QA OK: ${report.matrix.length} state/viewports, ${report.surfaces.length} surfaces, ${report.workflows.length} workflow engines`);
}

await main();
