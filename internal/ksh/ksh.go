// Package ksh：内置 kubectl 终端（REPL）。
//
// 不 spawn 外部 kubectl 二进制，而是进程内引入 k8s.io/kubectl 的命令树：
// 每读入一行命令就 NewKubectlCommand + SetArgs + ExecuteContext，标准输出
// 接到 WS 会话。kubeconfig 从 store 物化成本地临时文件（--kubeconfig 指定，
// 不碰用户的 ~/.kube/config），会话结束即删。
//
// 与 execsess（容器终端）共用的协议约定：二进制帧 = 键盘输入 / 终端输出，
// 文本帧 = JSON 控制消息（这里的 resize 不影响非 TTY 的 kubectl 输出，忽略）。
// 区别在于 kubectl 是行式 REPL：Go 侧自带一个小型行编辑器（回显 / 退格 /
// 历史），前台命令运行期间输入直通其 stdin（exec -i、apply -f - 可用）。
//
// 交互流命令（exec -it / logs -f）照常走 IOStreams：Ctrl+C 一律由网关拦截
// 转成 context 取消，不会转发为远程 TTY 的 SIGINT——退出交互式 exec 请输入
// exit。依赖外部进程的子命令（edit/diff/port-forward/proxy/plugin）在分发
// 前拦截，见 runner.go。
package ksh

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/gorilla/websocket"
)

const (
	idleTimeout = 15 * time.Minute
	maxSessions = 8
	// kubectlLibVersion 展示在横幅里（与 go.mod 的 k8s.io/* 大版本对应）。
	kubectlLibVersion = "v0.37"
)

// Session 一条 REPL 会话。
type Session struct {
	ID        string
	ClusterID int64
	Cluster   string

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	out        *wsWriter
	liner      *liner
	kubeconfig string // 临时 kubeconfig 路径，会话关闭即删

	mu       sync.Mutex
	lastUsed time.Time
	running  *runningCmd // 非空 = 前台命令运行中（定义见 runner.go）
}

// Manager 会话表 + 本地 WS 网关。
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session

	ln     net.Listener
	server *http.Server
}

func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}}
}

// Start 在 127.0.0.1 随机端口上启动 WS 网关，返回端口。
func (m *Manager) Start() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	m.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/ksh", m.handleWS)
	m.server = &http.Server{Handler: mux}
	go m.server.Serve(ln)

	go m.reaper()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Stop 关闭网关与全部会话。
func (m *Manager) Stop() {
	if m.server != nil {
		m.server.Close()
	}
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Close()
	}
}

// StopCluster 关闭某集群的全部 kubectl 会话（删除集群时调用）。
func (m *Manager) StopCluster(clusterID int64) {
	m.mu.Lock()
	var dead []*Session
	for id, s := range m.sessions {
		if s.ClusterID == clusterID {
			dead = append(dead, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, s := range dead {
		s.Close()
	}
}

func (m *Manager) reaper() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		m.mu.Lock()
		var dead []*Session
		for id, s := range m.sessions {
			s.mu.Lock()
			idle := now.Sub(s.lastUsed) > idleTimeout
			s.mu.Unlock()
			if idle {
				dead = append(dead, s)
				delete(m.sessions, id)
			}
		}
		m.mu.Unlock()
		for _, s := range dead {
			s.Close()
		}
	}
}

// WSInfo open 接口返回给前端的信息。
type WSInfo struct {
	Session string `json:"session"`
	Cluster string `json:"cluster"`
	// WSTemplate 形如 ws://127.0.0.1:PORT/ws/ksh?token=
	WSTemplate string `json:"ws_url"`
}

// Open 校验 kubeconfig、物化临时文件并登记会话（与 execsess.Open 对齐）。
func (m *Manager) Open(clusterID int64, clusterName string, kubeconfigLoader func() (string, error)) (*WSInfo, int, error) {
	m.mu.Lock()
	if len(m.sessions) >= maxSessions {
		m.mu.Unlock()
		return nil, http.StatusTooManyRequests, fmt.Errorf("kubectl 会话数已达上限（%d），请关闭其他终端后重试", maxSessions)
	}
	m.mu.Unlock()

	kubeconfigYAML, err := kubeconfigLoader()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("读取集群配置失败：%v", err)
	}
	// 提前校验：坏 kubeconfig 不该等用户敲下第一条命令才报错
	if _, err := clientcmd.Load([]byte(kubeconfigYAML)); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("kubeconfig 无效：%v", err)
	}

	f, err := os.CreateTemp("", "sailor-ksh-kubeconfig-*")
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("创建临时 kubeconfig 失败：%v", err)
	}
	if _, err := f.WriteString(kubeconfigYAML); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, http.StatusInternalServerError, fmt.Errorf("写入临时 kubeconfig 失败：%v", err)
	}
	f.Close()
	// CreateTemp 是 0600，显式再压一次以防 umask 差异
	_ = os.Chmod(f.Name(), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		ClusterID: clusterID, Cluster: clusterName,
		ctx: ctx, cancel: cancel,
		out:        &wsWriter{},
		kubeconfig: f.Name(),
		lastUsed:   time.Now(),
	}
	s.ID = newToken()
	s.liner = newLiner(promptFor(clusterName), s.out, s.submitLine, s.Close)

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	port := 0
	if m.ln != nil {
		port = m.ln.Addr().(*net.TCPAddr).Port
	}
	return &WSInfo{
		Session:    s.ID,
		Cluster:    clusterName,
		WSTemplate: fmt.Sprintf("ws://127.0.0.1:%d/ws/ksh?token=", port),
	}, 0, nil
}

func promptFor(cluster string) string {
	return "\x1b[1;36mkubectl@" + cluster + "\x1b[0m \x1b[1;33m$\x1b[0m "
}

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Close 幂等：取消会话与前台命令、删除临时 kubeconfig、通知前端并断开。
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.kubeconfig != "" {
			os.Remove(s.kubeconfig)
		}
		s.out.sendExit()
	})
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// ─── WS 网关 ──────────────────────────────────────────────────

func (m *Manager) handleWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	m.mu.Lock()
	s, ok := m.sessions[token]
	if ok {
		delete(m.sessions, token) // 一次性：连接建立后不再复用 token
	}
	m.mu.Unlock()
	if !ok {
		http.Error(w, "kubectl 会话不存在或已过期", http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// 会话从这一刻起由 WS 连接托管：断开即 Close（幂等）
	s.attach(conn)

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if msgType == websocket.TextMessage {
			continue // resize 控制帧：kubectl 输出不依赖 TTY 宽度，忽略
		}
		if len(data) > 0 {
			s.handleInput(data)
		}
	}
	s.Close()
}

// attach 绑定连接、打印横幅与提示符。
func (s *Session) attach(conn *websocket.Conn) {
	s.out.bind(conn)
	s.touch()
	fmt.Fprintf(s.out, "\x1b[1;36mSailor 内置 kubectl\x1b[0m（库 %s）\r\n", kubectlLibVersion)
	fmt.Fprintf(s.out, "集群：%s\r\n", s.Cluster)
	fmt.Fprintf(s.out, "直接输入子命令（kubectl 前缀可省略）· ↑/↓ 历史 · Ctrl+C 中断 · exit 退出\r\n")
	fmt.Fprintf(s.out, "不支持：edit / diff / port-forward / proxy / plugin（依赖外部进程）\r\n")
	s.liner.showPrompt()
}

// handleInput 分派键盘输入：前台命令运行中 → Ctrl+C 取消 / Ctrl+D 发 EOF /
// 其余直通 stdin（仅声明要读 stdin 的命令，见 wantStdin）；空闲 → 行编辑器。
func (s *Session) handleInput(chunk []byte) {
	s.touch()
	s.mu.Lock()
	run := s.running
	s.mu.Unlock()

	if run != nil {
		if len(chunk) == 1 {
			switch chunk[0] {
			case 0x03: // Ctrl+C：一律本地取消，不转发为远程 SIGINT
				run.cancel()
				fmt.Fprint(s.out, "\r\n\x1b[33m^C（已中断当前命令）\x1b[0m\r\n")
				return
			case 0x04: // Ctrl+D：给前台命令发 EOF（apply -f - 收尾）
				run.closeStdin()
				return
			}
		}
		run.feedStdin(chunk)
		return
	}
	s.liner.feed(chunk)
}

// ─── 输出通道：kubectl / 行编辑器并发写 → WS 二进制帧 ─────────

type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
	// lastNL 上次输出是否以换行结尾：决定提示符前要不要补一个回车
	lastNL bool
}

func (w *wsWriter) bind(conn *websocket.Conn) {
	w.mu.Lock()
	w.conn = conn
	w.lastNL = true
	w.mu.Unlock()
}

func (w *wsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return 0, io.ErrClosedPipe
	}
	if err := w.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	if n := len(p); n > 0 {
		w.lastNL = p[n-1] == '\n' || p[n-1] == '\r'
	}
	return len(p), nil
}

// ensureNewline 输出提示符前调用：kubectl 的输出偶尔不带尾换行。
func (w *wsWriter) ensureNewline() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.lastNL {
		w.conn.WriteMessage(websocket.BinaryMessage, []byte("\r\n"))
		w.lastNL = true
	}
}

// sendExit 会话结束时发 {type:"exit"} 文本帧并断开连接。
func (w *wsWriter) sendExit() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return
	}
	msg, _ := json.Marshal(map[string]string{"type": "exit"})
	w.conn.WriteMessage(websocket.TextMessage, msg)
	// 给前端一点时间把最后的数据刷出去
	time.Sleep(200 * time.Millisecond)
	w.conn.Close()
	w.conn = nil
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  8192,
	WriteBufferSize: 8192,
	// 桌面端网关只绑 127.0.0.1，来源校验放宽（webview 的 origin 是自定义 scheme）
	CheckOrigin: func(r *http.Request) bool { return true },
}
