# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

（Wails v2 桌面壳内的 Web UI：WKWebView / WebView2 渲染，UI 技术栈按 web 对待；macOS 为主力开发平台，Windows / Linux 各自原生构建。）

## Users

作者本人为主力用户——管理多套 K8s 集群的运维 / 开发者，日常在多环境（如 uat 等测试与生产集群）间切换查看与操作。产品立场为「自用为主、兼容发布」：优先贴合作者个人效率与习惯，但设计决策按可对外发布的标准取舍，不为个人便利留下需要返工的架构债。

## Product Purpose

单用户、多集群 Kubernetes 桌面控制台：把「面板查看、YAML 编辑、日志、容器终端、kubectl 命令行、应急操作（扩缩 / 重启 / 回滚 / 删除 / 调试容器注入）」收进一个原生桌面窗口。成功标准：日常集群巡检与应急排障不再需要在浏览器面板、终端、多份 kubeconfig 之间来回切换。

## Positioning

一体化工作台（用户确认的核心定位）：多集群统一面板 + 进程内 kubectl 命令树 + 桌面级容器终端，价值在「减少工具切换」。与 Lens / k9s 的差异不在单点功能对标，而在把查看与操作的一体化体验做完整；性能路径（读缓存写直连）是支撑这个定位的机制而非卖点本身。

## Operating Context

- 多份 kubeconfig 导入后以 AES-256-GCM 加密存储在本机数据目录，凭据不出本机
- 设计下限是真实大集群：作者测试环境 250+ 节点、16765 Pod（全量直连拉取 259.6s / 168MB），因此列表全部走本地缓存 + 后台周期同步，写操作直连 API Server
- 界面语言当前为纯中文（i18n 为 Open Decision，见下）
- 本项目是内部 Django 版控制台的 Go + Wails v2 重写，URL 结构与 Django 版对齐
- 长驻本地 WebSocket 网关（127.0.0.1 随机端口）服务容器终端与 kubectl 终端

## Capabilities and Constraints

已确认能力：多集群管理与一键切换；15 类资源的列表 / 详情 / YAML 编辑（真实 dry-run 校验）/ 扩缩 / 重启 / 回滚 / 删除；Pod 日志与容器终端；注入临时调试容器（ephemeral container）；内置 kubectl 终端（进程内命令树，非外部二进制）；节点管理（cordon / drain / 移除）；指标仪表盘（Metrics Server 降级链）；亮 / 暗主题。

约束（后续工作不得违反）：

- 单用户桌面应用，无服务端、无账号体系、无多人协作语义
- kubeconfig 与凭据只存在本机加密存储，任何功能不得将凭据发送到本机以外
- 内置 kubectl 不支持依赖外部进程的子命令（edit / diff / port-forward / proxy / 插件发现），新功能不得悄悄引入外部进程依赖
- 页面为服务端渲染整页跳转（非 SPA）；跨页面状态用 sessionStorage / localStorage 保持

Open Decisions（明确未决，勿当作已定方向实现）：

- 界面多语言（i18n）：作者在考虑但未定；未决期间新增界面文案不要求立即抽离，但不得写死难以抽取的结构

## Evidence on Hand

- README 中的大集群性能实测表（Pod 16765 条：直连 259.6s vs 缓存毫秒级）为真实测量数据
- `docs/screenshots/` 存有界面截图
- 无客户、证言、案例、基准报告——后续设计工作不得虚构此类素材

## Product Principles

1. **一体化优先**：新能力优先融入现有面板、弹窗、终端抽屉等已在场容器，减少页面跳转与工具切换
2. **读缓存、写直连**：查看永远毫秒级（本地缓存），写入永远反映集群真实状态（直连 API Server）
3. **凭据不出本机**：桌面单机可信边界是产品前提
4. **以大集群为设计下限**：列表、筛选、终端等界面在 16000+ Pod 规模下必须可用
5. **自用为主、兼容发布**：允许贴个人习惯的取舍，但不得引入对外发布时需要返工的结构性债务
