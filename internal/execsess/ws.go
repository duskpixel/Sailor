// gorilla/websocket 薄封装 + remotecommand executor 构造。
package execsess

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	wsTextMessage   = websocket.TextMessage
	wsBinaryMessage = websocket.BinaryMessage
)

type wsUpgradeFunc func(w http.ResponseWriter, r *http.Request) (wsConn, error)

type wsConn interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	Close() error
}

// gorillaConn 已满足 wsConn。
type wsWrapper struct{ up websocket.Upgrader }

func newWSWrapper() *wsWrapper {
	return &wsWrapper{up: websocket.Upgrader{
		ReadBufferSize:  8192,
		WriteBufferSize: 8192,
		// 桌面端网关只绑 127.0.0.1，来源校验放宽（webview 的 origin 是自定义 scheme）
		CheckOrigin: func(r *http.Request) bool { return true },
	}}
}

func (w *wsWrapper) Upgrade(rw http.ResponseWriter, r *http.Request) (wsConn, error) {
	return w.up.Upgrade(rw, r, nil)
}

// newExecutor 构建 K8s exec 执行器：WebSocket 优先（1.30+ 服务端 GA），
// SPDY 回退（老版本集群）。
func newExecutor(cfg *rest.Config, execURL string) (remotecommand.Executor, error) {
	u, err := url.Parse(execURL)
	if err != nil {
		return nil, fmt.Errorf("exec URL 无效：%v", err)
	}
	websocketExec, wsErr := remotecommand.NewWebSocketExecutor(cfg, "GET", execURL)
	spdyExec, spdyErr := remotecommand.NewSPDYExecutor(cfg, "POST", u)
	if wsErr != nil && spdyErr != nil {
		return nil, fmt.Errorf("创建 exec 执行器失败：ws=%v spdy=%v", wsErr, spdyErr)
	}
	if wsErr != nil {
		return spdyExec, nil
	}
	if spdyErr != nil {
		return websocketExec, nil
	}
	return remotecommand.NewFallbackExecutor(websocketExec, spdyExec, func(err error) bool {
		// WebSocket 升级被老版本 API Server 拒绝时回退 SPDY
		return true
	})
}
