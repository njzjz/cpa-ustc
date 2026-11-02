"use strict";

(() => {
  const element = (identifier) => document.getElementById(identifier);
  const prefix = "/v0/resource/plugins/ustc/donate/";
  let csrf = "";
  let confirmation = "";
  let action = "";
  let timer;

  function notice(message, error = false) {
    element("notice").textContent = message;
    element("notice").classList.toggle("error", error);
  }

  async function request(operation, headers = {}) {
    const response = await fetch(prefix + operation, {
      method: "GET", mode: "same-origin", credentials: "same-origin", cache: "no-store", redirect: "error",
      headers: { "X-USTC-Donation-Origin": location.origin, "X-USTC-Donation-CSRF": csrf, ...headers },
    });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(typeof body.error === "string" ? body.error : `请求失败（HTTP ${response.status}）`);
    return body;
  }

  function startButtons(disabled) {
    element("donate-start").disabled = disabled;
    element("withdraw-start").disabled = disabled;
  }

  function render(state) {
    action = state.action || "";
    element("device").hidden = state.status !== "pending";
    element("confirmation").hidden = state.status !== "ready";
    if (state.status === "pending") {
      const url = new URL(state.verification_url);
      if (url.protocol !== "https:" || url.host !== "id.ustc.edu.cn" || url.username || url.password || url.hash) throw new Error("学校授权链接校验失败");
      element("verification-link").href = url.href;
      element("user-code").textContent = state.user_code;
      element("device-expiry").textContent = `学校授权有效至：${new Date(state.expires_at).toLocaleString()}；本站会话最多 10 分钟。`;
      notice("请在学校页面完成认证，完成后返回本页确认。");
      clearTimeout(timer);
      timer = setTimeout(poll, Math.max(5, Number(state.interval) || 5) * 1000);
    } else {
      element("verification-link").removeAttribute("href");
      element("user-code").textContent = "";
    }
    if (state.status === "ready") {
      confirmation = state.confirmation;
      element("identity-label").textContent = state.label || "科大账号";
      element("confirm").textContent = action === "withdraw" ? "确认撤回我的捐献" : "确认捐献并加入号池";
      element("confirmation-description").textContent = action === "withdraw"
        ? "仅停用本次认证身份的捐献并清除其令牌，不影响其他账号。"
        : "请确认这是你自己的账号；确认后本 CPA 可持续使用其学校模型额度。";
      notice("学校认证成功，尚未修改号池。请确认身份与操作。");
    }
    if (state.status === "done" || state.status === "error") {
      confirmation = "";
      notice(state.message || "本次操作已结束。", state.status === "error");
      startButtons(false);
    }
  }

  async function poll() {
    try { render(await request("poll")); }
    catch (error) { notice(error.message + " 请刷新页面重新建立会话。", true); }
  }

  async function start(selectedAction) {
    if (selectedAction === "donate" && !element("consent").checked) {
      notice("请先阅读说明并明确同意模型调用授权。", true);
      return;
    }
    startButtons(true);
    element("confirmation").hidden = true;
    try {
      render(await request("start", { "X-USTC-Donation-Action": selectedAction,
        ...(selectedAction === "donate" ? { "X-USTC-Donation-Consent": "share" } : {}) }));
    } catch (error) { notice(error.message, true); startButtons(false); }
  }

  element("donate-start").addEventListener("click", () => start("donate"));
  element("withdraw-start").addEventListener("click", () => start("withdraw"));
  element("confirm").addEventListener("click", async () => {
    element("confirm").disabled = true;
    try { render(await request("confirm", { "X-USTC-Donation-Confirm": confirmation })); }
    catch (error) { notice(error.message + " 如需继续，请重新通过学校认证。", true); startButtons(false); }
    finally { confirmation = ""; element("confirm").disabled = false; element("confirmation").hidden = true; }
  });

  request("session").then((state) => {
    csrf = state.csrf;
    if (state.status === "pending" || state.status === "ready") { startButtons(true); poll(); }
    else { startButtons(false); notice("安全会话已建立。请选择捐献或撤回；无需提供任何 CPA 密钥。"); }
  }).catch((error) => notice(error.message, true));
  window.addEventListener("pagehide", () => { clearTimeout(timer); csrf = ""; confirmation = ""; });
})();
