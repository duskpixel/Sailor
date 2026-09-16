// Package execsess：Pod 容器终端会话（对应 Django 版 resources/pod_exec.py）。
//
// Django 版受 WSGI 限制，只能「服务端持有 K8s WebSocket + 浏览器 HTTP 轮询」，
// 输入到回显有一个轮询周期的延迟。桌面版直接升级：本地起一个 127.0.0.1
// WebSocket 网关，浏览器 xterm.js 直连，Go 侧用 client-go remotecommand
// （WebSocket 优先 / SPDY 回退）桥接到容器，实时双向无轮询。
//
// 安全约束保留两条仍然有效的：
//   - 会话 token 一次性签发（open 接口返回 ws_url 时带上），网关校验
//   - 空闲超时 + 总数上限，泄漏的会话不会一直占着 K8s 连接
package execsess

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"sailor/internal/k8sx"
	"sailor/internal/store"
)

const (
	idleTimeout = 5 * time.Minute
	maxSessions = 32
)

var shellCmd = []string{"/bin/sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}

// Session 一条到容器的 exec 流。
type Session struct {
	ID        string
	ClusterID int64
	Namespace string
	Pod       string
	Container string

	ctx    context.Context
	cancel context.CancelFunc

	stdin  io.Writer // 由 WS 网关写入
	stdout io.Reader // 由 WS 网关泵出
	sizes  *sizeQueue

	mu       sync.Mutex
	lastUsed time.Time
	done     chan struct{} // Stream 退出即关闭
	exitMsg  string
}

// Manager 会话表 + 本地 WS 网关。
type Manager struct {
	pool  *k8sx.Pool
	store *store.Store

	mu       sync.Mutex
	sessions map[string]*Session

	ln     net.Listener
	server *http.Server
}

func NewManager(pool *k8sx.Pool, st *store.Store) *Manager {
	return &Manager{pool: pool, store: st, sessions: map[string]*Session{}}
}

// Start 启动 127.0.0.1 随机端口上的 WS 网关。返回端口。
func (m *Manager) Start() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	m.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/exec", m.handleWS)
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
	m.mu.Unlock()
	for _, s := range sessions {
		s.Close()
	}
}

// StopCluster 关闭某集群的全部终端会话（删除集群时调用）。
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

// reaper 回收空闲会话（页面关掉后 WS 也会断，这里是兜底）。
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
	Session   string `json:"session"`
	Container string `json:"container"`
	// WSTemplate 形如 ws://127.0.0.1:PORT/ws/exec?token=
	WSTemplate string `json:"ws_url"`
}

// Open 建立到容器的 exec 流并注册会话。
func (m *Manager) Open(clusterID int64, kubeconfigLoader func() (string, error), namespace, podName, container string) (*WSInfo, int, error) {
	m.mu.Lock()
	if len(m.sessions) >= maxSessions {
		m.mu.Unlock()
		return nil, http.StatusTooManyRequests, fmt.Errorf("终端会话数已达上限（%d），请关闭其他终端后重试", maxSessions)
	}
	m.mu.Unlock()

	kubeconfigYAML, err := kubeconfigLoader()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("读取容器列表失败：%v", err)
	}
	cl, err := m.pool.Get(clusterID, func() (string, error) { return kubeconfigYAML, nil })
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("读取容器列表失败：%v", err)
	}

	// 容器名缺省取 spec.containers[0]（对应 Python open_session）
	if container == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pod, err := cl.Typed.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		cancel()
		if err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("读取容器列表失败：%v", err)
		}
		if len(pod.Spec.Containers) > 0 {
			container = pod.Spec.Containers[0].Name
		}
	}

	execURL := buildExecURL(cl.Config.Host, namespace, podName, container, shellCmd)
	executor, err := newExecutor(cl.Config, execURL)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("无法进入容器：%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	sizes := newSizeQueue()

	s := &Session{
		ClusterID: clusterID, Namespace: namespace, Pod: podName, Container: container,
		ctx: ctx, cancel: cancel, stdin: stdinWriter, stdout: stdoutReader, sizes: sizes,
		lastUsed: time.Now(), done: make(chan struct{}),
	}
	s.ID = newToken()

	go func() {
		defer close(s.done)
		defer stdoutWriter.Close()
		err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin:             stdinReader,
			Stdout:            stdoutWriter,
			Stderr:            nil, // TTY 模式下输出合流到 stdout
			Tty:               true,
			TerminalSizeQueue: sizes,
		})
		if err != nil && ctx.Err() == nil {
			s.mu.Lock()
			s.exitMsg = err.Error()
			s.mu.Unlock()
		}
	}()

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	port := 0
	if m.ln != nil {
		port = m.ln.Addr().(*net.TCPAddr).Port
	}
	return &WSInfo{
		Session:    s.ID,
		Container:  container,
		WSTemplate: fmt.Sprintf("ws://127.0.0.1:%d/ws/exec?token=", port),
	}, 0, nil
}

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Close 注销并关闭会话（幂等）。
func (s *Session) Close() {
	s.cancel()
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// sizeQueue 把 WS 上的 resize 消息转成 remotecommand 的 TerminalSizeQueue。
type sizeQueue struct {
	ch   chan remotecommand.TerminalSize
	once sync.Once
}

func newSizeQueue() *sizeQueue { return &sizeQueue{ch: make(chan remotecommand.TerminalSize, 4)} }

func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	s, ok := <-q.ch
	if !ok {
		return nil
	}
	return &s
}

func (q *sizeQueue) setSize(cols, rows uint16) {
	select {
	case q.ch <- remotecommand.TerminalSize{Width: cols, Height: rows}:
	default: // 队列满说明 K8s 侧还没消费，丢掉旧的
		select {
		case <-q.ch:
		default:
		}
		select {
		case q.ch <- remotecommand.TerminalSize{Width: cols, Height: rows}:
		default:
		}
	}
}

// ─── WS 网关 ──────────────────────────────────────────────────

// ws 协议：
//   - 前端 → 服务端：二进制帧 = 键盘输入；文本帧 = JSON 控制消息（resize）
//   - 服务端 → 前端：二进制帧 = 容器输出；文本帧 = {type:"meta"|"exit", ...}
type wsUpgrader = wsUpgradeFunc

func (m *Manager) handleWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	m.mu.Lock()
	s, ok := m.sessions[token]
	if ok {
		delete(m.sessions, token) // 一次性：连接建立后不再复用 token
	}
	m.mu.Unlock()
	if !ok {
		http.Error(w, "终端会话不存在或已过期", http.StatusNotFound)
		return
	}

	conn, err := upgrader(w, r)
	if err != nil {
		return
	}
	defer conn.Close()

	s.touch()
	go s.pumpOutput(conn)

	// 前端 → 容器
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		s.touch()
		if msgType == wsTextMessage {
			var ctrl struct {
				Type string `json:"type"`
				Cols int    `json:"cols"`
				Rows int    `json:"rows"`
			}
			if json.Unmarshal(data, &ctrl) == nil && ctrl.Type == "resize" {
				cols, rows := ctrl.Cols, ctrl.Rows
				if cols < 1 {
					cols = 80
				}
				if cols > 500 {
					cols = 500
				}
				if rows < 1 {
					rows = 24
				}
				if rows > 200 {
					rows = 200
				}
				s.sizes.setSize(uint16(cols), uint16(rows))
			}
			continue
		}
		if len(data) > 0 {
			s.stdin.Write(data)
		}
	}
	// WS 断开 → 关闭 exec 流
	s.Close()
}

// pumpOutput 容器 → 前端。
func (s *Session) pumpOutput(conn wsConn) {
	buf := make([]byte, 8192)
	for {
		n, err := s.stdout.Read(buf)
		if n > 0 {
			if werr := conn.WriteMessage(wsBinaryMessage, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			break
		}
	}
	s.mu.Lock()
	exitMsg := s.exitMsg
	s.mu.Unlock()
	// exec 流结束：通知前端
	type exitMsgT struct {
		Type string `json:"type"`
		Err  string `json:"err,omitempty"`
	}
	msg := exitMsgT{Type: "exit"}
	if exitMsg != "" {
		msg.Err = exitMsg
	}
	b, _ := json.Marshal(msg)
	conn.WriteMessage(wsTextMessage, b)
	// 给前端一点时间把最后的数据刷出去
	time.Sleep(200 * time.Millisecond)
	conn.Close()
}

// ─── gorilla/websocket 薄封装（便于单测 mock）────────────────

var upgraderImpl *wsWrapper

func init() { upgraderImpl = newWSWrapper() }

func upgrader(w http.ResponseWriter, r *http.Request) (wsConn, error) {
	return upgraderImpl.Upgrade(w, r)
}

// buildExecURL 拼 K8s exec 端点 URL。
func buildExecURL(apiHost, namespace, pod, container string, command []string) string {
	u := url.URL{
		Scheme: "https",
		Host:   stripScheme(apiHost),
		Path:   fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/exec", namespace, pod),
	}
	q := url.Values{}
	for _, c := range command {
		q.Add("command", c)
	}
	q.Set("container", container)
	q.Set("stdin", "true")
	q.Set("stdout", "true")
	q.Set("stderr", "true")
	q.Set("tty", "true")
	u.RawQuery = q.Encode()
	return u.String()
}

func stripScheme(host string) string {
	for _, p := range []string{"https://", "http://"} {
		if hasPrefix(host, p) {
			return host[len(p):]
		}
	}
	return host
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// 引用 scheme 包：client-go executor 需要已注册的核心类型。
var _ = scheme.Scheme
var _ corev1.Pod
var _ = rest.Config{}
