"use strict";

const assert = require("node:assert/strict");

// Keep CPA session/bootstrap real; mock only the external school authorization
// boundary so public-portal acceptance never uses real identities or budgets.
module.exports = async ({ browser, origin }) => {
  const context = await browser.newContext();
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const prefix = "/v0/resource/plugins/ustc/donate/";
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
    await page.goto(origin + "/v0/resource/plugins/ustc/donate");
    await page.locator("#donate-start").waitFor({ state: "visible" });
    await page.waitForFunction(() => !document.querySelector("#donate-start").disabled);
    assert.equal(await page.locator("#management-key").count(), 0);
    const cookies = await context.cookies();
    const session = cookies.find((cookie) => cookie.name === "ustc_donation_dev");
    assert(session && session.httpOnly && session.sameSite === "Strict");
    assert(!(await page.evaluate(() => document.cookie)).includes("ustc_donation_dev"));
    const protectedAPI = await context.request.get(origin + "/v8/management/credentials");
    assert([401, 403].includes(protectedAPI.status()));

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
    console.log("Donation browser acceptance passed: anonymous CPA bootstrap, HttpOnly session, consent, device authorization, confirmation, withdrawal, no management access, XSS and storage isolation.");
  } finally { await context.close(); }
};
