#!/usr/bin/env bash
# M3 端到端验收驱动：环境 → 起服务 → phase1 → **重启编排层进程** → phase2
# 用法：bash m3_e2e_run.sh
set -u
cd /d/golangproject/tickets || exit 1
export GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct
unset HTTP_PROXY HTTPS_PROXY http_proxy https_proxy ALL_PROXY all_proxy
export PYTHONIOENCODING=utf-8
export INTERNAL_KEY="${INTERNAL_KEY:-dev-internal-key}"

PW=$(grep '^DB_PASSWORD=' app.env | cut -d= -f2- | tr -d '\r"')
DSN="mysql://root:${PW}@127.0.0.1:3307/tickets"
GO_LOG=/tmp/m3-go.log
PY_LOG=/tmp/m3-orch.log

kill_port() {
  for p in $(netstat -ano 2>/dev/null | grep ":$1" | grep -i listening | awk '{print $5}' | sort -u); do
    taskkill /F /PID "$p" >/dev/null 2>&1
  done
}

echo "== 1. 停旧进程（Go / 编排层）=="
taskkill /F /IM main.exe >/dev/null 2>&1
kill_port 8001
sleep 2

echo "== 2. 起 Go 公网入口 :8080 =="
# PYTHON_BASE_URL 必须显式给：Go 是唯一公网入口，编排层只内网可达（§6.3）
nohup env PYTHON_BASE_URL=http://127.0.0.1:8001 INTERNAL_KEY="$INTERNAL_KEY" go run main.go > "$GO_LOG" 2>&1 &
for i in $(seq 1 60); do
  sleep 2
  if curl -s -m 2 --noproxy '*' http://127.0.0.1:8080/cities >/dev/null 2>&1; then
    echo "  Go 就绪 (t=$((i*2))s)"; break
  fi
done
go_ok=$(curl -s -m 3 --noproxy '*' -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/cities)
echo "  GET /cities → $go_ok"

start_orch() {
  cd /d/golangproject/tickets/orchestrator || exit 1
  nohup env GO_BASE_URL=http://127.0.0.1:8080 INTERNAL_KEY="$INTERNAL_KEY" LLM_MODEL=mock \
    CHECKPOINT_DSN="$DSN" \
    uv run uvicorn app.main:create_app --factory --host 127.0.0.1 --port 8001 >> "$PY_LOG" 2>&1 &
  for i in $(seq 1 45); do
    sleep 2
    code=$(curl -s -m 2 --noproxy '*' -o /dev/null -w '%{http_code}' http://127.0.0.1:8001/healthz 2>/dev/null)
    if [ "$code" = "200" ]; then echo "  编排层 ready (t=$((i*2))s)"; break; fi
  done
  cd /d/golangproject/tickets || exit 1
}

echo "== 3. 起编排层（带 CHECKPOINT_DSN；闸门默认关闭）=="
: > "$PY_LOG"
start_orch
grep -c "MySQL saver 已接入" "$PY_LOG" | sed 's/^/  checkpoint 接入日志行数: /'
curl -s -m 3 --noproxy '*' http://127.0.0.1:8001/healthz; echo

echo "== 3b. 清空答案缓存（让「第几次命中」可验证）=="
docker exec tickets-redis-1 redis-cli EVAL "local ks=redis.call('keys','cs:ac:*'); for i=1,#ks do redis.call('del',ks[i]) end; return #ks" 0 2>/dev/null
echo "  已清空 cs:ac:*"

echo "== 4. phase 1（多轮指代 / 缓存 / 流式 / 断开 / 触发反问）=="
cd /d/golangproject/tickets/orchestrator || exit 1
env -u PYTHONPATH -u PYTHONHOME uv run python "$(cd "$(dirname "$0")" && pwd -W)"/acceptance_m3.py --phase 1 2>&1 | tail -32
p1=${PIPESTATUS[0]}
echo "phase1 exit=$p1"

echo "== 5. **重启编排层进程**（checkpoint 在 MySQL 里，跨进程续跑）=="
kill_port 8001
sleep 3
start_orch

echo "== 6. phase 2（用同一 conv_id resume）=="
cd /d/golangproject/tickets/orchestrator || exit 1
env -u PYTHONPATH -u PYTHONHOME uv run python "$(cd "$(dirname "$0")" && pwd -W)"/acceptance_m3.py --phase 2 2>&1 | tail -16
p2=${PIPESTATUS[0]}
echo "phase2 exit=$p2"

echo "== 7. 汇总 =="
echo "phase1=$p1 phase2=$p2"
if [ "$p1" = "0" ] && [ "$p2" = "0" ]; then echo "M3_e2e_exit=0"; else echo "M3_e2e_exit=1"; fi
echo "-- 编排层日志里的 watcher/异常行（应为空）--"
grep -iE "traceback|error|exception" "$PY_LOG" | grep -v "orchestrator_error" | head -5
echo "-- 关键计数 --"
curl -s -m 5 --noproxy '*' http://127.0.0.1:8080/internal/metrics/snapshot -H "X-Internal-Key: $INTERNAL_KEY" \
  | python -c "import sys,json; d=json.load(sys.stdin)['counters']; ks=['sse_client_aborted_total','cache_lookup_total','cache_hit_total','cache_store_total','cache_promoted_total','cache_evicted_total','cache_skipped_personalized_total','orchestrator_unavailable_total']; [print(f'  {k}={d.get(k)}') for k in ks]"
