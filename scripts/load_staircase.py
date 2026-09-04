#!/usr/bin/env python3
"""
阶梯压测：并发从低到高逐步加压，找到系统的 QPS 拐点。

每个并发台阶用 hey 压固定时长，记录：QPS、平均延迟、P99 延迟、错误数。
拐点特征：QPS 不再增长甚至下降 + 延迟突然飙升 + 出现非 200。

用法：
  python scripts/load_staircase.py --steps 10,50,100,200,500 --duration 5s --url "..."

输出一个「并发 → QPS/延迟」对照表，肉眼即可定位拐点。
"""
import argparse
import re
import subprocess
import sys

HEY = ""


def run_hey(concurrency, duration, url):
    """跑一次 hey，返回 (qps, avg_ms, p99_ms, status_200, status_other)。"""
    cmd = [HEY, "-z", duration, "-c", str(concurrency), url]
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=int(duration[:-1]) + 15).stdout
    except subprocess.TimeoutExpired:
        return None

    qps = avg = p99 = status_200 = status_other = 0

    m = re.search(r"Requests/sec:\s+([\d.]+)", out)
    if m:
        qps = float(m.group(1))
    m = re.search(r"Average:\s+([\d.]+) secs", out)
    if m:
        avg = float(m.group(1)) * 1000
    # P99 出现在 "99% in X.XXX secs" 一行
    m = re.search(r"99% in\s+([\d.]+) secs", out)
    if m:
        p99 = float(m.group(1)) * 1000

    m = re.search(r"\[200\]\s+(\d+) responses", out)
    if m:
        status_200 = int(m.group(1))
    # 非 200 的响应数
    for code, n in re.findall(r"\[(\d+)\]\s+(\d+) responses", out):
        if code != "200":
            status_other += int(n)
    return qps, avg, p99, status_200, status_other


def main():
    global HEY
    ap = argparse.ArgumentParser()
    ap.add_argument("--steps", default="10,50,100,200,500", help="逗号分隔的并发台阶")
    ap.add_argument("--duration", default="5s", help="每台阶持续时长，如 5s")
    ap.add_argument("--url", default="http://localhost:8080/routes?origin_city_id=1&destination_city_id=3&departure_time=2026-08-21")
    ap.add_argument("--hey", default="", help="hey 可执行文件路径")
    args = ap.parse_args()

    HEY = args.hey or subprocess.run(["go", "env", "GOPATH"], capture_output=True, text=True).stdout.strip() + "/bin/hey.exe"

    steps = [int(s) for s in args.steps.split(",")]
    print(f"阶梯压测目标: {args.url}")
    print(f"每台阶时长: {args.duration}\n")
    print(f"{'并发':>6} | {'QPS':>10} | {'平均延迟':>10} | {'P99延迟':>10} | {'200数':>7} | {'非200':>6}")
    print("-" * 62)

    prev_qps = 0
    for c in steps:
        r = run_hey(c, args.duration, args.url)
        if r is None:
            print(f"{c:>6} |  超时/失败")
            continue
        qps, avg, p99, s200, sother = r
        flag = ""
        if qps < prev_qps * 0.9:
            flag = "  ⚠️ QPS 下降"
        elif p99 > 500:
            flag = "  ⚠️ P99>500ms"
        elif sother > 0:
            flag = "  ⚠️ 出现错误"
        print(f"{c:>6} | {qps:>10.0f} | {avg:>8.2f}ms | {p99:>8.2f}ms | {s200:>7} | {sother:>6}{flag}")
        prev_qps = qps

    print("\n拐点 = QPS 停止增长/下降、P99 开始飙升、出现非 200 的台阶")


if __name__ == "__main__":
    main()
