#!/usr/bin/env bash
# Manage AI agent observability demo traffic with start, stop, restart, status, or logs.
# Start manually as needed. Set FLASHCAT_ENDPOINT, FLASHCAT_API_KEY, and other settings in .env alongside this script.
set -euo pipefail
cd "$(dirname "$0")"
PROFILES=${MOCK_PROFILES:-"kefu order rag"}
PY=${PY:-./venv/bin/python}
mkdir -p run logs

start_one() {
  local p=$1
  if [ -f "run/$p.pid" ] && kill -0 "$(cat "run/$p.pid")" 2>/dev/null; then
    echo "$p 已在运行（pid $(cat "run/$p.pid")）"; return
  fi
  # Load .env into the environment to keep the API key out of the command line shown by ps.
  set -a; . ./.env; set +a
  setsid nohup "$PY" mock_traffic.py --profile "$p" >>"logs/$p.log" 2>&1 </dev/null &
  echo $! >"run/$p.pid"
  echo "$p 已启动（pid $!）"
}

stop_one() {
  local p=$1
  [ -f "run/$p.pid" ] || { echo "$p 未运行"; return; }
  local pid; pid=$(cat "run/$p.pid")
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid"   # The process flushes before exiting on TERM.
    for _ in $(seq 1 15); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
    kill -0 "$pid" 2>/dev/null && kill -KILL "$pid"
  fi
  rm -f "run/$p.pid"; echo "$p 已停止"
}

case "${1:-status}" in
  start) for p in $PROFILES; do start_one "$p"; done ;;
  stop) for p in $PROFILES; do stop_one "$p"; done ;;
  restart) for p in $PROFILES; do stop_one "$p"; done; for p in $PROFILES; do start_one "$p"; done ;;
  status)
    for p in $PROFILES; do
      if [ -f "run/$p.pid" ] && kill -0 "$(cat "run/$p.pid")" 2>/dev/null; then echo "$p 运行中（pid $(cat "run/$p.pid")）"; else echo "$p 未运行"; fi
    done ;;
  logs) tail -n 20 logs/*.log ;;
  *) echo "用法: $0 start|stop|restart|status|logs"; exit 1 ;;
esac
