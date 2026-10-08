package ksh

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 离线可用的最小 kubeconfig：server 指向不存在的端口也没关系，
// 测试只跑 version --client（不连网）。
const fakeKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:59999
  name: fake
contexts:
- context:
    cluster: fake
    user: fake
  name: fake
current-context: fake
users:
- name: fake
  user:
    token: abc
`

// readUntil 读 WS 直到输出中出现 want 或超时。
func readUntil(t *testing.T, conn *websocket.Conn, want string) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var sb strings.Builder
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("读 WS 失败（等待 %q）: %v，已收内容:\n%s", want, err, sb.String())
		}
		if msgType == websocket.TextMessage {
			// 控制帧不算输出
			continue
		}
		sb.Write(data)
		if strings.Contains(sb.String(), want) {
			return sb.String()
		}
	}
}

// TestSessionRoundtrip 走完整链路：open → WS 拨号 → 横幅/提示符 →
// 行编辑回显 → kubectl 库真执行 version --client → exit 退出帧。
func TestSessionRoundtrip(t *testing.T) {
	m := NewManager()
	port, err := m.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	info, status, err := m.Open(1, "e2e", func() (string, error) { return fakeKubeconfig, nil })
	if err != nil {
		t.Fatalf("Open: %v (status %d)", err, status)
	}

	wsURL := "ws://127.0.0.1:" + strconv.Itoa(port) + "/ws/ksh?token=" + url.QueryEscape(info.Session)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{})
	if err != nil {
		t.Fatalf("拨号: %v", err)
	}
	defer conn.Close()

	// 1) 横幅 + 提示符
	banner := readUntil(t, conn, "kubectl@e2e")
	if !strings.Contains(banner, "Sailor 内置 kubectl") {
		t.Errorf("横幅缺失: %q", banner)
	}

	// 2) 敲命令：回显 + version --client 真实输出（不连网）
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("version --client\r")); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, conn, "Client Version")
	if !strings.Contains(out, "version --client") {
		t.Errorf("行编辑回显缺失: %q", out)
	}
	// 命令结束后应回到提示符（提示符带 ANSI 颜色码，只匹配稳定前缀）
	readUntil(t, conn, "kubectl@e2e")

	// 3) exit：应收到 {type:"exit"} 文本帧或连接关闭
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	sawExit := false
	for i := 0; i < 20; i++ {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			break // 连接被服务端关闭，同样视为正常退出
		}
		if msgType == websocket.TextMessage && strings.Contains(string(data), `"exit"`) {
			sawExit = true
			break
		}
	}
	if !sawExit {
		t.Error("exit 后未收到 exit 控制帧（且连接未关闭）")
	}
}

// TestOpenRejectsBadKubeconfig 坏 kubeconfig 在 open 阶段就报 400。
func TestOpenRejectsBadKubeconfig(t *testing.T) {
	m := NewManager()
	_, status, err := m.Open(2, "bad", func() (string, error) { return "{{{{not yaml", nil })
	if err == nil {
		t.Fatal("坏 kubeconfig 应当报错")
	}
	if status != 400 {
		t.Errorf("status = %d, want 400", status)
	}
}
