// Package metrics：Prometheus 查询（对应 clusters/prometheus.py）+
// 集群资源指标聚合与降级（对应 clusters/views._fetch_metrics_data）。
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"sailor/internal/k8sx"
)

// ─── 单位换算 ─────────────────────────────────────────────────

var memRe = regexp.MustCompile(`^(\d+)\s*(Ki|Mi|Gi|Ti|K|M|G|T)?$`)

// ParseMemoryBytes K8s 内存串（Ki/Mi/Gi/bytes）→ bytes。
func ParseMemoryBytes(memStr string) int64 {
	if memStr == "" {
		return 0
	}
	m := memRe.FindStringSubmatch(memStr)
	if m == nil {
		return 0
	}
	val, _ := strconv.ParseInt(m[1], 10, 64)
	switch m[2] {
	case "Ki":
		return val * 1024
	case "Mi":
		return val * 1024 * 1024
	case "Gi":
		return val * 1024 * 1024 * 1024
	case "Ti":
		return val * 1024 * 1024 * 1024 * 1024
	case "K":
		return val * 1000
	case "M":
		return val * 1000 * 1000
	case "G":
		return val * 1000 * 1000 * 1000
	case "T":
		return val * 1000 * 1000 * 1000 * 1000
	}
	return val
}

// ParseMemory 转人类可读 GB/MB。
func ParseMemory(memStr string) string {
	b := ParseMemoryBytes(memStr)
	if b == 0 {
		if memStr == "" || regexp.MustCompile(`^\d`).MatchString(memStr) {
			return "0"
		}
		return memStr
	}
	if gb := float64(b) / (1024 * 1024 * 1024); gb >= 1 {
		return fmt.Sprintf("%.0f GB", gb)
	}
	return fmt.Sprintf("%.0f MB", float64(b)/(1024*1024))
}

// ParseCPUNano K8s CPU 串（'250m' / '1' / '2500n'）→ 核数（float）。
func ParseCPUNano(cpuStr string) float64 {
	if cpuStr == "" {
		return 0
	}
	switch {
	case hasSuffix(cpuStr, "n"):
		v, _ := strconv.ParseInt(cpuStr[:len(cpuStr)-1], 10, 64)
		return float64(v) / 1e9
	case hasSuffix(cpuStr, "m"):
		v, _ := strconv.ParseInt(cpuStr[:len(cpuStr)-1], 10, 64)
		return float64(v) / 1000.0
	}
	f, _ := strconv.ParseFloat(cpuStr, 64)
	return f
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

// ─── Prometheus 客户端 ────────────────────────────────────────

// PromClient 查询 Prometheus 即时查询。
type PromClient struct {
	BaseURL string
	Timeout time.Duration
	client  *http.Client
}

func NewPromClient(baseURL string) *PromClient {
	return &PromClient{BaseURL: trimRight(baseURL, '/'), Timeout: 5 * time.Second, client: &http.Client{Timeout: 5 * time.Second}}
}

func trimRight(s string, c byte) string {
	for len(s) > 0 && s[len(s)-1] == c {
		s = s[:len(s)-1]
	}
	return s
}

type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Query 执行即时查询，返回 {metric, value} 列表。
func (p *PromClient) Query(promql string) []map[string]any {
	req, err := http.NewRequest("GET", p.BaseURL+"/api/v1/query?query="+queryEscape(promql), nil)
	if err != nil {
		return nil
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var pr promResponse
	if json.NewDecoder(resp.Body).Decode(&pr) != nil || pr.Status != "success" {
		return nil
	}
	out := make([]map[string]any, 0, len(pr.Data.Result))
	for _, r := range pr.Data.Result {
		val := ""
		if len(r.Value) > 1 {
			if s, ok := r.Value[1].(string); ok {
				val = s
			}
		}
		metric := map[string]any{}
		for k, v := range r.Metric {
			metric[k] = v
		}
		out = append(out, map[string]any{"metric": metric, "value": val})
	}
	return out
}

// IsAvailable 探测 Prometheus 是否可用。
func (p *PromClient) IsAvailable() bool {
	resp, err := p.client.Get(p.BaseURL + "/api/v1/status/buildinfo")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func queryEscape(s string) string {
	// net/url.QueryEscape 更标准，这里为避免额外 import 直接内联常用转义
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~':
			b = append(b, c)
		case c == ' ':
			b = append(b, '+')
		default:
			b = append(b, '%')
			b = append(b, "0123456789ABCDEF"[c>>4], "0123456789ABCDEF"[c&0xf])
		}
	}
	return string(b)
}

// extractNode 从 Prometheus 标签里提取节点名（node > kubernetes_node > nodename
// > instance 去端口），对应 Python _extract_node。
func extractNode(metric map[string]any) string {
	for _, key := range []string{"node", "kubernetes_node", "nodename"} {
		if v, ok := metric[key].(string); ok && v != "" {
			return v
		}
	}
	if inst, ok := metric["instance"].(string); ok && inst != "" {
		for i := 0; i < len(inst); i++ {
			if inst[i] == ':' {
				return inst[:i]
			}
		}
		return inst
	}
	return ""
}

// GetNodeCPUUsage 返回 {node: cpu_cores_used}。
func (p *PromClient) GetNodeCPUUsage() map[string]float64 {
	results := p.Query("sum by (node) (rate(node_cpu_seconds_total{mode!=\"idle\"}[5m]))")
	if len(results) == 0 {
		results = p.Query("sum by (instance) (rate(node_cpu_seconds_total{mode!=\"idle\"}[5m]))")
	}
	out := map[string]float64{}
	for _, r := range results {
		m, _ := r["metric"].(map[string]any)
		if node := extractNode(m); node != "" {
			if v, err := strconv.ParseFloat(fmt.Sprintf("%v", r["value"]), 64); err == nil {
				out[node] = round2(v)
			}
		}
	}
	return out
}

// GetNodeMemoryUsage 返回 {node: memory_bytes_used}。
func (p *PromClient) GetNodeMemoryUsage() map[string]int64 {
	results := p.Query("sum by (node) (node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes)")
	if len(results) == 0 {
		results = p.Query("sum by (instance) (node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes)")
	}
	out := map[string]int64{}
	for _, r := range results {
		m, _ := r["metric"].(map[string]any)
		if node := extractNode(m); node != "" {
			if v, err := strconv.ParseFloat(fmt.Sprintf("%v", r["value"]), 64); err == nil {
				out[node] = int64(v)
			}
		}
	}
	return out
}

// GetNodeLoad 返回 {node: load1}。
func (p *PromClient) GetNodeLoad() map[string]float64 {
	results := p.Query("sum by (node) (node_load1)")
	if len(results) == 0 {
		results = p.Query("sum by (instance) (node_load1)")
	}
	out := map[string]float64{}
	for _, r := range results {
		m, _ := r["metric"].(map[string]any)
		if node := extractNode(m); node != "" {
			if v, err := strconv.ParseFloat(fmt.Sprintf("%v", r["value"]), 64); err == nil {
				out[node] = round2(v)
			}
		}
	}
	return out
}

// GetNodeDiskUsage 返回 {node: {used_gb, total_gb, percent}}。
func (p *PromClient) GetNodeDiskUsage() map[string]map[string]float64 {
	total := p.Query("sum by (node) (node_filesystem_size_bytes{mountpoint=\"/\",fstype!=\"tmpfs\"})")
	if len(total) == 0 {
		total = p.Query("sum by (instance) (node_filesystem_size_bytes{mountpoint=\"/\",fstype!=\"tmpfs\"})")
	}
	avail := p.Query("sum by (node) (node_filesystem_avail_bytes{mountpoint=\"/\",fstype!=\"tmpfs\"})")
	if len(avail) == 0 {
		avail = p.Query("sum by (instance) (node_filesystem_avail_bytes{mountpoint=\"/\",fstype!=\"tmpfs\"})")
	}

	totalMap := map[string]float64{}
	for _, r := range total {
		m, _ := r["metric"].(map[string]any)
		if node := extractNode(m); node != "" {
			if v, err := strconv.ParseFloat(fmt.Sprintf("%v", r["value"]), 64); err == nil {
				totalMap[node] = v
			}
		}
	}
	out := map[string]map[string]float64{}
	for _, r := range avail {
		m, _ := r["metric"].(map[string]any)
		node := extractNode(m)
		if node == "" {
			continue
		}
		totalBytes, ok := totalMap[node]
		if !ok {
			continue
		}
		availBytes, _ := strconv.ParseFloat(fmt.Sprintf("%v", r["value"]), 64)
		used := totalBytes - availBytes
		pct := 0.0
		if totalBytes > 0 {
			pct = round1(used / totalBytes * 100)
		}
		out[node] = map[string]float64{
			"used_gb":  round1(used / (1 << 30)),
			"total_gb": round1(totalBytes / (1 << 30)),
			"percent":  pct,
		}
	}
	return out
}

// GetGPUUtilization 返回 {node: [{gpu_index, utilization, model}]}。DCGM 优先，
// 降级 nvidia_smi_exporter。
func (p *PromClient) GetGPUUtilization() map[string][]map[string]any {
	results := p.Query("DCGM_FI_DEV_GPU_UTIL")
	if len(results) == 0 {
		results = p.Query("nvidia_smi_utilization_gpu_ratio * 100")
	}
	out := map[string][]map[string]any{}
	for _, r := range results {
		m, _ := r["metric"].(map[string]any)
		node := promGPUAnyNode(m)
		if node == "" {
			continue
		}
		v, _ := strconv.ParseFloat(fmt.Sprintf("%v", r["value"]), 64)
		out[node] = append(out[node], map[string]any{
			"gpu_index":   labelOr(m, "0", "gpu", "minor_number"),
			"utilization": round1(v),
			"model":       labelOr(m, "", "modelName", "gpu_model"),
		})
	}
	return out
}

// GetGPUMemory 返回 {node: [{gpu_index, used_mb, total_mb, model}]}。
func (p *PromClient) GetGPUMemory() map[string][]map[string]any {
	used := p.Query("DCGM_FI_DEV_FB_USED")
	total := p.Query("DCGM_FI_DEV_FB_TOTAL")
	if len(used) == 0 {
		used = p.Query("nvidia_smi_memory_used_bytes")
		total = p.Query("nvidia_smi_memory_total_bytes")
	}

	type gpuInfo struct {
		node  string
		gpu   string
		used  float64
		model string
	}
	usedMap := map[string]gpuInfo{}
	for _, r := range used {
		m, _ := r["metric"].(map[string]any)
		node := promGPUAnyNode(m)
		if node == "" {
			continue
		}
		gpu := labelOr(m, "0", "gpu", "minor_number")
		v, _ := strconv.ParseFloat(fmt.Sprintf("%v", r["value"]), 64)
		mb := v
		if v >= 1e6 { // nvidia_smi 是 bytes，DCGM 是 MiB
			mb = v / (1 << 20)
		}
		usedMap[node+":"+gpu] = gpuInfo{node: node, gpu: gpu, used: mb, model: labelOr(m, "", "modelName", "gpu_model")}
	}
	totalMap := map[string]float64{}
	for _, r := range total {
		m, _ := r["metric"].(map[string]any)
		node := promGPUAnyNode(m)
		gpu := labelOr(m, "0", "gpu", "minor_number")
		v, _ := strconv.ParseFloat(fmt.Sprintf("%v", r["value"]), 64)
		if v >= 1e6 {
			v = v / (1 << 20)
		}
		totalMap[node+":"+gpu] = v
	}

	out := map[string][]map[string]any{}
	for _, info := range usedMap {
		out[info.node] = append(out[info.node], map[string]any{
			"gpu_index": info.gpu,
			"used_mb":   int64(round1(info.used)),
			"total_mb":  int64(round1(totalMap[info.node+":"+info.gpu])),
			"model":     info.model,
		})
	}
	return out
}

// GPU 指标的节点标签兼容层（与 CPU/Mem 不同：多了 Hostname）。
func promGPUAnyNode(metric map[string]any) string {
	for _, key := range []string{"node", "Hostname"} {
		if v, ok := metric[key].(string); ok && v != "" {
			return v
		}
	}
	if inst, ok := metric["instance"].(string); ok && inst != "" {
		for i := 0; i < len(inst); i++ {
			if inst[i] == ':' {
				return inst[:i]
			}
		}
		return inst
	}
	return ""
}

// labelOr 依次取标签，取不到返回 dflt。
func labelOr(m map[string]any, dflt string, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return dflt
}

func round1(v float64) float64 { return float64(int(v*10+0.5*sign(v))) / 10 }
func round2(v float64) float64 { return float64(int(v*100+0.5*sign(v))) / 100 }
func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// ─── 集群资源指标聚合（Metrics Server 优先，降级 Pod requests）──

// ClusterData 是 /clusters/<id>/metrics/ 的返回结构。
type ClusterData struct {
	Nodes      []map[string]any `json:"nodes"`
	Summary    map[string]any   `json:"summary"`
	HasMetrics bool             `json:"has_metrics"`
	DataSource *string          `json:"data_source"`
	Error      *string          `json:"error"`
}

// Aggregator 带内存缓存的集群指标聚合器（60 秒 TTL）。
type Aggregator struct {
	pool *k8sx.Pool

	mu       sync.Mutex
	cacheKey int64
	cacheAt  time.Time
	cache    *ClusterData
}

func NewAggregator(pool *k8sx.Pool) *Aggregator {
	return &Aggregator{pool: pool}
}

// Fetch 聚合节点容量 + 用量。nodesFuture 失败时返回错误（不缓存空结果）。
func (a *Aggregator) Fetch(ctx context.Context, clusterID int64, typed *kubernetes.Clientset) (*ClusterData, error) {
	a.mu.Lock()
	if a.cache != nil && a.cacheKey == clusterID && time.Since(a.cacheAt) < 60*time.Second {
		c := a.cache
		a.mu.Unlock()
		return c, nil
	}
	a.mu.Unlock()

	data, err := a.fetch(ctx, typed)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.cache, a.cacheKey, a.cacheAt = data, clusterID, time.Now()
	a.mu.Unlock()
	return data, nil
}

func (a *Aggregator) fetch(ctx context.Context, typed *kubernetes.Clientset) (*ClusterData, error) {
	ctx2, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	var (
		wg          sync.WaitGroup
		nodeCap     = map[string]map[string]any{}
		metricsMap  = map[string]map[string]float64{}
		requestsMap = map[string]*reqAgg{}
		nodesErr    error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		nodesErr = fetchNodes(ctx2, typed, nodeCap)
	}()
	go func() {
		defer wg.Done()
		fetchNodeMetrics(ctx2, typed, metricsMap)
	}()
	go func() {
		defer wg.Done()
		fetchPodRequests(ctx2, typed, requestsMap)
	}()
	wg.Wait()
	// 节点容量是硬依赖；用量任务失败静默降级
	if nodesErr != nil {
		return nil, nodesErr
	}

	dataSource := (*string)(nil)
	if len(metricsMap) > 0 {
		s := "metrics_server"
		dataSource = &s
	} else if len(requestsMap) > 0 {
		s := "pod_requests"
		dataSource = &s
	}

	var nodes []map[string]any
	var totCPUCap, totCPUUsed, totMemCap, totMemUsed float64
	names := make([]string, 0, len(nodeCap))
	for n := range nodeCap {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		cap := nodeCap[name]
		cpuCap := cap["cpu_capacity"].(float64)
		memCap := cap["mem_capacity"].(int64)
		var cpuUsed, memUsed float64
		if len(metricsMap) > 0 {
			if met, ok := metricsMap[name]; ok {
				cpuUsed, memUsed = met["cpu_used"], float64(met["mem_used"])
			}
		} else if req, ok := requestsMap[name]; ok {
			cpuUsed, memUsed = req.cpuReq, float64(req.memReq)
		}
		totCPUCap += cpuCap
		totCPUUsed += cpuUsed
		totMemCap += float64(memCap)
		totMemUsed += memUsed

		nodes = append(nodes, map[string]any{
			"name":              name,
			"is_gpu":            cap["is_gpu"],
			"gpu_count":         cap["gpu_count"],
			"gpu_model":         cap["gpu_model"],
			"status":            cap["status"],
			"roles":             cap["roles"],
			"cpu_capacity":      round1(cpuCap),
			"cpu_used":          round2(cpuUsed),
			"cpu_percent":       pct(cpuUsed, cpuCap),
			"mem_capacity_gb":   round1(float64(memCap) / (1 << 30)),
			"mem_used_gb":       round1(memUsed / (1 << 30)),
			"mem_percent":       pct(memUsed, float64(memCap)),
			"container_runtime": cap["container_runtime"],
			"os_image":          cap["os_image"],
			"kernel_version":    cap["kernel_version"],
			"kubelet_version":   cap["kubelet_version"],
			"arch":              cap["arch"],
		})
	}

	summary := map[string]any{
		"cpu_capacity":    round1(totCPUCap),
		"cpu_used":        round1(totCPUUsed),
		"cpu_percent":     pct(totCPUUsed, totCPUCap),
		"mem_capacity_gb": round1(totMemCap / (1 << 30)),
		"mem_used_gb":     round1(totMemUsed / (1 << 30)),
		"mem_percent":     pct(totMemUsed, totMemCap),
	}
	return &ClusterData{
		Nodes:      nodes,
		Summary:    summary,
		HasMetrics: len(metricsMap) > 0 || len(requestsMap) > 0,
		DataSource: dataSource,
	}, nil
}

type reqAgg struct {
	cpuReq float64
	memReq int64
}

func fetchNodes(ctx context.Context, typed *kubernetes.Clientset, out map[string]map[string]any) error {
	nodeList, err := typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		cap := n.Status.Capacity
		labels := n.Labels
		ready := "NotReady"
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				if c.Status == corev1.ConditionTrue {
					ready = "Ready"
				}
				break
			}
		}
		gpuCount := 0
		if g, ok := cap["nvidia.com/gpu"]; ok {
			gpuCount, _ = strconv.Atoi(g.String())
		}
		gpuModel := labels["nvidia.com/gpu.product"]
		modelShort := gpuModel
		if i := indexByte(gpuModel, '-'); i > 0 {
			modelShort = gpuModel[:i]
		}
		roles := "worker"
		var rl []string
		for k := range labels {
			if hasPrefix(k, "node-role.kubernetes.io/") {
				rl = append(rl, k[len("node-role.kubernetes.io/"):])
			}
		}
		if len(rl) > 0 {
			sort.Strings(rl)
			roles = joinStrings(rl, ",")
		}
		memCap := int64(0)
		if m, ok := cap["memory"]; ok {
			memCap = ParseMemoryBytes(m.String())
		}
		cpuCap := 0.0
		if c, ok := cap["cpu"]; ok {
			cpuCap, _ = strconv.ParseFloat(c.String(), 64)
		}
		info := n.Status.NodeInfo
		out[n.Name] = map[string]any{
			"cpu_capacity":      cpuCap,
			"mem_capacity":      memCap,
			"is_gpu":            gpuCount > 0,
			"gpu_count":         gpuCount,
			"gpu_model":         modelShort,
			"status":            ready,
			"roles":             roles,
			"container_runtime": info.ContainerRuntimeVersion,
			"os_image":          info.OSImage,
			"kernel_version":    info.KernelVersion,
			"kubelet_version":   info.KubeletVersion,
			"arch":              info.Architecture,
		}
	}
	return nil
}

func fetchNodeMetrics(ctx context.Context, typed *kubernetes.Clientset, out map[string]map[string]float64) {
	// metrics.k8s.io NodeMetricsList
	data, err := typed.RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").
		Do(ctx).Raw()
	if err != nil {
		return
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"items"`
	}
	if json.Unmarshal(data, &list) != nil {
		return
	}
	for _, item := range list.Items {
		out[item.Metadata.Name] = map[string]float64{
			"cpu_used": ParseCPUNano(item.Usage.CPU),
			"mem_used": float64(ParseMemoryBytes(item.Usage.Memory)),
		}
	}
}

func fetchPodRequests(ctx context.Context, typed *kubernetes.Clientset, out map[string]*reqAgg) {
	podList, err := typed.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName == "" {
			continue
		}
		agg, ok := out[pod.Spec.NodeName]
		if !ok {
			agg = &reqAgg{}
			out[pod.Spec.NodeName] = agg
		}
		for j := range pod.Spec.Containers {
			reqs := pod.Spec.Containers[j].Resources.Requests
			agg.cpuReq += ParseCPUNano(reqs.Cpu().String())
			agg.memReq += ParseMemoryBytes(reqs.Memory().String())
		}
	}
}

func pct(used, cap float64) float64 {
	if cap > 0 {
		return round1(used / cap * 100)
	}
	return 0
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
