/* 智能AI客服 · /cs 抽屉（19.5 建议形态：右下角悬浮入口 + 全屏抽屉）
 *
 * 设计约束（照抄项目现有前端风格）：
 *   - 原生 JS，零依赖、零 CDN（项目静态资源禁止缓存：web.go 里 CacheDuration=-1）；
 *   - 游客可用（device_id 存 localStorage，服务端只存它的 hash）；
 *   - 流式渲染走 fetch + ReadableStream 读 SSE（不用 EventSource：要带 POST body 与请求头）。
 *
 * 事件协议（§12.1）：meta → delta →（interrupt?）→ done
 *   meta   分类完成（先渲染"正在为您查询…"）
 *   delta  正文增量
 *   interrupt 消歧反问（挂起；用户下一条消息用同一 conv_id 续跑）
 *   done   最终答案（**以它覆盖渲染**，因为引用剥除/边界声明在 done 之前定稿）
 */
(function () {
  if (window.__csDrawerLoaded) return;
  window.__csDrawerLoaded = true;

  const DEVICE_KEY = "cs_device_id";
  const CONV_KEY = "cs_conv_id";

  function uuid() {
    if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
    return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, function (c) {
      const r = (Math.random() * 16) | 0, v = c === "x" ? r : (r & 0x3) | 0x8;
      return v.toString(16);
    });
  }

  function deviceId() {
    let d = localStorage.getItem(DEVICE_KEY);
    if (!d) { d = "web-" + uuid(); localStorage.setItem(DEVICE_KEY, d); }
    return d;
  }

  function convId(newOne) {
    if (newOne) { const id = uuid(); localStorage.setItem(CONV_KEY, id); return id; }
    let c = localStorage.getItem(CONV_KEY);
    if (!c) { c = uuid(); localStorage.setItem(CONV_KEY, c); }
    return c;
  }

  function authHeader() {
    const t = localStorage.getItem("token") || localStorage.getItem("access_token");
    return t ? { Authorization: "Bearer " + t } : {};
  }

  const css = `
  .cs-fab{position:fixed;right:22px;bottom:26px;z-index:9998;width:56px;height:56px;border-radius:50%;
    background:#2563eb;color:#fff;border:none;cursor:pointer;box-shadow:0 6px 18px rgba(37,99,235,.35);
    font-size:22px;line-height:56px;text-align:center}
  .cs-fab:hover{background:#1d4ed8}
  .cs-mask{position:fixed;inset:0;background:rgba(15,23,42,.45);z-index:9999;display:none}
  .cs-mask.open{display:block}
  .cs-panel{position:fixed;right:0;top:0;height:100%;width:min(560px,100%);background:#fff;z-index:10000;
    display:flex;flex-direction:column;transform:translateX(102%);transition:transform .22s ease;box-shadow:-8px 0 28px rgba(0,0,0,.16)}
  .cs-panel.open{transform:none}
  .cs-head{display:flex;align-items:center;justify-content:space-between;padding:14px 16px;border-bottom:1px solid #e5e7eb}
  .cs-head b{font-size:15px}
  .cs-head .cs-sub{font-size:12px;color:#6b7280;margin-left:8px}
  .cs-head button{background:none;border:none;font-size:20px;cursor:pointer;color:#6b7280}
  .cs-body{flex:1;overflow-y:auto;padding:14px 16px;background:#f8fafc}
  .cs-msg{max-width:86%;margin:8px 0;padding:10px 12px;border-radius:12px;font-size:14px;line-height:1.6;
    white-space:pre-wrap;word-break:break-word}
  .cs-msg.user{margin-left:auto;background:#2563eb;color:#fff;border-bottom-right-radius:4px}
  .cs-msg.bot{background:#fff;color:#111827;border:1px solid #e5e7eb;border-bottom-left-radius:4px}
  .cs-meta{font-size:12px;color:#6b7280;margin:6px 0 0}
  .cs-tag{display:inline-block;font-size:11px;color:#374151;background:#eef2ff;border-radius:6px;padding:1px 6px;margin-right:6px}
  .cs-foot{border-top:1px solid #e5e7eb;padding:10px 12px;display:flex;gap:8px;align-items:flex-end}
  .cs-foot textarea{flex:1;resize:none;height:44px;padding:10px 12px;border:1px solid #d1d5db;border-radius:10px;
    font-size:14px;font-family:inherit;outline:none}
  .cs-foot textarea:focus{border-color:#2563eb}
  .cs-foot button{height:44px;padding:0 18px;border:none;border-radius:10px;background:#2563eb;color:#fff;
    font-size:14px;cursor:pointer}
  .cs-foot button:disabled{background:#93c5fd;cursor:not-allowed}
  .cs-quick{display:flex;flex-wrap:wrap;gap:6px;padding:8px 16px 0}
  .cs-quick span{font-size:12px;color:#2563eb;background:#eff6ff;border:1px solid #bfdbfe;border-radius:14px;
    padding:3px 10px;cursor:pointer}
  .cs-err{color:#b91c1c;font-size:12px;padding:6px 16px}
  `;

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  function build() {
    const style = document.createElement("style");
    style.textContent = css;
    document.head.appendChild(style);

    const fab = el("button", "cs-fab", "💬");
    fab.title = "智能客服";
    const mask = el("div", "cs-mask");
    const panel = el("div", "cs-panel");
    panel.innerHTML =
      '<div class="cs-head"><div><b>智能客服</b><span class="cs-sub">票务问答 · 可转人工</span></div>' +
      '<button class="cs-close" title="关闭">×</button></div>' +
      '<div class="cs-body"></div>' +
      '<div class="cs-quick"></div>' +
      '<div class="cs-err" style="display:none"></div>' +
      '<div class="cs-foot"><textarea placeholder="描述您的问题，例如：退票手续费怎么算"></textarea>' +
      '<button class="cs-send">发送</button></div>';
    document.body.appendChild(fab);
    document.body.appendChild(mask);
    document.body.appendChild(panel);

    const body = panel.querySelector(".cs-body");
    const ta = panel.querySelector("textarea");
    const sendBtn = panel.querySelector(".cs-send");
    const errBox = panel.querySelector(".cs-err");
    const quick = panel.querySelector(".cs-quick");

    ["退票手续费怎么算", "我的车票", "明天从 Shanghai South 到 Ningbo South 还有票吗", "转人工"]
      .forEach(function (q) {
        const s = el("span", null, q);
        s.onclick = function () { ta.value = q; send(); };
        quick.appendChild(s);
      });

    function open() { mask.classList.add("open"); panel.classList.add("open"); ta.focus(); }
    function close() { mask.classList.remove("open"); panel.classList.remove("open"); }
    fab.onclick = open;
    mask.onclick = close;
    panel.querySelector(".cs-close").onclick = close;

    function bubble(role, text) {
      const m = el("div", "cs-msg " + role, text);
      body.appendChild(m);
      body.scrollTop = body.scrollHeight;
      return m;
    }

    function showError(msg) {
      errBox.style.display = "block";
      errBox.textContent = msg;
    }

    let busy = false;
    async function send() {
      const q = (ta.value || "").trim();
      if (!q || busy) return;
      ta.value = "";
      busy = true;
      sendBtn.disabled = true;
      errBox.style.display = "none";
      bubble("user", q);
      const bot = bubble("bot", "正在为您查询…");
      const metaLine = el("div", "cs-meta");

      try {
        const resp = await fetch("/cs/ask?stream=1", {
          method: "POST",
          headers: Object.assign({ "Content-Type": "application/json" }, authHeader()),
          body: JSON.stringify({ question: q, conv_id: convId(false), device_id: deviceId() }),
        });
        if (!resp.ok || !resp.body) {
          throw new Error("HTTP " + resp.status);
        }

        const reader = resp.body.getReader();
        const decoder = new TextDecoder("utf-8");
        let buf = "";
        let text = "";
        let done = false;

        while (!done) {
          const chunk = await reader.read();
          if (chunk.done) break;
          buf += decoder.decode(chunk.value, { stream: true });
          buf = buf.replace(/\r\n/g, "\n");            // 统一行尾（上游/代理可能给 CRLF）
          const frames = buf.split("\n\n");
          buf = frames.pop();
          for (const frame of frames) {
            let event = "message", data = "";
            frame.split("\n").forEach(function (line) {
              if (line.indexOf("event: ") === 0) event = line.slice(7).trim();
              else if (line.indexOf("data: ") === 0) data += line.slice(6);
            });
            if (!data) continue;
            let payload = {};
            try { payload = JSON.parse(data); } catch (e) { payload = { text: data }; }

            if (event === "meta") {
              if (payload.category) {
                metaLine.innerHTML = '<span class="cs-tag">' + payload.category + "</span>" +
                  (payload.degraded && payload.degraded.orchestrator ? "编排层降级，已直接转人工" : "");
              }
            } else if (event === "delta") {
              text += payload.text || "";
              bot.textContent = text;
              body.scrollTop = body.scrollHeight;
            } else if (event === "interrupt") {
              const opts = (payload[0] && payload[0].options) || [];
              bot.textContent = ((payload[0] && payload[0].question) || "请补充信息") +
                (opts.length ? "\n" + opts.map(function (o, i) { return (i + 1) + ". " + o; }).join("\n") : "");
            } else if (event === "error") {
              showError("服务异常：" + (payload.kind || "unknown"));
            } else if (event === "done") {
              done = true;
              if (payload.answer) bot.textContent = payload.answer;   // 以 done 覆盖渲染
              if (payload.sources && payload.sources.length) {
                metaLine.innerHTML += '<span class="cs-tag">出处 ' + payload.sources.length + "</span>";
              }
              if (payload.transfer) metaLine.innerHTML += '<span class="cs-tag">已转人工</span>';
              if (payload.support_ticket_no) metaLine.innerHTML += '<span class="cs-tag">' + payload.support_ticket_no + "</span>";
            }
          }
        }
        bot.parentNode.insertBefore(metaLine, bot.nextSibling);
      } catch (e) {
        bot.textContent = "抱歉，服务暂时不可用，请稍后再试。";
        showError(String(e && e.message ? e.message : e));
      } finally {
        busy = false;
        sendBtn.disabled = false;
        ta.focus();
      }
    }

    sendBtn.onclick = send;
    ta.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); }
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", build);
  } else {
    build();
  }
})();
