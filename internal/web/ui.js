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
  if (typeof module !== "undefined")
    module.exports = { micro, usd, base, query };
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
          const d = el("div", v.time);
          d.append(
            el("br"),
            button(v.model, () => detail(v.id)),
          );
          return [
            d,
            v.key_id,
            v.provider + " / " + v.auth_index,
            v.latency_ms + "ms / " + v.tokens.total + " Token · TPS 不确定",
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
      const v = await api("/requests/" + encodeURIComponent(id));
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
          const quotas = el("div");
          Object.entries(windows).forEach(([w, n]) =>
            quotas.append(
              el(
                "p",
                n +
                  " " +
                  usd(k.used?.[w]) +
                  "/" +
                  (k.limits[w] ? usd(k.limits[w]) + "$" : "不限额") +
                  " · 下次重置 " +
                  (w === "total" ? "无" : k.resets?.[w] || "—"),
              ),
            ),
          );
          const actions = el("div", null, { class: "actions" });
          actions.append(
            button("编辑", () => keyForm(k)),
            button(k.enabled ? "停用" : "启用", () => {
              if (k.enabled && !confirm("停用将阻止此 Key 的后续请求，确认？")) return;
              mutate(() =>
                api("/keys/" + k.id, {
                  method: "PUT",
                  body: spec(k, { enabled: !k.enabled }),
                }),
              );
            }),
            button("撤销", () =>
              confirmed("撤销将立即使此 Key 失效，保留历史记录。", () =>
                api("/keys/" + k.id, { method: "DELETE" }),
              ),
            ),
            button("轮换", () =>
              confirmed("轮换将立即使旧密钥失效。", async () =>
                secret(
                  await api("/keys/" + k.id + "/rotate", { method: "POST" }),
                ),
              ),
            ),
            button("重置", () => reset(k)),
          );
          return [
            label,
            quotas,
            (k.expires_at || "永不过期") +
              " · 活跃 " +
              k.active +
              " / " +
              (k.max_concurrent || "不限并发"),
            (k.allowed_models || []).join("\n") || "不限模型",
            actions,
          ];
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
  function keyForm(k) {
    const d = dialog(k ? "编辑 Key" : "新建 Key"),
      f = el("form"),
      grid = el("div", null, { class: "grid" }),
      inputs = {};
    for (const [n, l, t, v] of [
      ["label", "标签", "text", k?.label || ""],
      [
        "expires_at",
        "到期时间",
        "datetime-local",
        k?.expires_at?.replace(/Z$/, "") || "",
      ],
      ["max_concurrent", "并发上限", "number", k?.max_concurrent || 0],
      [
        "allowed_models",
        "允许模型",
        "textarea",
        (k?.allowed_models || []).join("\n"),
      ],
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
    f.append(grid, err, save);
    f.onsubmit = async (e) => {
      e.preventDefault();
      save.disabled = true;
      try {
        const body = {
          label: inputs.label.value,
          enabled: enabled.checked,
          expires_at: inputs.expires_at.value
            ? new Date(inputs.expires_at.value + "Z").toISOString()
            : null,
          max_concurrent: Number(inputs.max_concurrent.value),
          allowed_models: inputs.allowed_models.value
            .split("\n")
            .map((s) => s.trim())
            .filter(Boolean),
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
          "/keys" + (k ? "/" + encodeURIComponent(k.id) : ""),
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
    close(d);
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
            await api("/keys/" + encodeURIComponent(k.id) + "/reset", {
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
