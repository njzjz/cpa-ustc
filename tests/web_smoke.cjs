"use strict";

// Optional browser acceptance against the real CPA process started by native_smoke.py.
// Install Playwright outside the repository and point CPA_USTC_PLAYWRIGHT_MODULE at it.
const assert = require("node:assert/strict");
const { chromium } = require(process.env.CPA_USTC_PLAYWRIGHT_MODULE || "playwright");

(async () => {
  const origin = process.env.CPA_USTC_WEB_URL;
  const managementKey = process.env.CPA_USTC_WEB_KEY;
  const browser = await chromium.launch({
    headless: true,
    ...(process.env.CPA_USTC_BROWSER ? { executablePath: process.env.CPA_USTC_BROWSER } : {}),
    args: ["--disable-gpu", "--no-sandbox"],
  });
  const page = await browser.newPage({ viewport: { width: 1080, height: 900 } });
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  try {
    await page.goto(origin + "/v0/resource/plugins/ustc/settings");
    await page.locator("#management-key").fill(managementKey);
    await page.getByRole("button", { name: "连接", exact: true }).click();
    await page.locator("#workspace").waitFor({ state: "visible" });
    assert.equal(await page.locator("#management-key").inputValue(), "");

    await page.locator('[name="iwan-server"]').fill("browser-saved-line");
    assert.equal(await page.locator('[name="budget-cooldown"]').inputValue(), "30m");
    await page.locator('[name="budget-cooldown"]').fill("3h");
    await page.locator('[name="iwan-binary"]').fill(process.env.CPA_USTC_WEB_IWAN);
    await page.getByRole("button", { name: "保存设置", exact: true }).click();
    await page.waitForFunction(() => document.querySelector("#notice").textContent.includes("设置已保存到 CPA"));
    let settings;
    for (let attempt = 0; attempt < 50; attempt++) {
      const response = await page.request.get(origin + "/v0/management/plugins/ustc/state", { headers: { Authorization: `Bearer ${managementKey}` } });
      settings = await response.json();
      if (settings.config["iwan-server"] === "browser-saved-line") break;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    assert.equal(settings.config["iwan-server"], "browser-saved-line");
    assert.equal(settings.config["budget-cooldown"], "3h");

    const label = 'Browser Account <img src=x onerror="window.injected=true">';
    await page.locator('#key-form [name="label"]').fill(label);
    await page.locator('#key-form [name="api_key"]').fill("native-test-key-b");
    await page.getByRole("button", { name: "加入 CPA 号池", exact: true }).click();
    const row = page.locator("#accounts tr").filter({ hasText: "Browser Account" });
    await row.waitFor();
    assert.equal(await row.locator("td").first().textContent(), label);
    assert.equal(await page.locator('#key-form [name="api_key"]').inputValue(), "");
    assert.equal(await row.locator("img").count(), 0);
    assert.equal(await page.evaluate(() => window.injected), undefined);
    await row.getByRole("button", { name: "查询额度", exact: true }).click();
    await row.getByText(/仅 API Key 无法查询额度/).waitFor();

    const quotaURL = "**/v8/management/plugins/ustc/quota";
    await page.route(quotaURL, async (route) => {
      const body = route.request().postDataJSON();
      assert.equal(typeof body.auth_index, "string");
      await route.fulfill({ json: { groups: [{ displayName: "用户共享额度", buckets: [{
        window: "24h", remainingFraction: 0.75, resetTime: "2026-11-03T16:00:00Z",
        description: '剩余 75 / 100 <img src=x onerror="window.quotaInjected=true">',
      }] }] } });
    });
    await row.getByRole("button", { name: "查询额度", exact: true }).click();
    await row.getByText(/用户共享额度 24h：剩余 75.0%/).waitFor();
    assert((await row.textContent()).includes("重置时间："));
    assert.equal(await row.locator("img").count(), 0);
    assert.equal(await page.evaluate(() => window.quotaInjected), undefined);
    await page.unroute(quotaURL);

    await page.route(quotaURL, (route) => route.fulfill({ json: { groups: [{ displayName: "API Key 限额", buckets: [
      { window: "3h", remainingFraction: 0.6, description: "仅部分额度快照可用，未展示部分状态未知" },
      { window: "12h", remainingFraction: 0.01, description: "仅部分额度快照可用，未展示部分状态未知" },
    ] }] } }));
    await row.getByRole("button", { name: "查询额度", exact: true }).click();
    await row.getByText(/API Key 限额 3h：剩余 60.0%/).waitFor();
    assert.equal(await row.getByText(/未展示部分状态未知/).count(), 2);
    assert(!(await row.textContent()).includes("24h"));
    await page.unroute(quotaURL);
    await row.getByRole("button", { name: "禁用", exact: true }).click();
    await row.getByRole("button", { name: "启用", exact: true }).waitFor();
    await row.getByRole("button", { name: "启用", exact: true }).click();
    await row.getByRole("button", { name: "禁用", exact: true }).waitFor();

    const diagnosticsURL = "**/v0/management/plugins/ustc/accounts";
    await page.route(diagnosticsURL, async (route) => {
      const response = await route.fetch();
      const data = await response.json();
      const account = data.accounts.find((entry) => entry.label === label);
      Object.assign(account, {
        status: "error", unavailable: true,
        status_message: 'HTTP 429 <img src=x onerror="window.errorInjected=true">',
        next_retry_after: new Date(Date.now() + 60000).toISOString(),
      });
      await route.fulfill({ response, json: data });
    });
    await page.getByRole("button", { name: "刷新账号", exact: true }).click();
    await row.getByText("冷却中", { exact: true }).waitFor();
    assert((await row.textContent()).includes("HTTP 429 <img"));
    assert((await row.textContent()).includes("可重试时间："));
    assert.equal(await row.locator("img").count(), 0);
    assert.equal(await page.evaluate(() => window.errorInjected), undefined);
    await page.unroute(diagnosticsURL);
    await page.getByRole("button", { name: "刷新账号", exact: true }).click();
    await row.getByRole("button", { name: "禁用", exact: true }).waitFor();

    await page.getByRole("button", { name: "登录 / 更新 iWAN", exact: true }).click();
    await page.locator("#iwan-url").waitFor({ state: "visible" });
    assert.match(await page.locator("#iwan-url").getAttribute("href"), /^https:\/\/auth\.ivpn\.ustc\.edu\.cn\//);
    assert.equal(new URL(await page.locator("#iwan-url").getAttribute("href")).searchParams.get("code_challenge"), "challenge");
    await page.locator("#iwan-callback").fill("com.panabit.mobile://oauth2redirect?state=browser-state&code=browser-code");
    await page.getByRole("button", { name: "提交 iWAN 授权", exact: true }).click();
    await page.waitForFunction(() => document.querySelector("#iwan-status").textContent.includes("iWAN 操作完成"));
    assert.equal(await page.locator("#iwan-callback").inputValue(), "");

    if (process.env.CPA_USTC_SCREENSHOT) await page.screenshot({ path: process.env.CPA_USTC_SCREENSHOT, fullPage: true });
    page.once("dialog", (dialog) => dialog.accept());
    await row.getByRole("button", { name: "删除", exact: true }).click();
    await row.waitFor({ state: "detached" });
    assert.equal(await page.locator("#accounts tr").count(), 1);
    const persisted = await page.evaluate(() => ({ local: { ...localStorage }, session: { ...sessionStorage } }));
    assert(!JSON.stringify(persisted).includes(managementKey));
    assert(!JSON.stringify(persisted).includes("native-test-key-b"));
    await page.getByRole("button", { name: "断开并清除密钥", exact: true }).click();
    assert.equal(await page.locator("#workspace").isHidden(), true);
    await require("./donation_web_smoke.cjs")({ browser, origin });
    assert.deepEqual(errors, []);
    console.log("Browser acceptance passed: configuration, CPA accounts, native quota query/display, cooldown diagnostics, native status/delete, iWAN callback, secret handling.");
  } catch (error) {
    console.error("Browser notice:", await page.locator("#notice").textContent());
    console.error("iWAN status:", await page.locator("#iwan-status").textContent());
    if (process.env.CPA_USTC_SCREENSHOT) await page.screenshot({ path: process.env.CPA_USTC_SCREENSHOT, fullPage: true });
    throw error;
  } finally {
    await browser.close();
  }
})().catch((error) => { console.error(error); process.exitCode = 1; });
