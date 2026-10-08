// runner：一行输入 → 分词 → 内建/拦截 → kubectl 命令树执行。
package ksh

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	genericclioptions "k8s.io/cli-runtime/pkg/genericclioptions"
	genericiooptions "k8s.io/cli-runtime/pkg/genericiooptions"
	kcmd "k8s.io/kubectl/pkg/cmd"
)

// tokenize 轻量 sh 风格分词：空格分隔，支持单引号 / 双引号 / 反斜杠转义。
// 不展开变量、不做命令替换 —— kubectl 参数用不到这些。
func tokenize(line string) ([]string, error) {
	var args []string
	var cur []byte
	inWord := false
	quote := byte(0)

	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if quote == '"' && c == '\\' && i+1 < len(line) &&
				(line[i+1] == '"' || line[i+1] == '\\') {
				i++
				cur = append(cur, line[i])
			} else {
				cur = append(cur, c)
			}
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == '\\' && i+1 < len(line):
			i++
			cur = append(cur, line[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				args = append(args, string(cur))
				cur = cur[:0]
				inWord = false
			}
		default:
			cur = append(cur, c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("引号未闭合")
	}
	if inWord {
		args = append(args, string(cur))
	}
	return args, nil
}

// runningCmd 前台命令句柄：Ctrl+C → cancel；键盘输入 → stdin（仅对声明
// 要读 stdin 的命令开启，见 wantStdin）。
type runningCmd struct {
	cancel func()

	mu    sync.Mutex
	stdin io.WriteCloser
}

func (r *runningCmd) feedStdin(p []byte) {
	r.mu.Lock()
	w := r.stdin
	r.mu.Unlock()
	if w != nil {
		w.Write(p)
	}
}

// closeStdin 发 EOF（Ctrl+D）：apply -f - 粘贴完内容后靠它结束输入。
func (r *runningCmd) closeStdin() {
	r.mu.Lock()
	w := r.stdin
	r.stdin = nil
	r.mu.Unlock()
	if w != nil {
		w.Close()
	}
}

// stripKubectlPrefix 允许用户带上 kubectl 前缀（肌肉记忆）。
func stripKubectlPrefix(args []string) []string {
	if len(args) > 0 && args[0] == "kubectl" {
		return args[1:]
	}
	return args
}

// wantStdin 判断命令是否真的读 stdin：exec -i/--stdin，或 * -f - 形态的
// apply/create/replace/delete。其余命令一律不接 stdin —— io.Pipe 无缓冲，
// 给不读 stdin 的命令喂键击会阻塞 WS 读循环（type-ahead 卡死）。
func wantStdin(args []string) bool {
	for i, a := range args {
		switch {
		case a == "-i" || a == "--stdin" || a == "--stdin=true":
			return true
		case a == "-f" || a == "--filename":
			if i+1 < len(args) && args[i+1] == "-" {
				return true
			}
		case a == "--filename=-":
			return true
		case a == "-if" || a == "-ti" || a == "-it": // exec 的合并短参
			return true
		}
	}
	return false
}

// unsupported 在进程内跑不了的子命令：依赖外部进程（$EDITOR / diff）或
// 是长驻网络进程。键为子命令名，值为给用户的提示。
// edit 不在此列——它被 routeEdit 拦截转发到应用内 YAML 编辑器。
var unsupported = map[string]string{
	"diff":         "diff 依赖外部 diff 程序，内置终端暂不支持",
	"port-forward": "port-forward 是长驻端口转发进程，请在自己的终端里运行",
	"proxy":        "proxy 是长驻代理进程，请在自己的终端里运行",
	"plugin":       "进程内执行不支持 kubectl 插件发现（krew 等插件不可用）",
}

// editAliases 把 edit 常见的简写 / 复数形式归一到内部 kind。
// 与 k8sx.GVRs 的 kind 键一致；Sailor 资源页支持哪些就收录哪些。
var editAliases = map[string]string{
	"deploy": "deployment", "deployments": "deployment",
	"sts": "statefulset", "statefulsets": "statefulset",
	"ds": "daemonset", "daemonsets": "daemonset",
	"po": "pod", "pods": "pod",
	"svc": "service", "services": "service",
	"ing": "ingress", "ingresses": "ingress",
	"cm": "configmap", "configmaps": "configmap",
	"secret": "secret", "secrets": "secret",
	"job": "job", "jobs": "job",
	"cj": "cronjob", "cronjobs": "cronjob",
	"ns": "namespace", "namespaces": "namespace",
	"pvc": "persistentvolumeclaim", "persistentvolumeclaims": "persistentvolumeclaim",
	"hpa": "hpa", "hpas": "hpa",
}

// parseEditArgs 解析 edit 参数：edit <type>[/<name>] [name] [-n ns]。
// 只认 -n / --namespace（含 = 形式）；其余 flag 一律报不支持，
// 避免静默忽略 -o 之类改变行为的东西。
func parseEditArgs(args []string) (kindAlias, name, namespace string, hasNS bool, err error) {
	positionals := []string{}
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--namespace":
			if i+1 >= len(args) {
				return "", "", "", false, fmt.Errorf("%s 缺少参数值", a)
			}
			namespace, hasNS = args[i+1], true
			i++
		case strings.HasPrefix(a, "--namespace="):
			namespace, hasNS = strings.TrimPrefix(a, "--namespace="), true
		case strings.HasPrefix(a, "-"):
			return "", "", "", false, fmt.Errorf("edit 暂不支持参数 %s（当前支持 -n/--namespace）", a)
		default:
			positionals = append(positionals, a)
		}
	}
	if len(positionals) == 0 {
		return "", "", "", false, fmt.Errorf("用法：edit <类型>[/<名称>] [名称] [-n 命名空间]")
	}
	if strings.Contains(positionals[0], "/") {
		parts := strings.SplitN(positionals[0], "/", 2)
		kindAlias = parts[0]
		if len(positionals) > 1 {
			return "", "", "", false, fmt.Errorf("类型/名称 形式下不要再跟第二个位置参数")
		}
		if len(parts) != 2 || parts[1] == "" {
			return "", "", "", false, fmt.Errorf("类型/名称 形式不完整")
		}
		name = parts[1]
		return kindAlias, name, namespace, hasNS, nil
	}
	if len(positionals) < 2 {
		return "", "", "", false, fmt.Errorf("用法：edit <类型> <名称> [-n 命名空间]")
	}
	return positionals[0], positionals[1], namespace, hasNS, nil
}

// routeEdit 把 kubectl edit 拦截转发到应用内 Monaco YAML 编辑器：
// 真 edit 靠 $EDITOR 外部进程，内置终端走不了——改用 Sailor 自己的
// 编辑弹窗（语法高亮 + 真实 dry-run 校验），体验反而更好。
// 通过 WS 文本帧 {type:"yaml-edit"} 交给前端打开弹窗。
func (s *Session) routeEdit(args []string) {
	alias, name, ns, hasNS, err := parseEditArgs(args)
	if err != nil {
		fmt.Fprintf(s.out, "\x1b[33m%s\x1b[0m\r\n", err)
		s.liner.showPrompt()
		return
	}
	kind, ok := editAliases[alias]
	if !ok {
		fmt.Fprintf(s.out, "\x1b[33medit 暂支持：%s\x1b[0m\r\n", strings.Join(sortedKeys(editAliases), " "))
		s.liner.showPrompt()
		return
	}
	// namespace 是 cluster-scoped：URL 不带 ns 段；其余缺省 default（与
	// 临时 kubeconfig 的默认上下文一致）
	namespace := ns
	if !hasNS && kind != "namespace" {
		namespace = "default"
	}
	if kind == "namespace" {
		namespace = ""
	}
	s.out.sendControl(map[string]string{
		"type": "yaml-edit", "kind": kind, "name": name,
		"namespace": namespace, "cluster": s.Cluster,
	})
	fmt.Fprintf(s.out, "\x1b[36m已转到 YAML 编辑器：%s %s\x1b[0m\r\n", kind, name)
	fmt.Fprint(s.out, "在弹窗右上角打开「编辑模式」修改，点「应用到集群」生效（提交前走真实 dry-run 校验）。\r\n")
	s.liner.showPrompt()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ─── Tab 补全 ─────────────────────────────────────────────────

// kubectlVerbs 内置 kubectl v0.37 的子命令表（静态维护，与链接的库同版本）。
var kubectlVerbs = []string{
	"annotate", "api-resources", "api-versions", "apply", "attach", "auth",
	"autoscale", "certificate", "cluster-info", "completion", "config",
	"cordon", "cp", "create", "debug", "delete", "describe", "diff",
	"drain", "edit", "events", "exec", "explain", "expose", "get",
	"kustomize", "label", "logs", "options", "patch", "plugin",
	"port-forward", "proxy", "replace", "rollout", "run", "scale", "set",
	"taint", "top", "uncordon", "version", "wait",
}

var builtinWords = []string{"clear", "exit", "help", "quit"}

// completionKinds 补全用的类型词表（别名/复数/全称 → 内部 kind）。
// 比 editAliases 宽：node / pv 等 Sailor 详情页没有的类型也补全——
// get / describe 等真实子命令认它们，只是名字补全没有缓存来源。
var completionKinds = func() map[string]string {
	m := map[string]string{}
	for k, v := range editAliases {
		m[k] = v
		if _, ok := m[v]; !ok {
			m[v] = v
		}
	}
	for _, e := range [][2]string{
		{"node", "node"}, {"nodes", "node"}, {"no", "node"},
		{"persistentvolume", "persistentvolume"}, {"persistentvolumes", "persistentvolume"}, {"pv", "persistentvolume"},
		{"endpoints", "endpoints"}, {"endpoint", "endpoints"}, {"ep", "endpoints"},
		{"replicaset", "replicaset"}, {"replicasets", "replicaset"}, {"rs", "replicaset"},
	} {
		m[e[0]] = e[1]
	}
	return m
}()

// clusterScopedKinds 这些类型不带命名空间（-n 过滤不适用）。
var clusterScopedKinds = map[string]bool{
	"namespace": true, "node": true, "persistentvolume": true,
}

// complete 行编辑器的 Tab 回调：返回（插入文本, 候选列表）。
// 按位置推断意图：首词=动词；-n 后=命名空间；类型词后=资源名（读同步
// 缓存，微秒级）；type/name 形式补名字部分；logs/exec 的首个参数直接
// 补 Pod 名；rollout 补子命令。
func (s *Session) complete(line string) (string, []string) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		fields = []string{""}
	}
	endsSpace := strings.HasSuffix(line, " ")
	word := fields[len(fields)-1]
	if endsSpace {
		word = ""
	} else {
		fields = fields[:len(fields)-1] // fields = 已完成的词
	}
	prev := ""
	if len(fields) > 0 {
		prev = fields[len(fields)-1]
	}

	// -n / --namespace 后面：命名空间名
	if prev == "-n" || prev == "--namespace" {
		return pickNames(s.namesOf("namespace", ""), word)
	}

	// type/name 形式：补 "/" 后的名字部分（保留已输入的 type/ 前缀）
	if i := strings.LastIndex(word, "/"); i > 0 {
		typ, part := word[:i], word[i+1:]
		if kind, ok := completionKinds[typ]; ok {
			ins, opts := pickNames(s.namesOf(kind, s.nsFor(kind, nsFlagValue(fields))), part)
			if opts != nil {
				for i := range opts {
					opts[i] = typ + "/" + opts[i]
				}
			}
			return ins, opts
		}
		return "", nil
	}

	// 首词：动词 + 内建
	// 汇总位置参数（剔除 flag 与 -n 的值），再按位置推断意图
	var pos []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "-n" || f == "--namespace" {
			i++ // 连值一起跳过
			continue
		}
		if strings.HasPrefix(f, "-") {
			continue // 其余 flag 单 token
		}
		pos = append(pos, f)
	}
	// 剥掉 kubectl 前缀（允许带前缀敲，补全语义与执行语义一致）
	if len(pos) > 0 && pos[0] == "kubectl" {
		pos = pos[1:]
	}
	if len(pos) == 0 {
		words := append([]string{"kubectl"}, builtinWords...)
		words = append(words, kubectlVerbs...)
		return pickWords(words, word)
	}

	// 特例动词：logs/exec 首个位置参数直接是 Pod 名；rollout 补子命令
	switch pos[0] {
	case "logs", "exec":
		if len(pos) == 1 {
			return pickNames(s.namesOf("pod", s.nsFor("pod", nsFlagValue(fields))), word)
		}
	case "rollout":
		if len(pos) == 1 {
			return pickWords([]string{"history", "pause", "restart", "resume", "status", "undo"}, word)
		}
	}

	// 最后一个位置参数是类型词 → 补资源名（pos>=2 确保不是动词位）
	if len(pos) >= 2 {
		if kind, ok := completionKinds[pos[len(pos)-1]]; ok {
			return pickNames(s.namesOf(kind, s.nsFor(kind, nsFlagValue(fields))), word)
		}
	}

	// 动词后的第一个位置参数：类型词
	if len(pos) == 1 {
		return pickWords(sortedKeys(completionKinds), word)
	}
	return "", nil
}

// nsFor 类型是否带命名空间：显式 -n 优先；cluster-scoped 恒空；
// 其余缺省 default（与 kubectl 默认一致）。
func (s *Session) nsFor(kind, nsFlag string) string {
	if clusterScopedKinds[kind] {
		return ""
	}
	if nsFlag != "" {
		return nsFlag
	}
	return "default"
}

func (s *Session) namesOf(kind, ns string) []string {
	if s.m == nil || s.m.names == nil {
		return nil
	}
	return s.m.names(s.ClusterID, kind, ns)
}

// nsFlagValue 从已输入词里找 -n x / --namespace x / --namespace=x。
func nsFlagValue(fields []string) string {
	for i, f := range fields {
		if f == "-n" || f == "--namespace" {
			if i+1 < len(fields) {
				return fields[i+1]
			}
		}
		if strings.HasPrefix(f, "--namespace=") {
			return strings.TrimPrefix(f, "--namespace=")
		}
	}
	return ""
}

// pickWords 静态词表补全：前缀过滤后，唯一候选 → 补全 + 空格；
// 多候选 → 补公共前缀（有延伸时）或列出。
func pickWords(words []string, word string) (string, []string) {
	sorted := append([]string{}, words...)
	sort.Strings(sorted)
	return pick(sorted, word, true)
}

func pickNames(names []string, word string) (string, []string) {
	return pick(names, word, false)
}

func pick(cands []string, word string, trailingSpace bool) (string, []string) {
	matched := []string{}
	for _, c := range cands {
		if strings.HasPrefix(c, word) {
			matched = append(matched, c)
		}
	}
	switch len(matched) {
	case 0:
		return "", nil
	case 1:
		ins := matched[0][len(word):]
		if trailingSpace {
			ins += " "
		}
		return ins, nil
	}
	// 公共前缀有延伸 → 只补前缀（bash 行为）；无延伸 → 列出
	prefix := matched[0]
	for _, c := range matched[1:] {
		for !strings.HasPrefix(c, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	if len(prefix) > len(word) {
		return prefix[len(word):], nil
	}
	return "", matched
}

const helpText = `内建命令：
  exit / quit / logout   关闭会话
  clear                  清屏
  help                   本帮助
其余交给 kubectl（前缀可省略）：get pods -A、describe deploy/nginx -n default、
  logs -f xxx、apply -f -（粘贴 YAML 后 Ctrl+D 结束输入）、exec -it xxx -- sh …

行编辑：Tab 补全（动词 / 类型 / 资源名 / -n 命名空间）· ↑/↓ 历史 ·
  ←/→ 移动光标 · Home/End 跳行首行尾 · Ctrl+A/E 同 Home/End · Ctrl+U 清行

注意：Ctrl+C 由 Sailor 拦截用于中断当前命令，不会传给远程容器 —— 退出
交互式 exec 请输入 exit。`

// submitLine 行编辑器回调：空闲时收到一整行。内建命令同步处理，kubectl
// 子命令异步执行（运行期间输入直通其 stdin，结束后回提示符）。
func (s *Session) submitLine(line string) {
	trimmed := strings.TrimSpace(line)
	switch trimmed {
	case "":
		s.liner.showPrompt()
		return
	case "exit", "quit", "logout":
		fmt.Fprint(s.out, "再见\r\n")
		s.Close()
		return
	case "clear":
		s.out.Write([]byte("\x1b[2J\x1b[H"))
		s.liner.showPrompt()
		return
	case "help", "?":
		fmt.Fprint(s.out, helpText+"\r\n")
		s.liner.showPrompt()
		return
	}

	args, err := tokenize(trimmed)
	if err != nil {
		fmt.Fprintf(s.out, "\x1b[31m%s\x1b[0m\r\n", err)
		s.liner.showPrompt()
		return
	}
	args = stripKubectlPrefix(args)
	if len(args) == 0 {
		s.liner.showPrompt()
		return
	}
	if args[0] == "edit" {
		s.routeEdit(args)
		return
	}
	if reason, bad := unsupported[args[0]]; bad {
		fmt.Fprintf(s.out, "\x1b[33m%s\x1b[0m\r\n", reason)
		s.liner.showPrompt()
		return
	}

	ctx, cancel := context.WithCancel(s.ctx)
	stdinReader, stdinWriter := io.Pipe()
	run := &runningCmd{cancel: cancel}
	if wantStdin(args) {
		run.stdin = stdinWriter
	}
	s.mu.Lock()
	s.running = run
	s.mu.Unlock()

	go func() {
		// kubectl 库内部个别路径会 panic（极端输入 / 版本错配），兜住别带崩应用
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(s.out, "\r\n\x1b[31m命令内部错误（已恢复）：%v\x1b[0m\r\n", r)
			}
			cancel()
			run.closeStdin()
			s.mu.Lock()
			s.running = nil
			s.mu.Unlock()
			s.out.ensureNewline()
			s.liner.showPrompt()
		}()
		runKubectl(ctx, s.kubeconfig, args, stdinReader, s.out)
	}()
}

// noPlugins 空插件处理器：进程内没有 PATH 插件发现，一律“找不到”。
type noPlugins struct{}

func (noPlugins) Lookup(filename string) (string, bool) { return "", false }
func (noPlugins) Execute(executablePath string, cmdArgs, environment []string) error {
	return nil
}

// runKubectl 进程内执行一条 kubectl 子命令。
//
// kubeconfig 双保险：ConfigFlags.KubeConfig 与 --kubeconfig 参数都指向会话
// 临时文件，确保不读 KUBECONFIG 环境变量、不碰 ~/.kube/config。输出统一走
// IOStreams → WS；错误由我们自己打印（SilenceErrors），行为贴近 kubectl
// 二进制只报 error: 不刷 usage。
func runKubectl(ctx context.Context, kubeconfigPath string, args []string, stdin io.Reader, out io.Writer) {
	kc := kubeconfigPath
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &kc

	root := kcmd.NewKubectlCommand(kcmd.KubectlOptions{
		IOStreams:     genericiooptions.IOStreams{In: stdin, Out: out, ErrOut: out},
		PluginHandler: noPlugins{},
		ConfigFlags:   flags,
	})
	root.SilenceUsage = true
	root.SilenceErrors = true

	full := append([]string{"--kubeconfig", kubeconfigPath}, args...)
	root.SetArgs(full)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(out, "\x1b[31merror: %v\x1b[0m\r\n", err)
	}
}
