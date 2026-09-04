const TOKEN_KEY = "ticket_master_access_token";
const token = localStorage.getItem(TOKEN_KEY);

if (!token) {
  window.location.replace("/login");
}

const state = {
  token,
  terminals: [],
  routes: [],
  seats: [],
  selectedRoute: null,
  selectedSeat: null,
  currentOrder: null
};

const el = {
  searchForm: document.getElementById("search-form"),
  originTerminal: document.getElementById("origin-terminal"),
  destinationTerminal: document.getElementById("destination-terminal"),
  departureDate: document.getElementById("departure-date"),
  routesList: document.getElementById("routes-list"),
  seatsList: document.getElementById("seats-list"),
  ticketsList: document.getElementById("tickets-list"),
  appStatus: document.getElementById("app-status"),
  terminalStatus: document.getElementById("terminal-status"),
  refreshButton: document.getElementById("refresh-button"),
  logoutButton: document.getElementById("logout-button")
};

function clearSessionAndReturnHome() {
  localStorage.removeItem(TOKEN_KEY);
  window.location.replace("/login");
}

function setStatus(message, tone = "") {
  if (!el.appStatus) {
    return;
  }

  el.appStatus.textContent = message;
  el.appStatus.dataset.tone = tone;
}

function setTerminalStatus(message, tone = "") {
  if (!el.terminalStatus) {
    return;
  }

  el.terminalStatus.textContent = message;
  el.terminalStatus.dataset.tone = tone;
}

async function request(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${state.token}`,
      ...(options.headers || {})
    }
  });

  const raw = await response.text();
  let data = null;

  if (raw) {
    try {
      data = JSON.parse(raw);
    } catch {
      data = raw;
    }
  }

  if (response.status === 401) {
    clearSessionAndReturnHome();
    throw new Error("登录状态已失效，请重新登录。");
  }

  if (!response.ok) {
    const message = typeof data === "object" && data && data.error ? data.error : `${response.status} ${response.statusText}`;
    throw new Error(message);
  }

  return data;
}

async function publicRequest(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      ...(options.headers || {})
    }
  });

  const raw = await response.text();
  let data = null;

  if (raw) {
    try {
      data = JSON.parse(raw);
    } catch {
      data = raw;
    }
  }

  if (!response.ok) {
    const message = typeof data === "object" && data && data.error ? data.error : `${response.status} ${response.statusText}`;
    throw new Error(message);
  }

  return data;
}

function formatTime(value) {
  if (!value) {
    return "-";
  }

  const date = new Date(value);
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false
  }).format(date);
}

function formatPrice(value) {
  return new Intl.NumberFormat("zh-CN", {
    style: "currency",
    currency: "CNY",
    maximumFractionDigits: 0
  }).format(value || 0);
}

function formatDateOnly(value) {
  const year = value.getFullYear();
  const month = String(value.getMonth() + 1).padStart(2, "0");
  const day = String(value.getDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
}

const CITY_NAMES = {
  Shanghai: "上海",
  Hangzhou: "杭州",
  Suzhou: "苏州",
  Nanjing: "南京",
  Ningbo: "宁波"
};

const TERMINAL_NAMES = {
  "Shanghai South": "上海南",
  "Shanghai Hongqiao": "上海虹桥",
  "Hangzhou East": "杭州东",
  "Hangzhou West": "杭州西",
  "Suzhou North": "苏州北",
  "Nanjing South": "南京南",
  "Ningbo South": "宁波南"
};

function cn(name) {
  return CITY_NAMES[name] || TERMINAL_NAMES[name] || name;
}

function routeLabel(route) {
  return `${cn(route.origin_city)} → ${cn(route.destination_city)}`;
}

function routeCanPurchase(route) {
  return Boolean(route) && route.can_purchase !== false;
}

function routeSaleMessage(route) {
  if (routeCanPurchase(route)) {
    return "当前班次已开售，可以继续选座和购票。";
  }

  return `当前班次暂未开售，开售时间：${formatTime(route.sale_open_at)}`;
}

function renderTerminals() {
  if (!state.terminals.length) {
    el.originTerminal.innerHTML = "";
    el.destinationTerminal.innerHTML = "";
    setTerminalStatus("未查询到车站数据，请先初始化站点信息。", "error");
    return;
  }

  const options = state.terminals
    .map((terminal) => `<option value="${terminal.id}">${cn(terminal.name)}</option>`)
    .join("");

  el.originTerminal.innerHTML = options;
  el.destinationTerminal.innerHTML = options;

  if (state.terminals.length > 1) {
    el.destinationTerminal.selectedIndex = 1;
  }
}

function renderRoutes() {
  if (!state.routes.length) {
    el.routesList.innerHTML = `<div class="empty">当前没有匹配班次，请更换查询条件后重试。</div>`;
    return;
  }

  el.routesList.innerHTML = state.routes.map((route) => {
    const canPurchase = routeCanPurchase(route);

    return `
      <article class="route-card">
        <div class="route-top">
          <div>
            <strong>${routeLabel(route)}</strong>
            <div class="meta">
              <span>出发：${formatTime(route.departure_time)}</span>
              <span>到达：${formatTime(route.arrival_time)}</span>
              <span>班次 ID：${route.bus_id}</span>
              <span>线路 ID：${route.route_id}</span>
              <span>余票：${route.available_seats}</span>
              <span>${canPurchase ? "状态：已开售" : `状态：未开售，${formatTime(route.sale_open_at)} 开售`}</span>
            </div>
          </div>
          <div class="pill ${canPurchase ? "" : "warn"}">${formatPrice(route.price)}</div>
        </div>
        <div class="button-row">
          <button
            class="btn-ghost"
            type="button"
            data-action="load-seats"
            data-route-id="${route.route_id}"
            data-bus-id="${route.bus_id}"
            ${canPurchase ? "" : "disabled"}
          >
            ${canPurchase ? "加载座位" : "未到开售时间"}
          </button>
        </div>
      </article>
    `;
  }).join("");
}

function renderSeats(route, seats) {
  if (!route) {
    el.seatsList.innerHTML = `<div class="empty">尚未选择班次。</div>`;
    return;
  }

  if (!seats.length) {
    el.seatsList.innerHTML = `<div class="empty">${routeLabel(route)} 当前没有座位记录。</div>`;
    return;
  }

  const canPurchase = routeCanPurchase(route);
  const selectedSeatId = state.selectedSeat ? state.selectedSeat.seat_id : null;
  const selectedSeat = seats.find((seat) => seat.seat_id === selectedSeatId) || null;
  const availableCount = seats.filter((seat) => seat.status === "available").length;

  const selectedText = !canPurchase
    ? routeSaleMessage(route)
    : selectedSeat
      ? `当前已选择座位 ${selectedSeat.seat_number}，可以继续预定或直接购票。`
      : "请从下方可用座位中选择一个。";

  el.seatsList.innerHTML = `
    <div class="seat-shell">
      <div>
        <strong>${routeLabel(route)}</strong>
        <div class="meta">
          <span>总座位：${seats.length}</span>
          <span>可售：${availableCount}</span>
          <span>${canPurchase ? "已开售" : `未开售，${formatTime(route.sale_open_at)} 开售`}</span>
        </div>
      </div>
      <div class="seat-legend">
        <span><i class="seat-dot"></i> 可选</span>
        <span><i class="seat-dot selected"></i> 已选中</span>
        <span><i class="seat-dot unavailable"></i> 不可用</span>
      </div>
      <div class="seat-grid">
        ${seats.map((seat) => {
          const isAvailable = seat.status === "available";
          const isSelected = selectedSeatId === seat.seat_id;
          const classNames = [
            "seat-tile",
            !isAvailable ? "unavailable" : "",
            isSelected ? "selected" : ""
          ].filter(Boolean).join(" ");

          return `
            <button
              class="${classNames}"
              type="button"
              data-action="select-seat"
              data-seat-id="${seat.seat_id}"
              data-seat-number="${seat.seat_number}"
              data-seat-status="${seat.status}"
              ${isAvailable && canPurchase ? "" : "disabled"}
            >
              ${seat.seat_number}
            </button>
          `;
        }).join("")}
      </div>
      <div class="status">${selectedText}</div>
      ${state.currentOrder ? `
        <div class="pay-methods" role="radiogroup" aria-label="支付方式">
          <label class="pay-method">
            <input type="radio" name="pay-channel" value="alipay" checked>
            <span class="pay-method-check"></span>
            <span class="pay-method-label">支付宝支付</span>
          </label>
          <label class="pay-method">
            <input type="radio" name="pay-channel" value="wechat">
            <span class="pay-method-check"></span>
            <span class="pay-method-label">微信支付</span>
          </label>
        </div>
        <div class="button-row">
          <button class="btn-primary" type="button" data-action="pay-order">支付 ${formatPrice(state.currentOrder.amount)}</button>
        </div>
      ` : `
        <div class="button-row">
          <button class="btn-primary" type="button" data-action="create-order" ${selectedSeat && canPurchase ? "" : "disabled"}>下单锁定座位</button>
        </div>
      `}
    </div>
  `;
}

function renderTickets(tickets) {
  if (!tickets || !tickets.length) {
    el.ticketsList.innerHTML = `<div class="empty">你目前还没有票据记录。</div>`;
    return;
  }

  el.ticketsList.innerHTML = tickets.map((ticket) => `
    <article class="ticket-card">
      <div class="ticket-top">
        <div>
          <strong>票据 #${ticket.ticket_id}</strong>
          <div class="meta">
            <span>班次 ID：${ticket.bus_id}</span>
            <span>座位：${ticket.seat_number}</span>
            <span>座位 ID：${ticket.seat_id}</span>
          </div>
        </div>
        <div class="pill">${ticket.status}</div>
      </div>
      <div class="meta">
        <span>出发：${formatTime(ticket.departure_time)}</span>
        <span>到达：${formatTime(ticket.arrival_time)}</span>
        <span>下单时间：${formatTime(ticket.reserved_at)}</span>
        <span>票价：${formatPrice(ticket.price)}</span>
      </div>
      <div class="button-row">
        <button class="btn-danger" type="button" data-action="cancel-ticket" data-ticket-id="${ticket.ticket_id}" ${ticket.status === "canceled" ? "disabled" : ""}>退票</button>
      </div>
    </article>
  `).join("");
}

async function loadTerminals() {
  try {
    const terminals = await publicRequest("/terminals", { method: "GET" });
    state.terminals = terminals || [];
    renderTerminals();
    applyQueryParams();
    setTerminalStatus(`已加载 ${state.terminals.length} 个车站选项。`, "success");
  } catch (error) {
    setTerminalStatus(`加载车站失败：${error.message}`, "error");
  }
}

async function loadProfile() {
  try {
    const data = await request("/user/info", { method: "GET" });
    const user = data && data.user ? data.user : null;

    if (user) {
      setStatus(`当前登录用户：${user.username}，可以开始查询和购票。`, "success");
    } else {
      setStatus("用户信息已刷新。", "success");
    }
  } catch (error) {
    setStatus(`加载用户信息失败：${error.message}`, "error");
  }
}

async function loadTickets() {
  try {
    const tickets = await request("/user/tickets", { method: "GET" });
    renderTickets(tickets || []);
  } catch (error) {
    renderTickets([]);
    setStatus(`加载票据失败：${error.message}`, "error");
  }
}

async function handleSearch(event) {
  event.preventDefault();

  const dateValue = el.departureDate.value;
  if (!dateValue) {
    setStatus("请选择出发日期。", "error");
    return;
  }

  const query = new URLSearchParams({
    origin_city_id: el.originTerminal.value,
    destination_city_id: el.destinationTerminal.value,
    departure_time: dateValue
  });

  try {
    const routes = await publicRequest(`/routes?${query.toString()}`, { method: "GET" });
    state.routes = routes || [];
    state.selectedRoute = null;
    state.selectedSeat = null;
    state.seats = [];
    renderRoutes();
    el.seatsList.innerHTML = `<div class="empty">请选择一个班次后再查看座位。</div>`;
    setStatus(`查询完成，共返回 ${state.routes.length} 条班次结果。`, "success");
  } catch (error) {
    state.routes = [];
    renderRoutes();
    setStatus(`查询失败：${error.message}`, "error");
  }
}

async function loadSeatsForRoute(route) {
  if (!routeCanPurchase(route)) {
    state.selectedRoute = route;
    state.selectedSeat = null;
    state.seats = [];
    el.seatsList.innerHTML = `<div class="status">${routeSaleMessage(route)}</div>`;
    setStatus(routeSaleMessage(route), "error");
    return;
  }

  try {
    const seats = await publicRequest(`/routes/${route.route_id}/buses/${route.bus_id}/seats`, { method: "GET" });
    state.selectedRoute = route;
    state.selectedSeat = null;
    state.seats = seats || [];
    renderSeats(route, state.seats);
    setStatus(`已加载 ${routeLabel(route)} 的座位信息。`, "success");
  } catch (error) {
    el.seatsList.innerHTML = `<div class="empty">加载座位失败。</div>`;
    setStatus(`加载座位失败：${error.message}`, "error");
  }
}

async function createOrder() {
  if (!state.selectedRoute || !state.selectedSeat) {
    setStatus("请先选择班次和座位。", "error");
    return;
  }

  try {
    const order = await request("/orders", {
      method: "POST",
      body: JSON.stringify({
        route_id: state.selectedRoute.route_id,
        bus_id: state.selectedRoute.bus_id,
        seat_id: Number(state.selectedSeat.seat_id)
      })
    });

    state.currentOrder = order;
    setStatus(`下单成功：订单号 ${order.order_no}，金额 ${formatPrice(order.amount)}，请尽快支付。`, "success");
    renderSeats(state.selectedRoute, state.seats);
  } catch (error) {
    setStatus(`下单失败：${error.message}`, "error");
  }
}

async function payOrder() {
  if (!state.currentOrder) {
    setStatus("当前没有待支付订单。", "error");
    return;
  }

  const checked = document.querySelector('input[name="pay-channel"]:checked');
  const channel = checked ? checked.value : "alipay";

  // 微信支付未接入：诚实提示，不落 mock 分支
  if (channel === "wechat") {
    setStatus("微信支付需企业商户号，暂未接入；请选择支付宝支付。", "error");
    return;
  }

  const orderNo = state.currentOrder.order_no;
  try {
    const result = await request(`/orders/${orderNo}/pay`, {
      method: "POST",
      body: JSON.stringify({ channel })
    });

    // 支付宝渠道：弹出中央悬浮二维码弹窗，扫码付款后自动轮询确认
    if (channel === "alipay" && result.qr_code) {
      showPayModal(result.qr_code, orderNo, state.currentOrder.expires_at);
      setStatus(`已生成支付宝二维码，请扫码支付 ${formatPrice(state.currentOrder.amount)}。`, "success");
      // 开始轮询订单状态（扫码付款后自动出票）
      pollOrderStatus(orderNo);
      return;
    }

    // mock 渠道：直接出票成功
    state.currentOrder = null;
    state.selectedSeat = null;
    await loadSeatsForRoute(state.selectedRoute);
    await loadTickets();
    setStatus(`支付成功：订单 ${orderNo} 已完成支付，出票 #${result.ticket_id}。`, "success");
  } catch (error) {
    // 订单已过期：禁用按钮 + 明确提示
    if (error.message && error.message.includes("expired")) {
      disablePayActions();
      hidePayModal();
      setStatus("订单已超时，请重新选座下单。", "error");
      return;
    }
    setStatus(`支付失败：${error.message}`, "error");
  }
}

// 弹出中央悬浮二维码弹窗（qr_code 是支付宝返回的码串，如 https://qr.alipay.com/xxx）
function showPayModal(qrCode, orderNo, expiresAt) {
  const modal = document.getElementById("pay-modal");
  const qrBox = document.getElementById("pay-modal-qr");
  const orderBox = document.getElementById("pay-modal-order");
  if (!modal || !qrBox) {
    return;
  }

  renderQrInto(qrBox, qrCode);
  if (orderBox) {
    orderBox.textContent = `订单号：${orderNo}`;
  }

  modal.classList.add("is-open");
  modal.setAttribute("aria-hidden", "false");

  // 启动支付剩余时间倒计时
  startCountdown(expiresAt);
}

function renderQrInto(qrBox, qrCode) {
  // 用 qrserver 公共 API 将码串渲染为二维码图片（客户端生成，无后端依赖）
  const imgUrl = `https://api.qrserver.com/v1/create-qr-code/?size=220x220&data=${encodeURIComponent(qrCode)}`;
  qrBox.innerHTML = `<img src="${imgUrl}" alt="支付宝支付二维码" width="220" height="220" />`;
}

function hidePayModal() {
  const modal = document.getElementById("pay-modal");
  if (modal) {
    modal.classList.remove("is-open");
    modal.setAttribute("aria-hidden", "true");
  }
  stopCountdown();
}

// 倒计时：显示订单剩余支付时间，到 0 提示超时并停止
let countdownTimer = null;

function startCountdown(expiresAt) {
  stopCountdown();
  const el = document.getElementById("pay-modal-countdown");
  if (!el) {
    return;
  }

  const deadline = expiresAt ? new Date(expiresAt).getTime() : null;
  if (!deadline || Number.isNaN(deadline)) {
    el.textContent = "";
    return;
  }

  const tick = () => {
    const remain = deadline - Date.now();
    if (remain <= 0) {
      el.textContent = "订单已超时，请关闭弹窗重新下单";
      el.dataset.tone = "danger";
      stopCountdown();
      // 超时后禁用支付相关按钮，防止继续触发支付
      disablePayActions();
      return;
    }
    const totalSec = Math.ceil(remain / 1000);
    const m = Math.floor(totalSec / 60);
    const s = totalSec % 60;
    el.textContent = `支付剩余时间 ${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
    el.dataset.tone = totalSec <= 60 ? "danger" : "";
  };

  tick();
  countdownTimer = setInterval(tick, 1000);
}

function stopCountdown() {
  if (countdownTimer) {
    clearInterval(countdownTimer);
    countdownTimer = null;
  }
}

// 手动刷新二维码：重新调用支付接口获取新的支付宝二维码
async function refreshPayQr() {
  if (!state.currentOrder) {
    return;
  }
  const orderNo = state.currentOrder.order_no;
  try {
    const result = await request(`/orders/${orderNo}/pay`, {
      method: "POST",
      body: JSON.stringify({ channel: "alipay" })
    });
    if (result.qr_code) {
      const qrBox = document.getElementById("pay-modal-qr");
      renderQrInto(qrBox, result.qr_code);
      setStatus("二维码已刷新，请扫码支付。", "success");
    }
  } catch (error) {
    // 订单已过期：禁用按钮 + 关弹窗 + 明确提示
    if (error.message && error.message.includes("expired")) {
      disablePayActions();
      hidePayModal();
      setStatus("订单已超时，请重新选座下单。", "error");
      return;
    }
    setStatus(`刷新二维码失败：${error.message}`, "error");
  }
}

// 订单过期后禁用支付相关操作
function disablePayActions() {
  const refreshBtn = document.querySelector('button[data-action="refresh-pay-qr"]');
  if (refreshBtn) {
    refreshBtn.disabled = true;
  }
  const payBtn = document.querySelector('button[data-action="pay-order"]');
  if (payBtn) {
    payBtn.disabled = true;
  }
}

// 支付宝付款跳回后，轮询订单状态确认支付（主动查单兜底）
async function pollOrderStatus(orderNo) {
  const maxAttempts = 30; // 最多轮询 30 次（约 60 秒）
  for (let i = 0; i < maxAttempts; i++) {
    try {
      const result = await request(`/orders/${orderNo}/status`, { method: "GET" });
      if (result.status === "paid" || result.status === "already_paid") {
        hidePayModal();
        state.currentOrder = null;
        state.selectedSeat = null;
        setStatus(`支付成功：订单 ${orderNo} 已完成支付，出票 #${result.ticket_id || "—"}。`, "success");
        await loadTickets();
        if (state.selectedRoute) {
          await loadSeatsForRoute(state.selectedRoute);
        }
        return;
      }
      if (result.status !== "pending") {
        // 订单被关单/取消/过期：关闭弹窗 + 禁用支付 + 提示
        hidePayModal();
        disablePayActions();
        if (result.status === "canceled") {
          setStatus(`订单已超时关闭，请重新选座下单。`, "error");
        } else {
          setStatus(`订单状态：${result.status}`, "error");
        }
        return;
      }
    } catch (error) {
      // 轮询期间接口异常，继续重试
    }
    await new Promise((resolve) => setTimeout(resolve, 2000));
  }
  setStatus(`订单 ${orderNo} 仍在等待支付确认，请稍后在个人中心查看。`, "error");
}

async function cancelTicket(ticketId) {
  try {
    const result = await request(`/tickets/${ticketId}`, { method: "DELETE" });
    setStatus(result && result.message ? result.message : "退票成功。", "success");
    await loadTickets();

    if (state.selectedRoute && routeCanPurchase(state.selectedRoute)) {
      await loadSeatsForRoute(state.selectedRoute);
    }
  } catch (error) {
    setStatus(`退票失败：${error.message}`, "error");
  }
}

function handleLogout() {
  localStorage.removeItem(TOKEN_KEY);
  window.location.replace("/login");
}

function setDefaultDate() {
  const now = new Date();
  now.setDate(now.getDate() + 1);
  el.departureDate.value = formatDateOnly(now);
}

function applyQueryParams() {
  const params = new URLSearchParams(window.location.search);
  const origin = params.get("origin");
  const dest = params.get("dest");
  const date = params.get("date");

  if (origin) el.originTerminal.value = origin;
  if (dest) el.destinationTerminal.value = dest;
  if (date) el.departureDate.value = date;

  if (origin && dest && date) {
    el.searchForm.requestSubmit();
  }
}

document.addEventListener("click", async (event) => {
  const target = event.target.closest("button[data-action]");
  if (!target) {
    return;
  }

  const action = target.dataset.action;

  if (action === "load-seats") {
    const route = state.routes.find((item) =>
      Number(item.route_id) === Number(target.dataset.routeId) &&
      Number(item.bus_id) === Number(target.dataset.busId)
    );

    if (!route) {
      setStatus("未找到对应班次，请重新查询。", "error");
      return;
    }

    await loadSeatsForRoute(route);
    return;
  }

  if (action === "select-seat") {
    if (target.dataset.seatStatus !== "available" || !routeCanPurchase(state.selectedRoute)) {
      return;
    }

    state.selectedSeat = {
      seat_id: Number(target.dataset.seatId),
      seat_number: Number(target.dataset.seatNumber)
    };
    renderSeats(state.selectedRoute, state.seats);
    return;
  }

  if (action === "create-order") {
    await createOrder();
    return;
  }

  if (action === "pay-order") {
    await payOrder();
    return;
  }

  if (action === "close-pay-modal") {
    hidePayModal();
    return;
  }

  if (action === "refresh-pay-qr") {
    await refreshPayQr();
    return;
  }

  if (action === "cancel-ticket") {
    await cancelTicket(target.dataset.ticketId);
  }
});

el.searchForm.addEventListener("submit", handleSearch);
el.refreshButton.addEventListener("click", async () => {
  await loadProfile();
  await loadTickets();
});
el.logoutButton.addEventListener("click", handleLogout);

setDefaultDate();
loadTerminals();
loadProfile();
loadTickets();

// 支付宝付款跳回时携带 orderNo 参数，自动轮询确认支付
(function handleAlipayReturn() {
  const params = new URLSearchParams(window.location.search);
  const orderNo = params.get("orderNo");
  if (orderNo) {
    setStatus(`检测到支付返回，正在确认订单 ${orderNo} 的支付状态…`, "success");
    pollOrderStatus(orderNo);
  }
})();
