(function(){
  "use strict";
  var $ = function(s){ return document.querySelector(s); };
  var state = { data:null };
  // 设置表单是否被用户改过：改过之后 refresh() 不得用服务端值覆盖。
  var settingsDirty = false;

  function toast(msg, kind){
    var t = $("#toast");
    t.textContent = msg;
    t.className = "toast show " + (kind||"");
    // 错误必须能被读到：切到 alert 且不自动消失（点击关闭）。
    t.setAttribute("role", kind === "err" ? "alert" : "status");
    t.setAttribute("aria-live", kind === "err" ? "assertive" : "polite");
    clearTimeout(toast._t);
    if (kind !== "err") {
      toast._t = setTimeout(function(){ t.className = "toast"; }, 4600);
    }
  }
  function pw(){ return $("#pw").value.trim(); }

  // 管理密码只放内存 + sessionStorage（当前标签页），绝不再写 localStorage：
  // 明文密码落在 Local Storage 里，任何同源脚本或本机进程都能直接读走，
  // 而它等价于「导出完整登录态」的权限。
  var PW_KEY = "codearts2api_pw";
  var pwMigrated = false;
  function loadPw(){
    var v = "";
    try {
      if (localStorage.getItem(PW_KEY) !== null) {
        localStorage.removeItem(PW_KEY);
        pwMigrated = true;
      }
      v = sessionStorage.getItem(PW_KEY) || "";
    } catch(e){}
    return v;
  }
  function storePw(v){
    try {
      if (v) sessionStorage.setItem(PW_KEY, v);
      else sessionStorage.removeItem(PW_KEY);
    } catch(e){}
  }
  function clearStoredPw(){
    try { localStorage.removeItem(PW_KEY); sessionStorage.removeItem(PW_KEY); } catch(e){}
  }

  function api(path, opts){
    opts = opts || {};
    var h = { "Content-Type":"application/json" };
    if (pw()) h["X-Admin-Password"] = pw();
    return fetch(path, {
      method: opts.method || "GET",
      headers: h,
      body: opts.body ? JSON.stringify(opts.body) : undefined
    }).then(function(r){
      return r.text().then(function(txt){
        var j = null;
        try { j = JSON.parse(txt); } catch(e){}
        if (!r.ok) {
          var m = (j && j.error && j.error.message) || txt || ("HTTP " + r.status);
          var err = new Error(m); err.status = r.status; throw err;
        }
        return j;
      });
    });
  }

  // --- DOM 构建助手（全程 textContent，绝不拼接 HTML） ---
  function mk(tag, cls, text){
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }
  function cell(text, cls){ return mk("td", cls, text); }
  function row(cells){
    var r = mk("tr");
    for (var i = 0; i < cells.length; i++) r.appendChild(cells[i]);
    return r;
  }
  function pill(text, cls){ return mk("span", "badge " + (cls||""), text); }
  function emptyRow(cols, text){
    var c = cell(text, "muted");
    c.colSpan = cols;
    var r = mk("tr");
    r.appendChild(c);
    return r;
  }
  function btn(label, cls, fn){
    var b = mk("button", "btn " + (cls||""), label);
    b.type = "button";
    b.onclick = busyable(b, fn);
    return b;
  }

  // busyable 在异步操作期间禁用按钮，避免双击造成重复提交
  // （重复签到 / 重复创建密钥 / 重复删除）。
  function busyable(el, fn){
    return function(ev){
      if (el && el.disabled) return;
      var r = fn.call(this, ev);
      if (el && r && typeof r.then === "function") {
        el.disabled = true;
        el.setAttribute("aria-busy", "true");
        r.then(function(){}, function(){}).then(function(){
          el.disabled = false;
          el.removeAttribute("aria-busy");
        });
      }
      return r;
    };
  }
  // on 给静态按钮绑定处理器并自动加 in-flight 保护。
  function on(id, fn){
    var el = $(id);
    el.onclick = busyable(el, fn);
    return el;
  }

  function legacyCopy(text){
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.className = "sr-only";
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try { ok = document.execCommand("copy"); } catch(e){ ok = false; }
    document.body.removeChild(ta);
    return ok;
  }
  // copyText 优先用异步剪贴板 API（127.0.0.1 属于安全上下文），
  // 失败再回退到已废弃的 execCommand。
  function copyText(text, okMsg){
    var done = function(ok){
      toast(ok ? (okMsg || "已复制") : "复制失败，请手动选择后按 Ctrl+C", ok ? "ok" : "err");
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).then(
        function(){ done(true); },
        function(){ done(legacyCopy(text)); });
    }
    done(legacyCopy(text));
    return Promise.resolve();
  }

  function fmtTime(s){
    if (!s) return "—";
    var d = new Date(s * 1000);
    var p = function(n){ return (n<10?"0":"")+n; };
    return d.getFullYear()+"-"+p(d.getMonth()+1)+"-"+p(d.getDate())+" "+p(d.getHours())+":"+p(d.getMinutes())+":"+p(d.getSeconds());
  }
  function fmtDur(sec){
    if (sec === undefined || sec === null) return "—";
    if (sec <= 0) return "已过期";
    var h = Math.floor(sec/3600), m = Math.floor((sec%3600)/60);
    if (h > 0) return h + " 小时 " + m + " 分";
    return m + " 分 " + (sec%60) + " 秒";
  }
  function fmtNum(n){
    if (n === undefined || n === null) return "—";
    return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  }

  function setHealth(cls, text){
    var el = $("#health");
    el.className = "badge " + cls;
    el.textContent = "";
    el.appendChild(mk("i", "dot"));
    el.appendChild(mk("span", null, text));
  }

  function loadHealth(){
    fetch("/health").then(function(r){ return r.json(); }).then(function(h){
      var ready = h.ready || 0, total = h.accounts || 0;
      var ok = total > 0 && ready > 0;
      setHealth(ok ? "ok" : (total === 0 ? "warn" : "err"),
        "账号 "+ready+"/"+total+" 就绪 · 模型 "+(h.models||0));
    }).catch(function(){ setHealth("err", "不可达"); });
  }

  function renderOverview(d){
    var s = d.stats || {};
    var accts = d.accounts || [];
    var ready = accts.filter(function(a){ return a.enabled && a.status === "ok"; }).length;
    var soonest = null;
    accts.forEach(function(a){
      if (a.expires_at && (soonest === null || a.expires_at < soonest)) soonest = a.expires_at;
    });
    var kpis = [
      ["总请求", fmtNum(s.total_requests || 0)],
      ["失败请求", fmtNum(s.failed_requests || 0)],
      ["输出字符", fmtNum(s.total_chars_out || 0)],
      ["账号（就绪/总）", ready + " / " + accts.length],
      ["密钥", (d.keys||[]).length],
      ["今日签到", accts.filter(function(a){ return a.claimed_today; }).length + " / " + accts.length],
      ["积分模型", (d.models||[]).filter(function(m){ return m.channel === "credit"; }).length +
        ((d.settings||{}).allow_credit_models ? " 已启用" : " 已关闭")],
      ["启动时间", fmtTime(s.started_at)]
    ];
    var box = $("#kpis");
    box.textContent = "";
    kpis.forEach(function(k){
      var c = mk("div", "kpi");
      c.appendChild(mk("div", "k", k[0]));
      c.appendChild(mk("div", "v", k[1]));
      if (k[0].indexOf("账号") === 0 && soonest) {
        c.appendChild(mk("div", "sub", "最早过期 " + fmtTime(soonest)));
      }
      box.appendChild(c);
    });

    renderBalance(d.balance);

    var tb = $("#models");
    tb.textContent = "";
    var models = d.models || [];
    if (!models.length) { tb.appendChild(emptyRow(7, "无")); return; }
    models.forEach(function(m){
      var isCredit = m.channel === "credit";
      var chCell = cell("");
      chCell.appendChild(isCredit ? pill("积分", "warn") : pill("免费额度", "ok"));
      tb.appendChild(row([
        cell(m.id, "mono"), cell(m.name), chCell,
        cell(isCredit ? (m.credit || "—") : "—", "mono"),
        cell(m.context ? fmtNum(m.context) : "—", "mono"),
        cell(m.max_output ? fmtNum(m.max_output) : "—", "mono"),
        cell(m.description, "muted")
      ]));
    });
    $("#modelHint").textContent = d.upstream_error ? ("最近一次上游刷新失败：" + d.upstream_error) : "";
  }

  function renderBalance(b){
    var el = $("#balance");
    el.textContent = "";
    if (!b) { el.className = "muted"; el.textContent = "尚未查询。"; return; }
    el.className = "";
    var used = b.daily_tokens_used || 0, limit = b.daily_token_limit || 0;
    var pct = limit > 0 ? Math.min(100, Math.round(used * 100 / limit)) : 0;
    var line = mk("div", null,
      "通道 " + (b.channel || "—") +
      " · 今日已用 " + fmtNum(used) + " / " + fmtNum(limit) + " tokens" +
      " · 剩余 " + fmtNum(b.total_balance) + "（" + pct + "%）");
    el.appendChild(line);
    var bar = mk("div", "bar " + (pct >= 90 ? "err" : (pct >= 70 ? "warn" : "")));
    var fill = mk("i");
    fill.style.width = pct + "%";
    bar.appendChild(fill);
    el.appendChild(bar);
  }

  function renderAccounts(d){
    var tb = $("#accounts");
    tb.textContent = "";
    var accts = d.accounts || [];
    if (!accts.length) {
      tb.appendChild(emptyRow(9, "暂无账号，点击「导入本机 CodeArts Agent 登录态」或在下方手工录入"));
      return;
    }
    accts.forEach(function(a){
      var statusCell = cell("");
      if (a.enabled) {
        var okStatus = a.status === "ok";
        statusCell.appendChild(pill(okStatus ? "ok" : (a.status || "error"), okStatus ? "ok" : "err"));
      } else {
        statusCell.appendChild(pill("已停用"));
      }
      if (a.has_refresh) statusCell.appendChild(document.createTextNode(" "));
      if (a.has_refresh) statusCell.appendChild(pill("可续期", "ok"));

      var expCell = cell("", "mono");
      if (a.expires_at) {
        expCell.textContent = fmtTime(a.expires_at) + "（" + fmtDur(a.expires_in) + "）";
        if (a.expires_in !== undefined && a.expires_in <= 300) expCell.classList.add("warn-text");
      } else {
        expCell.textContent = "—";
      }

      var claimCell = cell("");
      if (a.claimed_today) {
        claimCell.appendChild(pill("已领", "ok"));
      } else {
        claimCell.appendChild(pill("未领", "warn"));
      }
      if (a.last_claim_msg) claimCell.title = a.last_claim_msg;
      if (a.last_claim_err) claimCell.title = a.last_claim_err;

      var ops = cell("");
      ops.appendChild(btn("探活", "", function(){ checkin(a.id); }));
      ops.appendChild(document.createTextNode(" "));
      ops.appendChild(btn("签到", "", function(){ claim(a.id); }));
      ops.appendChild(document.createTextNode(" "));
      ops.appendChild(btn("重读", "", function(){ accountOp("reimport", {id:a.id}); }));
      ops.appendChild(document.createTextNode(" "));
      ops.appendChild(btn("导出", "", function(){ exportSession(a.id); }));
      ops.appendChild(document.createTextNode(" "));
      ops.appendChild(btn(a.enabled ? "停用" : "启用", "", function(){ accountOp("toggle", {id:a.id}); }));
      if (a.failures > 0) {
        ops.appendChild(document.createTextNode(" "));
        ops.appendChild(btn("清错", "", function(){ accountOp("clear-errors", {id:a.id}); }));
      }
      ops.appendChild(document.createTextNode(" "));
      ops.appendChild(btn("删除", "danger", function(){
        if (confirm("确定删除账号 " + a.name + "？")) accountOp("remove", {id:a.id});
      }));

      var r = row([
        cell(a.name), cell(a.source, "muted"), cell(a.access_key || "—", "mono"),
        expCell, statusCell, claimCell, cell(a.failures, "mono"),
        cell(fmtTime(a.last_used), "muted"), ops
      ]);
      if (a.last_err) r.title = a.last_err;
      tb.appendChild(r);
    });
  }

  function renderKeys(d){
    var tb = $("#keys");
    tb.textContent = "";
    var keys = d.keys || [];
    if (!keys.length) {
      tb.appendChild(emptyRow(7, "暂无密钥（未签发时 /v1 仅允许本机直连）"));
      return;
    }
    keys.forEach(function(k){
      var st = cell("");
      st.appendChild(pill(k.enabled ? "启用" : "停用", k.enabled ? "ok" : ""));
      var ops = cell("");
      ops.appendChild(btn(k.enabled ? "停用" : "启用", "", function(){ keyOp("toggle", k.key); }));
      ops.appendChild(document.createTextNode(" "));
      ops.appendChild(btn("删除", "danger", function(){
        if (confirm("确定删除该密钥？")) keyOp("key", k.key);
      }));
      tb.appendChild(row([
        cell(k.key, "mono"), cell(k.name), st, cell(k.requests, "mono"),
        cell(fmtTime(k.created_at), "muted"), cell(fmtTime(k.last_used), "muted"), ops
      ]));
    });
  }

  function renderLogs(logs){
    var tb = $("#logs");
    tb.textContent = "";
    if (!logs || !logs.length) { tb.appendChild(emptyRow(9, "暂无日志")); return; }
    logs.forEach(function(l){
      var st = cell("");
      st.appendChild(pill(l.status, l.status >= 400 ? "err" : "ok"));
      var errCell = cell(l.err || "", "muted trunc");
      if (l.err) errCell.title = l.err;
      tb.appendChild(row([
        cell(fmtTime(l.time), "muted"), cell(l.model, "mono"), cell(l.stream ? "是" : "否", "mono"),
        st, cell(l.latency_ms + "ms", "mono"), cell(l.prompt_len + " / " + l.output_len, "mono"),
        cell(l.account, "mono"), cell(l.key, "mono"), errCell
      ]));
    });
  }

  function renderSettings(s){
    // 用户正在编辑设置时不要用服务端值覆盖 —— 否则在别的页签点一下
    // 「探活」触发的 refresh() 会静默丢掉未保存的改动。
    $("#sPw").placeholder = s.admin_password_set ? "已设置，留空则不修改" : "未设置";
    if (settingsDirty) return;
    $("#sDefaultModel").value = s.default_model || "";
    $("#sForceModel").value = s.force_model || "";
    $("#sTimeout").value = s.request_timeout_sec || "";
    $("#sRetries").value = s.max_retries || "";
    $("#sLogKeep").value = s.log_keep || "";
    $("#sLiveTTL").value = s.live_ttl_sec || "";
    $("#sCors").value = s.cors_origin || "";
    $("#sUserData").value = s.user_data_dir || "";
    $("#sLoginPort").value = s.login_port || 0;
    $("#sAllowCredit").checked = !!s.allow_credit_models;
    $("#sAllowAnonymous").checked = !!s.allow_anonymous;
    $("#sAutoClaim").checked = !!s.auto_claim;
    $("#sClaimHour").value = (s.claim_after_hour === undefined ? 0 : s.claim_after_hour);
    $("#sClaimMinute").value = (s.claim_after_minute === undefined ? 5 : s.claim_after_minute);
    $("#sAutoRefresh").checked = !!s.auto_refresh;
    $("#sWriteBack").checked = !!s.write_back_to_ide;
    $("#sPluginName").value = s.plugin_name || "";
    $("#sPluginVersion").value = s.plugin_version || "";
    $("#sIDEName").value = s.ide_name || "";
    $("#sIDEVersion").value = s.ide_version || "";
    $("#sAliases").value = JSON.stringify(s.model_aliases || {}, null, 2);
  }

  function markSettingsDirty(){
    if (settingsDirty) return;
    settingsDirty = true;
    $("#settingsDirty").classList.remove("hidden");
  }
  function clearSettingsDirty(){
    settingsDirty = false;
    $("#settingsDirty").classList.add("hidden");
  }

  function refresh(){
    return api("/admin/api/state").then(function(d){
      state.data = d;
      renderOverview(d);
      renderAccounts(d);
      renderKeys(d);
      renderSettings(d.settings || {});
      loadHealth();
    }).catch(function(e){
      if (e.status === 401) {
        toast("需要管理密码：请在右上角输入后点「记住」\n" + e.message, "err");
        $("#pw").focus();
      } else if (e.status === 403) {
        toast("管理接口拒绝访问：未设置密码时仅允许本机直连\n" + e.message, "err");
      } else {
        toast("加载失败：" + e.message, "err");
      }
    });
  }
  function refreshLogs(){
    var limit = $("#logLimit").value;
    return api("/admin/api/logs?limit=" + encodeURIComponent(limit)).then(function(d){
      renderLogs(d.logs);
    }).catch(function(e){ toast("日志加载失败：" + e.message, "err"); });
  }

  function accountOp(action, extra){
    var body = { action: action };
    if (extra) for (var k in extra) body[k] = extra[k];
    return api("/admin/api/accounts", {method:"POST", body:body}).then(function(){
      toast("操作成功", "ok");
      return refresh();
    }).catch(function(e){ toast("失败：" + e.message, "err"); });
  }
  function keyOp(kind, key){
    return api("/admin/api/keys?" + kind + "=" + encodeURIComponent(key), {method:"DELETE"})
      .then(function(){ toast("操作成功","ok"); return refresh(); })
      .catch(function(e){ toast("失败：" + e.message, "err"); });
  }
  function checkin(id){
    toast("探活中，请稍候…");
    return api("/admin/api/checkin", {method:"POST", body: id ? {id:id} : {}}).then(function(r){
      var results = r.results || [];
      var bad = results.filter(function(x){ return !x.ok; });
      toast("探活完成：" + results.length + " 个账号，失败 " + bad.length +
        (bad.length ? "\n" + bad.map(function(x){ return x.name + ": " + x.err; }).join("\n") : ""),
        bad.length ? "err" : "ok");
      return refresh();
    }).catch(function(e){ toast("探活失败：" + e.message, "err"); });
  }

  function claim(id){
    toast("签到中，请稍候…");
    return api("/admin/api/claim", {method:"POST", body: id ? {id:id} : {}}).then(function(r){
      var results = r.results || [];
      var bad = results.filter(function(x){ return !x.ok; });
      toast("签到完成：" + (r.succeeded||0) + "/" + (r.total||0) + " 成功" +
        (bad.length ? "\n" + bad.map(function(x){ return x.name + ": " + x.message; }).join("\n") : ""),
        bad.length ? "err" : "ok");
      return refresh();
    }).catch(function(e){ toast("签到失败：" + e.message, "err"); });
  }

  function exportSession(id){
    toast("导出中…");
    api("/admin/api/accounts", {method:"POST", body:{action:"export", id:id}}).then(function(r){
      $("#exportJSON").value = r.session_json || "";
      $("#exportMeta").textContent = "账号 " + (r.name||"") +
        " · 凭据有效期至 " + fmtTime(r.expires_at) +
        " · 可续期 " + (r.has_refresh ? "是" : "否");
      $("#exportPanel").classList.remove("hidden");
      toast("已导出（含 refresh_token，请勿外传）", "ok");
    }).catch(function(e){ toast("导出失败：" + e.message, "err"); });
  }

  // --- 浏览器登录 ---
  var loginTimer = null, loginSid = null;
  function stopLogin(){
    if (loginTimer) { clearInterval(loginTimer); loginTimer = null; }
    loginSid = null;
  }
  function pollLogin(){
    if (!loginSid) return;
    api("/admin/api/login/poll", {method:"POST", body:{session_id:loginSid}}).then(function(r){
      var map = {
        pending:   "等待在浏览器里完成登录…",
        confirmed: "登录成功",
        failed:    "登录失败",
        expired:   "授权超时（5 分钟）",
        canceled:  "已取消"
      };
      $("#loginStatus").textContent = (map[r.status] || r.status) + (r.message ? " · " + r.message : "");
      if (r.status === "confirmed") {
        stopLogin();
        toast("浏览器登录成功：" + (r.account_name || r.account || "") +
          (r.access_key ? "\nAK " + r.access_key : ""), "ok");
        $("#loginPanel").classList.add("hidden");
        refresh();
      } else if (r.status === "failed" || r.status === "expired" || r.status === "canceled") {
        stopLogin();
      }
    }).catch(function(e){ stopLogin(); toast("轮询失败：" + e.message, "err"); });
  }

  var WELFARE_STATUS = {
    ELIGIBLE:  ["可领取", "ok"],
    CLAIMED:   ["已领取", "warn"],
    CONFIRMED: ["已到账", "ok"],
    CONSUMED:  ["已使用", ""],
    ENTRY:     ["需先完成前置", "warn"]
  };

  function renderWelfare(campaigns){
    var tb = $("#welfare");
    tb.textContent = "";
    if (!campaigns || !campaigns.length) { tb.appendChild(emptyRow(7, "暂无活动")); return; }
    campaigns.forEach(function(c){
      var st = WELFARE_STATUS[c.status] || [c.status || "—", ""];
      var stCell = cell(""); stCell.appendChild(pill(st[0], st[1]));

      var autoCell = cell("");
      autoCell.appendChild(c.auto_claimable ? pill("是", "ok") : pill("否", ""));

      var reward = c.benefit_amount ? (c.benefit_amount + " " + (c.benefit_unit || "")) : "—";
      var period = (c.start_time || "").slice(0,10) + " ~ " + (c.end_time || "").slice(0,10);

      tb.appendChild(row([
        cell(c.title), cell(c.type, "mono"), cell(reward, "mono"),
        stCell, autoCell, cell(c.trigger_event || c.trigger_mode || "—", "mono"),
        cell(period, "muted")
      ]));
    });
  }

  function loadWelfare(){
    $("#welfareHint").textContent = "加载中…";
    return api("/admin/api/welfare").then(function(r){
      renderWelfare(r.campaigns);
      $("#welfareHint").textContent = "账号：" + (r.account || "");
    }).catch(function(e){
      $("#welfareHint").textContent = "";
      toast("获取活动列表失败：" + e.message, "err");
    });
  }

  // isTrustedAuthURL 只允许华为云门户 / 本机回环的授权地址。
  //
  // 授权 URL 由网关自己拼出来（/admin/api/login/start），理论上不可注入；
  // 但这里是唯一一处「拿后端返回的 URL 去跳转」的地方，加一道白名单成本很低，
  // 可以挡住管理密码泄露、或将来某个 bug 让 portal_host 变成可控的情况。
  function isTrustedAuthURL(raw){
    var u;
    try { u = new URL(raw); } catch(e){ return false; }
    if (u.protocol !== "https:" && u.protocol !== "http:") return false;
    var host = u.hostname.toLowerCase();
    if (u.protocol === "http:") {
      // 明文 http 只允许本机（本地联调）
      return host === "127.0.0.1" || host === "localhost" || host === "::1";
    }
    return host === "huaweicloud.com" || host.endsWith(".huaweicloud.com")
        || host === "huaweicloud.cn"  || host.endsWith(".huaweicloud.cn")
        || host === "huawei.com"      || host.endsWith(".huawei.com");
  }

  // setLoginURL 把授权地址写进只读输入框，并只在通过白名单时才挂到链接上。
  function setLoginURL(raw, callback, tunnel){
    var ok = isTrustedAuthURL(raw);
    $("#loginURL").value = raw || "";
    $("#loginCallback").textContent = callback || "—";
    $("#loginTunnel").value = tunnel || "";
    var a = $("#loginLink");
    if (ok) {
      a.href = raw;
      a.classList.remove("hidden");
    } else {
      a.removeAttribute("href");
      a.classList.add("hidden");
      toast("授权地址不在白名单内，已禁止直接打开：\n" + raw +
        "\n（仅允许 *.huaweicloud.com / *.huaweicloud.cn / *.huawei.com 与本机回环）", "err");
    }
    return ok;
  }

  // --- events ---
  var TAB_IDS = { overview:1, accounts:1, keys:1, logs:1, settings:1 };
  // activateTab 是唯一的页签切换入口：同步 class / aria-selected / URL hash。
  // warm=false 时只切显示、不触发额外请求（首次加载用）。
  function activateTab(name, updateHash, warm){
    if (!TAB_IDS[name]) return;
    document.querySelectorAll("nav button").forEach(function(x){
      var sel = x.dataset.tab === name;
      x.classList.toggle("on", sel);
      x.setAttribute("aria-selected", sel ? "true" : "false");
      x.tabIndex = sel ? 0 : -1;
    });
    document.querySelectorAll("section[role=tabpanel]").forEach(function(x){
      x.classList.toggle("on", x.id === "tab-" + name);
    });
    if (updateHash) {
      try { history.replaceState(null, "", "#" + name); } catch(e){ location.hash = name; }
    }
    if (warm === false) return;
    if (name === "logs") refreshLogs();
    if (name === "overview") loadWelfare();
  }
  document.querySelectorAll("nav button").forEach(function(b){
    b.onclick = function(){ activateTab(b.dataset.tab, true); };
    // WAI-ARIA tabs：左右方向键切换
    b.addEventListener("keydown", function(e){
      var keys = Object.keys(TAB_IDS);
      var i = keys.indexOf(b.dataset.tab);
      if (e.key === "ArrowRight") { i = (i + 1) % keys.length; }
      else if (e.key === "ArrowLeft") { i = (i - 1 + keys.length) % keys.length; }
      else return;
      e.preventDefault();
      activateTab(keys[i], true);
      document.querySelector('nav button[data-tab="' + keys[i] + '"]').focus();
    });
  });
  window.addEventListener("hashchange", function(){
    activateTab(location.hash.slice(1), false);
  });

  on("#savePw", function(){
    storePw(pw());
    toast("已在本标签页记住管理密码（关闭标签页即失效）", "ok");
    return refresh();
  });
  on("#forgetPw", function(){
    clearStoredPw();
    $("#pw").value = "";
    toast("已清除本标签页记住的管理密码", "ok");
  });
  on("#refresh", function(){
    var jobs = [refresh()];
    if ($("#tab-logs").classList.contains("on")) jobs.push(refreshLogs());
    return Promise.all(jobs);
  });
  on("#importLocal", function(){ return accountOp("import", {}); });
  on("#addManual", function(){
    var ak = $("#manualAK").value.trim(), sk = $("#manualSK").value.trim();
    if (!ak || !sk) { toast("请填写 Access Key 与 Secret Key", "err"); return; }
    return accountOp("manual", {
      name: $("#manualName").value.trim(), access_key: ak, secret_key: sk,
      security_token: $("#manualSTS").value.trim()
    }).then(function(){
      $("#manualAK").value = ""; $("#manualSK").value = ""; $("#manualSTS").value = "";
    });
  });
  on("#addSession", function(){
    var js = $("#sessionJSON").value.trim();
    if (!js) { toast("请粘贴 session JSON", "err"); return; }
    return accountOp("session-json", {name: $("#sessionName").value.trim(), session_json: js}).then(function(){
      $("#sessionJSON").value = "";
    });
  });
  on("#checkinAll", function(){ return checkin(null); });
  on("#claimAll", function(){ return claim(null); });
  on("#loadWelfare", loadWelfare);
  on("#claimWelfareNow", function(){ return claim(null).then(loadWelfare); });
  on("#genInvite", function(){
    toast("生成中…");
    return api("/admin/api/invite", {method:"POST", body:{}}).then(function(r){
      $("#inviteURL").value = r.invite_url || "";
      $("#inviteStat").textContent = "已激活 " + (r.activation_count||0) + " / " + (r.max_activations||0) +
        " 次 · 状态 " + (r.status||"") + " · 账号 " + (r.account||"");
      $("#invitePanel").classList.remove("hidden");
      toast("已生成邀请链接", "ok");
    }).catch(function(e){ toast("生成失败：" + e.message, "err"); });
  });
  on("#copyInvite", function(){ return copyText($("#inviteURL").value, "已复制邀请链接"); });
  on("#browserLogin", function(){
    api("/admin/api/login/start", {method:"POST", body:{}}).then(function(r){
      setLoginURL(r.auth_url, r.callback, r.tunnel_hint);
      $("#loginPasted").value = "";
      $("#loginStatus").textContent = "等待在浏览器里完成登录…";
      $("#loginPanel").classList.remove("hidden");
      stopLogin();
      loginSid = r.session_id;
      loginTimer = setInterval(pollLogin, 2000);
    }).catch(function(e){ toast("启动登录失败：" + e.message, "err"); });
  });
  on("#loginCopy", function(){
    var v = $("#loginURL").value;
    if (!v) { toast("请先点击「浏览器登录」获取授权地址", "err"); return; }
    return copyText(v, "已复制授权地址");
  });
  on("#copyExport", function(){
    return copyText($("#exportJSON").value, "已复制到剪贴板（注意不要外传）");
  });
  on("#closeExport", function(){
    $("#exportJSON").value = "";
    $("#exportPanel").classList.add("hidden");
  });
  on("#loginSubmit", function(){
    var u = $("#loginPasted").value.trim();
    if (!u) { toast("请把浏览器地址栏里那条回调 URL 粘进来", "err"); return; }
    if (!loginSid) { toast("登录会话已失效，请重新点击「浏览器登录」", "err"); return; }
    api("/admin/api/login/callback", {method:"POST", body:{session_id:loginSid, url:u}}).then(function(){
      $("#loginStatus").textContent = "已收到授权码，正在换取凭据…";
      $("#loginPasted").value = "";
      pollLogin();
    }).catch(function(e){ toast("提交失败：" + e.message, "err"); });
  });
  on("#copyTunnel", function(){
    var v = $("#loginTunnel").value;
    if (!v) { toast("请先点击「浏览器登录」", "err"); return; }
    return copyText(v, "已复制 SSH 隧道命令");
  });
  on("#loginCancel", function(){
    if (loginSid) api("/admin/api/login/cancel", {method:"POST", body:{session_id:loginSid}}).catch(function(){});
    stopLogin();
    $("#loginPanel").classList.add("hidden");
  });
  on("#refreshModels", function(){
    toast("正在从上游刷新模型清单…");
    api("/admin/api/models/refresh", {method:"POST"}).then(function(r){
      toast("已刷新，共 " + (r.models||[]).length + " 个模型", "ok");
      return refresh();
    }).catch(function(e){ toast("刷新失败：" + e.message, "err"); });
  });
  on("#refreshBalance", function(){
    toast("查询中…");
    api("/admin/api/balance").then(function(r){
      renderBalance(r.balance);
      toast("额度已更新", "ok");
    }).catch(function(e){ toast("查询失败：" + e.message, "err"); });
  });
  on("#addKey", function(){
    api("/admin/api/keys", {method:"POST", body:{name:$("#keyName").value.trim()}}).then(function(r){
      toast("已创建密钥（仅显示一次）：\n" + r.key, "ok");
      $("#keyName").value = "";
      return refresh();
    }).catch(function(e){ toast("创建失败：" + e.message, "err"); });
  });
  on("#reloadLogs", refreshLogs);
  on("#clearLogs", function(){
    if (!confirm("确定清空全部日志？")) return;
    api("/admin/api/logs", {method:"DELETE"}).then(function(){ toast("已清空","ok"); return refreshLogs(); })
      .catch(function(e){ toast("失败：" + e.message, "err"); });
  });
  on("#clearStats", function(){
    if (!confirm("确定重置统计？")) return;
    api("/admin/api/stats", {method:"DELETE"}).then(function(){ toast("已重置","ok"); return refresh(); })
      .catch(function(e){ toast("失败：" + e.message, "err"); });
  });
  on("#saveSettings", function(){
    var aliases;
    try { aliases = JSON.parse($("#sAliases").value || "{}"); }
    catch(e){ toast("模型别名不是合法 JSON", "err"); return; }
    var body = {
      model_aliases: aliases,
      default_model: $("#sDefaultModel").value.trim(),
      force_model: $("#sForceModel").value.trim(),
      request_timeout_sec: parseInt($("#sTimeout").value, 10) || 0,
      max_retries: parseInt($("#sRetries").value, 10) || 0,
      log_keep: parseInt($("#sLogKeep").value, 10) || 0,
      live_ttl_sec: parseInt($("#sLiveTTL").value, 10) || 0,
      cors_origin: $("#sCors").value.trim(),
      user_data_dir: $("#sUserData").value.trim(),
      login_port: parseInt($("#sLoginPort").value, 10) || 0,
      allow_credit_models: $("#sAllowCredit").checked,
      allow_anonymous: $("#sAllowAnonymous").checked,
      auto_claim: $("#sAutoClaim").checked,
      claim_after_hour: parseInt($("#sClaimHour").value, 10) || 0,
      claim_after_minute: parseInt($("#sClaimMinute").value, 10) || 0,
      auto_refresh: $("#sAutoRefresh").checked,
      write_back_to_ide: $("#sWriteBack").checked,
      plugin_name: $("#sPluginName").value.trim(),
      plugin_version: $("#sPluginVersion").value.trim(),
      ide_name: $("#sIDEName").value.trim(),
      ide_version: $("#sIDEVersion").value.trim()
    };
    var np = $("#sPw").value;
    if (np !== "") body.admin_password = np;
    return api("/admin/api/settings", {method:"POST", body:body}).then(function(r){
      $("#sPw").value = "";
      toast("设置已保存", "ok");
      clearSettingsDirty();
      renderSettings(r.settings || {});
      if (np !== "") { storePw(np); $("#pw").value = np; }
      return refresh();
    }).catch(function(e){ toast("保存失败：" + e.message, "err"); });
  });
  on("#clearPw", function(){
    if (!confirm("确定清除管理密码？\n清除后管理接口仅允许本机直连访问，反向代理 / 容器场景将无法再打开控制台。")) return;
    return api("/admin/api/settings", {method:"POST", body:{clear_admin_password:true}}).then(function(r){
      $("#sPw").value = "";
      clearStoredPw();
      $("#pw").value = "";
      toast("管理密码已清除", "ok");
      clearSettingsDirty();
      renderSettings(r.settings || {});
      return refresh();
    }).catch(function(e){ toast("清除失败：" + e.message, "err"); });
  });
  $("#logLimit").onchange = refreshLogs;

  // 只读输入框：点击即全选。
  // 用事件绑定而不是内联 onclick 属性，这样 CSP 不需要放开内联事件处理器。
  document.querySelectorAll("[data-select-on-click]").forEach(function(el){
    el.addEventListener("click", function(){ this.select(); });
  });

  // 设置表单一旦被编辑就标记为脏，refresh() 不再覆盖它。
  ["input", "change"].forEach(function(evt){
    $("#tab-settings").addEventListener(evt, markSettingsDirty);
  });

  var initialPw = loadPw();
  if (initialPw) $("#pw").value = initialPw;
  if (pwMigrated) {
    toast("已清除旧版本存在 localStorage 里的明文管理密码，请重新输入一次。", "err");
  }
  $("#pw").addEventListener("keydown", function(e){ if (e.key === "Enter") { $("#savePw").click(); } });
  $("#toast").addEventListener("click", function(){ this.className = "toast"; });

  var initialTab = location.hash.slice(1);
  activateTab(TAB_IDS[initialTab] ? initialTab : "overview", false, false);
  refresh();
  setInterval(loadHealth, 20000);
})();
