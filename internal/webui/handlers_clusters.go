// 集群管理 handlers：页面、表单、集群 API、节点操作、日志与指标。
// 对应 Django clusters/views.py 与 dashboard/views.py。
package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"sailor/internal/metrics"
	"sailor/internal/store"
	sigsyaml "sigs.k8s.io/yaml"
)

// ─── 页面 ─────────────────────────────────────────────────────

type dashboardContent struct{}

func (s *Server) pageDashboard(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "dashboard", "dashboard", "仪表盘", &dashboardContent{})
}

type clusterListContent struct{ Clusters []*store.Cluster }

func (s *Server) pageClusterList(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "cluster_list", "", "集群列表", &clusterListContent{Clusters: s.Store.ListClusters()})
}

func (s *Server) pageClusterAdd(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "cluster_add", "", "导入集群", &clusterFormContent{})
}

type clusterFormContent struct{ Cluster *store.Cluster }

func (s *Server) pageClusterDetail(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.renderPage(w, r, "cluster_detail", "", c.Display(), &clusterFormContent{Cluster: c})
}

func (s *Server) pageClusterEdit(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		s.handleClusterEdit(w, r, c)
		return
	}
	s.renderPage(w, r, "cluster_edit", "", "编辑集群", &clusterFormContent{Cluster: c})
}

func (s *Server) pageNodes(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.renderPage(w, r, "nodes", "", "节点管理", &clusterFormContent{Cluster: c})
}

type nodeDetailContent struct {
	Cluster  *store.Cluster
	NodeName string
}

func (s *Server) pageNodeDetail(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.renderPage(w, r, "node_detail", "", "节点详情", &nodeDetailContent{Cluster: c, NodeName: r.PathValue("name")})
}

// ─── 集群表单 ─────────────────────────────────────────────────

// parseKubeconfig 校验 kubeconfig YAML 并返回 server 地址。
func parseKubeconfig(raw string) (server string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("YAML 解析失败：%v", rec)
		}
	}()
	jsonBytes, err := sigsyaml.YAMLToJSON([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("YAML 解析失败：%v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return "", fmt.Errorf("YAML 解析失败：%v", err)
	}
	clusters, _ := doc["clusters"].([]interface{})
	if len(clusters) == 0 {
		return "", fmt.Errorf("Kubeconfig 格式无效：缺少 clusters 字段")
	}
	first, _ := clusters[0].(map[string]interface{})
	clusterObj, _ := first["cluster"].(map[string]interface{})
	if clusterObj == nil {
		return "", fmt.Errorf("Kubeconfig 格式无效：缺少 clusters 字段")
	}
	if s, _ := clusterObj["server"].(string); s != "" {
		return s, nil
	}
	return "", nil
}

func (s *Server) clusterAdd(w http.ResponseWriter, r *http.Request) {
	name := trimSpace(r.FormValue("name"))
	displayName := trimSpace(r.FormValue("display_name"))
	description := r.FormValue("description")
	promURL := trimSpace(r.FormValue("prometheus_url"))
	kubeconfigRaw := trimSpace(r.FormValue("kubeconfig_text"))
	if f, _, err := r.FormFile("kubeconfig_file"); err == nil {
		defer f.Close()
		if b, err := io.ReadAll(f); err == nil && len(b) > 0 {
			kubeconfigRaw = trimSpace(string(b))
		}
	}

	fail := func(msg string) {
		redirectWithFlash(w, r, "/clusters/add/", msg, "error")
	}
	if name == "" {
		fail("集群名称不能为空")
		return
	}
	if kubeconfigRaw == "" {
		fail("请上传 Kubeconfig 文件或粘贴 YAML 内容")
		return
	}
	apiServer, err := parseKubeconfig(kubeconfigRaw)
	if err != nil {
		fail(err.Error())
		return
	}
	if _, exists := s.Store.GetClusterByName(name); exists {
		fail(fmt.Sprintf("集群名称 %q 已存在", name))
		return
	}

	c := &store.Cluster{
		Name:          name,
		DisplayName:   displayName,
		Description:   description,
		APIServer:     apiServer,
		PrometheusURL: promURL,
		Status:        "unknown",
	}
	c.SetKubeconfig(kubeconfigRaw, s.Store.AEAD())
	if err := s.Store.CreateCluster(c); err != nil {
		fail("保存失败：" + err.Error())
		return
	}
	go s.refreshClusterInfo(c.ID)
	redirectWithFlash(w, r, "/clusters/", fmt.Sprintf("集群 %q 导入成功，正在后台获取集群信息...", c.Display()), "success")
}

func (s *Server) handleClusterEdit(w http.ResponseWriter, r *http.Request, c *store.Cluster) {
	c.DisplayName = trimSpace(r.FormValue("display_name"))
	if c.DisplayName == "" {
		c.DisplayName = c.Name
	}
	c.Description = r.FormValue("description")
	c.PrometheusURL = trimSpace(r.FormValue("prometheus_url"))

	kubeconfigRaw := trimSpace(r.FormValue("kubeconfig_text"))
	if f, _, err := r.FormFile("kubeconfig_file"); err == nil {
		defer f.Close()
		if b, err := io.ReadAll(f); err == nil && len(b) > 0 {
			kubeconfigRaw = trimSpace(string(b))
		}
	}
	if kubeconfigRaw != "" {
		apiServer, err := parseKubeconfig(kubeconfigRaw)
		if err != nil {
			redirectWithFlash(w, r, fmt.Sprintf("/clusters/%d/edit/", c.ID), err.Error(), "error")
			return
		}
		c.APIServer = apiServer
		c.SetKubeconfig(kubeconfigRaw, s.Store.AEAD())
	}
	if err := s.Store.UpdateCluster(c); err != nil {
		redirectWithFlash(w, r, fmt.Sprintf("/clusters/%d/edit/", c.ID), "保存失败："+err.Error(), "error")
		return
	}
	go s.refreshClusterInfo(c.ID)
	redirectWithFlash(w, r, fmt.Sprintf("/clusters/%d/", c.ID), fmt.Sprintf("集群 %q 更新成功", c.Display()), "success")
}

func (s *Server) clusterDelete(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	confirmName := trimSpace(r.FormValue("confirm_name"))
	if confirmName != c.Name {
		redirectWithFlash(w, r, fmt.Sprintf("/clusters/%d/", c.ID), "集群名称不匹配，删除取消", "error")
		return
	}
	s.Syncer.StopForCluster(c.ID)
	s.Pool.Remove(c.ID)
	s.Exec.StopCluster(c.ID)
	s.Ksh.StopCluster(c.ID)
	name := c.Display()
	if err := s.Store.DeleteCluster(c.ID); err != nil {
		redirectWithFlash(w, r, "/clusters/", "删除失败："+err.Error(), "error")
		return
	}
	redirectWithFlash(w, r, "/clusters/", fmt.Sprintf("集群 %q 已删除", name), "success")
}

func (s *Server) clusterRefresh(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	go s.refreshClusterInfo(c.ID)
	redirectWithFlash(w, r, fmt.Sprintf("/clusters/%d/", c.ID), fmt.Sprintf("集群 %q 正在后台刷新...", c.Display()), "success")
}

func (s *Server) clusterUpdatePrometheus(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	c.PrometheusURL = trimSpace(r.FormValue("prometheus_url"))
	_ = s.Store.UpdateCluster(c)
	msg := fmt.Sprintf("Prometheus 地址已更新为: %s", c.PrometheusURL)
	if c.PrometheusURL == "" {
		msg = "Prometheus 地址已清空，将使用 Metrics Server"
	}
	redirectWithFlash(w, r, fmt.Sprintf("/clusters/%d/", c.ID), msg, "success")
}

// clusterSelect 切换当前集群（?next= 里的集群 ID 一并替换，对齐 Django 版）。
func (s *Server) clusterSelect(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.Store.SetActiveCluster(c.ID)
	next := r.URL.Query().Get("next")
	if next == "" || !beginsWithSlash(next) {
		next = "/"
	}
	resRe := regexp.MustCompile(`/resources/\d+/`)
	cluRe := regexp.MustCompile(`/clusters/\d+/`)
	next = resRe.ReplaceAllString(next, fmt.Sprintf("/resources/%d/", c.ID))
	next = cluRe.ReplaceAllString(next, fmt.Sprintf("/clusters/%d/", c.ID))
	redirectWithFlash(w, r, next, fmt.Sprintf("已切换到集群 %s", c.Display()), "success")
}

func beginsWithSlash(s string) bool {
	return len(s) > 0 && s[0] == '/' && (len(s) == 1 || s[1] != '/')
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// ─── 集群 API ────────────────────────────────────────────────

// friendlyError 对应 Python _friendly_error。
func friendlyError(err error) string {
	return syncerDescribe(err)
}

func (s *Server) clusterNodesAPI(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	cl, err := s.Pool.Get(c.ID, loader)
	if err != nil {
		JSON(w, 200, map[string]interface{}{"nodes": []interface{}{}, "error": friendlyError(err),
			"stats": map[string]int{"total": 0, "ready": 0, "not_ready": 0, "gpu_nodes": 0}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	nodes := []map[string]interface{}{}
	err = listNodesSummary(ctx, cl.Typed, &nodes)
	stats := map[string]int{"total": len(nodes)}
	for _, n := range nodes {
		if n["status"] == "Ready" {
			stats["ready"]++
		}
		if n["is_gpu"] == true {
			stats["gpu_nodes"]++
		}
	}
	stats["not_ready"] = len(nodes) - stats["ready"]
	resp := map[string]interface{}{"nodes": nodes, "error": nil, "stats": stats}
	if err != nil {
		resp["error"] = friendlyError(err)
	}
	JSON(w, 200, resp)
}

func (s *Server) clusterMetricsAPI(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	cl, err := s.Pool.Get(c.ID, loader)
	if err != nil {
		JSON(w, 200, map[string]interface{}{"nodes": []interface{}{}, "error": friendlyError(err), "has_metrics": false, "data_source": nil,
			"summary": map[string]interface{}{"cpu_capacity": 0, "cpu_used": 0, "cpu_percent": 0, "mem_capacity_gb": 0, "mem_used_gb": 0, "mem_percent": 0}})
		return
	}
	data, err := s.Agg.Fetch(r.Context(), c.ID, cl.Typed)
	if err != nil {
		JSON(w, 200, map[string]interface{}{"nodes": []interface{}{}, "error": friendlyError(err), "has_metrics": false, "data_source": nil,
			"summary": map[string]interface{}{"cpu_capacity": 0, "cpu_used": 0, "cpu_percent": 0, "mem_capacity_gb": 0, "mem_used_gb": 0, "mem_percent": 0}})
		return
	}
	data.Error = nil
	JSON(w, 200, data)
}

// clusterDebugPromAPI 对应 Python cluster_debug_prom。
func (s *Server) clusterDebugPromAPI(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	result := map[string]interface{}{
		"prometheus_url":  c.PrometheusURL,
		"k8s_nodes":       []string{},
		"prom_cpu_sample": []interface{}{},
		"prom_mem_sample": []interface{}{},
	}
	if cl, err := s.Pool.Get(c.ID, loader); err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if nodeList, err := cl.Typed.CoreV1().Nodes().List(ctx, metav1ListOptionsNil()); err == nil {
			names := []string{}
			for i := range nodeList.Items {
				names = append(names, nodeList.Items[i].Name)
			}
			result["k8s_nodes"] = names
		} else {
			result["k8s_error"] = err.Error()
		}
	}
	if c.PrometheusURL != "" {
		prom := metrics.NewPromClient(c.PrometheusURL)
		result["prom_available"] = prom.IsAvailable()
		raw := prom.Query("sum by (instance, node, kubernetes_node) (rate(node_cpu_seconds_total{mode!=\"idle\"}[5m]))")
		if len(raw) > 3 {
			raw = raw[:3]
		}
		result["prom_cpu_sample"] = raw
		raw2 := prom.Query("node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes")
		if len(raw2) > 3 {
			raw2 = raw2[:3]
		}
		result["prom_mem_sample"] = raw2
	}
	JSON(w, 200, result)
}

// ─── 日志（clusters 与 resources 两侧共用）────────────────────

func (s *Server) podLogsAPI(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	fetchPodLogs(s, w, r, c, loader, r.PathValue("ns"), r.PathValue("pod"))
}

// ─── 数字工具 ────────────────────────────────────────────────

func atoiOr(s string, dflt int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return dflt
}
