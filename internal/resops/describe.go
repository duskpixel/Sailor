// Describe：Deployment / StatefulSet / DaemonSet / Service 的详情聚合
// （info + conditions + events + 关联 Pods [+ Endpoints]）。
// 对应 Python K8sResourceManager.describe_* 系列方法。
package resops

import (
	"context"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sailor/internal/k8sx"
)

// Describe 是四个 describe_* 的统一返回形状。
type Describe struct {
	Info       map[string]any   `json:"info"`
	Conditions []map[string]any `json:"conditions,omitempty"`
	Endpoints  []map[string]any `json:"endpoints,omitempty"`
	Events     []map[string]any `json:"events"`
	Pods       []map[string]any `json:"pods"`
}

func (o *Ops) describeWorkload(ctx context.Context, kind, name, namespace string) (*Describe, error) {
	obj, err := o.GetResource(ctx, kind, name, namespace)
	if err != nil {
		return nil, err
	}
	spec, _ := obj.Object["spec"].(map[string]any)
	status, _ := obj.Object["status"].(map[string]any)
	meta, _ := obj.Object["metadata"].(map[string]any)

	info := map[string]any{
		"name":        str(meta["name"]),
		"namespace":   dfltstr(meta["namespace"]),
		"created":     tsSec(meta["creationTimestamp"]),
		"labels":      strMap(meta["labels"]),
		"annotations": filterLastApplied(strMap(meta["annotations"])),
	}
	conditions := []map[string]any{}
	if conds, _ := status["conditions"].([]any); conds != nil {
		for _, c := range conds {
			cm, _ := c.(map[string]any)
			tsKey := "lastTransitionTime"
			if kind == "deployment" {
				tsKey = "lastUpdateTime"
			}
			conditions = append(conditions, map[string]any{
				"type":        str(cm["type"]),
				"status":      str(cm["status"]),
				"reason":      dfltstr(cm["reason"]),
				"message":     str(cm["message"]),
				"last_update": tsSec(cm[tsKey]),
			})
		}
	}

	selector := map[string]any{}
	if sel, _ := spec["selector"].(map[string]any); sel != nil {
		if ml, _ := sel["matchLabels"].(map[string]any); ml != nil {
			selector = ml
		}
	}

	info["containers"] = containersInfo(spec)
	info["generation"] = toInt(meta["generation"])
	observed := 0
	if status != nil {
		observed = toInt(status["observedGeneration"])
	}
	info["observed_generation"] = observed

	d := &Describe{Info: info, Conditions: conditions}

	var pods []map[string]any
	switch kind {
	case "deployment":
		info["replicas"] = toInt(spec["replicas"])
		ready, avail, updated, unavail := 0, 0, 0, 0
		if status != nil {
			ready = toInt(status["readyReplicas"])
			avail = toInt(status["availableReplicas"])
			updated = toInt(status["updatedReplicas"])
			unavail = toInt(status["unavailableReplicas"])
		}
		info["ready_replicas"] = ready
		info["available_replicas"] = avail
		info["updated_replicas"] = updated
		info["unavailable_replicas"] = unavail
		// 更新策略
		strategy := map[string]any{"type": "-"}
		if st, _ := spec["strategy"].(map[string]any); st != nil {
			strategy["type"] = dfltstr(st["type"])
			if ru, _ := st["rollingUpdate"].(map[string]any); ru != nil {
				strategy["max_surge"] = quantityOr(ru["maxSurge"], "-")
				strategy["max_unavailable"] = quantityOr(ru["maxUnavailable"], "-")
			}
		}
		info["strategy"] = strategy
		info["selector"] = selector
		pods = o.listPodsForDeployment(ctx, namespace, name, selector)

	case "statefulset":
		info["replicas"] = toInt(spec["replicas"])
		ready, cur, updated := 0, 0, 0
		if status != nil {
			ready = toInt(status["readyReplicas"])
			cur = toInt(status["currentReplicas"])
			updated = toInt(status["updatedReplicas"])
		}
		info["ready_replicas"] = ready
		info["current_replicas"] = cur
		info["updated_replicas"] = updated
		strategy := map[string]any{"type": "-"}
		if st, _ := spec["updateStrategy"].(map[string]any); st != nil {
			strategy["type"] = dfltstr(st["type"])
			if ru, _ := st["rollingUpdate"].(map[string]any); ru != nil {
				strategy["partition"] = toInt(ru["partition"])
				strategy["max_unavailable"] = quantityOr(ru["maxUnavailable"], "1")
			}
		}
		info["strategy"] = strategy
		info["selector"] = selector
		info["service_name"] = dfltstr(spec["serviceName"])
		if p := str(spec["podManagementPolicy"]); p != "" {
			info["pod_management_policy"] = p
		} else {
			info["pod_management_policy"] = "OrderedReady"
		}
		pods = o.listPodsByOwner(ctx, namespace, "StatefulSet", name, selector)

	case "daemonset":
		desired, cur, ready, updated, avail := 0, 0, 0, 0, 0
		if status != nil {
			desired = toInt(status["desiredNumberScheduled"])
			cur = toInt(status["currentNumberScheduled"])
			ready = toInt(status["numberReady"])
			updated = toInt(status["updatedNumberScheduled"])
			avail = toInt(status["numberAvailable"])
		}
		info["desired"] = desired
		info["current"] = cur
		info["ready"] = ready
		info["updated"] = updated
		info["available"] = avail
		strategy := map[string]any{"type": "-"}
		if st, _ := spec["updateStrategy"].(map[string]any); st != nil {
			strategy["type"] = dfltstr(st["type"])
			if ru, _ := st["rollingUpdate"].(map[string]any); ru != nil {
				strategy["max_unavailable"] = quantityOr(ru["maxUnavailable"], "1")
				strategy["max_surge"] = quantityOr(ru["maxSurge"], "0")
			}
		}
		info["strategy"] = strategy
		info["selector"] = selector
		pods = o.listPodsByOwner(ctx, namespace, "DaemonSet", name, selector)
	}

	d.Pods = pods
	if pods == nil {
		d.Pods = []map[string]any{}
	}
	podNames := make([]string, len(d.Pods))
	for i, p := range d.Pods {
		podNames[i], _ = p["name"].(string)
	}
	d.Events = o.listEventsForTree(ctx, namespace, kindDisplayName(kind), name, podNames)
	if d.Events == nil {
		d.Events = []map[string]any{}
	}
	return d, nil
}

// DescribeDeployment 等四个导出方法。
func (o *Ops) DescribeDeployment(ctx context.Context, name, ns string) (*Describe, error) {
	return o.describeWorkload(ctx, "deployment", name, ns)
}
func (o *Ops) DescribeStatefulset(ctx context.Context, name, ns string) (*Describe, error) {
	return o.describeWorkload(ctx, "statefulset", name, ns)
}
func (o *Ops) DescribeDaemonset(ctx context.Context, name, ns string) (*Describe, error) {
	return o.describeWorkload(ctx, "daemonset", name, ns)
}

func kindDisplayName(kind string) string {
	switch kind {
	case "deployment":
		return "Deployment"
	case "statefulset":
		return "StatefulSet"
	case "daemonset":
		return "DaemonSet"
	}
	return kind
}

// DescribeService Service 详情：信息 + Endpoints + Events + 关联 Pods。
func (o *Ops) DescribeService(ctx context.Context, name, namespace string) (*Describe, error) {
	obj, err := o.GetResource(ctx, "service", name, namespace)
	if err != nil {
		return nil, err
	}
	spec, _ := obj.Object["spec"].(map[string]any)
	status, _ := obj.Object["status"].(map[string]any)
	meta, _ := obj.Object["metadata"].(map[string]any)

	ports := []map[string]any{}
	if pl, _ := spec["ports"].([]any); pl != nil {
		for _, p := range pl {
			pm, _ := p.(map[string]any)
			pi := map[string]any{
				"name":        dfltstr(pm["name"]),
				"protocol":    dfltstr(pm["protocol"]),
				"port":        toInt(pm["port"]),
				"target_port": fmt.Sprintf("%v", orDash(pm["targetPort"])),
			}
			if np := toInt(pm["nodePort"]); np != 0 {
				pi["node_port"] = np
			}
			ports = append(ports, pi)
		}
	}

	lbIngress := []string{}
	if st, _ := status["loadBalancer"].(map[string]any); st != nil {
		if ings, _ := st["ingress"].([]any); ings != nil {
			for _, ing := range ings {
				im, _ := ing.(map[string]any)
				if s := str(im["ip"]); s != "" {
					lbIngress = append(lbIngress, s)
				} else if s := str(im["hostname"]); s != "" {
					lbIngress = append(lbIngress, s)
				} else {
					lbIngress = append(lbIngress, "-")
				}
			}
		}
	}

	svcType := str(spec["type"])
	if svcType == "" {
		svcType = "ClusterIP"
	}
	selector := strMap(spec["selector"])

	info := map[string]any{
		"name":             str(meta["name"]),
		"namespace":        str(meta["namespace"]),
		"created":          tsSec(meta["creationTimestamp"]),
		"labels":           strMap(meta["labels"]),
		"annotations":      filterLastApplied(strMap(meta["annotations"])),
		"type":             svcType,
		"cluster_ip":       dfltstr(spec["clusterIP"]),
		"external_ips":     strList(spec["externalIPs"]),
		"ports":            ports,
		"selector":         selector,
		"session_affinity": dfltstr(spec["sessionAffinity"]),
		"lb_ingress":       lbIngress,
		"containers":       []map[string]any{},
	}
	// 端口字段（概览表格）前端在 service 分支会读 info.ports；conditions 留空
	d := &Describe{Info: info}

	// Endpoints
	dyn, err := o.dyn()
	if err == nil {
		ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
		ep, err := dyn.Resource(k8sx.GVRs["endpoints"]).Namespace(namespace).Get(ctx2, name, metav1.GetOptions{})
		cancel()
		if err == nil {
			endpoints := []map[string]any{}
			for _, subset := range toList(ep.Object["subsets"]) {
				sm, _ := subset.(map[string]any)
				if sm == nil {
					continue
				}
				epPorts := []map[string]any{}
				for _, p := range toList(sm["ports"]) {
					pm, _ := p.(map[string]any)
					epPorts = append(epPorts, map[string]any{
						"port":     toInt(pm["port"]),
						"protocol": dfltstr(pm["protocol"]),
						"name":     dfltstr(pm["name"]),
					})
				}
				addrs := func(key string, ready bool) {
					for _, a := range toList(sm[key]) {
						am, _ := a.(map[string]any)
						if am == nil {
							continue
						}
						target := ""
						if tr, _ := am["targetRef"].(map[string]any); tr != nil {
							if k := str(tr["kind"]); k != "" {
								target = k + "/" + str(tr["name"])
							} else {
								target = str(tr["name"])
							}
						}
						endpoints = append(endpoints, map[string]any{
							"ip":     str(am["ip"]),
							"node":   dfltstr(am["nodeName"]),
							"target": target,
							"ready":  ready,
							"ports":  epPorts,
						})
					}
				}
				addrs("addresses", true)
				addrs("notReadyAddresses", false)
			}
			d.Endpoints = endpoints
		}
	}
	if d.Endpoints == nil {
		d.Endpoints = []map[string]any{}
	}

	var pods []map[string]any
	if len(selector) > 0 {
		pods = o.listPodsBySelector(ctx, namespace, toAnyMap(selector))
	}
	if pods == nil {
		pods = []map[string]any{}
	}
	d.Pods = pods
	podNames := make([]string, len(pods))
	for i, p := range pods {
		podNames[i], _ = p["name"].(string)
	}
	d.Events = o.listEventsForTree(ctx, namespace, "Service", name, podNames)
	if d.Events == nil {
		d.Events = []map[string]any{}
	}
	return d, nil
}

// ─── 工具 ─────────────────────────────────────────────────────

func toList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func strMap(v any) map[string]string {
	out := map[string]string{}
	if m, ok := v.(map[string]any); ok {
		for k, val := range m {
			out[k] = fmt.Sprintf("%v", val)
		}
	}
	return out
}

func strList(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, e := range l {
			out = append(out, fmt.Sprintf("%v", e))
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func filterLastApplied(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if !beginsWith(k, "kubectl.kubernetes.io/last-applied") {
			out[k] = v
		}
	}
	return out
}

func beginsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func containersInfo(spec map[string]any) []map[string]any {
	out := []map[string]any{}
	tmpl, _ := spec["template"].(map[string]any)
	if tmpl == nil {
		return out
	}
	podSpec, _ := tmpl["spec"].(map[string]any)
	if podSpec == nil {
		return out
	}
	for _, c := range toList(podSpec["containers"]) {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		portList := []string{}
		for _, p := range toList(cm["ports"]) {
			pm, _ := p.(map[string]any)
			proto := str(pm["protocol"])
			if proto == "" {
				proto = "TCP"
			}
			portList = append(portList, fmt.Sprintf("%v/%s", orDash(pm["containerPort"]), proto))
		}
		out = append(out, map[string]any{
			"name":  str(cm["name"]),
			"image": dfltstr(cm["image"]),
			"ports": portList,
		})
	}
	return out
}

func quantityOr(v any, dflt string) string {
	switch x := v.(type) {
	case string:
		if x != "" {
			return x
		}
	case float64:
		if x == float64(int(x)) {
			return fmt.Sprintf("%d", int(x))
		}
		return fmt.Sprintf("%v", x)
	case int:
		return fmt.Sprintf("%d", x)
	}
	return dflt
}

func orDash(v any) any {
	if v == nil {
		return "-"
	}
	return v
}

// mapKeysSorted 供排序输出（保留给未来扩展）。
func mapKeysSorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
