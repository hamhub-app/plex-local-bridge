#!/bin/sh
# 隔离容器内的 DSM 会话桩；不打入 SPK，也不接受真实凭据。
if [ "$(id -un)" != PlexLocalBridge ]; then exit 1; fi
if [ "${HTTP_COOKIE:-}" = "id=fixture" ]; then echo node; fi
# 验证调用方只按接口约定使用 stdout，不将未约定的退出码解释为会话结果。
exit 1
