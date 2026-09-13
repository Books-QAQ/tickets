#!/usr/bin/env bash
# M3 最终取证：① Go/Python 单测 ② M1 回归 ③ M2 回归 ④ M3 全量（phase1 12 项 + 重启续跑 3 项）
# 顺序说明：先跑回归（会污染熔断/缓存），再跑 M3 驱动（它自己重启服务 + 清空 cs:ac:*，状态干净）
set -u
cd /d/golangproject/tickets || exit 1
export GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct
unset HTTP_PROXY HTTPS_PROXY http_proxy https_proxy ALL_PROXY all_proxy
export PYTHONIOENCODING=utf-8
export INTERNAL_KEY="${INTERNAL_KEY:-dev-internal-key}"
PW=$(grep '^DB_PASSWORD=' app.env | cut -d= -f2- | tr -d '\r"')

echo "########## 0. 重启 Go（加载 verify 幂等 + 熔断口径修复）##########"
for p in $(netstat -ano 2>/dev/null | grep ':8080' | grep -i listening | awk '{print $5}' | sort -u); do taskkill /F /PID "$p" >/dev/null 2>&1; done
sleep 2
nohup env PYTHON_BASE_URL=http://127.0.0.1:8001 INTERNAL_KEY="$INTERNAL_KEY" go run main.go > /tmp/m3-go-final2.log 2>&1 &
for i in $(seq 1 60); do sleep 2; code=$(curl -s -m 2 --noproxy '*' -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/cities 2>/dev/null); [ "$code" = "200" ] && { echo "  Go ready"; break; }; done

echo "########## 1. 单测 ##########"
go test ./internal/... 2>&1 | grep -E "^FAIL|^ok " | head -8
cd /d/golangproject/tickets/orchestrator || exit 1
env -u PYTHONPATH -u PYTHONHOME uv run pytest -q 2>&1 | tail -2

echo "########## 2. 清空答案缓存 + 刷新夹具（保证回归可重复）##########"
docker exec tickets-redis-1 redis-cli EVAL "local ks=redis.call('keys','cs:ac:*'); for i=1,#ks do redis.call('del',ks[i]) end; return #ks" 0 2>/dev/null | sed 's/^/  清空缓存键数: /'
docker exec tickets-mysql-1 mysql -uroot -p"$PW" tickets -e \
  "SET time_zone='+08:00'; UPDATE orders SET status='pending', expired_at=DATE_ADD(NOW(), INTERVAL 15 MINUTE) WHERE order_no LIKE '22222222%';" 2>&1 | grep -v Warning

echo "########## 3. M1 回归（20 条）##########"
env -u PYTHONPATH -u PYTHONHOME uv run python "$(cd "$(dirname "$0")" && pwd -W)"/acceptance.py > /tmp/final_m1.txt 2>&1
m1=$?
echo "M1_acceptance_exit=$m1"
grep -E "作答数|覆盖率|违规|FAIL" /tmp/final_m1.txt | head -6

echo "########## 4. M2 回归（22 项）##########"
USER_A=5 USER_B=6 env -u PYTHONPATH -u PYTHONHOME uv run python "$(cd "$(dirname "$0")" && pwd -W)"/acceptance_m2.py > /tmp/final_m2.txt 2>&1
m2=$?
echo "M2_acceptance_exit=$m2"
grep -E "合计|FAIL" /tmp/final_m2.txt | head -5

echo "########## 5. M3 全量（阶段一 + 重启后续跑）##########"
bash "$(cd "$(dirname "$0")" && pwd -W)"/run_m3_e2e.sh > /tmp/final_m3.txt 2>&1
echo "M3_driver_exit=$?"
grep -E "M3 验收汇总|M3_e2e_exit|phase1=|phase2=|FAIL" /tmp/final_m3.txt | head -8

echo "########## 6. 总结 ##########"
echo "M1_exit=$m1 M2_exit=$m2"
grep -E "phase1=|phase2=" /tmp/final_m3.txt | tail -1
