#!/bin/sh
# 仅在管理员明确授权后运行。普通 SPK 生命周期不执行此文件。
set -eu
PATH=/usr/syno/sbin:/usr/syno/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH
umask 077
PKG_BASE=/var/packages/PlexLocalBridge
PRIVATE_DIR=/usr/local/libexec/plex-local-bridge
PRIVATE_BIN=$PRIVATE_DIR/plex-local-bridge
PRIVATE_CONFIG=$PRIVATE_DIR/config.json
GATEWAY_STATE=/var/run/plex-local-bridge
CONTROL_SOCKET=$PKG_BASE/var/run/control.sock
GATEWAY_LOG=$PRIVATE_DIR/gateway.log

if [ "$(id -u)" != 0 ]; then
    echo "Run this command explicitly as a DSM administrator using sudo." >&2
    exit 1
fi

case "${1:-}" in
    enable)
        # 审批时复制程序与配置；root 后续不执行可被套件账号替换的文件。
        if [ -L "$PRIVATE_DIR" ] || [ -L "$GATEWAY_STATE" ]; then
            echo "Refusing symbolic links in administrator-managed paths." >&2
            exit 1
        fi
        mkdir -p "$PRIVATE_DIR" "$GATEWAY_STATE"
        chown root:root "$PRIVATE_DIR" "$GATEWAY_STATE"
        chmod 0700 "$PRIVATE_DIR" "$GATEWAY_STATE"
        if [ -x "$PRIVATE_BIN" ]; then
            "$PRIVATE_BIN" gateway-stop --state "$GATEWAY_STATE"
        fi
        cp "$PKG_BASE/target/bin/plex-local-bridge" "$PRIVATE_DIR/program.new"
        chmod 0700 "$PRIVATE_DIR/program.new"
        chown root:root "$PRIVATE_DIR/program.new"
        mv -f "$PRIVATE_DIR/program.new" "$PRIVATE_BIN"
        cp "$PKG_BASE/etc/config.json" "$PRIVATE_DIR/config.new"
        chmod 0600 "$PRIVATE_DIR/config.new"
        chown root:root "$PRIVATE_DIR/config.new"
        mv -f "$PRIVATE_DIR/config.new" "$PRIVATE_CONFIG"
        cp "$0" "$PRIVATE_DIR/gatewayctl.new"
        chmod 0700 "$PRIVATE_DIR/gatewayctl.new"
        chown root:root "$PRIVATE_DIR/gatewayctl.new"
        mv -f "$PRIVATE_DIR/gatewayctl.new" "$PRIVATE_DIR/gatewayctl"
        exec "$PRIVATE_DIR/gatewayctl" start
        ;;
    start)
        if "$PRIVATE_BIN" gateway-running --state "$GATEWAY_STATE" >/dev/null 2>&1; then exit 0; fi
        if [ -f "$GATEWAY_LOG" ] && [ "$(wc -c < "$GATEWAY_LOG")" -gt 5242880 ]; then mv -f "$GATEWAY_LOG" "$GATEWAY_LOG.1"; fi
        nohup "$PRIVATE_BIN" gateway --config "$PRIVATE_CONFIG" --state "$GATEWAY_STATE" --socket "$CONTROL_SOCKET" </dev/null >>"$GATEWAY_LOG" 2>&1 &
        gateway_pid=$!
        attempt=0
        while [ "$attempt" -lt 10 ]; do
            if "$PRIVATE_BIN" gateway-status --state "$GATEWAY_STATE" >/dev/null 2>&1; then
                echo "Plex entry interception is active."
                exit 0
            fi
            if ! kill -0 "$gateway_pid" 2>/dev/null; then
                echo "Gateway did not start. See $GATEWAY_LOG" >&2
                exit 1
            fi
            sleep 1
            attempt=$((attempt+1))
        done
        echo "Gateway started and is waiting for a matching package proxy. See $GATEWAY_LOG"
        ;;
    stop)
        "$PRIVATE_BIN" gateway-stop --state "$GATEWAY_STATE"
        "$PRIVATE_BIN" recover --state "$GATEWAY_STATE"
        echo "Plex native entry restored."
        ;;
    status)
        "$PRIVATE_BIN" gateway-status --state "$GATEWAY_STATE"
        ;;
    *)
        echo "Usage: $0 {enable|start|stop|status}" >&2
        exit 1
        ;;
esac
