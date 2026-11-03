# Plex Local Bridge

[简体中文](README.md) | [English](README.en.md)

[![CI](https://github.com/hamhub-app/plex-local-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/hamhub-app/plex-local-bridge/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/hamhub-app/plex-local-bridge?include_prereleases)](https://github.com/hamhub-app/plex-local-bridge/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**解决 Plex 外网无法播放、403 和原始文件读取失败等问题。**

Plex Local Bridge 是由 HamHub 开源的群晖 Plex 本机代理套件，将公网播放请求转发到 NAS 上的 Plex 服务，处理由访问来源识别引起的播放失败。

通过 DSM WebUI 设置和启用，继续使用原来的公网地址、客户端和路由器端口映射。

## 它解决什么问题

- Plex 外网无法播放，或媒体请求返回 403。
- 已配置 Plex 远程访问，可以连接服务器，但原始文件无法播放。
- 希望在群晖本机处理转发，保留现有的访问入口和端口映射。

默认转发链路：`公网客户端 → NAS:32400 → 本机代理:13240 → 原生 Plex:32400`。

代理保留 Plex 账号鉴权、Range、WebSocket 和流式传输，不转码、不缓存媒体、不扫描全片。

## 怎么使用

适用于 **DSM 7.2.2+ 的原生 Plex Media Server 套件**。开始前，确保公网请求已能到达 NAS，Plex 已认领并启用账号鉴权；“无需身份验证即可访问的 IP 地址和网络列表”应保持为空。

1. 从 [GitHub Releases](https://github.com/hamhub-app/plex-local-bridge/releases) 的 **Assets** 下载对应架构的 `.spk`，在 DSM「套件中心 → 手动安装」中直接选择安装，**无需解压或自行编译，也无需登录 GitHub**。Intel / AMD 64 位 NAS 选择 `x86_64.spk`，ARM 64 位 NAS 选择 `armv8.spk`。
2. 启动套件并打开 WebUI。默认 NAS 入口为 **32400**、代理监听为 **13240**，也可以自行修改。
3. 只使用公网 IPv4 时，取消勾选“同时接管 IPv6 入口”。
4. 点击“保存并应用”→“启用 / 更新接管”，在 **DSM 原生窗口**确认管理员密码。
5. 页面显示“接管中”后，用原来的公网地址重新播放。

每个 Release 同时提供两种架构的 SPK 和对应的 `.spk.sha256` 校验文件。开发测试包仍可从 [GitHub Actions](https://github.com/hamhub-app/plex-local-bridge/actions/workflows/ci.yml) 下载 `plex-local-bridge-spk` 产物，解压后使用其中的 SPK。

需要 NAS 重启后继续接管时，在授权前勾选“NAS 重启后自动恢复接管”。管理员密码只交给 DSM 校验，套件后端不接收、不记录、不保存密码。

启用 DSM 防火墙时，请允许代理监听端口，并保持与原 Plex 端口相同的来源范围。缺少 `ip6table_nat.ko` 的系统应关闭 IPv6 接管。

## 兼容性与范围

| 环境 | 状态 |
| --- | --- |
| DS220+ / x86_64 / DSM 7.4.1 | 已验证 WebUI、管理员授权、IPv4 接管，用户已确认公网播放成功。 |
| armv8 | 提供交叉构建与隔离 Linux 集成测试，真实 ARM NAS 待验证。 |
| HTTPS | 使用 Plex 原有证书透传，无法改写加密的来源头；效果取决于服务器版本和连接路径。 |

当前为 **0.2.6-0009 实验版本**，主要针对原生 Plex 套件。不同 Plex 版本、Plex Relay、Docker 网络或其它反向代理的访问路径，需要单独验证。

## 从源码构建

需要 Go 1.24+、Python 3.9+。

```sh
git clone https://github.com/hamhub-app/plex-local-bridge.git
cd plex-local-bridge
python3 scripts/build_spk.py --arch amd64
python3 scripts/build_spk.py --arch arm64
```

产物位于 `dist/`，附带 SHA-256。`amd64` 对应 x86_64，`arm64` 对应 armv8。

发布时将 `VERSION` 更新为套件版本并推送对应的 `v<VERSION>` 标签（例如 `v0.2.6-0009`）。CI 通过测试、构建与校验后，会自动创建实验版 Release 并上传两种架构的 SPK 和 SHA-256 文件；标签必须与 `VERSION` 一致。

更多配置、权限原理、诊断恢复和测试命令见 [详细文档](doc/DETAILS.md)。

## 关于 HamHub

HamHub 是一款全能媒体播放器，支持网盘、NAS，以及 Plex、Emby、Jellyfin 等媒体服务，将影视资源集中到统一媒体库，支持媒体播放与离线下载。

官网：[www.hamhub.app](https://www.hamhub.app/)

## 许可证

[MIT License](LICENSE)。本项目与 Plex、Synology 官方无关联。
