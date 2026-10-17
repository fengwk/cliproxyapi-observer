/* Plain DOM only: authenticated data is never interpolated as markup. */
(function () {
  "use strict";
  const MAX = 9000000000000000n;
  function micro(value) {
    const s = String(value).trim();
    if (!/^\d+(\.\d{1,6})?$/.test(s))
      throw Error("请输入非负美元金额，最多六位小数");
    const [a, b = ""] = s.split(".");
    const n = BigInt(a) * 1000000n + BigInt(b.padEnd(6, "0"));
    if (n > MAX) throw Error("金额超过安全上限");
    return Number(n);
  }
  function usd(n) {
    const v = BigInt(n || 0);
    return (
      (v / 1000000n).toString() +
      (v % 1000000n
        ? "." + (v % 1000000n).toString().padStart(6, "0").replace(/0+$/, "")
        : "")
    );
  }
  function base(path) {
    return path
      .replace(/\/ui(?:\.html)?\/?$/, "")
      .replace("/resource/plugins/", "/management/plugins/");
  }
  function query(values, page = 1) {
    const p = new URLSearchParams({ page: String(page), page_size: "25" });
    for (const [k, v] of Object.entries(values)) {
      if (!v) continue;
      if (k === "from" || k === "to") p.set(k, v + "T00:00:00Z");
      else p.set(k, v);
    }
    return "?" + p;
  }
  // datetime-local is explicitly UTC in this form; normalize only its display
  // precision, retaining the server's nanosecond string for unchanged edits.
  function expiryInput(original) {
    return original ? new Date(original).toISOString().slice(0, -1) : "";
  }
  function expiryValue(original, input) {
    if (!input) return null;
    const utc = new Date(input + "Z").toISOString();
    return original && Date.parse(original) === Date.parse(utc)
      ? original
      : utc;
  }
  if (typeof module !== "undefined")
    module.exports = {
      micro,
      usd,
      base,
      query,
      expiryInput,
      expiryValue,
      utcShort,
    };
  if (typeof document === "undefined") return;
  const $ = (s) => document.querySelector(s);
  let seq = 0;
  function el(tag, text, attrs = {}) {
    const e = document.createElement(tag);
    if (text !== null && text !== undefined) e.textContent = String(text);
    for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, String(v));
    return e;
  }
  function field(label, input) {
    if (!input.id) input.id = "field-" + ++seq;
    const l = el("label", label, { for: input.id });
    l.append(input);
    return l;
  }
  function moneyInput(v) {
    return el("input", null, {
      value: usd(v),
      inputmode: "decimal",
      required: "",
    });
  }
  window.ObserverUI = { el, field, moneyInput, micro };
  const content = $("#content"),
    login = $("#login"),
    nav = $("nav"),
    message = $("#message");
  let key = sessionStorage.getItem("observer.management-key") || "",
    page = "请求日志",
    generation = 0,
    controller,
    timer,
    dirty = false,
    filters = {},
    pageNo = 1;
  const apiBase = base(location.pathname);
  function stop() {
    generation++;
    if (controller) controller.abort();
    clearTimeout(timer);
  }
  function logout() {
    stop();
    key = "";
    window.ObserverModels.clearCache();
    sessionStorage.removeItem("observer.management-key");
    document.querySelectorAll("dialog").forEach((d) => {
      d.querySelectorAll("input").forEach((i) => (i.value = ""));
      d.close();
      d.remove();
    });
    content.replaceChildren();
    nav.hidden = true;
    login.hidden = false;
    $("#tools").replaceChildren();
  }
  async function api(path, options = {}) {
    const { body, ...rest } = options;
    const r = await fetch(apiBase + path, {
      ...rest,
      headers: {
        Authorization: "Bearer " + key,
        ...(body ? { "Content-Type": "application/json" } : {}),
      },
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    if (r.status === 401) {
      logout();
      throw Error("管理密钥无效或已失效，请重新登录");
    }
    if (!r.ok) {
      let e;
      try {
        e = await r.json();
      } catch {}
      throw Error(e?.error?.message || "请求失败 " + r.status);
    }
    return r.status === 204 ? null : r.json();
  }
  function fail(e) {
    if (e.name !== "AbortError")
      message.textContent =
        e.message +
        (/partial|histor|boundary|日|历史/i.test(e.message)
          ? "。请同时选择开始与结束 UTC 日期（结束日期不含），历史数据不支持半日查询。"
          : "");
  }
  function button(text, fn) {
    const b = el("button", text, { type: "button" });
    b.onclick = fn;
    return b;
  }
  function select(id, options) {
    const s = el("select", null, {
      id,
      "aria-label": id === "theme" ? "主题" : "自动刷新",
    });
    options.forEach(([v, t]) => s.append(el("option", t, { value: v })));
    return s;
  }
  function theme() {
    let t = $("#theme").value;
    if (t === "auto") {
      try {
        t =
          (window.parent !== window &&
            window.parent.document.documentElement.dataset.theme) ||
          "";
      } catch {}
      if (!["white", "light", "dark"].includes(t))
        t = matchMedia("(prefers-color-scheme: dark)").matches
          ? "dark"
          : "light";
    }
    document.documentElement.dataset.theme = t;
  }
  function shell() {
    login.hidden = true;
    nav.hidden = false;
    nav.replaceChildren();
    ["请求日志", "Key 管理", "设置"].forEach((p) => {
      const b = button(p, () => {
        stop();
        page = p;
        dirty = false;
        pageNo = 1;
        refresh();
      });
      b.setAttribute("aria-current", p === page ? "page" : "false");
      nav.append(b);
    });
    if (!$("#theme")) {
      const t = select("theme", [
        ["white", "纯白"],
        ["light", "浅色"],
        ["dark", "深色"],
        ["auto", "自动"],
      ]);
      t.value = "auto";
      t.onchange = theme;
      const i = select("interval", [
        [0, "关闭自动刷新"],
        [5, "5 秒"],
        [15, "15 秒"],
        [30, "30 秒"],
        [60, "60 秒"],
      ]);
      i.value = "5";
      i.onchange = schedule;
      const r = button("刷新 / 查询", () => {
        if (page === "请求日志") readFilters();
        dirty = false;
        refresh();
      });
      r.id = "refresh";
      $("#tools").append(
        field("主题", t),
        field("自动刷新", i),
        r,
        button("退出", logout),
      );
      theme();
    }
  }
  function schedule() {
    clearTimeout(timer);
    if (
      key &&
      !document.hidden &&
      !$("dialog[open]") &&
      !dirty &&
      $("#interval")?.value !== "0"
    )
      timer = setTimeout(refresh, Number($("#interval").value) * 1000);
  }
  async function refresh() {
    if (!key) return;
    stop();
    const g = generation;
    controller = new AbortController();
    const signal = controller.signal;
    message.textContent = "";
    shell();
    try {
      if (page === "请求日志") {
        const range = { ...filters };
        if (!range.from) range.from = new Date().toISOString().slice(0, 10);
        const params = new URLSearchParams(query(range, pageNo).slice(1));
        if (!range.to) params.set("to", new Date().toISOString());
        const q = "?" + params;
        const [s, r] = await Promise.all([
          api("/summary" + q, { signal }),
          api("/requests" + q, { signal }),
        ]);
        if (g === generation) logs(s, r);
      } else if (page === "Key 管理") {
        const k = await api("/keys", { signal });
        if (g === generation) keys(k.items || []);
      } else {
        const [s, p, h] = await Promise.all(
          ["/settings", "/prices", "/health"].map((p) => api(p, { signal })),
        );
        if (g === generation) {
          content.replaceChildren();
          window.ObserverSettings(content, s, p, h, api, refresh);
        }
      }
    } catch (e) {
      if (g === generation) fail(e);
    } finally {
      if (g === generation) schedule();
    }
  }
  login.onsubmit = async (e) => {
    e.preventDefault();
    key = login.querySelector("input").value;
    login.querySelector("input").value = "";
    try {
      await api("/health");
      sessionStorage.setItem("observer.management-key", key);
      refresh();
    } catch (e) {
      fail(e);
    }
  };
  document.addEventListener("focusin", (e) => {
    if (
      content.contains(e.target) &&
      e.target.matches("input,textarea,select")
    ) {
      dirty = true;
      stop();
    }
  });
  document.addEventListener("click", (e) => {
    if (page === "设置" && content.contains(e.target)) {
      dirty = true;
      stop();
    }
  });
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) stop();
    else schedule();
  });
  document.addEventListener("input", (e) => {
    if (content.contains(e.target)) {
      dirty = true;
      stop();
    }
  });
  matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
    $("#theme") && theme();
  });
  function readFilters() {
    const f = $("#filters");
    if (f) {
      filters = Object.fromEntries(new FormData(f));
      pageNo = 1;
    }
  }
  function table(headers, rows) {
    const wrap = el("div", null, { class: "table-wrap" }),
      t = el("table"),
      head = el("tr");
    headers.forEach((h) => head.append(el("th", h, { scope: "col" })));
    const thead = el("thead");
    thead.append(head);
    const body = el("tbody");
    rows.forEach((cells) => {
      const tr = el("tr");
      cells.forEach((c) => {
        const td = el("td");
        td.append(
          c instanceof Node ? c : document.createTextNode(String(c ?? "—")),
        );
        tr.append(td);
      });
      body.append(tr);
    });
    if (!rows.length) {
      const td = el("td", "暂无数据", { colspan: headers.length });
      const tr = el("tr");
      tr.append(td);
      body.append(tr);
    }
    t.append(thead, body);
    wrap.append(t);
    return wrap;
  }
  function logs(s, r) {
    content.replaceChildren();
    const f = el("form", null, { id: "filters", "aria-label": "筛选条件" });
    for (const [name, label] of Object.entries({
      from: "开始日期 (UTC)",
      to: "结束日期 (UTC，不含)",
      key_id: "Key ID",
      model: "模型",
      provider: "提供商",
      auth_index: "账户索引",
      status: "状态",
    })) {
      const i =
        name === "status"
          ? select("filter-status", [
              ["", "全部"],
              ["success", "成功"],
              ["failed", "失败"],
            ])
          : el("input", null, {
              type: ["from", "to"].includes(name) ? "date" : "text",
            });
      i.name = name;
      i.value = filters[name] || "";
      f.append(field(label, i));
    }
    f.onsubmit = (e) => {
      e.preventDefault();
      readFilters();
      dirty = false;
      refresh();
    };
    content.append(f);
    const sum = el("div", null, { class: "summary" });
    [
      ["总费用", usd(s.cost_micro_usd) + "$"],
      ["Token", s.tokens],
      ["请求", s.requests],
      ["失败", s.failures],
    ].forEach(([label, v]) => {
      const d = el("div", label);
      d.append(el("strong", v));
      sum.append(d);
    });
    content.append(
      sum,
      el("p", "请求 / 失败趋势 · " + s.granularity + " · UTC", {
        class: "muted",
      }),
    );
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("viewBox", "0 0 1000 140");
    svg.setAttribute("class", "trend");
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", "请求与失败趋势");
    const b = s.buckets || [],
      max = Math.max(1, ...b.map((v) => v.requests));
    for (const [k, c] of [
      ["requests", "request-line"],
      ["failures", "failure-line"],
    ]) {
      const l = document.createElementNS(svg.namespaceURI, "polyline");
      l.setAttribute(
        "points",
        b
          .map(
            (v, i) =>
              `${10 + (i * 980) / Math.max(1, b.length - 1)},${130 - (v[k] * 120) / max}`,
          )
          .join(" "),
      );
      l.setAttribute("class", c);
      svg.append(l);
    }
    content.append(svg);
    content.append(
      el(
        "small",
        b.length
          ? `${b[0].time} — ${b[b.length - 1].time} · 请求峰值 ${max} · 深线：请求 / 红线：失败`
          : "暂无趋势数据",
      ),
    );
    content.append(
      table(
        [
          "时间 / 模型",
          "Key",
          "提供商 / 账户",
          "延迟 / Token",
          "实际费用",
          "状态",
        ],
        (r.items || []).map((v) => {
          const d = el("div", utcShort(v.time, true), { title: v.time });
          d.append(
            el("br"),
            button(v.model, () => detail(v.id)),
          );
          const keyCell = el("div");
          const id = v.key_id || "—";
          keyCell.append(
            el(
              "span",
              /^[a-f0-9]{64}$/i.test(id) ? id.slice(0, 12) + "…" : id,
              { title: id },
            ),
          );
          if (/^[a-f0-9]{64}$/i.test(id))
            keyCell.append(
              button("复制完整 Key ID", async () => {
                try {
                  await navigator.clipboard.writeText(id);
                } catch {
                  message.textContent =
                    "复制失败，请从 Key ID 提示中复制完整标识。";
                }
              }),
            );
          const usage = el(
            "div",
            v.latency_ms + "ms / " + v.tokens.total + " Token",
          );
          if (v.accounting_quality === "unclassified")
            usage.append(el("small", "Token 口径未分类", { class: "muted" }));
          return [
            d,
            keyCell,
            v.provider + " / " + v.auth_index,
            usage,
            usd(v.cost_micro_usd) +
              "$ · " +
              (v.price_source === "unpriced" ? "未定价（0）" : v.price_source),
            (v.failed ? "失败" : "成功") + " " + (v.status_code || "—"),
          ];
        }),
      ),
    );
    const p = el("div", null, { class: "pagination" });
    const prev = button("上一页", () => {
        pageNo--;
        refresh();
      }),
      next = button("下一页", () => {
        pageNo++;
        refresh();
      });
    prev.disabled = pageNo <= 1;
    next.disabled = !r.has_more;
    p.append(prev, el("span", "第 " + pageNo + " 页"), next);
    content.append(p);
  }
  function dialog(title) {
    stop();
    const d = el("dialog"),
      h = el("h2", title);
    h.id = "dialog-title";
    d.setAttribute("aria-labelledby", h.id);
    d.append(h);
    d.addEventListener("close", () => {
      d.replaceChildren();
      d.remove();
      schedule();
    });
    document.body.append(d);
    d.showModal();
    return d;
  }
  function close(d) {
    d.append(button("关闭", () => d.close()));
  }
  async function detail(id) {
    const d = dialog("请求详情");
    close(d);
    try {
      const v = await api("/requests/detail?id=" + encodeURIComponent(id));
      if (!d.open) return;
      const status = {
        available: "可用",
        disabled: "采集未开启",
        expired: "采集已过期",
        missing: "未捕获",
        truncated: "内容已截断",
        redacted: "内容已脱敏",
      };
      d.append(
        el("p", "采集状态：" + (status[v.capture_status] || v.capture_status)),
      );
      d.append(
        table(
          ["请求头（已脱敏）", "值"],
          Object.entries(v.headers || {})
            .filter(
              ([k]) => !/authorization|cookie|api.?key|token|secret/i.test(k),
            )
            .map(([k, v]) => [k, Array.isArray(v) ? v.join(", ") : v]),
        ),
      );
      d.append(
        el("h3", "请求 JSON"),
        el("pre", JSON.stringify(redact(v.body), null, 2) ?? "无可用正文"),
      );
    } catch (e) {
      d.append(el("p", e.message));
    }
  }
  function redact(v) {
    if (Array.isArray(v)) return v.map(redact);
    if (v && typeof v === "object")
      return Object.fromEntries(
        Object.entries(v).map(([k, v]) => [
          k,
          /authorization|cookie|api.?key|token|secret|password/i.test(k)
            ? "[redacted]"
            : redact(v),
        ]),
      );
    return v;
  }
  function utcShort(value, year = false) {
    if (!value || !Number.isFinite(Date.parse(value))) return "—";
    const iso = new Date(value).toISOString();
    return iso.slice(year ? 0 : 5, 10) + " " + iso.slice(11, 16) + " UTC";
  }
  const windows = { five_hour: "5小时", week: "周", month: "月", total: "总" };
  function keys(items) {
    content.replaceChildren(
      el("h1", "Key 管理"),
      button("新建 Key", () => keyForm()),
    );
    content.append(
      table(
        ["标签 / 状态", "额度：已用 / 上限", "到期 / 并发", "允许模型", "操作"],
        items.map((k) => {
          const label = el("div", k.label || k.id);
          label.append(
            el("p", (k.enabled ? "启用" : "停用") + " · " + k.id),
            el("p", k.blocked_reason ? "计费暂停：" + k.blocked_reason : ""),
          );
          const quotas = el("div", null, { class: "key-quotas" });
          Object.entries(windows).forEach(([w, n]) => {
            const row = el("div", null, { class: "quota-line" });
            row.append(
              el(
                "span",
                (w === "five_hour" ? "5h" : n) +
                  " " +
                  usd(k.used?.[w]) +
                  "/" +
                  (k.limits[w] ? usd(k.limits[w]) + "$" : "不限额"),
              ),
            );
            const resetAt = k.resets?.[w];
            row.append(
              el(
                "small",
                w === "total"
                  ? "无自动重置"
                  : resetAt && Date.parse(resetAt) <= Date.now()
                    ? "下次请求重启周期"
                    : "重置 " + utcShort(resetAt),
                { title: resetAt || "" },
              ),
            );
            quotas.append(row);
          });
          const actions = el("div", null, { class: "actions" });
          actions.append(
            button("编辑", () => keyForm(k)),
            button(k.enabled ? "停用" : "启用", () => {
              if (k.enabled && !confirm("停用将阻止此 Key 的后续请求，确认？"))
                return;
              mutate(() =>
                api("/keys?id=" + encodeURIComponent(k.id), {
                  method: "PUT",
                  body: spec(k, { enabled: !k.enabled }),
                }),
              );
            }),
            button("撤销", () =>
              confirmed("撤销将立即使此 Key 失效，保留历史记录。", () =>
                api("/keys?id=" + encodeURIComponent(k.id), {
                  method: "DELETE",
                }),
              ),
            ),
            button("轮换", () =>
              confirmed("轮换将立即使旧密钥失效。", async () =>
                secret(
                  await api("/keys/rotate?id=" + encodeURIComponent(k.id), {
                    method: "POST",
                  }),
                ),
              ),
            ),
            button("重置", () => reset(k)),
          );
          if (k.blocked_reason)
            actions.append(button("核对后恢复", () => recover(k)));
          const expiry = el(
            "div",
            k.expires_at ? utcShort(k.expires_at, true) : "永不过期",
            { title: k.expires_at || "" },
          );
          if (k.expires_at && Date.parse(k.expires_at) <= Date.now())
            expiry.append(el("small", "已过期", { class: "error" }));
          expiry.append(
            el(
              "div",
              "活跃 " + k.active + " / " + (k.max_concurrent || "不限并发"),
            ),
          );
          const chips = el("div", null, { class: "model-chips" });
          (k.allowed_models || []).forEach((id) =>
            chips.append(el("span", id)),
          );
          if (!k.allowed_models?.length) chips.append(el("span", "不限模型"));
          return [label, quotas, expiry, chips, actions];
        }),
      ),
    );
  }
  function spec(k, override = {}) {
    return {
      label: k.label,
      enabled: k.enabled,
      expires_at: k.expires_at || null,
      max_concurrent: k.max_concurrent,
      allowed_models: k.allowed_models || [],
      limits: k.limits,
      ...override,
    };
  }
  async function mutate(fn) {
    try {
      await fn();
      refresh();
    } catch (e) {
      fail(e);
    }
  }
  function confirmed(text, fn) {
    if (confirm(text)) mutate(fn);
  }
  // The management credential never reaches the inference API. A capability is
  // held only in this request's memory and is valid for one fixed catalog GET.
  async function loadModelCatalog({ signal }) {
    let capability = await api("/models/token", { method: "POST", signal });
    try {
      if (signal.aborted) throw new DOMException("已取消", "AbortError");
      if (typeof capability?.token !== "string" || !capability.token)
        throw Error("模型目录令牌无效");
      const response = await fetch("/v1/models", {
        method: "GET",
        signal,
        cache: "no-store",
        mode: "same-origin",
        credentials: "omit",
        redirect: "error",
        headers: { Authorization: "Bearer " + capability.token },
      });
      capability.token = "";
      if (!response.ok)
        throw Error("权威模型目录读取失败，请重试或手动添加精确 ID");
      // Bound bytes before JSON parsing, not after allocating an unbounded body.
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let text = "",
        bytes = 0;
      try {
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          bytes += value.byteLength;
          if (bytes > 2 * 1024 * 1024) {
            await reader.cancel();
            throw Error("模型目录超过 2 MiB 上限，请手动添加精确 ID");
          }
          text += decoder.decode(value, { stream: true });
        }
        text += decoder.decode();
      } finally {
        reader.releaseLock();
      }
      const catalog = JSON.parse(text);
      if (
        !Array.isArray(catalog.data) ||
        catalog.data.length > 10000 ||
        catalog.data.some(
          (m) => typeof m?.id !== "string" || !m.id || m.id.length > 2048,
        )
      )
        throw Error("模型目录格式或数量超出限制，请手动添加精确 ID");
      return catalog.data.map((model) => model.id);
    } finally {
      if (capability) capability.token = "";
      capability = null;
    }
  }
  function keyForm(k) {
    const d = dialog(k ? "编辑 Key" : "新建 Key"),
      f = el("form"),
      grid = el("div", null, { class: "grid" }),
      inputs = {};
    for (const [n, l, t, v] of [
      ["label", "标签", "text", k?.label || ""],
      ["expires_at", "到期时间", "datetime-local", expiryInput(k?.expires_at)],
      ["max_concurrent", "并发上限", "number", k?.max_concurrent || 0],
    ]) {
      const i = el(
        t === "textarea" ? "textarea" : "input",
        null,
        t === "textarea" ? {} : { type: t },
      );
      i.value = v;
      if (t === "datetime-local") i.step = "0.001";
      if (t === "number") {
        i.min = "0";
        i.step = "1";
        i.required = true;
      }
      inputs[n] = i;
      grid.append(
        field(
          l +
            (n === "expires_at"
              ? " (UTC，可留空)"
              : n === "allowed_models"
                ? " (每行精确模型，空为不限)"
                : n === "max_concurrent"
                  ? " (0 不限)"
                  : ""),
          i,
        ),
      );
    }
    for (const [w, n] of Object.entries(windows)) {
      const i = moneyInput(k?.limits[w] || 0);
      inputs[w] = i;
      grid.append(field(n + "额度 (USD，0 不限)", i));
    }
    const enabled = el("input", null, { type: "checkbox" });
    enabled.checked = k ? k.enabled : true;
    grid.append(field("启用", enabled));
    const err = el("p", null, { role: "alert", class: "error" }),
      save = el("button", "保存");
    d.classList.add("key-dialog");
    const bodyArea = el("div", null, { class: "dialog-body" });
    const modelHost = el("div");
    const modelController = new AbortController();
    const models = window.ObserverModels(
      modelHost,
      k?.allowed_models || [],
      loadModelCatalog,
      modelController.signal,
    );
    d.addEventListener("close", () => {
      modelController.abort();
      models.dispose();
    });
    const footer = el("div", null, { class: "dialog-footer" });
    footer.append(
      err,
      save,
      button("取消", () => d.close()),
    );
    bodyArea.append(grid, modelHost);
    f.append(bodyArea, footer);
    f.onsubmit = async (e) => {
      e.preventDefault();
      save.disabled = true;
      try {
        const body = {
          label: inputs.label.value,
          enabled: enabled.checked,
          expires_at: expiryValue(k?.expires_at, inputs.expires_at.value),
          max_concurrent: Number(inputs.max_concurrent.value),
          allowed_models: models.read(),
          limits: Object.fromEntries(
            Object.keys(windows).map((w) => [w, micro(inputs[w].value)]),
          ),
        };
        if (
          !Number.isSafeInteger(body.max_concurrent) ||
          body.max_concurrent < 0
        )
          throw Error("并发需为非负整数");
        const result = await api(
          "/keys" + (k ? "?id=" + encodeURIComponent(k.id) : ""),
          { method: k ? "PUT" : "POST", body },
        );
        d.close();
        if (!k) secret(result);
        refresh();
      } catch (e) {
        err.textContent = e.message;
      } finally {
        save.disabled = false;
      }
    };
    d.append(f);
  }
  function secret(result) {
    const d = dialog("密钥仅显示一次"),
      p = el("input", null, {
        type: "password",
        readonly: "",
        "aria-label": "新密钥",
      });
    p.value = result.secret;
    result.secret = "";
    d.append(
      el("p", "请立即复制并安全保存；关闭后无法再次查看。"),
      p,
      button(
        "显示 / 隐藏",
        () => (p.type = p.type === "password" ? "text" : "password"),
      ),
      button("复制", async () => {
        try {
          await navigator.clipboard.writeText(p.value);
        } catch {
          p.type = "text";
          p.select();
        }
      }),
    );
    d.addEventListener("cancel", (e) => {
      if (!confirm("已安全保存密钥？关闭后无法再次查看。")) e.preventDefault();
    });
    d.append(
      button("关闭", () => {
        if (confirm("已安全保存密钥？关闭后无法再次查看。")) {
          p.value = "";
          d.close();
        }
      }),
    );
  }
  function recover(k) {
    const d = dialog("核对后恢复计费暂停");
    d.append(
      el(
        "p",
        "此 Key 因存储或计费异常暂停。系统会自动重试已保留的计费事件，请先修复存储并等待重试完成。",
      ),
      el(
        "p",
        "若崩溃后用量未知或丢失，管理员必须先核对并补偿缺失用量，再明确确认恢复。恢复不会重置金额、额度或历史费用。",
      ),
    );
    const ack = el("input", null, { type: "checkbox" });
    d.append(
      field("我已核对并处理缺失用量，确认存储正常，承担未知用量核对责任", ack),
    );
    const action = button("确认核对后恢复", async () => {
      if (
        !ack.checked ||
        !confirm(
          "确认已核对并处理缺失用量？此操作仅解除暂停，不会重置已用金额。",
        )
      )
        return;
      action.disabled = true;
      try {
        await api("/keys/recover?id=" + encodeURIComponent(k.id), {
          method: "POST",
          body: { acknowledge: true },
        });
        d.close();
        refresh();
      } catch (e) {
        note.textContent = e.message;
      } finally {
        action.disabled = !ack.checked;
      }
    });
    action.disabled = true;
    ack.onchange = () => (action.disabled = !ack.checked);
    const note = el("p", null, { role: "alert" });
    d.append(note, action);
    close(d);
  }
  function reset(k) {
    const d = dialog("重置额度"),
      checks = [];
    d.append(el("p", "只重置所选周期。计费写入故障不会被重置清除。"));
    for (const [w, n] of Object.entries(windows)) {
      const c = el("input", null, { type: "checkbox", value: w });
      checks.push(c);
      d.append(field(n, c));
    }
    d.append(
      button("确认重置", () => {
        const selected = checks.filter((c) => c.checked).map((c) => c.value);
        if (selected.length && confirm("确认重置所选周期？"))
          mutate(async () => {
            await api("/keys/reset?id=" + encodeURIComponent(k.id), {
              method: "POST",
              body: { windows: selected },
            });
            d.close();
          });
      }),
    );
    close(d);
  }
  // Only observe same-origin presentation; never inspect host credentials.
  try {
    if (window.parent !== window)
      new MutationObserver(() => {
        if ($("#theme")?.value === "auto") theme();
      }).observe(window.parent.document.documentElement, {
        attributes: true,
        attributeFilter: ["data-theme"],
      });
  } catch {}
  if (key) refresh();
})();
