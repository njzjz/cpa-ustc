"use strict";

const assert = require("node:assert/strict");
const { spawn } = require("node:child_process");
const { once } = require("node:events");
const net = require("node:net");
const path = require("node:path");

// Keep CPA session/bootstrap real; mock only the external school authorization
// boundary so public-portal acceptance never uses real identities or budgets.
module.exports = async ({ browser, origin: cpaOrigin, managementKey }) => {
  const listener = net.createServer();
  listener.listen(0, "127.0.0.1");
  await once(listener, "listening");
  const port = listener.address().port;
  await new Promise((resolve) => listener.close(resolve));
  const origin = `http://127.0.0.1:${port}`;
  const configPath = cpaOrigin + "/v8/management/config/plugins/configs/ustc";
  const admin = await browser.newContext({ extraHTTPHeaders: { Authorization: `Bearer ${managementKey}` } });
  try {
    const current = await admin.request.get(configPath);
    assert(current.ok());
    const data = await current.json();
    const saved = await admin.request.put(configPath, { headers: { "If-Match": current.headers().etag }, data: {
      ...(data.value || data), "donation-mode": "open", "donation-origin": origin,
    } });
    assert(saved.ok(), `gateway config save failed: ${saved.status()} ${await saved.text()}`);
    await assertEventually(async () => {
      const response = await admin.request.get(cpaOrigin + "/v0/management/plugins/ustc/state");
      return response.ok() && (await response.json()).config["donation-origin"] === origin;
    });
  } finally { await admin.close(); }
  const gateway = spawn(path.resolve("dist", process.platform === "win32" ? "cpa-ustc.exe" : "cpa-ustc"), [
    "serve-donations", "--listen", `127.0.0.1:${port}`, "--origin", origin, "--cpa-url", cpaOrigin,
    "--management-key-env", "CPA_USTC_TEST_GATEWAY_KEY",
  ], { env: { ...process.env, CPA_USTC_TEST_GATEWAY_KEY: managementKey }, stdio: "ignore" });
  const context = await browser.newContext();
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const prefix = "/api/donation/";
  let action = "";
  let confirmations = 0;
  let csrf = "";
  let intent = "";
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.startsWith(prefix)) {
      assert.equal(request.headers().authorization, undefined);
      assert.equal(new URL(request.url()).search, "");
    }
  });
  try {
    await assertEventually(async () => {
      const response = await context.request.get(origin + "/");
      return response.ok();
    });
    await page.route("**" + prefix + "start", async (route) => {
      const headers = await route.request().allHeaders();
      assert.equal(headers["x-ustc-donation-origin"], origin);
      assert.equal(headers.authorization, undefined);
      csrf = headers["x-ustc-donation-csrf"];
      assert(csrf.length >= 40);
      action = headers["x-ustc-donation-action"];
      if (action === "donate") assert.equal(headers["x-ustc-donation-consent"], "share");
      await route.fulfill({ json: { status: "pending", action, user_code: "TEST-CODE", interval: 5,
        verification_url: "https://id.ustc.edu.cn/oidc/device?user_code=TEST-CODE", expires_at: new Date(Date.now() + 600000).toISOString() } });
    });
    await page.route("**" + prefix + "poll", (route) => {
      intent = `one-use-confirmation-${confirmations}`;
      return route.fulfill({ json: { status: "ready", action, label: '<img src=x onerror="window.donorInjected=true">', confirmation: intent } });
    });
    await page.route("**" + prefix + "confirm", async (route) => {
      assert.equal(route.request().headers()["x-ustc-donation-csrf"], csrf);
      assert.equal(route.request().headers()["x-ustc-donation-confirm"], intent);
      confirmations++;
      await route.fulfill({ json: { status: "done", action, message: action === "withdraw" ? "捐献已撤回" : "捐献成功" } });
    });
    await page.goto(origin + "/");
    await page.locator("#donate-start").waitFor({ state: "visible" });
    await page.waitForFunction(() => !document.querySelector("#donate-start").disabled);
    assert.equal(await page.locator("#management-key").count(), 0);
    const cookies = await context.cookies();
    const session = cookies.find((cookie) => cookie.name === "ustc_donation_dev");
    assert(session && session.httpOnly && session.sameSite === "Strict");
    assert(!(await page.evaluate(() => document.cookie)).includes("ustc_donation_dev"));
    const protectedAPI = await context.request.get(origin + "/v8/management/credentials");
    assert.equal(protectedAPI.status(), 404);

    await page.locator("#donate-start").click();
    await page.getByText("请先阅读说明并明确同意模型调用授权。", { exact: true }).waitFor();
    await page.locator("#consent").check();
    await page.locator("#donate-start").click();
    await page.locator("#user-code").getByText("TEST-CODE", { exact: true }).waitFor();
    assert.match(await page.locator("#verification-link").getAttribute("href"), /^https:\/\/id\.ustc\.edu\.cn\//);
    await page.locator("#confirmation").waitFor({ state: "visible" });
    assert.equal(confirmations, 0);
    assert.equal(await page.locator("#identity-label img").count(), 0);
    assert.equal(await page.evaluate(() => window.donorInjected), undefined);
    await page.getByRole("button", { name: "确认捐献并加入号池", exact: true }).click();
    await page.getByText("捐献成功", { exact: true }).waitFor();
    await page.locator("#consent").uncheck();
    await page.locator("#withdraw-start").click();
    await page.getByRole("button", { name: "确认撤回我的捐献", exact: true }).waitFor();
    await page.getByRole("button", { name: "确认撤回我的捐献", exact: true }).click();
    await page.getByText("捐献已撤回", { exact: true }).waitFor();
    assert.equal(confirmations, 2);
    const storage = await page.evaluate(() => ({ local: { ...localStorage }, session: { ...sessionStorage } }));
    assert.deepEqual(storage, { local: {}, session: {} });
    assert.deepEqual(errors, []);
    console.log("Donation browser acceptance passed: restricted gateway, authenticated CPA bootstrap, HttpOnly session, consent, confirmation, withdrawal, no browser key or management access.");
  } finally {
    await context.close();
    gateway.kill("SIGINT");
    if (gateway.exitCode === null) await once(gateway, "exit");
  }
};

async function assertEventually(check) {
  const deadline = Date.now() + 10000;
  while (Date.now() < deadline) {
    try { if (await check()) return; } catch {}
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  assert.fail("donation gateway or configuration did not become ready");
}
