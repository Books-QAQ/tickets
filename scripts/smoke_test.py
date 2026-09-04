#!/usr/bin/env python3
"""
票务系统冒烟测试（smoke test）

验证核心 happy path 是否跑通，覆盖 7 个关键检查点：
  1. 服务就绪（首页 200）
  2. 注册 + 登录（拿 token）
  3. 查询线路（routes）
  4. 查询座位（seats）
  5. 下单（CreateOrder → Redis 锁 + MySQL 预留 + MQ 消息）
  6. mock 支付出票（PayOrder → 幂等出票）
  7. 查询订单状态 + 票列表

用法：python scripts/smoke_test.py [--base-url http://localhost:8080]
退出码：0 = 全部通过；1 = 有失败项
"""
import argparse
import json
import sys
import urllib.request
import urllib.error

BASE = "http://localhost:8080"
PASS = 0
FAIL = 0
FAILURES = []


def http(method, path, body=None, token=None):
    url = BASE + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read().decode()
            try:
                return resp.status, json.loads(raw)
            except json.JSONDecodeError:
                return resp.status, raw
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except json.JSONDecodeError:
            return e.code, raw
    except urllib.error.URLError as e:
        # 服务不可达（连接拒绝/超时）
        return 0, f"连接失败: {e.reason}"


def check(name, cond, detail=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print(f"  ✅ {name}")
    else:
        FAIL += 1
        FAILURES.append(name)
        print(f"  ❌ {name}  {detail}")


def main():
    global BASE
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default=BASE)
    args = ap.parse_args()
    BASE = args.base_url.rstrip("/")

    print(f"冒烟测试目标: {BASE}\n")

    # 1. 服务就绪
    status, _ = http("GET", "/")
    check("1. 服务就绪（首页 200）", status == 200, f"status={status}")

    # 2. 注册 + 登录
    username = "smoke_" + str(int(__import__("time").time()))
    status, _ = http("POST", "/register", {"username": username, "full_name": "冒烟测试", "password": "smoke1234"})
    check("2a. 注册用户", status in (200, 201), f"status={status}")

    status, login = http("POST", "/login", {"username": username, "password": "smoke1234"})
    login_obj = login if isinstance(login, dict) else {}
    token = login_obj.get("response", {}).get("access_token", "") if isinstance(login_obj.get("response"), dict) else ""
    check("2b. 登录拿 token", status == 200 and bool(token), f"status={status} token={'有' if token else '无'}")

    # 3. 查询线路
    status, routes = http("GET", "/routes?origin_city_id=1&destination_city_id=3&departure_time=2026-08-21")
    routes = routes if isinstance(routes, list) else []
    check("3. 查询线路", status == 200 and len(routes) > 0,
          f"status={status} count={len(routes)}")
    route = routes[0] if routes else {}
    route_id = route.get("route_id", 1)
    bus_id = route.get("bus_id", 1)

    # 4. 查询座位
    status, seats = http("GET", f"/routes/{route_id}/buses/{bus_id}/seats")
    seats = seats if isinstance(seats, list) else []
    avail = [s for s in seats if isinstance(s, dict) and s.get("status") == "available"]
    check("4. 查询座位（有可用座）", status == 200 and len(avail) > 0,
          f"status={status} available={len(avail)}")

    # 5. 下单
    seat_id = avail[0]["seat_id"] if avail else 1
    status, order = http("POST", "/orders", {"route_id": route_id, "bus_id": bus_id, "seat_id": seat_id}, token)
    order = order if isinstance(order, dict) else {}
    order_no = order.get("order_no", "")
    check("5. 下单（pending 订单）", status == 201 and bool(order_no),
          f"status={status} order_no={order_no}")

    # 6. mock 支付出票
    if order_no:
        status, pay = http("POST", f"/orders/{order_no}/pay", {"channel": "mock"}, token)
        pay = pay if isinstance(pay, dict) else {}
        paid = pay.get("status") in ("paid", "already_paid")
        check("6. mock 支付出票", status == 200 and paid,
              f"status={status} pay_status={pay.get('status')} err={pay.get('error','')}")
    else:
        check("6. mock 支付出票", False, "无订单号，跳过")

    # 7. 查询订单状态 + 票列表
    if order_no:
        status, st = http("GET", f"/orders/{order_no}/status", token=token)
        st = st if isinstance(st, dict) else {}
        check("7a. 订单状态已支付", status == 200 and st.get("status") in ("paid", "already_paid"),
              f"status={status} order_status={st.get('status')}")
        status, tickets = http("GET", "/user/tickets", token=token)
        tickets = tickets if isinstance(tickets, list) else []
        check("7b. 票列表包含新票", status == 200 and len(tickets) > 0,
              f"status={status} ticket_count={len(tickets)}")
    else:
        check("7a. 订单状态已支付", False, "无订单号")
        check("7b. 票列表包含新票", False, "无订单号")

    print(f"\n{'='*40}")
    print(f"结果: {PASS} 通过, {FAIL} 失败")
    if FAILURES:
        print("失败项: " + ", ".join(FAILURES))
    sys.exit(1 if FAIL else 0)


if __name__ == "__main__":
    main()
