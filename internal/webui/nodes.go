// 节点 API / 节点操作 / Pod 日志 / 错误翻译等共享辅助。
package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"sailor/internal/metrics"
	"sailor/internal/store"
	"sailor/internal/syncer"
)

func metav1ListOptionsNil() metav1.ListOptions { return metav1.ListOptions{} }

// syncerDescribe 复用 syncer 的中文错误翻译。
func syncerDescribe(err error) string {
	if apierrors.IsUnauthorized(err) {
		return "kubeconfig 凭证无效或已过期（401 Unauthorized）"
	}
	if apierrors.IsForbidden(err) {
		return fmt.Sprintf("当前 kubeconfig 权限不足（403 %s）", apiStatusReason(err))
	}
	if se, ok := err.(interface{ APIStatus() metav1.Status }); ok {
		st := se.APIStatus()
		return fmt.Sprintf("K8s API 错误（%d）：%s", st.Code, st.Message)
	}
	return syncer.DescribeError(err)
}

func apiStatusReason(err error) string {
	if se, ok := err.(interface{ APIStatus() metav1.Status }); ok {
		return se.APIStatus().Message
	}
	return err.Error()
}

// ─── 节点列表 / 详情 ──────────────────────────────────────────

func nodeRoles(labels map[string]string) string {
	var roles []string
	for k := range labels {
		if strings.HasPrefix(k, "node-role.kubernetes.io/") {
			roles = append(roles, strings.TrimPrefix(k, "node-role.kubernetes.io/"))
		}
	}
	if len(roles) == 0 {
		return "worker"
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func gpuDisplay(gpuModel string, gpuCount string) (isGPU bool, display string, nodeType string) {
	isGPU = gpuCount != "" && gpuCount != "0"
	if isGPU {
		short := gpuModel
		if i := strings.Index(gpuModel, "-"); i > 0 {
			short = gpuModel[:i]
		}
		if short == "" {
			short = "GPU"
		}
		return true, fmt.Sprintf("%s x %s", short, gpuCount), short
	}
	if gpuModel != "" {
		return false, "-", gpuModel
	}
	return false, "-", "CPU"
}

func listNodesSummary(ctx context.Context, typed *kubernetes.Clientset, out *[]map[string]interface{}) error {
	nodeList, err := typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		conditions := map[string]string{}
		for _, c := range n.Status.Conditions {
			conditions[string(c.Type)] = string(c.Status)
		}
		cap := n.Status.Capacity
		labels := n.Labels
		gpuCount := "0"
		if g, ok := cap["nvidia.com/gpu"]; ok {
			gpuCount = g.String()
		}
		gpuModel := labels["nvidia.com/gpu.product"]
		isGPU, gpuDisplayStr, nodeType := gpuDisplay(gpuModel, gpuCount)

		status := "NotReady"
		if conditions["Ready"] == "True" {
			status = "Ready"
		}
		*out = append(*out, map[string]interface{}{
			"name":            n.Name,
			"status":          status,
			"roles":           nodeRoles(labels),
			"node_type":       nodeType,
			"is_gpu":          isGPU,
			"k8s_version":     n.Status.NodeInfo.KubeletVersion,
			"os":              n.Status.NodeInfo.OSImage,
			"cpu_capacity":    cap.Cpu().String(),
			"memory_capacity": metrics.ParseMemory(cap.Memory().String()),
			"gpu":             gpuDisplayStr,
		})
	}
	return nil
}

// nodeInfoAPI GET /clusters/{id}/node/{name}/info/。
func (s *Server) nodeInfoAPI(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	nodeName := r.PathValue("name")

	cl, err := s.Pool.Get(c.ID, loader)
	var nodeInfo map[string]interface{}
	pods := []map[string]interface{}{}
	var apiErr error
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		nodeInfo, pods, apiErr = fetchNodeInfo(ctx, cl.Typed, nodeName)
	} else {
		apiErr = err
	}

	resp := map[string]interface{}{"node": nodeInfo, "pods": pods, "error": nil}
	if apiErr != nil {
		resp["error"] = friendlyError(apiErr)
	}
	JSON(w, 200, resp)
}

func fetchNodeInfo(ctx context.Context, typed *kubernetes.Clientset, name string) (map[string]interface{}, []map[string]interface{}, error) {
	node, err := typed.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	conditions := map[string]string{}
	for _, c := range node.Status.Conditions {
		conditions[string(c.Type)] = string(c.Status)
	}
	capacity := node.Status.Capacity
	allocatable := node.Status.Allocatable
	labels := node.Labels
	gpuCount := "0"
	if g, ok := capacity["nvidia.com/gpu"]; ok {
		gpuCount = g.String()
	}
	gpuModel := labels["nvidia.com/gpu.product"]
	isGPU, _, nodeType := gpuDisplay(gpuModel, gpuCount)
	status := "NotReady"
	if conditions["Ready"] == "True" {
		status = "Ready"
	}
	created := ""
	if node.CreationTimestamp.Unix() > 0 {
		created = node.CreationTimestamp.Time.Format(time.RFC3339)
	}
	info := map[string]interface{}{
		"name":               node.Name,
		"status":             status,
		"schedulable":        node.Spec.Unschedulable != true,
		"drained":            node.Annotations["armada.io/drained-at"] != "",
		"drained_at":         node.Annotations["armada.io/drained-at"],
		"roles":              nodeRoles(labels),
		"k8s_version":        node.Status.NodeInfo.KubeletVersion,
		"os":                 node.Status.NodeInfo.OSImage,
		"kernel":             node.Status.NodeInfo.KernelVersion,
		"container_runtime":  node.Status.NodeInfo.ContainerRuntimeVersion,
		"arch":               node.Status.NodeInfo.Architecture,
		"cpu_capacity":       capacity.Cpu().String(),
		"cpu_allocatable":    allocatable.Cpu().String(),
		"memory_capacity":    metrics.ParseMemory(capacity.Memory().String()),
		"memory_allocatable": metrics.ParseMemory(allocatable.Memory().String()),
		"gpu_count":          gpuCount,
		"gpu_model":          gpuModel,
		"node_type":          nodeType,
		"is_gpu":             isGPU,
		"created":            created,
	}

	podList, err := typed.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + name,
	})
	if err != nil {
		return info, []map[string]interface{}{}, nil
	}
	pods := []map[string]interface{}{}
	for i := range podList.Items {
		p := &podList.Items[i]
		ready := 0
		restarts := 0
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += int(cs.RestartCount)
		}
		ip := "-"
		if p.Status.PodIP != "" {
			ip = p.Status.PodIP
		}
		pods = append(pods, map[string]interface{}{
			"name":      p.Name,
			"namespace": p.Namespace,
			"status":    string(p.Status.Phase),
			"ready":     fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers)),
			"restarts":  restarts,
			"ip":        ip,
		})
	}
	return info, pods, nil
}

// ─── 节点操作 ─────────────────────────────────────────────────

type nodeActionFunc func(ctx context.Context, typed *kubernetes.Clientset, nodeName string) (string, error)

func (s *Server) nodeAction(fn nodeActionFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
		if !ok {
			return
		}
		cl, err := s.Pool.Get(c.ID, loader)
		if err != nil {
			JSONError(w, 500, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		msg, err := fn(ctx, cl.Typed, r.PathValue("name"))
		if err != nil {
			JSONError(w, 500, err.Error())
			return
		}
		JSON(w, 200, map[string]interface{}{"success": true, "message": msg})
	}
}

func patchNode(ctx context.Context, typed *kubernetes.Clientset, name string, patch []byte) error {
	_, err := typed.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

func nodeCordon(ctx context.Context, typed *kubernetes.Clientset, name string) (string, error) {
	err := patchNode(ctx, typed, name, []byte(`{"spec":{"unschedulable":true}}`))
	if err != nil {
		return "", fmt.Errorf("停止调度失败：%v", err)
	}
	return fmt.Sprintf("节点 %s 已停止调度", name), nil
}

func nodeUncordon(ctx context.Context, typed *kubernetes.Clientset, name string) (string, error) {
	err := patchNode(ctx, typed, name, []byte(`{"spec":{"unschedulable":null},"metadata":{"annotations":{"armada.io/drained-at":null}}}`))
	if err != nil {
		return "", fmt.Errorf("恢复调度失败：%v", err)
	}
	return fmt.Sprintf("节点 %s 已恢复调度", name), nil
}

func nodeDrain(ctx context.Context, typed *kubernetes.Clientset, name string) (string, error) {
	// Step 1: cordon + 打 drain 注解（区分 cordon 和 drain）
	drainPatch := map[string]interface{}{
		"spec":     map[string]interface{}{"unschedulable": true},
		"metadata": map[string]interface{}{"annotations": map[string]interface{}{"armada.io/drained-at": time.Now().Format("2006-01-02 15:04:05")}},
	}
	b, _ := json.Marshal(drainPatch)
	if err := patchNode(ctx, typed, name, b); err != nil {
		return "", fmt.Errorf("排空失败：%v", err)
	}
	// Step 2: 驱逐非 DaemonSet / 非 mirror Pod
	podList, err := typed.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + name,
	})
	if err != nil {
		return "", fmt.Errorf("排空失败：%v", err)
	}
	evicted, skipped := 0, 0
	for i := range podList.Items {
		pod := &podList.Items[i]
		isDaemonSet := false
		for _, ref := range pod.OwnerReferences {
			if ref.Kind == "DaemonSet" {
				isDaemonSet = true
				break
			}
		}
		isMirror := pod.Annotations["kubernetes.io/config.mirror"] != ""
		if isDaemonSet || isMirror {
			skipped++
			continue
		}
		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		}
		err := typed.CoreV1().Pods(pod.Namespace).EvictV1(ctx, eviction)
		if err != nil {
			// PDB 冲突等只记日志不中断（对齐 Django 版）
			skipped++
			continue
		}
		evicted++
	}
	return fmt.Sprintf("节点 %s 已排空，驱逐 %d 个 Pod，跳过 %d 个", name, evicted, skipped), nil
}

func nodeDelete(ctx context.Context, typed *kubernetes.Clientset, name string) (string, error) {
	err := typed.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return "", fmt.Errorf("移除节点失败：%v", err)
	}
	return fmt.Sprintf("节点 %s 已从集群中移除", name), nil
}

// ─── Pod 日志 ─────────────────────────────────────────────────

// fetchPodLogs 对应 Python clusters/pod_logs.fetch_pod_logs。
func fetchPodLogs(s *Server, w http.ResponseWriter, r *http.Request, c *store.Cluster, loader func() (string, error), namespace, podName string) {
	container := r.URL.Query().Get("container")
	tailLines := atoiOr(r.URL.Query().Get("tail_lines"), 200)
	previous := r.URL.Query().Get("previous") == "true"

	cl, err := s.Pool.Get(c.ID, loader)
	if err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	core := cl.Typed.CoreV1()

	if container == "" {
		pod, err := core.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err == nil && len(pod.Spec.Containers) > 0 {
			container = pod.Spec.Containers[0].Name
		}
	}

	opts := &corev1.PodLogOptions{
		Container: container,
		TailLines: ptrInt64(int64(tailLines)),
		Previous:  previous,
	}
	req := core.Pods(namespace).GetLogs(podName, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		// 上次日志不存在 —— pod 从未重启过
		msg := strings.ToLower(err.Error())
		if previous && (strings.Contains(msg, "previous terminated") || apierrors.IsNotFound(err) || strings.Contains(msg, "previous")) {
			JSON(w, 200, map[string]interface{}{"success": true, "logs": "", "container": container, "previous": previous, "no_previous": true})
			return
		}
		JSONError(w, 500, err.Error())
		return
	}
	defer stream.Close()
	logs, err := io.ReadAll(stream)
	if err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	JSON(w, 200, map[string]interface{}{"success": true, "logs": string(logs), "container": container, "previous": previous})
}

func ptrInt64(v int64) *int64 { return &v }

// atoiOr 在 handlers_clusters.go 已定义。
var _ = policyv1.SchemeGroupVersion
