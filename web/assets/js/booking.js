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
  selectedSeat: null
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

function routeLabel(route) {
  return `${route.origin_city} -> ${route.destination_city}`;
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
    .map((terminal) => `<option value="${terminal.id}">${terminal.name} (ID ${terminal.id})</option>`)
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
      <div class="button-row">
        <button class="btn-ghost" type="button" data-action="reserve-selected" ${selectedSeat && canPurchase ? "" : "disabled"}>预定当前座位</button>
        <button class="btn-primary" type="button" data-action="purchase-selected" ${selectedSeat && canPurchase ? "" : "disabled"}>直接购票</button>
      </div>
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

async function performBooking(action) {
  if (!state.selectedRoute) {
    setStatus("请先选择班次。", "error");
    return;
  }

  if (!routeCanPurchase(state.selectedRoute)) {
    setStatus(routeSaleMessage(state.selectedRoute), "error");
    return;
  }

  if (!state.selectedSeat) {
    setStatus("请先选择一个可用座位。", "error");
    return;
  }

  const endpoint = action === "reserve-selected" ? "/routes/reserve" : "/routes/purchase";
  const label = action === "reserve-selected" ? "预定" : "购票";

  try {
    const result = await request(endpoint, {
      method: "POST",
      body: JSON.stringify({
        route_id: state.selectedRoute.route_id,
        bus_id: state.selectedRoute.bus_id,
        seat_id: Number(state.selectedSeat.seat_id)
      })
    });

    setStatus(`${label}成功：票据 #${result.ticket_id}，座位 ID ${result.seat_id}。`, "success");
    await loadSeatsForRoute(state.selectedRoute);
    await loadTickets();
  } catch (error) {
    setStatus(`${label}失败：${error.message}`, "error");
  }
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

  if (action === "reserve-selected" || action === "purchase-selected") {
    await performBooking(action);
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
