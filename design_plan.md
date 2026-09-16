# Sailor 前端设计文档

## 1. 项目概述

Sailor 是一个单用户桌面端的 Kubernetes 多集群管理应用（Wails v2 + Go）。
前端的定位是**商业级、专业、紧凑**的现代控制台风格，参考 Linear、Vercel、Rancher 等
产品；它管理的是真实生产集群，一切视觉决策为"长时间盯着不累、信息一眼可读"服务，
装饰性元素只保留一层环境氛围（背景光斑与网格）。

## 2. 设计原则

1. **克制的视觉效果** —— 去掉过重的阴影、夸张的 hover 动画、花哨的装饰元素
2. **紧凑的布局** —— 减小 padding、缩小字号、收窄间距
3. **有意义的颜色** —— 用颜色传达信息（状态、类型、重要性），而非纯装饰
4. **一致的交互** —— 统一的按钮尺寸、表格样式、卡片风格
5. **清晰的层次** —— 通过字重、透明度、尺寸建立视觉层次，而非依赖边框和阴影

## 3. 技术栈

| 层 | 选型 | 说明 |
|---|---|---|
| 渲染 | Go `html/template` | 服务端渲染页面骨架，`{{define}}` 片段组合 |
| 样式 | Tailwind CSS 4 + daisyUI 5 | daisyUI 只用其 token 体系与少量组件类 |
| 交互 | Alpine.js | 列表状态、筛选、弹窗开关等轻状态 |
| 动效 | GSAP + 自研 fx 层（CSS） | 常驻氛围动效与交互动效，见 §6 |
| 编辑器 | Monaco（本地 vendor） | YAML 查看 / 编辑，主题随应用主题 |
| 图表 | ECharts | 仪表盘 CPU / 内存 / 容量 |
| 终端 | xterm.js | 容器终端，WebSocket 直通 |

前端依赖全部本地化（`static/js/`、`static/css/`），不赌 CDN 可用性；
Tailwind CLI 只负责把 `static/css/input.css` 编译成 `frontend/dist/static/css/output.css`。

## 4. 信息架构与布局

### 4.1 统一融合式布局

桌面窗口没有"网页感"的页头页脚，整个窗口是一块连续的表面：

- 侧边栏顶到窗口顶，macOS 红绿灯落在侧栏顶部区域；
- 顶栏（h-12 / 48px）从侧栏右缘开始，随侧栏开合平移；
- 顶栏中央是集群选择器（当前集群状态点 + 名称），右侧是主题切换；
- 侧栏与顶栏用 `fx-glass`（半透明 + backdrop blur）浮在画布上，不画分隔线，
  靠表面色差与内容区分层；
- 页头保持透明、与内容区共用画布：标题与面包屑同行（基线对齐），
  右侧为页面动作区与全局刷新指示，整体只占一行高度；
- 内容区 padding `p-4`。

### 4.2 侧栏信息架构

```
仪表盘
集群        集群列表 / 节点管理 / 命名空间
工作负载    Deployments / StatefulSets / DaemonSets / Jobs / CronJobs / Pods
弹性伸缩    HPA / KEDA ScaledObjects / KEDA ScaledJobs
网络与存储  Services / Ingress / PV·PVC
配置        ConfigMaps / Secrets
（footer）  品牌 logo + 版本号
```

- 展开 15rem、收起 4rem（只留图标），折叠按钮骑在侧栏右边框上（垂直居中）；
- 折叠状态持久化到 `localStorage.sidebarOpen`；
- 当前页面项高亮：12% 主色底 + 主色文字 + 左缘发光条（亮色下去掉发光）。

### 4.3 资源列表页骨架

所有资源列表共用 `resources/base_list.html`：

```
┌ fx-card ─────────────────────────────────────────────┐
│ [图标] 标题 [计数]    [命名空间▾] [搜索] [页面动作]      │
│ 集群错误横幅（仅 sync 失败时）                          │
│ table：table_headers … table_row（页面级覆盖）          │
│ 分页 · 空态（搜索无结果 / 首次同步 / 集群错误 / 暂无数据）│
└──────────────────────────────────────────────────────┘
```

页面级模板只覆盖 `table_headers` / `table_row` / `extra_actions` 等插槽，
筛选（命名空间 combobox 支持子串搜索）、名称搜索、分页、乐观更新、
智能轮询全部由骨架提供（见 §8）。

可选 CRD 资源（KEDA）在集群未安装时展示引导空态（"未检测到 KEDA" +
部署文档链接），由列表 API 透传的 `optional_installed` 标记驱动 ——
这是唯一允许"空态分支"进入骨架的特例，因为"未安装"不是错误。

## 5. 主题系统

### 5.1 token 驱动

- 主题由 `<html data-theme="dark|light">` 驱动，daisyUI 5 token 体系
  （`--color-base-100/200/300/content`、`--color-primary/secondary/info/success/warning/error`
  及对应 `-content`）；
- 自定义样式**禁止硬编码颜色**，一律 `color-mix(in oklab, var(--color-*) N%, transparent)`
  从 token 派生 —— 主题切换时所有派生色自动跟随；
- 亮色主题是定制过的（`static/css/input.css` 中 `@plugin "daisyui/theme"`）：
  状态色整体加深一档（大量 `text-*` 直接落在白底上，浅色版既刺眼又读不清；
  加深后实底组件配近白 content，对比过 AA），base-200/300 带同族冷灰拉开画布层次。

### 5.2 两套主题的差异化参数

| 维度 | 暗色（默认） | 亮色 |
|---|---|---|
| 阴影 | 重黑阴影（34%~46% 黑） | 收敛浅阴影（4%~14% 黑），靠发丝边分层 |
| 背景光斑 | primary/info/secondary 30%/26%/20% | 降到 11%/10%/8%，只留环境氛围 |
| 网格底纹 | white 5% | base-content 4.5% |
| 装饰光效 | 数字微光、激活项辉光保留 | 关闭（白底上只会显灰） |

### 5.3 主题切换与联动

- 顶栏 swap 控件（daisyUI `theme-controller`）+ `localStorage.theme` 持久化；
- Monaco：`monacoTheme()` 读 `data-theme`（亮 `vs` / 暗 `vs-dark`），
  MutationObserver 监听属性变化，弹窗开着切换即时跟随；
- ECharts：图表创建时按 `data-theme` 选文字 / 网格 / tooltip 配色；
- 滚动条、选中态等由 token 派生，自动适配。

## 6. 视觉动效层（fx）

`static/css/sailor-fx.css` + `static/js/sailor-fx.js` 构成独立视觉层，
只写 CSS 变量与装饰 DOM，不碰业务状态：

- **分层 token**：光晕三色 `--fx-a1/a2/a3`、发丝线 `--hair/--hair-2`、三档阴影
  （静置 / 浮起 / 弹层），全部从主题 token 派生；
- **玻璃拟态** `fx-glass` / `fx-glass-strong`：半透明 + blur + saturate；
- **卡片** `fx-card`：发丝边 + 浅阴影，hover 抬升；`fx-card-glow` 鼠标跟随柔光；
- **氛围动效**：三团光斑由 GSAP 驱动独立随机周期漂移（避免整组同频钟摆感），
  网格底纹静态；
- **交互动效**：边框流光 `fx-edge`、渐变流动文字 `fx-flow`、品牌 logo 流动 `fx-logo`、
  表格行 hover 左缘色条、状态点脉冲 `fx-pulse`、点击水波纹 `fx-wave`、
  骨架屏 shimmer、入场 `fx-rise` / `fx-stagger`；
- **可访问性**：`prefers-reduced-motion: reduce` 下常驻动画全停、
  hover 反馈降为瞬时（保留必要信息）。

## 7. 组件规范

### 7.1 尺寸与密度

- 页面动作按钮：`btn-sm h-8 min-h-0 text-xs`；
- 筛选输入框、搜索框：`input-sm h-8 min-h-0 text-xs`；
- 表格：`table-sm`，表头 `text-xs font-semibold`，单元格文字 `text-xs` 为主；
- 弹窗：YAML 编辑器 90vw×90vh，describe 85vw×88vh，确认类弹窗 max-w-md。

### 7.2 语义色用法

| 颜色 | 含义 | 典型场景 |
|---|---|---|
| success | 健康 / 完成 / 在线 | Running 徽章、Complete、副本就绪 |
| warning | 中间态 / 降级 | Pending、已挂起、副本未就绪、集群错误横幅 |
| error | 失败 / 危险操作 | Failed、Terminating、删除按钮 hover |
| info | 进行中 / 提示 | Running 徽章、编辑模式提示条 |
| primary | 品牌 / 主操作 / 选中 | 主按钮、当前侧栏项、Age 列 |

半透明底 + 同色文字的 chip 形态（`bg-x/10 border-x/20 text-x`）用于
命名空间、cron 表达式等"标签型"信息；实底 badge 用于状态。

### 7.3 行内操作

表格行内操作用 28px 图标按钮（`row-action`），悬停显示语义底色：
危险操作 `is-danger`（红）、警告类 `is-warn`（琥珀）；tooltip 用原生 `title`
（DaisyUI tooltip 会被 `overflow-x-auto` 裁切）。危险动作（删除）走确认弹窗，
可逆操作（重启、CronJob 挂起/触发）直接执行 + toast 反馈。

### 7.4 反馈

- **toast**：右上角 `showToast(msg, type)`，操作结果统一出口；
- **乐观更新**：删除→标记 Terminating/移除、创建→顶部插入、更新→原位替换，
  配 `_pendingOps` TTL 防止缓存回读"复活"旧数据；
- **全局刷新指示**：页头右侧"刷新中"，由 Alpine store `ui.refreshing` 驱动；
- **错误分级**：请求失败（服务不可达）/ 集群 sync 失败（横幅 + 重试）/ 表单校验（内联提示）。

## 8. 状态与数据约定

- 列表读本地缓存（`/resources/{id}/api/{kind}/`），写操作直连 API Server；
- 写操作成功后端触发该类资源立即同步，前端同时做乐观更新，
  1.5s 后静默刷新拉真实数据；
- 智能轮询：存在 Terminating 资源时 2.5s→6s 递减（最长 2 分钟），
  存在不健康工作负载时 5s（最长 1 分钟），否则不轮询；
- URL 是状态的一部分：`?namespace=&search=&page=` 由 Alpine 同步，
  刷新页面恢复筛选条件；
- 事件协议：`resource-updated`（delete/create/update/无 action=强制刷新），
  弹窗与列表解耦，只认事件。

## 9. 后续规划

- **Events 查看器**：独立事件列表页，Warning/Normal 颜色编码，按资源类型与命名空间过滤；
- **详情页深化**：Pod conditions / volumes / QoS，Service endpoints 列表
  （现有 describe 弹窗之上的自然延伸）；
- **快照对比**：YAML 编辑前后 diff 视图；
- **CRD 支持**：动态 GVR 发现，把任意 CRD 挂进列表页骨架。
