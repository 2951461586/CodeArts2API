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
    var c = cell(text, "empty");
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
  // fmtShort 给 KPI 卡片用的紧凑时间：10-16 08:19（完整值放在 title 里）
  function fmtShort(s){
    if (!s) return "—";
    var d = new Date(s * 1000);
    var p = function(n){ return (n < 10 ? "0" : "") + n; };
    return (d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes());
  }

  function setHealth(cls, text){
    var el = $("#health");
    el.className = "badge " + cls;
    el.title = text;
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
    var claimed = accts.filter(function(a){ return a.claimed_today; }).length;
    var soonest = null;
    accts.forEach(function(a){
      if (a.expires_at && (soonest === null || a.expires_at < soonest)) soonest = a.expires_at;
    });
    var kpis = [
      { k:"总请求", v: fmtNum(s.total_requests || 0) },
      { k:"失败请求", v: fmtNum(s.failed_requests || 0) },
      { k:"输出字符", v: fmtNum(s.total_chars_out || 0) },
      { k:"账号（就绪/总）", v: ready + " / " + accts.length,
        sub: soonest ? "最早过期 " + fmtShort(soonest) : "", title: soonest ? fmtTime(soonest) : "" },
      { k:"密钥", v: (d.keys || []).length },
      { k:"今日领取", v: claimed + " / " + accts.length },
      { k:"可用模型", v: (d.models || []).length,
        sub: (d.settings || {}).allow_credit_models ? "计费已开启" : "计费已关闭" },
      { k:"启动时间", v: fmtShort(s.started_at), title: fmtTime(s.started_at) }
    ];
    var box = $("#kpis");
    box.textContent = "";
    kpis.forEach(function(item){
      var c = mk("div", "kpi");
      if (item.title) c.title = item.title;
      c.appendChild(mk("div", "k", item.k));
      c.appendChild(mk("div", "v", item.v));
      if (item.sub) c.appendChild(mk("div", "sub", item.sub));
      box.appendChild(c);
    });

  	var tb = $("#models");
    tb.textContent = "";
    var models = d.models || [];
    // 每次渲染重建元数据索引：上游清单可能删掉了模型，留着旧条目会让
    // 「测活全部」去探测已经不存在的模型，并让单行重绘拿到 undefined。
    lastModels = {};
    if (!models.length) { tb.appendChild(emptyRow(7, "无")); return; }
    models.forEach(function(m){
      lastModels[m.id] = m;
      tb.appendChild(modelRow(m));
    });
    $("#modelHint").textContent = d.upstream_error ? ("最近一次上游刷新失败：" + d.upstream_error) : "";
  }

  // 每个模型的测活状态：id -> {state:"idle|busy|ok|err", err}
  var modelProbeState = {};
  // 最近一次渲染的模型元数据，供单行重绘时复用
  var lastModels = {};

  function modelRow(m){
    var actionCell = cell("", "actions-cell");
    // 别名只是名字映射，指向的仍是上游真实模型，单独测活没有意义。
    if (m.source === "alias") {
      actionCell.appendChild(mk("span", "hint m-0", "别名"));
      return row([
        cell(m.id, "mono"), cell(m.name),
        cell(m.credit || "—", "mono"),
        cell(m.context ? fmtNum(m.context) : "—", "mono"),
        cell(m.max_output ? fmtNum(m.max_output) : "—", "mono"),
        cell(m.description, "muted"),
        actionCell
      ]);
    }

    var btn = mk("button", "btn xs", "测活");
    var st = modelProbeState[m.id] || { state: "idle" };
    if (st.state === "busy") {
      btn.disabled = true;
      btn.textContent = "测活中…";
    } else if (st.state === "ok" || st.state === "err") {
      btn.textContent = "重测";
    }
    btn.onclick = function(){ probeOne(m.id, m.name || m.id); };

    actionCell.appendChild(btn);
    if (st.state === "ok") {
      actionCell.appendChild(document.createTextNode(" "));
      actionCell.appendChild(pill("可用", "ok"));
    } else if (st.state === "err") {
      actionCell.appendChild(document.createTextNode(" "));
      var bad = pill("不可用", "err");
      bad.title = st.err || "";
      actionCell.appendChild(bad);
    }

    var r = row([
      cell(m.id, "mono"), cell(m.name),
      cell(m.credit || "—", "mono"),
      cell(m.context ? fmtNum(m.context) : "—", "mono"),
      cell(m.max_output ? fmtNum(m.max_output) : "—", "mono"),
      cell(m.description, "muted"),
      actionCell
    ]);
    if (st.state === "err" && st.err) r.title = st.err;
    return r;
  }

  // probeOne 对单个模型测活，只重绘那一行。
  function probeOne(modelID, label){
    if (modelProbeState[modelID] && modelProbeState[modelID].state === "busy") return;
    modelProbeState[modelID] = { state: "busy" };
    redrawModelRow(modelID);
    $("#modelHint").textContent = "正在测活 " + label + "…";

    return api("/admin/api/models/probe", {method:"POST", body:{models:[modelID]}}).then(function(r){
      var row0 = (r.results || [])[0] || {};
      modelProbeState[modelID] = row0.ok
        ? { state: "ok", account: row0.account }
        : { state: "err", err: row0.error || "未知错误", account: row0.account };
      redrawModelRow(modelID);
      $("#modelHint").textContent = row0.ok
        ? (label + " 可用" + (row0.account ? "（账号 " + row0.account + "）" : ""))
        : (label + " 不可用：" + (row0.error || "未知错误"));
    }).catch(function(e){
      modelProbeState[modelID] = { state: "err", err: (e && e.message) || "请求失败" };
      redrawModelRow(modelID);
      $("#modelHint").textContent = label + " 测活失败：" + ((e && e.message) || "未知错误");
    });
  }

  // redrawModelRow 只替换指定模型那一行，避免整表重绘打断其他行。
  function redrawModelRow(modelID){
    var meta = lastModels[modelID];
    if (!meta) return; // 清单已刷新、该模型不在了，无事可做
    var tb = $("#models");
    var rows = tb.children;
    for (var i = 0; i < rows.length; i++) {
      var first = rows[i].firstChild;
      if (first && first.textContent === modelID) {
        tb.replaceChild(modelRow(meta), rows[i]);
        return;
      }
    }
  }

  // 每个账号的积分额度：id -> {loading, ok, credit, error}
  var creditState = {};
  var creditLoading = null;

  // loadCredits 查询账号积分额度（上游统计插件）。
  // 默认走服务端 60 秒缓存；force=true 时带 ?fresh=1 强制回源。
  function loadCredits(force){
    if (creditLoading) return creditLoading;
    var accts = (state.data && state.data.accounts) || [];
    if (!accts.length) return Promise.resolve();
    accts.forEach(function(a){ creditState[a.id] = { loading: true }; });
    if (state.data) renderAccounts(state.data);
    var url = "/admin/api/credits" + (force ? "?fresh=1" : "");
    creditLoading = api(url).then(function(d){
      (d.accounts || []).forEach(function(x){
        creditState[x.id] = x.ok
          ? { loading:false, ok:true, credit:x.credit }
          : { loading:false, ok:false, error:x.error };
      });
      if (state.data) renderAccounts(state.data);
    }).catch(function(e){
      var msg = (e && e.message) || "查询失败";
      accts.forEach(function(a){ creditState[a.id] = { loading:false, ok:false, error:msg }; });
      if (state.data) renderAccounts(state.data);
      toast("查询积分额度失败：" + msg, "err");
    }).then(function(){ creditLoading = null; });
    return creditLoading;
  }

  // creditCell 渲染一个账号的积分额度：剩余/总量 + 进度条 + 重置日期。
  function creditCell(a){
    var c = cell("", "cell-stack");
    var st = creditState[a.id];
    if (!st || st.loading) {
      c.appendChild(mk("span", "muted", st ? "查询中…" : "—"));
      return c;
    }
    if (!st.ok) {
      var bad = mk("span", "muted", "查询失败");
      bad.title = st.error || "";
      c.appendChild(bad);
      return c;
    }
    var info = st.credit || {};
    if (!info.available) {
      var none = mk("span", "muted", "无积分套餐");
      none.title = info.reason || "";
      c.appendChild(none);
      return c;
    }
    var total = Number(info.total) || 0;
    var remain = Number(info.remaining) || 0;
    var used = Number(info.used) || 0;
    c.appendChild(mk("span", "mono", fmtNum(remain) + " / " + fmtNum(total)));

    var bar = mk("div", "bar credit-bar");
    var fill = mk("i");
    var pct = total > 0 ? Math.max(0, Math.min(100, (remain / total) * 100)) : 0;
    fill.style.width = pct.toFixed(1) + "%";
    if (pct <= 20) bar.classList.add("err");
    else if (pct <= 50) bar.classList.add("warn");
    bar.appendChild(fill);
    c.appendChild(bar);

    var bits = ["已用 " + fmtNum(used)];
    if (info.expiring) bits.push("即将到期 " + fmtNum(info.expiring));
    if (info.reset_date) bits.push("重置 " + info.reset_date);
    if (info.status && info.status !== "active") bits.push(info.status);
    var sub = mk("span", "sub", bits.join(" · "));
    sub.title = (info.package_name || "") + (info.spec_code ? (" / " + info.spec_code) : "");
    c.appendChild(sub);
    return c;
  }

  function renderAccounts(d){
    var tb = $("#accounts");
    tb.textContent = "";
    var accts = d.accounts || [];
    if (!accts.length) {
      tb.appendChild(emptyRow(10, "暂无账号，点击「导入本机 CodeArts Agent 登录态」或在下方手工录入"));
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

      var expCell = cell("", "cell-stack");
      if (a.permanent) {
        // 永久 AK/SK：没有过期时间，也不参与续期。
        expCell.appendChild(mk("span", "mono", "永不过期"));
        expCell.appendChild(mk("span", "sub", "IAM AK/SK"));
      } else if (a.expires_at) {
        expCell.appendChild(mk("span", "mono", fmtTime(a.expires_at)));
        var left = mk("span", "sub", fmtDur(a.expires_in));
        if (a.expires_in !== undefined && a.expires_in <= 300) left.classList.add("warn-text");
        expCell.appendChild(left);
      } else {
        expCell.appendChild(mk("span", "mono", "—"));
      }

      var claimCell = cell("");
      if (a.claimed_today) {
        claimCell.appendChild(pill("已领", "ok"));
      } else {
        claimCell.appendChild(pill("未领", "warn"));
      }
      if (a.last_claim_msg) claimCell.title = a.last_claim_msg;
      if (a.last_claim_err) claimCell.title = a.last_claim_err;

      var ops = cell("", "actions-cell");
      ops.appendChild(btn("探活", "xs", function(){ checkin(a.id); }));
      ops.appendChild(btn("领福利", "xs", function(){ claim(a.id); }));
      ops.appendChild(btn("重读", "xs", function(){ accountOp("reimport", {id:a.id}); }));
      ops.appendChild(btn("导出", "xs", function(){ exportSession(a.id); }));
      ops.appendChild(btn(a.enabled ? "停用" : "启用", "xs", function(){ accountOp("toggle", {id:a.id}); }));
      if (a.failures > 0) {
        ops.appendChild(btn("清错", "xs", function(){ accountOp("clear-errors", {id:a.id}); }));
      }
      ops.appendChild(btn("删除", "xs danger", function(){
        if (confirm("确定删除账号 " + a.name + "？")) accountOp("remove", {id:a.id});
      }));

      var r = row([
        cell(a.name), cell(a.source, "muted"), cell(a.access_key || "—", "mono"),
        expCell, creditCell(a), statusCell, claimCell, cell(a.failures, "mono"),
        cell(fmtTime(a.last_used), "muted nowrap"), ops
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
      var ops = cell("", "actions-cell");
      ops.appendChild(btn(k.enabled ? "停用" : "启用", "xs", function(){ keyOp("toggle", k.key); }));
      ops.appendChild(btn("删除", "xs danger", function(){
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
      st.appendChild(pill(l.status, l.status >= 500 ? "err" : (l.status >= 400 ? "warn" : "ok")));
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
    $("#sRateLimit").checked = !!s.rate_limit_enabled;
    $("#sRateQPS").value = s.rate_limit_qps || "";
    $("#sRateBurst").value = s.rate_limit_burst || "";
    $("#sRateConc").value = (s.rate_limit_concurrency === undefined ? "" : s.rate_limit_concurrency);
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
      var now = new Date();
      var p2 = function(n){ return (n < 10 ? "0" : "") + n; };
      $("#lastSync").textContent = "已更新 " + p2(now.getHours()) + ":" + p2(now.getMinutes()) + ":" + p2(now.getSeconds());
      // 账号页可见时补一次额度（首次进页签时 refresh 可能还没回来）
      if ($("#tab-accounts").classList.contains("on")) loadCredits(false);
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
    toast("领取中，请稍候…");
    return api("/admin/api/claim", {method:"POST", body: id ? {id:id} : {}}).then(function(r){
      var results = r.results || [];
      var bad = results.filter(function(x){ return !x.ok; });
      toast("领取完成：" + (r.succeeded||0) + "/" + (r.total||0) + " 成功" +
        (bad.length ? "\n" + bad.map(function(x){ return x.name + ": " + x.message; }).join("\n") : ""),
        bad.length ? "err" : "ok");
      return refresh();
    }).catch(function(e){ toast("领取失败：" + e.message, "err"); });
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

  // 当前选中的福利账号（空 = 让后端按调度池挑一个）
  var welfareAccountID = "";

  function loadWelfare(){
    $("#welfareHint").textContent = "加载中…";
    var q = welfareAccountID ? ("?id=" + encodeURIComponent(welfareAccountID)) : "";
    return api("/admin/api/welfare" + q).then(function(r){
      renderWelfare(r.campaigns);
      renderWelfareAccountPicker(r);
      $("#welfareHint").textContent = r.claimable_count
        ? ("当前账号 " + (r.account || "") + "，可领 " + r.claimable_count + " 项")
        : ("当前账号 " + (r.account || "") + "，暂无可领");
    }).catch(function(e){
      $("#welfareHint").textContent = "";
      toast("获取活动列表失败：" + e.message, "err");
    });
  }

  // renderWelfareAccountPicker 把账号下拉框填满，并同步当前选择。
  //
  // 之前只显示一个账号名，很容易被误解成「只有这个号能领」——实际上
  // 活动清单是按账号查的，每个号的可领项都不同。
  function renderWelfareAccountPicker(r){
    var sel = $("#welfareAccount");
    if (!sel) return;
    var list = r.accounts || [];
    sel.textContent = "";
    list.forEach(function(a){
      var o = document.createElement("option");
      o.value = a.id;
      o.textContent = a.name + (a.permanent ? "（永久 AK/SK）" : "");
      if (a.id === r.id) o.selected = true;
      sel.appendChild(o);
    });
    if (r.id) welfareAccountID = r.id;
    sel.disabled = list.length <= 1;
    // 账号只有一个时说明白，别让用户以为漏了
    sel.title = list.length <= 1
      ? "当前只有 1 个账号"
      : "活动清单按账号查询，每个账号的可领项不同";
  }

  // isTrustedAuthURL 只允许华为云门户 / 本机回环的授权地址。
  //
  // 授权 URL 由网关自己拼出来（/admin/api/login/start），理论上不可注入；
  // 但这里是唯一一处「拿后端返回的 URL 去跳转」的地方，加一道白名单成本很低，
  // 可以挡住管理密码泄露、或将来某个 bug 让 portal_host 变成可控的情况。
  function isTrustedAuthURL(raw){
    var u;
    try { u = new URL(raw); } catch { return false; }
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
  var TAB_META = {
    overview: { title:"概览", hint:"运行状态与用量", doc:"概览" },
    accounts: { title:"账号", hint:"账号池、凭据与活动福利领取", doc:"账号" },
    keys:     { title:"密钥", hint:"API 密钥的签发与吊销", doc:"密钥" },
    logs:     { title:"日志", hint:"请求元信息与错误", doc:"日志" },
    settings: { title:"设置", hint:"运行参数与访问控制", doc:"设置" }
  };
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
    var meta = TAB_META[name];
    $("#pageTitle").textContent = meta.title;
    $("#pageHint").textContent = meta.hint;
    document.title = meta.doc + " · CodeArts2API 控制台";
    if (updateHash) {
      try { history.replaceState(null, "", "#" + name); } catch { location.hash = name; }
    }
    if (warm === false) return;
    if (name === "logs") refreshLogs();
    if (name === "overview") loadWelfare();
    if (name === "accounts") loadCredits(false);
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
    updatePwToggle();
    $("#pwPanel").classList.remove("open");
    $("#pwToggle").setAttribute("aria-expanded", "false");
    toast("已在本标签页记住管理密码（关闭标签页即失效）", "ok");
    return refresh();
  });
  on("#forgetPw", function(){
    clearStoredPw();
    $("#pw").value = "";
    updatePwToggle();
    toast("已清除本标签页记住的管理密码", "ok");
  });
  // 顶栏密码框折叠：默认只露一个锁图标，点开才展开输入框。
  on("#pwToggle", function(){
    var panel = $("#pwPanel");
    var open = panel.classList.toggle("open");
    $("#pwToggle").setAttribute("aria-expanded", open ? "true" : "false");
    if (open) $("#pw").focus();
  });
  // 点面板外部自动收起，避免挡住下面的内容
  document.addEventListener("click", function(ev){
    var panel = $("#pwPanel");
    if (!panel || !panel.classList.contains("open")) return;
    if (panel.contains(ev.target) || $("#pwToggle").contains(ev.target)) return;
    panel.classList.remove("open");
    $("#pwToggle").setAttribute("aria-expanded", "false");
  });
  // 更新顶栏锁图标的状态文案
  function updatePwToggle(){
    var has = pw() !== "";
    var btn = $("#pwToggle");
    var txt = $("#pwToggleText");
    if (!btn || !txt) return;
    txt.textContent = has ? "已解锁" : "未解锁";
    btn.classList.toggle("on", has);
    btn.title = has ? "已记住管理密码（仅本标签页）" : "管理密码：解锁账号、密钥等管理接口";
  }
  on("#refresh", function(){
    var jobs = [refresh()];
    if ($("#tab-logs").classList.contains("on")) jobs.push(refreshLogs());
    return Promise.all(jobs);
  });
  on("#importLocal", function(){ return accountOp("import", {}); });
  // 凭据类型切换：永久 AK/SK 不需要 security_token，隐藏该项并更新提示。
  // 注意 select 用 change 事件，不能复用只绑 onclick 的 on()。
  $("#manualKind").addEventListener("change", function(){
    var isAKSK = this.value === "aksk";
    $("#manualSTSField").classList.toggle("hidden", isAKSK);
    if (isAKSK) $("#manualSTS").value = "";
    $("#manualKindHint").textContent = isAKSK
      ? "永久 AK/SK 不过期、无需续期；请勿与 STS 临时凭据混用。"
      : "STS 临时凭据约 2 小时过期，到期需重新录入或用 refresh_token 续期。";
  });
  on("#addManual", function(){
    var ak = $("#manualAK").value.trim(), sk = $("#manualSK").value.trim();
    var kind = $("#manualKind").value;
    if (!ak || !sk) { toast("请填写 Access Key 与 Secret Key", "err"); return; }
    var payload = {
      name: $("#manualName").value.trim(), access_key: ak, secret_key: sk,
      credential_kind: kind
    };
    if (kind !== "aksk") payload.security_token = $("#manualSTS").value.trim();
    return accountOp("manual", payload).then(function(){
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
  on("#refreshCredits", function(){ return loadCredits(true); });
  on("#probeAllModels", function(){
    var ids = Object.keys(lastModels).filter(function(id){
      return lastModels[id].source !== "alias";
    });
    if (!ids.length) { toast("模型清单为空，先点「从上游刷新清单」", "err"); return; }
    // 每个模型都是一次真实请求、真实扣积分，全量探测前必须二次确认。
    if (!window.confirm("将向 " + ids.length + " 个模型各发一次真实请求（max_tokens=8，会真实扣积分），确定继续？")) return;
    ids.forEach(function(id){ modelProbeState[id] = { state: "busy" }; redrawModelRow(id); });
    $("#modelHint").textContent = "正在测活 " + ids.length + " 个模型…（每个会真实消耗极少量积分）";

    return api("/admin/api/models/probe", {method:"POST", body:{}}).then(function(r){
      (r.results || []).forEach(function(x){
        modelProbeState[x.model] = x.ok
          ? { state: "ok", account: x.account }
          : { state: "err", err: x.error || "未知错误", account: x.account };
        redrawModelRow(x.model);
      });
      $("#modelHint").textContent = "可用 " + (r.available||0) + " / " + (r.total||0) +
        (r.unavailable ? "，不可用 " + r.unavailable : "");
      toast("测活完成：" + (r.available||0) + "/" + (r.total||0) + " 可用", "ok");
    }).catch(function(e){
      var msg = (e && e.message) ? e.message : "未知错误";
      ids.forEach(function(id){ modelProbeState[id] = { state: "err", err: msg }; redrawModelRow(id); });
      $("#modelHint").textContent = "测活失败：" + msg;
      toast("测活失败：" + msg, "err");
    });
  });
  on("#claimAll", function(){ return claim(null); });
  on("#loadWelfare", loadWelfare);
  // 切换账号后重新拉该账号的活动清单（各账号的领取状态不同）
  $("#welfareAccount").addEventListener("change", function(){
    welfareAccountID = this.value;
    loadWelfare();
  });
  on("#claimWelfareNow", function(){ return claim(null).then(loadWelfare); });
  on("#genInvite", function(){
    toast("生成中…");
    // 用当前选中的账号生成，避免「看到的是 A 号、生成的是 B 号」
    var body = welfareAccountID ? {id: welfareAccountID} : {};
    return api("/admin/api/invite", {method:"POST", body:body}).then(function(r){
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
    catch { toast("模型别名不是合法 JSON", "err"); return; }
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
      rate_limit_enabled: $("#sRateLimit").checked,
      rate_limit_qps: parseInt($("#sRateQPS").value, 10) || 0,
      rate_limit_burst: parseInt($("#sRateBurst").value, 10) || 0,
      rate_limit_concurrency: parseInt($("#sRateConc").value, 10) || 0,
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
  updatePwToggle();
  if (pwMigrated) {
    toast("已清除旧版本存在 localStorage 里的明文管理密码，请重新输入一次。", "err");
  }
  $("#pw").addEventListener("keydown", function(e){ if (e.key === "Enter") { $("#savePw").click(); } });
  $("#toast").addEventListener("click", function(){ this.className = "toast"; });

  var initialTab = location.hash.slice(1);
  initialTab = TAB_IDS[initialTab] ? initialTab : "overview";
  activateTab(initialTab, false, true);
  refresh();
  setInterval(loadHealth, 20000);
})();
