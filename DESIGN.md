---
name: Sailor
description: 深潜驾驶舱——多集群 Kubernetes 桌面控制台的视觉系统
colors:
  signal-indigo: "oklch(58% 0.233 277.117)"
  rose-beacon: "oklch(65% 0.241 354.308)"
  radar-teal: "oklch(77% 0.152 181.912)"
  horizon-blue: "oklch(74% 0.16 232.661)"
  sonar-green: "oklch(76% 0.177 163.223)"
  lantern-amber: "oklch(82% 0.189 84.429)"
  flare-red: "oklch(71% 0.194 13.428)"
  abyss-slate: "oklch(25.33% 0.016 252.42)"
  abyss-slate-2: "oklch(23.26% 0.014 253.1)"
  abyss-slate-3: "oklch(21.15% 0.012 254.09)"
  foam-white: "oklch(97.807% 0.029 256.847)"
  wheelhouse-black: "#0b0f14"
  terminal-foam: "#d6deeb"
  compass-teal: "#7fdbca"
typography:
  display:
    fontFamily: "system-ui, -apple-system, PingFang SC, sans-serif"
    fontSize: "24px"
    fontWeight: 700
  headline:
    fontFamily: "system-ui, -apple-system, PingFang SC, sans-serif"
    fontSize: "18.75px"
    fontWeight: 700
  title:
    fontFamily: "system-ui, -apple-system, PingFang SC, sans-serif"
    fontSize: "14px"
    fontWeight: 600
  body:
    fontFamily: "system-ui, -apple-system, PingFang SC, sans-serif"
    fontSize: "12px"
    lineHeight: 1.5
  label:
    fontFamily: "system-ui, -apple-system, PingFang SC, sans-serif"
    fontSize: "10px"
    fontWeight: 600
    letterSpacing: "0.1em"
  mono:
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace"
rounded:
  sm: "8px"
  md: "12px"
  lg: "16px"
  modal: "20px"
  pill: "9999px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "12px"
  lg: "16px"
  xl: "20px"
  xxl: "24px"
components:
  button-primary:
    backgroundColor: "{colors.signal-indigo}"
    textColor: "{colors.foam-white}"
    rounded: "{rounded.sm}"
    height: "32px"
    padding: "0 12px"
  button-ghost:
    backgroundColor: "transparent"
    textColor: "{colors.foam-white}"
    rounded: "{rounded.sm}"
    height: "32px"
    padding: "0 12px"
  sidebar-item:
    rounded: "{rounded.sm}"
    padding: "8px 12px"
    textColor: "{colors.foam-white}"
  sidebar-item-active:
    backgroundColor: "color-mix(in oklab, {colors.signal-indigo} 12%, transparent)"
    textColor: "{colors.signal-indigo}"
    rounded: "{rounded.sm}"
    padding: "8px 12px"
  panel-section:
    backgroundColor: "color-mix(in oklab, {colors.abyss-slate-2} 40%, transparent)"
    rounded: "{rounded.md}"
    padding: "20px"
  modal-box:
    backgroundColor: "{colors.abyss-slate}"
    rounded: "{rounded.modal}"
    padding: "0"
  status-badge:
    rounded: "{rounded.pill}"
    typography: "{typography.label}"
  terminal-canvas:
    backgroundColor: "{colors.wheelhouse-black}"
    textColor: "{colors.terminal-foam}"
---

# Design System: Sailor

## Overview

**Creative North Star: "深潜驾驶舱"**

Sailor 是一艘深潜器的驾驶舱：操作者坐在黑暗里，仪表悬浮在舷窗外漂移的光斑之上。系统默认呈现为深海暗色——蓝灰板岩的舱壁（base-100/200/300），泡沫白的读数，一团团模糊的彩色光斑在固定网格海图上缓慢漂移（GSAP 驱动，非同频钟摆）。顶栏与侧栏是舱内玻璃仪表盘（fx-glass：18px 背景模糊 + 发丝边），内容则像打印在仪表板上的卡片，靠发丝线而非阴影彼此分层。真正悬浮在舱内的只有弹窗、底部抽屉与下拉菜单——它们才拥有重阴影。

这是一个高密度运维界面：正文 12px、控件高 32px、行内图标操作代替文字按钮。所有颜色都是航海仪器语言——信号靛是当前航向，声呐绿 / 灯笼琥珀 / 信号弹红 / 天际灯蓝是仪器读数，语义固定。亮色主题（甲板之上）保留同一套信号色相，仅把板岩换成冷白纸面、状态色各加深一档以通过白底对比度。

**Key Characteristics:**

- 深海暗色为默认身份，亮色为同相位的"甲板"变体
- 玻璃只属于导航 chrome；内容表面是发丝边 + 半透明板岩，不磨砂
- 阴影是稀缺资源，只表达"真的悬浮"（弹窗 / 抽屉 / 下拉）
- 高密度信息排版：12px 正文、等宽数据字、胶囊状态徽章
- 交互反馈轻而快：100–240ms，卡片悬浮 -2px 微抬升

## Colors

一套航海仪器信号色，悬浮在三级深海板岩之上；状态色语义固定，不作装饰。

### Primary

- **深海信号靛**（`signal-indigo`, oklch(58% 0.233 277.117)）：当前航向——侧栏激活项、主按钮、链接、关键数字。屏幕占比永远小。
- 亮色变体加深为 oklch(45% 0.24 277.023)（甲板强光下信号灯更沉）。

### Secondary

- **玫瑰信标**（`rose-beacon`, oklch(65% 0.241 354.308)）：少数统计维度与次级强调，出现频率低于主色。

### Tertiary

- **雷达青**（`radar-teal`, oklch(77% 0.152 181.912)）：扫描线的颜色——终端光标、聚焦提示、KEDA 等辅助仪器。

### Neutral

- **深海板岩 100/200/300**（`abyss-slate` 系列，oklch(25.33% / 23.26% / 21.15%，色相约 253°）：舱壁三级表面——画布、卡片、边框/发丝线，靠明度而非色相区分。
- **泡沫白**（`foam-white`, oklch(97.807% 0.029 256.847)）：全部正文与读数，微带蓝相与板岩同族。
- **驾驶舱黑**（`wheelhouse-black`, #0b0f14）与 **终端泡沫 / 罗盘青**（#d6deeb / #7fdbca）：终端专用子调色板（Ayu 系），与 UI 主题同族但独立成套，含 16 色 ANSI 双主题映射。

### Named Rules

**The Signal-Rarity Rule.** 深海信号靛只用于"当前与主操作"：激活导航项、主按钮、链接、关键数字。任何屏幕上主色面积 ≤10%，它的稀缺就是它的信号强度。

**The Fixed-Semantics Rule.** 声呐绿 = 健康 / 成功，灯笼琥珀 = 进行中 / 注意，信号弹红 = 异常 / 危险，天际灯蓝 = 中性信息。状态色永远表达状态，不挪作装饰或品牌点缀。

## Typography

**Display / Body Font:** system-ui, -apple-system, PingFang SC（系统无衬线，桌面原生观感）
**Label/Mono Font:** ui-monospace, SFMono-Regular, Menlo（数据、代码、kubectl 输出）

**Character:** 系统字栈保证桌面应用的原生质感与中文渲染；等宽字只用于"被测量的东西"——资源名、IP、YAML、终端。无任何装饰性字体。

### Hierarchy

- **Display**（700, 24px）：仪表盘统计大数字（如副本数）。
- **Headline**（700, 18.75px）：弹窗标题。
- **Title**（600, 14px）：卡片 / 分节标题、弹窗内小节。
- **Body**（400, 12px, 1.5）：列表与表单的密集正文——这是界面的默认字号。
- **Label**（600, 10px, +0.1em 字距，大写拉丁 / 中文原样）：侧栏分组名、徽章、轴标签。

### Named Rules

**The Measured-Data Rule.** 凡是被复制、比对、逐字符阅读的值（资源名、命名空间、IP、哈希、YAML、终端输出）一律等宽；等宽不是"技术感"装饰。

## Layout

固定顶栏（48px 高，含集群切换与全局状态）+ 可折叠侧栏（展开 240px / 收起 64px，图标居中）+ 右侧内容区（16px 内边距）。桌面应用无响应式断点，窗口最小 1080×700；列表页为高密度表格 + 工具行（命名空间组合框、搜索、分页 20 行/页）。弹窗宽度按任务分级（确认框 420–540px，YAML/详情 900–1100px）。kubectl 终端是全局底部抽屉：贴边收起成 50px 细条、展开默认 42vh、上缘可拖拽，永远浮在所有页面之上。间距节奏 4px 基数：8 控件内、12 相关组、16 区块、20–24 容器。

## Elevation & Depth

混合体系，按"是否真的悬浮"分层：导航 chrome 是磨砂玻璃（backdrop-filter blur(18px) saturate(1.4)，底色 base-100 74% 透明混色）；静止内容表面靠发丝边框（`--hair`）与板岩明度阶分层，不投影；只有弹窗、底部抽屉、下拉菜单拥有重阴影（shadow-2xl 级）表达物理悬浮。卡片悬停有 -2px 抬升 + 轻阴影（0 4px 12px oklch(0 0 0 / 0.1)）作为操作反馈，静止时归零。

### Shadow Vocabulary

- **overlay**（`0 25px 50px -12px rgb(0 0 0 / 0.25)`）：弹窗与抽屉——唯一的"重"阴影。
- **menu**（`0 10px 15px -3px rgb(0 0 0 / 0.1), 0 4px 6px -4px rgb(0 0 0 / 0.1)`）：下拉菜单与浮层。
- **card-hover**（`0 4px 12px oklch(0 0 0 / 0.1)`）：仅悬停态，黑色阴影两主题通用。

### Named Rules

**The Chrome-Only Glass Rule.** backdrop-filter 只属于顶栏与侧栏（含 strong 变体）。内容卡片、分节、表格永不磨砂——磨砂是"舷窗"，不是"纸"。

**The Floating-Shadow Rule.** 阴影只授予真正悬浮在界面之上的东西。静态内容一律发丝边框 + 色调分层；卡片内再嵌投影卡片是被禁止的（分节面板用 border + 40% 透明板岩底）。

## Shapes

圆润但不软：控件 8px、卡片/分节 12px、大型容器 16px、弹窗 20px（定制值）形成四级圆角阶梯；胶囊（9999px）只给小元素——状态徽章、指示圆点、圆形图标按钮。表格行不做圆角。图标体系统一 Font Awesome 实心（fas），尺寸 10–14px，与文字同色或语义色。背景海图是 46px 双轴网格，仅以顶部径向遮罩淡出，属于"舷窗外"层，不与内容表面竞争。

## Components

### Buttons

- **Shape:** 8px 圆角（DaisyUI btn 覆写 rounded-lg）；sm 档高 32px（h-8），紧凑图标圆钮 32–36px。
- **Primary:** 信号靛底 + 泡沫白字；仅主操作（确认、注入调试容器）。
- **Hover / Focus:** 150–200ms 色彩过渡；ghost 变体 hover 换 5–10% 内容色底。
- **行内操作（signature）：** 表格行尾用无框图标按钮（row-action）+ 原生 title 提示，替代一排文字按钮——高密度界面的核心交互形态。

### Chips

- **Style:** 胶囊状态徽章（badge-xs）：语义色 15% 透明底 + 同色 30% 边 + 同色文字，如 Running=声呐绿、Pending=灯笼琥珀、CrashLoopBackOff=信号弹红。
- **State:** 选中态用主色 10–15% 底（命名空间组合框、集群切换）。

### Cards / Containers

- **Corner Style:** 分节面板 12px（border-base-300/60 + bg-base-200/40，无阴影）；弹窗 20px。
- **Background:** 深海板岩 200 的 40% 透明混色，透出下层海图。
- **Shadow Strategy:** 静止无阴影；悬停 -2px 抬升 + card-hover 阴影；见 Elevation 节。
- **Internal Padding:** 16px（tile）至 20px（分节）；弹窗头部 16–24px。
- **统计 tile（signature）：** 居中图标 + 24px 语义色大数字 + 10px 标签，右上角 80px 同色 6% 透明光晕（stat-card orb）。

### Inputs / Fields

- **Style:** DaisyUI input + input-bordered，8px 圆角，12px 字号，等宽字用于镜像名/搜索等技术值。
- **Focus:** 主题 focus 环（主色），无自造发光。
- **命名空间组合框（signature）：** 输入框 + 子串过滤面板，替代原生 select——大集群 ns 数量下必须可搜索。

### Navigation

- **Style:** 侧栏项 = 图标（14px, 60% 透明）+ 标签，8px 圆角，10px 大写分组标题（30% 透明）；折叠态只剩居中图标。
- **Active:** 主色 12% 透明底 + 主色图标文字（The Signal-Rarity Rule 的最大消费场景）。
- **Hover:** 非激活项 5% 内容色底；200ms。顶栏集群切换为玻璃胶囊下拉。

### 终端（signature）

kubectl 抽屉与容器终端共用一套终端子调色板：驾驶舱黑底（#0b0f14）、终端泡沫字（#d6deeb）、罗盘青光标（#7fdbca），16 色 ANSI 双主题映射；亮色主题下整组翻转为白底深字（#ffffff / #232936 / #0e7490）。提示符用 ANSI 亮青 + 亮黄（集群名 / `$`），两套调色板中都保证可读。

## Do's and Don'ts

### Do:

- **Do** 用语义 token 写颜色（bg-primary、text-success、bg-warning/10）——所有状态横幅、徽章、分节底色一律 token 化，双主题自动正确。
- **Do** 数据值、资源名、终端输出用 font-mono。
- **Do** 弹窗标题用 text-lg（18.75px）与正文拉开层级。
- **Do** 新面板先复用"分节面板"（border + base-200/40 + 12px 圆角 + 无阴影）这个已确立的单层表面语法。
- **Do** 交互反馈控制在 100–240ms，卡片微抬升 -2px 是上限。

### Don't:

- **Don't** 硬编码 hex 颜色（曾出现浅色横幅在暗色主题刺眼的问题，已全部 token 化）。
- **Don't** 在卡片内再嵌带阴影的卡片；不用 border-left 彩条做强调。
- **Don't** 给内容区加 backdrop-filter 或大面积渐变文字。
- **Don't** 把状态色当装饰色用，或让主色面积超过一屏 10%。
- **Don't** 降低信息密度换取留白——正文 12px、控件 32px 高是这个产品的密度契约。
