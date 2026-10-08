// Package ksh：内置 kubectl 终端（REPL）。
//
// 不 spawn 外部 kubectl 二进制，而是进程内引入 k8s.io/kubectl 的命令树：
// 每读入一行命令就 NewKubectlCommand + SetArgs + ExecuteContext，标准输出
// 接到 WS 会话。kubeconfig 从 store 物化成本地临时文件（--kubeconfig 指定，
// 不碰用户的 ~/.kube/config），会话结束即删。
//
// 会话与 WS 连接解耦：抽屉收起、页面跳转、切换集群都会断开 WS，但会话
// （行编辑状态 / 历史 / 前台命令）在 Go 侧继续存活，断连期间的输出写入
// 256KB 滚动缓冲，重连时整段回放 —— 前端语义是「恢复现场」而非重开。
// 二进制帧 = 键盘输入 / 终端输出，文本帧 = JSON 控制消息（resize 不影响
// 非 TTY 的 kubectl 输出，忽略）。
//
// Go 侧自带一个小型行编辑器（回显 / 退格 / 历史），前台命令运行期间输入
// 直通其 stdin（exec -i、apply -f - 可用）。Ctrl+C 一律由网关拦截转成
// context 取消，不会转发为远程 TTY 的 SIGINT——退出交互式 exec 请输入
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
	// ringCap 断连期间输出的滚动缓冲上限；超限丢最旧的内容（回放会缺头）。
	ringCap = 256 << 10
	// kubectlLibVersion 展示在横幅里（与 go.mod 的 k8s.io/* 大版本对应）。
	kubectlLibVersion = "v0.37"
)

// Session 一条 REPL 会话。生命周期独立于任何一条 WS 连接。
type Session struct {
	ID        string
	ClusterID int64
	Cluster   string

	m         *Manager
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	out        *wsWriter
	liner      *liner
	kubeconfig string // 临时 kubeconfig 路径，会话关闭即删
	booted     bool   // 已打印过横幅（重连不重复）

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

// Close 按 ID 显式关闭会话（前端点抽屉关闭按钮）。返回是否存在。
func (m *Manager) Close(id string) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if ok {
		s.Close()
	}
	return ok
}

// Attach 取既有会话的连接信息（页面跳转后恢复）。校验会话存活且属于该集群。
func (m *Manager) Attach(clusterID int64, id string) (*WSInfo, int, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("kubectl 会话不存在或已过期")
	}
	if s.ClusterID != clusterID {
		return nil, http.StatusNotFound, fmt.Errorf("kubectl 会话不属于该集群")
	}
	port := 0
	if m.ln != nil {
		port = m.ln.Addr().(*net.TCPAddr).Port
	}
	return &WSInfo{
		Session:    s.ID,
		Cluster:    s.Cluster,
		WSTemplate: fmt.Sprintf("ws://127.0.0.1:%d/ws/ksh?token=", port),
	}, 0, nil
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
			// 断连后无人交互也持续产出输出的会话（logs -f）视为活跃：
			// touch 同时发生在输入与输出两条路径上
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

// WSInfo open / attach 接口返回给前端的信息。
type WSInfo struct {
	Session string `json:"session"`
	Cluster string `json:"cluster"`
	// WSTemplate 形如 ws://127.0.0.1:PORT/ws/ksh?token=
	WSTemplate string `json:"ws_url"`
}

// Open 校验 kubeconfig、物化临时文件并登记会话。
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
		m:   m,
		ctx: ctx, cancel: cancel,
		out:        &wsWriter{},
		kubeconfig: f.Name(),
		lastUsed:   time.Now(),
	}
	s.ID = newToken()
	s.liner = newLiner(promptFor(clusterName), s.out, s.submitLine, s.Close)
	// 输出也算活跃：断连后持续产出的会话（logs -f）不该被空闲回收
	s.out.onTouch = s.touch

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	info, _, err := m.Attach(clusterID, s.ID)
	if err != nil { // 理论不可达，仅防御
		s.Close()
		return nil, http.StatusInternalServerError, err
	}
	return info, 0, nil
}

func promptFor(cluster string) string {
	return "\x1b[1;36mkubectl@" + cluster + "\x1b[0m \x1b[1;33m$\x1b[0m "
}

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Close 幂等：注销会话、取消前台命令、删除临时 kubeconfig、通知并断开连接。
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		if s.m != nil {
			s.m.mu.Lock()
			delete(s.m.sessions, s.ID)
			s.m.mu.Unlock()
		}
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
	id := r.URL.Query().Get("token")
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		http.Error(w, "kubectl 会话不存在或已过期", http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	// attach 会接管既有连接（页面跳转时旧连接可能还没断干净）
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
	// 连接断开 ≠ 会话结束：detach 保留会话，输出转入滚动缓冲等待重连
	s.detach(conn)
	conn.Close()
}

// attach 绑定连接。首次打印横幅 + 提示符；重连完全无感：滚动缓冲已在
// rebind 里整段回放，终端画面停在断开前的原样（提示符/半行输入都在缓冲
// 尾部），不再输出任何“已恢复”类提示，输入从原处继续。
func (s *Session) attach(conn *websocket.Conn) {
	s.out.rebind(conn)
	s.touch()
	if s.booted {
		return
	}
	s.booted = true
	fmt.Fprintf(s.out, "\x1b[1;36mSailor 内置 kubectl\x1b[0m（库 %s）\r\n", kubectlLibVersion)
	fmt.Fprintf(s.out, "集群：%s\r\n", s.Cluster)
	fmt.Fprintf(s.out, "直接输入子命令（kubectl 前缀可省略）· ↑/↓ 历史 · Ctrl+C 中断 · exit 退出\r\n")
	fmt.Fprintf(s.out, "不支持：edit / diff / port-forward / proxy / plugin（依赖外部进程）\r\n")
	s.liner.showPrompt()
}

// detach 断开指定连接（按身份比对：接管后旧连接的断开不得拆掉新连接）。
func (s *Session) detach(conn *websocket.Conn) {
	s.out.unbind(conn)
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

// ─── 输出通道：kubectl / 行编辑器并发写 → 滚动缓冲 + WS 二进制帧 ──

// wsWriter 输出永远追加进滚动缓冲（断连期间不丢内容），有连接时同时转发。
type wsWriter struct {
	mu      sync.Mutex
	conn    *websocket.Conn
	ring    []byte
	onTouch func() // 有输出时回调（会话记活跃；锁外调用防交叉死锁）

	// lastNL 上次输出是否以换行结尾：决定提示符前要不要补一个回车
	lastNL bool
}

// rebind 绑定新连接（接管旧连接）：先把缓冲整段回放给新连接，再开始实时
// 转发 —— 两者在同一把锁内完成，回放与实时输出不会交错。
func (w *wsWriter) rebind(conn *websocket.Conn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil {
		w.conn.Close()
	}
	if len(w.ring) > 0 {
		conn.WriteMessage(websocket.BinaryMessage, w.ring)
	}
	w.conn = conn
}

// unbind 按身份断开：只在与 conn 匹配时才解绑（防接管竞态）。
func (w *wsWriter) unbind(conn *websocket.Conn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == conn {
		w.conn.Close()
		w.conn = nil
	}
}

func (w *wsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.ring = append(w.ring, p...)
	if over := len(w.ring) - ringCap; over > 0 {
		w.ring = append([]byte{}, w.ring[over:]...)
	}
	if n := len(p); n > 0 {
		w.lastNL = p[n-1] == '\n' || p[n-1] == '\r'
	}
	conn := w.conn
	var err error
	if conn != nil {
		err = conn.WriteMessage(websocket.BinaryMessage, p)
	}
	w.mu.Unlock()

	if w.onTouch != nil {
		w.onTouch()
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// ensureNewline 输出提示符前调用：kubectl 的输出偶尔不带尾换行。
func (w *wsWriter) ensureNewline() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastNL {
		return
	}
	if w.conn != nil {
		w.conn.WriteMessage(websocket.BinaryMessage, []byte("\r\n"))
	}
	w.ring = append(w.ring, '\r', '\n')
	w.lastNL = true
}

// sendExit 会话结束时发 {type:"exit"} 文本帧并断开当前连接。
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

// 引用 io：wsWriter 需要满足 io.Writer
var _ io.Writer = (*wsWriter)(nil)
