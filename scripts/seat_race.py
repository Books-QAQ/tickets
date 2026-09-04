#!/usr/bin/env python3
"""
抢座并发压测：N 个用户同时抢「同一个座位」，验证防超卖。

压测点：
  - Redis SetNX 预占锁在热点竞争下是否干净分流
  - MySQL 条件更新 available→reserved 是否保证恰好 1 人成功
  - 其余人是否干净 409，而非拖垮服务

用法：
  python scripts/seat_race.py --users 50 --route-id 1 --bus-id 1 --seat-id <id>

说明：
  每个"用户"都用同一个 token（简化：模拟同身份并发点击），
  核心是验证"同一座位 N 并发请求 → 恰好 1 个 201，其余干净 409"。
"""
import argparse
import json
import sys
import time
import urllib.request
import urllib.error
from concurrent.futures import ThreadPoolExecutor, as_completed

BASE = "http://localhost:8080"


def http(method, path, body=None, token=None, base=BASE):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method)
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
        return 0, f"连接失败: {e.reason}"


def place_order(args, token, base):
    """单个并发请求：抢指定座位。返回 (status, order_no)。"""
    status, resp = http("POST", "/orders",
                        {"route_id": args.route_id, "bus_id": args.bus_id, "seat_id": args.seat_id},
                        token, base)
    resp = resp if isinstance(resp, dict) else {}
    return status, resp.get("order_no", "")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--users", type=int, default=50, help="并发抢座用户数")
    ap.add_argument("--route-id", type=int, default=1)
    ap.add_argument("--bus-id", type=int, default=1)
    ap.add_argument("--seat-id", type=int, default=1)
    ap.add_argument("--base-url", default=BASE)
    args = ap.parse_args()
    base = args.base_url.rstrip("/")

    print(f"抢座压测: {args.users} 用户并发抢 座位#{args.seat_id} (route={args.route_id}, bus={args.bus_id})\n")

    # 拿 token
    username = "race_" + str(int(time.time()))
    http("POST", "/register", {"username": username, "full_name": "抢座压测", "password": "race1234"}, base=base)
    _, login = http("POST", "/login", {"username": username, "password": "race1234"}, base=base)
    login = login if isinstance(login, dict) else {}
    token = login.get("response", {}).get("access_token", "")
    if not token:
        print("❌ 登录失败，无法拿到 token")
        sys.exit(1)

    # 并发抢座
    start = time.time()
    success = 0
    conflict = 0      # 409：Redis 锁被同批并发占
    not_found = 0     # 404：座位已被更早的批次占成 reserved（CheckSeatAvailability 查不到 available）
    other = 0
    winners = []
    with ThreadPoolExecutor(max_workers=args.users) as pool:
        futures = [pool.submit(place_order, args, token, base) for _ in range(args.users)]
        for fut in as_completed(futures):
            status, order_no = fut.result()
            if status == 201:
                success += 1
                winners.append(order_no)
            elif status == 409:
                conflict += 1
            elif status == 404:
                not_found += 1
            else:
                other += 1
    elapsed = time.time() - start

    print(f"耗时: {elapsed:.2f}s")
    print(f"并发数: {args.users}")
    print(f"  成功(201 下单): {success}")
    print(f"  冲突(409 Redis锁被占): {conflict}")
    print(f"  座位已reserved(404): {not_found}")
    print(f"  其他状态: {other}")

    # 关键断言：成功数 ≤ 1（不超卖），且无异常状态
    # 注意：若座位事先已 reserved，则 0 成功 + 全 404 也是正确结果。
    ok = (success <= 1) and (other == 0) and (success + conflict + not_found == args.users)
    if success == 1:
        print(f"\n✅ 防超卖正确：恰好 1 人抢到（{winners[0][:13]}...），其余 {conflict + not_found} 人干净拒绝")
    elif success == 0:
        print(f"\n✅ 座位已被占（reserved），0 人抢到，全部干净拒绝 404——不超卖")
    else:
        print(f"\n❌ 异常：{success} 人抢到同一座位，防超卖被破坏！")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
