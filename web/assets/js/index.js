const HOT_ROUTES = [
  { origin: 1, dest: 3 },  // 上海南 → 杭州东
  { origin: 1, dest: 7 },  // 上海南 → 宁波南
  { origin: 5, dest: 6 },  // 苏州北 → 南京南
  { origin: 3, dest: 1 },  // 杭州东 → 上海南
  { origin: 2, dest: 5 },  // 上海虹桥 → 苏州北
  { origin: 6, dest: 2 }   // 南京南 → 上海虹桥
];

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

function formatDate(d) {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

function tomorrow() {
  const d = new Date();
  d.setDate(d.getDate() + 1);
  return formatDate(d);
}

function formatClock(value) {
  if (!value) return "-";
  return new Date(value).toLocaleString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
}

async function fetchJSON(path) {
  const resp = await fetch(path);
  if (!resp.ok) {
    throw new Error(`${resp.status}`);
  }
  return resp.json();
}

async function loadTerminals() {
  const origin = document.getElementById("origin-terminal");
  const destination = document.getElementById("destination-terminal");

  try {
    const terminals = await fetchJSON("/terminals");
    const options = terminals.map((t) => `<option value="${t.id}">${cn(t.name)}</option>`).join("");
    origin.innerHTML = options;
    destination.innerHTML = options;

    // 默认：上海南 → 杭州东
    const shanghai = terminals.find((t) => t.name === "Shanghai South");
    const hangzhou = terminals.find((t) => t.name === "Hangzhou East");
    if (shanghai) origin.value = shanghai.id;
    if (hangzhou) destination.value = hangzhou.id;
  } catch (error) {
    origin.innerHTML = `<option value="">加载失败</option>`;
    destination.innerHTML = `<option value="">加载失败</option>`;
  }
}

async function loadHotRoutes() {
  const grid = document.getElementById("route-grid");
  const date = tomorrow();
  const results = [];

  try {
    for (const item of HOT_ROUTES) {
      const routes = await fetchJSON(
        `/routes?origin_city_id=${item.origin}&destination_city_id=${item.dest}&departure_time=${date}`
      );
      if (Array.isArray(routes) && routes.length) {
        results.push(routes[0]);
      }
    }

    if (!results.length) {
      grid.innerHTML = `<div class="empty">暂无热门线路数据。</div>`;
      return;
    }

    grid.innerHTML = results.map((route) => `
      <article class="route-card">
        <div class="route-line">${cn(route.origin_city)} <span class="arrow">→</span> ${cn(route.destination_city)}</div>
        <div class="route-meta">
          <span>发车 ${formatClock(route.departure_time)}</span>
          <span>余票 ${route.available_seats}</span>
          <span>班次 #${route.bus_id}</span>
        </div>
        <div class="route-foot">
          <div class="route-price">¥${route.price} <small>起</small></div>
          <a class="btn-primary" href="/booking">购票</a>
        </div>
      </article>
    `).join("");
  } catch (error) {
    grid.innerHTML = `<div class="empty">加载热门线路失败。</div>`;
  }
}

document.getElementById("search-bar").addEventListener("submit", (event) => {
  event.preventDefault();
  const origin = document.getElementById("origin-terminal").value;
  const destination = document.getElementById("destination-terminal").value;
  const date = document.getElementById("departure-date").value;
  window.location.assign(`/booking?origin=${origin}&dest=${destination}&date=${date}`);
});

document.getElementById("departure-date").value = tomorrow();
loadTerminals();
loadHotRoutes();
