"use strict";

(() => {
  const element = (id) => document.getElementById(id);
  const pluginAPI = "/v0/management/plugins/ustc/";
  const hostAPI = "/v8/management";
  let managementKey = "";
  let connectionVersion = 0;
  let oauthState = "";
  let oauthTimer;
  let iwanTimer;

  function notify(message, error = false) {
    const notice = element("notice");
    notice.textContent = message;
    notice.classList.toggle("error", error);
    notice.hidden = false;
  }

  async function api(path, method = "GET", body, extraHeaders = {}) {
    if (!managementKey) throw new Error("请先连接 CPA 管理接口");
    const version = connectionVersion;
    const response = await fetch(path, {
      method, cache: "no-store", credentials: "omit", redirect: "error",
      headers: { Authorization: `Bearer ${managementKey}`, "Content-Type": "application/json", ...extraHeaders },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    if (version !== connectionVersion) throw new Error("管理连接已改变，请重试");
    const data = await response.json().catch(() => ({}));
    if (version !== connectionVersion) throw new Error("管理连接已改变，请重试");
    if (!response.ok) throw new Error(response.status === 401 || response.status === 403
      ? "CPA 管理密钥无效或不允许此地址管理"
      : typeof data.error === "string" ? data.error : `请求失败（HTTP ${response.status}）`);
    return { data, etag: response.headers.get("ETag") };
  }

  function bind(id, event, action) {
    element(id).addEventListener(event, async (inputEvent) => {
      inputEvent.preventDefault();
      const button = inputEvent.submitter || (inputEvent.currentTarget.tagName === "BUTTON" ? inputEvent.currentTarget : null);
      if (button) button.disabled = true;
      try { await action(); } catch (error) { notify(error.message, true); }
      finally { if (button) button.disabled = false; }
    });
  }

  async function loadSettings() {
    const { data } = await api(pluginAPI + "state");
    const form = element("settings-form");
    for (const [name, value] of Object.entries(data.config)) {
      if (form.elements.namedItem(name)) form.elements.namedItem(name).value = value;
    }
    element("network-status").textContent = `客户端：${data.iwan_installed ? "已安装" : "未找到"}；线路凭据：${data.iwan_configured ? "已配置（有效性以实际连接为准）" : "未配置"}`;
  }

  async function loadAccounts() {
    const { data } = await api(pluginAPI + "accounts");
    const table = element("accounts");
    table.replaceChildren();
    for (const account of data.accounts) {
      const row = document.createElement("tr");
      for (const text of [account.label || account.id, account.name, account.disabled ? "已禁用" : account.status || "就绪"]) {
        const cell = document.createElement("td");
        cell.textContent = text;
        row.append(cell);
      }
      const actions = document.createElement("td");
      const toggle = document.createElement("button");
      toggle.type = "button";
      toggle.className = "secondary";
      toggle.textContent = account.disabled ? "启用" : "禁用";
      toggle.addEventListener("click", () => accountAction(toggle, async () => {
        await api(hostAPI + "/credentials/status", "PATCH", { name: account.name, auth_index: account.auth_index, disabled: !account.disabled });
      }));
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "danger";
      remove.textContent = "删除";
      remove.addEventListener("click", () => accountAction(remove, async () => {
        if (!window.confirm(`从 CPA 号池删除 ${account.label || account.name}？`)) return;
        await api(hostAPI + "/credentials", "DELETE", { names: [account.name] });
      }));
      actions.append(toggle, remove);
      row.append(actions);
      table.append(row);
    }
    element("account-count").textContent = `CPA 号池中共有 ${data.accounts.length} 个 ustc 账号。调度策略在 CPA 中统一设置。`;
  }

  async function accountAction(button, action) {
    button.disabled = true;
    try { await action(); await loadAccounts(); }
    catch (error) { notify(error.message, true); }
    finally { button.disabled = false; }
  }

  // OAuth sessions and credential persistence belong to CPA, not this page.
  async function pollOAuth() {
    const state = oauthState;
    if (!state || !managementKey) return;
    try {
      const { data } = await api(hostAPI + "/oauth/status?state=" + encodeURIComponent(state));
      if (state !== oauthState) return;
      if (data.status === "ok") {
        oauthState = "";
        element("sso-url").hidden = true;
        element("sso-status").textContent = "科大账号已保存到 CPA 号池。";
        element("sso-callback").value = "";
        await loadAccounts();
      } else if (data.status === "error") {
        oauthState = "";
        throw new Error(data.error || "科大登录失败，请重新发起");
      } else oauthTimer = setTimeout(pollOAuth, 1500);
    } catch (error) { notify(error.message, true); }
  }

  async function pollIwan() {
    if (!managementKey) return;
    try {
      const { data } = await api(pluginAPI + "iwan/job");
      element("iwan-status").textContent = data.status === "pending" ? "iWAN 操作进行中，请按提示完成授权…" : data.message || (data.status === "success" ? "iWAN 操作完成。" : "");
      const link = element("iwan-url");
      link.hidden = !data.url;
      element("iwan-callback-form").hidden = !data.url;
      if (data.url) link.href = data.url;
      if (data.status === "pending") iwanTimer = setTimeout(pollIwan, 1000);
      else if (data.status === "success") {
        element("iwan-callback").value = "";
        if (data.binary) {
          element("settings-form").elements.namedItem("iwan-binary").value = data.binary;
          notify("客户端已安装；请点击“保存设置”应用新路径，再登录 iWAN。");
        } else await loadSettings();
      } else if (data.status === "error") notify(data.message, true);
    } catch (error) { notify(error.message, true); }
  }

  bind("connection-form", "submit", async () => {
    connectionVersion++;
    managementKey = element("management-key").value.trim();
    element("management-key").value = "";
    try {
      await loadSettings();
      await loadAccounts();
      element("workspace").hidden = false;
      notify("已连接。管理密钥仅保存在当前页面内存。");
      clearTimeout(iwanTimer);
      await pollIwan();
    } catch (error) { disconnect(); throw error; }
  });

  function disconnect() {
    connectionVersion++;
    managementKey = "";
    oauthState = "";
    clearTimeout(oauthTimer);
    clearTimeout(iwanTimer);
    element("workspace").hidden = true;
    element("management-key").value = "";
    element("key-form").reset();
    element("sso-callback").value = "";
    element("iwan-callback").value = "";
    element("accounts").replaceChildren();
    for (const id of ["sso-url", "iwan-url"]) { element(id).removeAttribute("href"); element(id).hidden = true; }
  }

  bind("disconnect", "click", () => { disconnect(); notify("已断开并清除页面中的密钥。"); });
  bind("reload-settings", "click", loadSettings);
  bind("reload-accounts", "click", loadAccounts);
  bind("settings-form", "submit", async () => {
    const values = Object.fromEntries(new FormData(element("settings-form")));
    await api(pluginAPI + "validate", "POST", values);
    const path = hostAPI + "/config/plugins/configs/ustc";
    const current = await api(path);
    const config = current.data.value ?? current.data;
    await api(path, "PUT", { ...config, ...values }, current.etag ? { "If-Match": current.etag } : {});
    notify("设置已保存到 CPA。配置热更新可能需要几秒；修改网络设置会结束进行中的登录。");
  });
  bind("key-form", "submit", async () => {
    const form = element("key-form");
    const values = Object.fromEntries(new FormData(form));
    await api(pluginAPI + "accounts", "POST", values);
    form.reset();
    await loadAccounts();
    notify("API Key 账号已加入 CPA 号池。");
  });
  bind("sso-login", "click", async () => {
    if (oauthState) throw new Error("已有登录进行中，请先完成或刷新页面后重试");
    const { data } = await api(hostAPI + "/oauth/auth-url?provider=ustc");
    oauthState = data.state;
    const link = element("sso-url");
    link.href = data.url;
    link.hidden = false;
    element("sso-status").textContent = "请点击下方链接，在学校官方页面完成登录。";
    clearTimeout(oauthTimer);
    oauthTimer = setTimeout(pollOAuth, 1500);
  });
  bind("sso-callback-form", "submit", async () => {
    await api(pluginAPI + "oauth/callback", "POST", { url: element("sso-callback").value.trim() });
    element("sso-callback").value = "";
    clearTimeout(oauthTimer);
    await pollOAuth();
  });
  bind("iwan-login", "click", async () => {
    await api(pluginAPI + "iwan/login", "POST", {});
    clearTimeout(iwanTimer);
    await pollIwan();
  });
  bind("iwan-install", "click", async () => {
    if (!window.confirm("安装非官方第三方 ustc-iwan v26.9.1 客户端？下载后将校验发布附件的 SHA-256，不修改系统 VPN。")) return;
    await api(pluginAPI + "iwan/install", "POST", {});
    clearTimeout(iwanTimer);
    await pollIwan();
  });
  bind("iwan-callback-form", "submit", async () => {
    await api(pluginAPI + "iwan/callback", "POST", { url: element("iwan-callback").value.trim() });
    element("iwan-callback").value = "";
    notify("iWAN 授权已提交，正在获取线路配置。");
  });
  window.addEventListener("pagehide", disconnect);
})();
