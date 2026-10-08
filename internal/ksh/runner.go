// runner：一行输入 → 分词 → 内建/拦截 → kubectl 命令树执行。
package ksh

import (
	"context"
	"fmt"
	"io"
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
var unsupported = map[string]string{
	"edit":         "edit 依赖外部编辑器（$EDITOR），请改用资源的 YAML 编辑弹窗",
	"diff":         "diff 依赖外部 diff 程序，内置终端暂不支持",
	"port-forward": "port-forward 是长驻端口转发进程，请在自己的终端里运行",
	"proxy":        "proxy 是长驻代理进程，请在自己的终端里运行",
	"plugin":       "进程内执行不支持 kubectl 插件发现（krew 等插件不可用）",
}

const helpText = `内建命令：
  exit / quit / logout   关闭会话
  clear                  清屏
  help                   本帮助
其余交给 kubectl（前缀可省略）：get pods -A、describe deploy/nginx -n default、
  logs -f xxx、apply -f -（粘贴 YAML 后 Ctrl+D 结束输入）、exec -it xxx -- sh …

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
