package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sailor/internal/execsess"
	"sailor/internal/k8sx"
	"sailor/internal/ksh"
	"sailor/internal/metrics"

	"sailor/internal/resops"
	"sailor/internal/store"
	"sailor/internal/syncer"
)

// Server 汇集所有依赖并装配路由。
type Server struct {
	Store   *store.Store
	Pool    *k8sx.Pool
	Syncer  *syncer.Manager
	Agg     *metrics.Aggregator
	Exec    *execsess.Manager
	Ksh     *ksh.Manager
	Tpl     *Renderer
	Assets  fs.FS // 可选：静态资源（供独立测试；Wails 里由 asset server 直接服务）
	WSPort  int
	Version string
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// Handler 装配全部路由（URL 与 Django 版对齐）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	if s.Assets != nil {
		fileServer := http.FileServer(http.FS(s.Assets))
		mux.Handle("GET /static/", fileServer)
		mux.Handle("GET /favicon.svg", fileServer)
	}

	// ─── 页面 ─────────────────────────────────────────────
	mux.HandleFunc("GET /{$}", s.pageDashboard)
	mux.HandleFunc("GET /clusters/", s.pageClusterList)
	mux.HandleFunc("GET /clusters/add/{$}", s.pageClusterAdd)
	mux.HandleFunc("GET /clusters/{id}/", s.pageClusterDetail)
	mux.HandleFunc("GET /clusters/{id}/edit/", s.pageClusterEdit)
	mux.HandleFunc("POST /clusters/{id}/edit/", s.pageClusterEdit)
	mux.HandleFunc("GET /clusters/{id}/select/", s.clusterSelect)
	mux.HandleFunc("GET /clusters/{id}/nodes/manage/", s.pageNodes)
	mux.HandleFunc("GET /clusters/{id}/node/{name}/", s.pageNodeDetail)
	mux.HandleFunc("GET /resources/{id}/{kind}/", s.pageResourceList)

	// ─── 集群表单 / 操作（动作端点一律 {$} 精确匹配，避免子树歧义）───
	mux.HandleFunc("POST /clusters/add/{$}", s.clusterAdd)
	mux.HandleFunc("POST /clusters/{id}/delete/{$}", s.clusterDelete)
	mux.HandleFunc("POST /clusters/{id}/refresh/{$}", s.clusterRefresh)
	mux.HandleFunc("POST /clusters/{id}/prometheus/{$}", s.clusterUpdatePrometheus)

	// ─── 集群 API ────────────────────────────────────────
	mux.HandleFunc("GET /clusters/{id}/nodes/", s.clusterNodesAPI)
	mux.HandleFunc("GET /clusters/{id}/metrics/{$}", s.clusterMetricsAPI)
	mux.HandleFunc("GET /clusters/{id}/debug-prom/{$}", s.clusterDebugPromAPI)
	mux.HandleFunc("GET /clusters/{id}/node/{name}/info/{$}", s.nodeInfoAPI)
	mux.HandleFunc("POST /clusters/{id}/node/{name}/cordon/{$}", s.nodeAction(nodeCordon))
	mux.HandleFunc("POST /clusters/{id}/node/{name}/uncordon/{$}", s.nodeAction(nodeUncordon))
	mux.HandleFunc("POST /clusters/{id}/node/{name}/drain/{$}", s.nodeAction(nodeDrain))
	mux.HandleFunc("POST /clusters/{id}/node/{name}/delete/{$}", s.nodeAction(nodeDelete))
	mux.HandleFunc("GET /clusters/{id}/pod/{ns}/{pod}/logs/{$}", s.podLogsAPI)

	// ─── 资源 API ────────────────────────────────────────
	mux.HandleFunc("GET /resources/{id}/api/{kind}/", s.resourceListAPI)
	mux.HandleFunc("GET /resources/{id}/api/{kind}/{ns}/{name}/describe/{$}", s.resourceDescribeAPI)
	mux.HandleFunc("GET /resources/{id}/api/{kind}/{ns}/{name}/revisions/{$}", s.resourceRevisionsAPI)
	mux.HandleFunc("POST /resources/{id}/namespaces/create/{$}", s.namespaceCreate)
	mux.HandleFunc("POST /resources/{id}/namespaces/{name}/delete/{$}", s.namespaceDelete)
	mux.HandleFunc("POST /resources/{id}/namespaces/{name}/force-finalize/{$}", s.namespaceForceFinalize)
	// kind 枚举成字面量：避免 {kind} 万用段与 "namespaces"/"delete" 等字面量段
	// 在 ServeMux 里产生无法判定的模式冲突
	for _, kind := range []string{"deployments", "statefulsets", "daemonsets"} {
		mux.HandleFunc("POST /resources/{id}/"+kind+"/{ns}/{name}/restart/{$}", s.resourceRestart)
		mux.HandleFunc("POST /resources/{id}/"+kind+"/{ns}/{name}/rollback/{$}", s.resourceRollback)
	}
	for _, kind := range []string{"deployments", "statefulsets"} {
		mux.HandleFunc("POST /resources/{id}/"+kind+"/{ns}/{name}/scale/{$}", s.resourceScale)
	}
	mux.HandleFunc("POST /resources/{id}/cronjobs/{ns}/{name}/suspend/{$}", s.cronjobSuspend)
	mux.HandleFunc("POST /resources/{id}/cronjobs/{ns}/{name}/trigger/{$}", s.cronjobTrigger)
	mux.HandleFunc("POST /resources/{id}/scaledobjects/{ns}/{name}/pause/{$}", s.scaledPause("scaledobject"))
	mux.HandleFunc("POST /resources/{id}/scaledjobs/{ns}/{name}/pause/{$}", s.scaledPause("scaledjob"))
	mux.HandleFunc("GET /resources/{id}/pods/{ns}/{pod}/logs/{$}", s.podLogsAPI)
	mux.HandleFunc("POST /resources/{id}/pods/{ns}/{pod}/exec/open/{$}", s.podExecOpen)
	mux.HandleFunc("POST /resources/{id}/pods/{ns}/{pod}/exec/close/{$}", s.podExecClose)
	mux.HandleFunc("POST /resources/{id}/pods/{ns}/{pod}/debug/add/{$}", s.podDebugAdd)
	mux.HandleFunc("POST /resources/{id}/ksh/open/{$}", s.kshOpen)
	mux.HandleFunc("POST /resources/{id}/ksh/attach/{$}", s.kshAttach)
	mux.HandleFunc("POST /resources/{id}/ksh/close/{$}", s.kshClose)
	mux.HandleFunc("GET /resources/{id}/yaml/{kind}/{name}/", s.resourceYAML)      // cluster-scoped
	mux.HandleFunc("GET /resources/{id}/yaml/{kind}/{ns}/{name}/", s.resourceYAML) // namespaced
	mux.HandleFunc("POST /resources/{id}/apply/{$}", s.resourceApplyAPI)
	mux.HandleFunc("POST /resources/{id}/validate/{$}", s.resourceValidateAPI)
	for _, kind := range []string{"namespace", "pod", "deployment", "statefulset", "daemonset",
		"service", "ingress", "configmap", "secret", "persistentvolumeclaim", "job", "cronjob",
		"hpa", "scaledobject", "scaledjob"} {
		mux.HandleFunc("POST /resources/{id}/delete/"+kind+"/{ns}/{name}/{$}", s.resourceDelete)
		mux.HandleFunc("POST /resources/{id}/delete/"+kind+"/{name}/{$}", s.resourceDelete)
	}

	return s.recoverWrap(mux)
}

func (s *Server) recoverWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[webui] panic %s %s: %v", r.Method, r.URL.Path, rec)
				if isAJAX(r) {
					JSONError(w, 500, fmt.Sprintf("服务器内部错误：%v", rec))
				} else {
					http.Error(w, "Internal Server Error", 500)
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ─── 通用工具 ─────────────────────────────────────────────

func isAJAX(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest" ||
		strings.Contains(r.Header.Get("Accept"), "application/json") ||
		strings.Contains(r.Header.Get("Content-Type"), "application/json")
}

func pathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

// pageData 组装带全局上下文的 PageData。
func (s *Server) pageData(r *http.Request, pageID, title string, content interface{}) *PageData {
	var active interface{}
	if c := s.Store.ActiveCluster(); c != nil {
		active = c
	}
	return &PageData{
		PageID:        pageID,
		Title:         title,
		SidebarActive: sidebarActive(r.URL.Path),
		CurrentPath:   r.URL.Path,
		Flash:         flashFromQuery(r),
		AllClusters:   s.Store.ListClusters(),
		ActiveCluster: active,
		WSPort:        s.WSPort,
		Version:       s.Version,
		Content:       content,
		KEDAInstalled: s.kedaInstalled(),
	}
}

// sidebarActive 从请求路径推侧栏高亮段：资源页取 kind 复数；集群详情 /
// 编辑归"集群列表"；节点管理与节点详情归"节点管理"。
func sidebarActive(path string) string {
	if path == "/" {
		return "dashboard"
	}
	if rest, ok := strings.CutPrefix(path, "/clusters/"); ok {
		// rest 形如 "{id}/nodes/manage/" 或 "{id}/node/{name}/"
		if strings.Contains(rest, "/nodes/") || strings.Contains(rest, "/node/") {
			return "nodes"
		}
		return "clusters"
	}
	if rest, ok := strings.CutPrefix(path, "/resources/"); ok {
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) >= 2 {
			return parts[1] // /resources/{id}/{kind}/…
		}
	}
	return ""
}

// kedaInstalled 读取同步器写入的 KEDA 安装标记，决定侧栏 KEDA 入口是否显示。
// 标记缺失（首轮同步还没探测到 KEDA 类资源）时保持显示，探测收敛后：
// 未安装 → 收起菜单（切到已安装的集群会自动恢复）。
func (s *Server) kedaInstalled() bool {
	c := s.Store.ActiveCluster()
	if c == nil {
		return true
	}
	marker := s.Store.GetCache(c.ID, "keda")
	if marker == nil {
		return true
	}
	var m struct {
		Installed bool `json:"installed"`
	}
	if json.Unmarshal(marker.Data, &m) != nil {
		return true
	}
	return m.Installed
}

// renderPage 渲染失败时兜底 500。
func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, page, pageID, title string, content interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.Tpl.Render(w, page, s.pageData(r, pageID, title, content)); err != nil {
		log.Printf("[webui] render %s failed: %v", page, err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// clusterCtx 取集群 + kubeconfig 懒加载器。
func (s *Server) clusterCtx(w http.ResponseWriter, r *http.Request, idParam string) (*store.Cluster, func() (string, error), bool) {
	id, err := strconv.ParseInt(idParam, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil, false
	}
	c, err := s.Store.GetCluster(id)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil, false
	}
	return c, func() (string, error) { return c.GetKubeconfig(s.Store.AEAD()) }, true
}

// opsFor 返回资源操作集。
func (s *Server) opsFor(clusterID int64, loader func() (string, error)) *resops.Ops {
	return &resops.Ops{ClusterID: clusterID, Pool: s.Pool, Loader: loader}
}

// ─── 集群后台信息刷新（对应 _refresh_cluster_info）───────────

func (s *Server) refreshClusterInfo(clusterID int64) {
	c, err := s.Store.GetCluster(clusterID)
	if err != nil {
		return
	}
	loader := func() (string, error) { return c.GetKubeconfig(s.Store.AEAD()) }
	cl, err := s.Pool.Refresh(clusterID, loader)
	if err != nil {
		c.Status = "offline"
		_ = s.Store.UpdateCluster(c)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 版本
	if v, err := cl.Typed.Discovery().ServerVersion(); err == nil {
		c.K8sVersion = v.GitVersion
	}
	// 节点
	if nodeList, err := cl.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		c.NodeCount = len(nodeList.Items)
		osSet := map[string]bool{}
		for i := range nodeList.Items {
			osSet[nodeList.Items[i].Status.NodeInfo.OSImage] = true
		}
		var oss []string
		for os := range osSet {
			oss = append(oss, os)
		}
		sortStrings(oss)
		c.OSInfo = strings.Join(oss, " / ")
	}
	c.Status = "online"
	_ = s.Store.UpdateCluster(c)

	if fresh, err := s.Store.GetCluster(clusterID); err == nil {
		s.Syncer.StartForCluster(fresh)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
