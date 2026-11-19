#!/usr/bin/env sh
# 万象服务一键管理脚本（Linux / macOS）
#
# 用法：
#   ./scripts/run.sh build        编译全部二进制
#   ./scripts/run.sh start        后台启动四个独立进程（分离模式）
#   ./scripts/run.sh stop         按 PID 精确停止四个服务
#   ./scripts/run.sh restart      先 stop 再 start
#   ./scripts/run.sh status       查看四个服务运行状态
#   ./scripts/run.sh single       启动单进程聚合入口（聚合模式）
#   ./scripts/run.sh single-stop  停止聚合入口
#
# 两种运行模式：
#   - 分离模式（start/stop）：四个服务各自独立进程，一个挂了不影响其他。
#   - 聚合模式（single/single-stop）：单个 wanxiang 进程内起四个 goroutine，
#     supervisor 做 panic 隔离与自动重启，日志集中在 logs/wanxiang.log。
#
# 说明：
#   - 只启动 bin/ 下已编译好的二进制，编译请先执行 build。
#   - 所有服务从项目根目录运行，读取同一个 .env 与 SQLite。
#   - 分离模式日志写入 logs/<service>.log，PID 写入 data/pids/<service>.pid；
#     聚合模式日志写入 logs/wanxiang.log，PID 写入 data/pids/wanxiang.pid。

set -u

# 项目根目录（脚本位于 scripts/，向上取一级）。
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR="$ROOT/bin"
PID_DIR="$ROOT/data/pids"
LOG_DIR="$ROOT/logs"

# 四个服务：名称 -> 二进制文件名。顺序即启动顺序。
SERVICES="web-server admin-server websocket-server scrcpy-server"
# 聚合入口：单个进程内启动全部四个服务。
AGGREGATE="wanxiang"

log() { printf '%s\n' "$*"; }
die() { printf '错误：%s\n' "$*" >&2; exit 1; }

# 确保必要的目录存在。
ensure_dirs() {
  mkdir -p "$PID_DIR" "$LOG_DIR"
}

# 判断某个服务是否在运行（PID 文件存在且进程存活）。
is_running() {
  name="$1"
  pid_file="$PID_DIR/$name.pid"
  [ -f "$pid_file" ] || return 1
  pid="$(cat "$pid_file" 2>/dev/null)"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null
}

cmd_build() {
  cd "$ROOT" || die "无法进入项目根目录 $ROOT"
  mkdir -p "$BIN_DIR"
  log "开始编译四个服务..."
  for name in $SERVICES; do
    log "  -> 编译 $name"
    if ! go build -o "$BIN_DIR/$name" "./cmd/$name"; then
      die "编译 $name 失败"
    fi
  done
  log "  -> 编译 $AGGREGATE（聚合入口）"
  if ! go build -o "$BIN_DIR/$AGGREGATE" "./cmd/$AGGREGATE"; then
    die "编译 $AGGREGATE 失败"
  fi
  log "编译完成，产物位于 $BIN_DIR/"
}

cmd_start() {
  ensure_dirs
  cd "$ROOT" || die "无法进入项目根目录 $ROOT"

  [ -f "$ROOT/.env" ] || log "提示：未找到 .env，将使用默认配置（建议先 cp .env.example .env）"

  for name in $SERVICES; do
    bin="$BIN_DIR/$name"
    [ -x "$bin" ] || die "找不到可执行文件 $bin，请先执行 build"

    if is_running "$name"; then
      log "[跳过] $name 已在运行 (pid $(cat "$PID_DIR/$name.pid"))"
      continue
    fi

    # 后台启动，输出重定向到日志文件。
    "$bin" >> "$LOG_DIR/$name.log" 2>&1 &
    pid=$!
    printf '%s\n' "$pid" > "$PID_DIR/$name.pid"
    log "[启动] $name (pid $pid)，日志：$LOG_DIR/$name.log"
  done
  log "全部启动完成。查看状态：./scripts/run.sh status"
}

cmd_stop() {
  ensure_dirs
  for name in $SERVICES; do
    pid_file="$PID_DIR/$name.pid"
    if is_running "$name"; then
      pid="$(cat "$pid_file")"
      kill "$pid" 2>/dev/null
      # 等待优雅退出，超时后强制杀。
      i=0
      while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 30 ]; do
        sleep 0.1
        i=$((i + 1))
      done
      if kill -0 "$pid" 2>/dev/null; then
        kill -9 "$pid" 2>/dev/null
        log "[停止] $name (pid $pid，强制)"
      else
        log "[停止] $name (pid $pid)"
      fi
    else
      log "[跳过] $name 未在运行"
    fi
    rm -f "$pid_file"
  done
  log "全部停止完成。"
}

cmd_restart() {
  cmd_stop
  sleep 1
  cmd_start
}

cmd_status() {
  ensure_dirs
  all_down=1
  for name in $SERVICES; do
    if is_running "$name"; then
      log "  [运行中] $name (pid $(cat "$PID_DIR/$name.pid"))"
      all_down=0
    else
      log "  [已停止] $name"
    fi
  done
  if is_running "$AGGREGATE"; then
    log "  [运行中] $AGGREGATE 聚合入口 (pid $(cat "$PID_DIR/$AGGREGATE.pid"))"
    all_down=0
  fi
  if [ "$all_down" -eq 1 ]; then
    log "所有服务均未运行。"
  fi
}

cmd_single() {
  ensure_dirs
  cd "$ROOT" || die "无法进入项目根目录 $ROOT"

  bin="$BIN_DIR/$AGGREGATE"
  [ -x "$bin" ] || die "找不到聚合入口 $bin，请先执行 build"

  if is_running "$AGGREGATE"; then
    log "[跳过] $AGGREGATE 聚合入口已在运行 (pid $(cat "$PID_DIR/$AGGREGATE.pid"))"
    return
  fi

  "$bin" >> "$LOG_DIR/$AGGREGATE.log" 2>&1 &
  pid=$!
  printf '%s\n' "$pid" > "$PID_DIR/$AGGREGATE.pid"
  log "[启动] $AGGREGATE 聚合入口 (pid $pid)，日志：$LOG_DIR/$AGGREGATE.log"
}

cmd_single_stop() {
  ensure_dirs
  if is_running "$AGGREGATE"; then
    pid="$(cat "$PID_DIR/$AGGREGATE.pid")"
    kill "$pid" 2>/dev/null
    i=0
    while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 30 ]; do
      sleep 0.1
      i=$((i + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then
      kill -9 "$pid" 2>/dev/null
      log "[停止] $AGGREGATE 聚合入口 (pid $pid，强制)"
    else
      log "[停止] $AGGREGATE 聚合入口 (pid $pid)"
    fi
  else
    log "[跳过] $AGGREGATE 聚合入口未在运行"
  fi
  rm -f "$PID_DIR/$AGGREGATE.pid"
}

case "${1:-}" in
  build)        cmd_build ;;
  start)        cmd_start ;;
  stop)         cmd_stop ;;
  restart)      cmd_restart ;;
  status)       cmd_status ;;
  single)       cmd_single ;;
  single-stop)  cmd_single_stop ;;
  *)
    log "用法：$0 {build|start|stop|restart|status|single|single-stop}"
    exit 1
    ;;
esac
