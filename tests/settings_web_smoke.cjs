"use strict";

const assert = require("node:assert/strict");

// Keep conditional writes on the real CPA host; only emulate reverse-proxy
// headers and competing configuration saves at the browser boundary.
module.exports = async ({ page, origin, managementKey }) => {
  const path = origin + "/v8/management/config/plugins/configs/ustc";
  const headers = { Authorization: `Bearer ${managementKey}` };
  const serverInput = page.locator('[name="iwan-server"]');
  const originalServer = await serverInput.inputValue();
  const marker = "browser-concurrent-test";
  let markerWritten = false;
  let mode = "weak";
  let reads = [];
  let writes = [];
  let changes = 0;

  async function readConfig() {
    const response = await page.request.get(path, { headers });
    assert.equal(response.status(), 200);
    return { data: await response.json(), etag: response.headers().etag };
  }

  async function submit() {
    const button = page.getByRole("button", { name: "保存设置", exact: true });
    await button.click();
    await page.waitForFunction(() => !document.querySelector('#settings-form button[type="submit"]').disabled);
    return page.locator("#notice").textContent();
  }

  async function intercept(route) {
    const request = route.request();
    if (request.method() === "GET") {
      const response = await route.fetch();
      const responseHeaders = { ...response.headers() };
      reads.push(responseHeaders.etag);
      if (mode === "missing") delete responseHeaders.etag;
      else if (mode === "rewritten") responseHeaders.etag = 'W/"proxy-generated-tag"';
      else responseHeaders.etag = "W/" + responseHeaders.etag;
      await route.fulfill({ response, headers: responseHeaders });
      return;
    }
    if (request.method() !== "PUT") { await route.continue(); return; }
    writes.push(request.headers()["if-match"]);
    if (mode === "conflict" || (mode === "retry" && writes.length === 1)) {
      const current = await readConfig();
      const response = await page.request.patch(path, {
        headers: { ...headers, "If-Match": current.etag }, data: { [marker]: ++changes },
      });
      assert.equal(response.status(), 200);
      markerWritten = true;
    }
    if (mode === "server-error") {
      await route.fulfill({ status: 500, json: { error: "settings-test-error" } });
      return;
    }
    await route.continue();
  }

  await page.route(path, intercept);
  try {
    assert.match(await submit(), /设置已保存到 CPA/, "a weakened source-hash ETag must still permit a conditional save");
    assert.equal(reads.length, 1);
    assert.deepEqual(writes, reads);
    assert.match(writes[0], /^"[a-f0-9]{64}"$/);

    mode = "retry"; reads = []; writes = [];
    assert.match(await submit(), /设置已保存到 CPA/, "one competing save should trigger a fresh read and retry");
    assert.equal(reads.length, 2);
    assert.deepEqual(writes, reads);
    assert.notEqual(writes[0], writes[1]);
    assert.equal((await readConfig()).data[marker], changes, "retry must retain newly added configuration fields");

    mode = "conflict"; reads = []; writes = [];
    await serverInput.fill("browser-unsaved-line");
    assert.match(await submit(), /配置.*修改.*尚未保存/);
    assert.equal(reads.length, 3, "configuration conflicts must have a bounded retry count");
    assert.deepEqual(writes, reads);
    assert.equal(await serverInput.inputValue(), "browser-unsaved-line");
    assert.equal((await readConfig()).data["iwan-server"], originalServer);

    for (const missingMode of ["missing", "rewritten"]) {
      mode = missingMode; reads = []; writes = [];
      assert.match(await submit(), /ETag/);
      assert.equal(reads.length, 1);
      assert.equal(writes.length, 0, "missing or unrecognized version headers must never cause an unconditional write");
      assert.equal(await serverInput.inputValue(), "browser-unsaved-line");
    }

    mode = "server-error"; reads = []; writes = [];
    assert.equal(await submit(), "settings-test-error");
    assert.equal(reads.length, 1);
    assert.deepEqual(writes, reads, "non-conflict errors must not be retried");
    console.log("Settings browser acceptance passed: proxy-weakened ETags, fresh conflict retries, preserved fields, bounded failures and no unconditional writes.");
  } finally {
    await page.unroute(path, intercept);
    await serverInput.fill(originalServer);
    if (markerWritten) {
      const current = await readConfig();
      const response = await page.request.delete(path + "/" + marker, { headers: { ...headers, "If-Match": current.etag } });
      assert.equal(response.status(), 200);
    }
  }
};
