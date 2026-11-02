# 配置、权限与开发

[返回项目说明](../README.md)

## 配置

普通配置保存在 `/var/packages/PlexLocalBridge/etc/config.json`：

```json
{
  "upstream_url": "auto",
  "public_port": 32400,
  "listen_port": 13240,
  "intercept_ipv6": true,
  "max_connections": 256
}
```

| 字段 | 含义 |
| --- | --- |
| `upstream_url` | `auto` 优先选择 NAS 默认路由接口的私有 IPv4:32400；也可指定 `http://NAS本机IP:端口`。不接受域名、账号、路径或查询参数。 |
| `public_port` | 接管的 NAS 侧端口，默认 32400；区别于路由器的公网端口。 |
| `listen_port` | 代理监听端口，默认 13240；不能与入口或上游端口相同。 |
| `intercept_ipv6` | 默认开启；只使用 IPv4 或系统缺少 IPv6 NAT 支持时关闭。 |
| `max_connections` | HTTP 与 TLS 共用的客户端 TCP 连接上限，范围 1–4096。 |

“保存并应用”会重启普通代理，正在播放的连接可能中断。新配置启动失败时恢复原配置；不同窗口同时编辑时，旧版本保存会被拒绝。

入口端口、监听端口、上游或 IPv6 接管配置变化后，需要重新授权。升级 SPK 后，如需更新管理员副本，也点击“启用 / 更新接管”。直接编辑配置文件时，应先停止普通套件，保持文件归属和读权限，修改后启动并重新授权。

## 转发与权限

```mermaid
flowchart LR
    C[客户端] --> N[NAS:32400]
    N -->|管理员授权的 PREROUTING 规则| B[普通代理:13240]
    B -->|本机连接| P[原生 Plex:32400]
```

HTTP 转发改写 Host，移除外部 Forwarded、X-Forwarded-*、X-Real-IP 等来源头，并保留客户端鉴权信息。本机 OUTPUT 不重定向，避免上游请求循环。媒体按请求流式转发，不扫描、预取全片或持久化媒体。启动时检查认领与匿名访问状态，之后每五分钟复查。

所有 SPK 生命周期脚本均以套件账号运行。管理 CGI 在读取会话和配置前核对实际身份，必要时先降到固定的 PlexLocalBridge 账号；降权失败则拒绝请求。只有 administrators 组成员可以管理设置。

管理员明确授权后，启用操作把程序、配置和控制脚本复制到 root 保护的 `/usr/local/libexec/plex-local-bridge/`。后续管理员进程只执行该独立副本，不执行普通套件账号可替换的程序。普通账号不能修改已批准的端口或上游；管理员状态文件对它只读。

DSM 任务计划中的 `PlexLocalBridge-Authorize` 和 `PlexLocalBridge-Stop` 是禁用定时触发、仅明确授权时运行的固定命令；`PlexLocalBridge-Startup` 是可选开机任务。已有同名任务内容不符时，页面拒绝覆盖。

管理员接管每三秒检查代理健康状态。普通代理停止、崩溃或卸载后，撤销专属规则并等待；恢复后重新接管。规则撤销存在检查间隔。管理员进程被强制终止、断电或防火墙命令失败时，可能需要手动恢复。

仅管理 nat 表中的 `PLB_GATE_V1` 链和指向该链的 PREROUTING 跳转，兼容清理旧版带 PlexLocalBridge comment 的规则。不清空全表，不修改 OUTPUT、FORWARD；发现混入其它规则时，保留混合链、撤销自己的入口并报告错误。

初次授权会校验并按依赖加载 `/lib/modules` 中 root 持有、内核版本匹配的系统模块。不下载模块、不修改内核、不卸载已有模块；规则不依赖 `xt_comment.ko`。

### 会话与密码

页面复用 DSM 登录 Cookie，并将当前会话的 SynoToken 放在 `X-SYNO-TOKEN` 请求头中；凭证只保存在页面内存，不写入浏览器存储或请求 URL。管理请求另外使用绑定会话的 CSRF 校验。

密码只通过 DSM 原生确认窗口交给内置密码校验接口；HTTP 下使用 DSM SDK 的密码加密。套件后端不接收、不记录、不保存管理员密码。管理员任务使用 PasswordConfirm v2 返回的短期确认凭证；v1 只返回成功标记，不能用于此流程。

日志不记录请求 URL、token、请求头、文件名或客户端 IP。日志启动时超过 5 MiB 保留一个轮转副本。管理入口不在媒体端口暴露。

## 诊断与恢复

管理页显示普通代理、管理员接管和最近错误。服务端错误保留 JSON 原因，避免 DSM nginx 的通用 5xx 页面掩盖诊断。

| 现象 | 处理 |
| --- | --- |
| 提示缺少 `ip6table_nat.ko` | 关闭 IPv6 接管，保存后重新授权。 |
| 安装时提示 root 权限 | 使用当前版本；旧的 0.1.0 和 0.2.0 安装包存在权限声明兼容问题。 |
| 更新后仍显示旧窗口或 `common:prompt_invalid` | 刷新 DSM 页面后重新打开，确保使用 0.2.6 或更新版本。 |
| 配置保存后未接管 | 检查代理运行状态，再更新管理员授权副本。 |

如 DSM 页面授权不可用，可使用管理员终端作为后备：

```sh
sudo sh /var/packages/PlexLocalBridge/target/admin/gatewayctl.sh enable
```

普通代理和管理员接管的状态、日志：

```sh
sudo /var/packages/PlexLocalBridge/target/bin/plex-local-bridge daemon-status
sudo tail -n 80 /var/packages/PlexLocalBridge/var/service.log
sudo /usr/local/libexec/plex-local-bridge/gatewayctl status
sudo tail -n 80 /usr/local/libexec/plex-local-bridge/gateway.log
```

点击“停用端口接管”并完成 DSM 身份确认，会恢复原生入口并禁用本套件的开机恢复任务。手动停用命令：

```sh
sudo /usr/local/libexec/plex-local-bridge/gatewayctl stop
```

`stop` 不删除配置。卸载普通 SPK 后，管理员进程仍会等待，但不保留接管规则；完全停用也应执行上述命令，并停用自行创建的开机任务。

手动恢复不依赖普通配置能否解析，但必须先停止管理员接管进程：

```sh
sudo /usr/local/libexec/plex-local-bridge/plex-local-bridge recover --state /var/run/plex-local-bridge
```

## 从源码构建

需要 Go 1.24+、Python 3.9+；前端测试使用 Node.js 22+。无第三方 Go 模块依赖，构建和单元测试不操作宿主防火墙。

```sh
git clone https://github.com/hamhub-app/plex-local-bridge.git
cd plex-local-bridge
go test -race ./...
go vet ./...
python3 -m unittest discover -s tests -v
node --test tests/*.test.cjs
python3 scripts/build_spk.py --arch amd64
python3 scripts/build_spk.py --arch arm64
```

产物位于 `dist/`，附带 SHA-256。amd64 对应 Synology x86_64，arm64 对应 armv8，不支持 32 位 ARM。SPK 包含 MIT 许可证。打包使用明确文件清单、USTAR 和固定时间戳，不收集本机配置、日志或凭据。

可选 Linux 集成验证需要 Docker。仅临时 NAS 容器具有 NET_ADMIN，不使用 host 网络、不映射宿主端口；退出时删除容器和独立网络：

```sh
python3 tests/integration_docker.py --arch amd64
python3 tests/integration_docker.py --arch arm64
```

本机开发使用 `serve --config local.json --socket /tmp/plb-dev.sock`，上游必须是本机已认领且要求鉴权的 Plex；该命令不操作防火墙。DSM 普通套件入口是 `daemon`，管理员副本入口是 `gateway`。

## 参考

- [Synology：第三方 root 套件的签名限制](https://help.synology.com/developer-guide/getting_started/system_requirement.html)
- [Synology 权限配置](https://help.synology.com/developer-guide/privilege/privilege_config.html)
- [Synology DSM 应用集成](https://help.synology.com/developer-guide/integrate_dsm/desktopapp.html)
- [Synology DSM 会话校验](https://help.synology.com/developer-guide/integrate_dsm/web_authentication.html)
- [Synology 套件生命周期](https://help.synology.com/developer-guide/synology_package/scripts.html)
- [Plex Remote Access](https://support.plex.tv/articles/200931138-troubleshooting-remote-access/)
- [Plex 网络设置](https://support.plex.tv/articles/200430283-network/)
- [Go ReverseProxy](https://pkg.go.dev/net/http/httputil#ReverseProxy)

## 许可证

[MIT License](../LICENSE)，Copyright © 2026 HamHub contributors。
