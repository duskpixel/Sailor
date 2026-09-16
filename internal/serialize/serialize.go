// Package serialize 把 K8s 对象（unstructured map）压成前端列表需要的轻量 JSON。
// 字段名与 Django 版 sync_service._serialize_item 逐一对应，前端 JS 无需改动。
package serialize

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Item 将 unstructured 对象序列化为轻量 map。
func Item(kind string, obj map[string]any) map[string]any {
	meta, _ := obj["metadata"].(map[string]any)
	base := map[string]any{
		"name":      str(meta["name"]),
		"namespace": meta["namespace"], // cluster-scoped 时为 nil，JSON 里是 null（与 Python 一致）
		"created":   tsLocal(meta["creationTimestamp"]),
		"age":       age(meta["creationTimestamp"]),
	}

	switch kind {
	case "namespace":
		if meta["deletionTimestamp"] != nil {
			base["status"] = "Terminating"
		} else if st, _ := obj["status"].(map[string]any); st != nil {
			base["status"] = str(st["phase"])
		} else {
			base["status"] = "-"
		}

	case "pod":
		spec, _ := obj["spec"].(map[string]any)
		st, _ := obj["status"].(map[string]any)
		containers, _ := spec["containers"].([]any)
		statuses, _ := st["containerStatuses"].([]any)
		initStatuses, _ := st["initContainerStatuses"].([]any)

		ready := 0
		restarts := 0
		for _, s := range statuses {
			sm, _ := s.(map[string]any)
			if bl, _ := sm["ready"].(bool); bl {
				ready++
			}
			restarts += toInt(sm["restartCount"])
		}
		terminating := meta["deletionTimestamp"] != nil

		reason, message := "", ""
		for gi, group := range [][]any{statuses, initStatuses} {
			isInit := gi == 1
			for _, s := range group {
				sm, _ := s.(map[string]any)
				state, _ := sm["state"].(map[string]any)
				if w, _ := state["waiting"].(map[string]any); w != nil && str(w["reason"]) != "" {
					r := str(w["reason"])
					if isInit && r == "PodInitializing" {
						continue
					}
					if isInit {
						r = "Init:" + r
					}
					reason, message = r, str(w["message"])
					break
				}
				if !isInit {
					if t, _ := state["terminated"].(map[string]any); t != nil && str(t["reason"]) != "" {
						reason, message = str(t["reason"]), str(t["message"])
						break
					}
				}
			}
			if reason != "" {
				break
			}
		}
		phase := "Unknown"
		if p := str(st["phase"]); p != "" {
			phase = p
		}
		if terminating {
			phase = "Terminating"
		}
		message = truncate(message, 200)
		node := "-"
		if n := str(spec["nodeName"]); n != "" {
			node = n
		}
		ip := "-"
		if p := str(st["podIP"]); p != "" {
			ip = p
		}
		base["status_phase"] = phase
		base["status_reason"] = reason
		base["status_message"] = message
		base["ready_str"] = fmt.Sprintf("%d/%d", ready, len(containers))
		base["restarts"] = restarts
		base["node"] = node
		base["ip"] = ip

	case "deployment", "statefulset":
		spec, _ := obj["spec"].(map[string]any)
		st, _ := obj["status"].(map[string]any)
		base["replicas"] = toInt(spec["replicas"])
		base["ready_replicas"] = toInt(st["readyReplicas"])
		if kind == "deployment" {
			base["available"] = toInt(st["availableReplicas"])
		}
		base["image"] = firstImage(spec)

	case "daemonset":
		spec, _ := obj["spec"].(map[string]any)
		st, _ := obj["status"].(map[string]any)
		base["desired"] = toInt(st["desiredNumberScheduled"])
		base["current"] = toInt(st["currentNumberScheduled"])
		base["ready"] = toInt(st["numberReady"])
		base["image"] = firstImage(spec)

	case "job":
		spec, _ := obj["spec"].(map[string]any)
		st, _ := obj["status"].(map[string]any)
		conds, _ := st["conditions"].([]any)
		phase := "Pending"
		if suspend, _ := spec["suspend"].(bool); suspend {
			phase = "Suspended"
		}
		for _, c := range conds {
			cm, _ := c.(map[string]any)
			switch str(cm["type"]) {
			case "Complete":
				phase = "Complete"
			case "Failed":
				phase = "Failed"
			}
		}
		if phase == "Pending" {
			if active := toInt(st["active"]); active > 0 {
				phase = "Running"
			} else if str(st["startTime"]) != "" {
				phase = "Running"
			}
		}
		// completions 可空：nil 表示次数不固定，前端展示 "-"
		completions := any("-")
		if v := spec["completions"]; v != nil {
			completions = toInt(v)
		}
		base["status_phase"] = phase
		base["succeeded"] = toInt(st["succeeded"])
		base["failed"] = toInt(st["failed"])
		base["active"] = toInt(st["active"])
		base["completions"] = completions
		base["parallelism"] = toInt(spec["parallelism"])
		base["duration"] = durationStr(st["startTime"], st["completionTime"])
		base["image"] = firstImage(spec)

	case "cronjob":
		spec, _ := obj["spec"].(map[string]any)
		st, _ := obj["status"].(map[string]any)
		activeJobs, _ := st["active"].([]any)
		jt, _ := spec["jobTemplate"].(map[string]any)
		jtSpec, _ := jt["spec"].(map[string]any)
		suspend := false
		if v, ok := spec["suspend"].(bool); ok {
			suspend = v
		}
		policy := str(spec["concurrencyPolicy"])
		if policy == "" {
			policy = "Allow"
		}
		lastSchedule := "-"
		if s := str(st["lastScheduleTime"]); s != "" {
			lastSchedule = age(st["lastScheduleTime"])
		}
		base["schedule"] = str(spec["schedule"])
		base["suspend"] = suspend
		base["active"] = len(activeJobs)
		base["last_schedule"] = lastSchedule
		base["concurrency_policy"] = policy
		base["image"] = firstImage(map[string]any{"template": jtSpec["template"]})

	case "hpa":
		spec, _ := obj["spec"].(map[string]any)
		st, _ := obj["status"].(map[string]any)
		conds, _ := st["conditions"].([]any)
		// minReplicas 可空，K8s 默认 1
		minR := 1
		if v := spec["minReplicas"]; v != nil {
			minR = toInt(v)
		}
		target := "-"
		if ref, _ := spec["scaleTargetRef"].(map[string]any); ref != nil {
			if n := str(ref["name"]); n != "" {
				target = n
				if k := str(ref["kind"]); k != "" {
					target = k + "/" + n
				}
			}
		}
		phase := "Unknown"
		for _, c := range conds {
			cm, _ := c.(map[string]any)
			if str(cm["type"]) == "ScalingActive" {
				if str(cm["status"]) == "True" {
					phase = "Active"
				} else {
					phase = "Inactive"
				}
			}
		}
		base["target"] = target
		base["min_replicas"] = minR
		base["max_replicas"] = toInt(spec["maxReplicas"])
		base["current_replicas"] = toInt(st["currentReplicas"])
		base["desired_replicas"] = toInt(st["desiredReplicas"])
		base["metrics_str"] = hpaMetricsStr(spec["metrics"])
		base["status_phase"] = phase

	case "scaledobject", "scaledjob":
		anns, _ := meta["annotations"].(map[string]any)
		// KEDA ≥2.10：pause = 注解 autoscaling.keda.sh/paused-replicas 存在
		paused := false
		if _, ok := anns["autoscaling.keda.sh/paused-replicas"]; ok {
			paused = true
		}
		spec, _ := obj["spec"].(map[string]any)
		st, _ := obj["status"].(map[string]any)
		conds, _ := st["conditions"].([]any)
		phase := "Unknown"
		for _, c := range conds {
			cm, _ := c.(map[string]any)
			if str(cm["type"]) == "Ready" {
				if str(cm["status"]) == "True" {
					phase = "Ready"
				} else {
					phase = "Error"
				}
			}
		}
		base["triggers_str"] = triggerTypes(spec["triggers"])
		base["paused"] = paused
		base["status_phase"] = phase
		if kind == "scaledobject" {
			ref, _ := spec["scaleTargetRef"].(map[string]any)
			target := "-"
			if n := str(ref["name"]); n != "" {
				target = n
			}
			minR := 1
			if v := spec["minReplicaCount"]; v != nil {
				minR = toInt(v)
			}
			base["target"] = target
			base["min_replicas"] = minR
			base["max_replicas"] = toInt(spec["maxReplicaCount"])
		} else {
			base["max_replica_count"] = toInt(spec["maxReplicaCount"])
		}

	case "service":
		spec, _ := obj["spec"].(map[string]any)
		ports, _ := spec["ports"].([]any)
		parts := make([]string, 0, len(ports))
		for _, p := range ports {
			pm, _ := p.(map[string]any)
			proto := str(pm["protocol"])
			if proto == "" {
				proto = "TCP"
			}
			seg := strconv.Itoa(toInt(pm["port"]))
			if np := toInt(pm["nodePort"]); np != 0 {
				seg += ":" + strconv.Itoa(np)
			}
			seg += "/" + proto
			parts = append(parts, seg)
		}
		out := "-"
		if len(parts) > 0 {
			out = strings.Join(parts, ", ")
		}
		svcType := str(spec["type"])
		if svcType == "" {
			svcType = "-"
		}
		cip := str(spec["clusterIP"])
		if cip == "" {
			cip = "-"
		}
		base["type"] = svcType
		base["cluster_ip"] = cip
		base["ports"] = out

	case "configmap":
		data, _ := obj["data"].(map[string]any)
		base["key_count"] = len(data)
		base["keys"] = sortedKeys(data)

	case "secret":
		data, _ := obj["data"].(map[string]any)
		st := str(obj["type"])
		if st == "" {
			st = "Opaque"
		}
		base["type"] = st
		base["key_count"] = len(data)
		base["keys"] = sortedKeys(data)

	case "ingress":
		spec, _ := obj["spec"].(map[string]any)
		rules, _ := spec["rules"].([]any)
		var parts []string
		for _, r := range rules {
			rm, _ := r.(map[string]any)
			host := str(rm["host"])
			if host == "" {
				host = "*"
			}
			http, _ := rm["http"].(map[string]any)
			paths, _ := http["paths"].([]any)
			for _, p := range paths {
				pm, _ := p.(map[string]any)
				path := str(pm["path"])
				if path == "" {
					path = "/"
				}
				svcName, svcPort := "-", "-"
				backend, _ := pm["backend"].(map[string]any)
				if backend != nil {
					svc, _ := backend["service"].(map[string]any)
					if svc != nil {
						if s := str(svc["name"]); s != "" {
							svcName = s
						}
						if port, _ := svc["port"].(map[string]any); port != nil {
							if n := toInt(port["number"]); n != 0 {
								svcPort = strconv.Itoa(n)
							}
						}
					}
				}
				parts = append(parts, fmt.Sprintf("%s%s → %s:%s", host, path, svcName, svcPort))
			}
		}
		out := "-"
		if len(parts) > 0 {
			out = strings.Join(parts, "; ")
		}
		cls := str(spec["ingressClassName"])
		if cls == "" {
			cls = "-"
		}
		base["rules_str"] = out
		base["class_name"] = cls

	case "persistentvolumeclaim":
		spec, _ := obj["spec"].(map[string]any)
		st, _ := obj["status"].(map[string]any)
		storage := "-"
		if req, _ := spec["resources"].(map[string]any); req != nil {
			if reqs, _ := req["requests"].(map[string]any); reqs != nil {
				if s := str(reqs["storage"]); s != "" && s != "0" {
					storage = s
				}
			}
		}
		phase := "Unknown"
		if p := str(st["phase"]); p != "" {
			phase = p
		}
		ams, _ := spec["accessModes"].([]any)
		amList := make([]string, 0, len(ams))
		for _, a := range ams {
			amList = append(amList, str(a))
		}
		sc := str(spec["storageClassName"])
		if sc == "" {
			sc = "-"
		}
		base["status_phase"] = phase
		base["storage"] = storage
		base["access_modes"] = strings.Join(amList, ", ")
		base["storage_class"] = sc
	}

	return base
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstImage(spec map[string]any) string {
	tmpl, _ := spec["template"].(map[string]any)
	if tmpl == nil {
		return "-"
	}
	podSpec, _ := tmpl["spec"].(map[string]any)
	if podSpec == nil {
		return "-"
	}
	containers, _ := podSpec["containers"].([]any)
	if len(containers) == 0 {
		return "-"
	}
	cm, _ := containers[0].(map[string]any)
	if img := str(cm["image"]); img != "" {
		return img
	}
	return "-"
}

// tsLocal 把 RFC3339 时间戳转成本地 "2006-01-02 15:04"（对齐 Python
// tz.localtime(ts).strftime('%Y-%m-%d %H:%M')）。
func tsLocal(v any) string {
	t := parseTS(v)
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func tsLocalSec(v any) string {
	t := parseTS(v)
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// TsLocalSec 暴露给 describe 等包外场景。
func TsLocalSec(v any) string { return tsLocalSec(v) }

func parseTS(v any) time.Time {
	s, _ := v.(string)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// age 复刻 Python 版的 kubectl 风格时长：3d4h / 3d / 5h3m / 5h / 7m / 30s。
func age(v any) string {
	t := parseTS(v)
	if t.IsZero() {
		return "-"
	}
	total := int(time.Since(t).Seconds())
	if total < 0 {
		return "0s"
	}
	days := total / 86400
	hours := (total % 86400) / 3600
	minutes := (total % 3600) / 60
	switch {
	case days > 0:
		if hours > 0 {
			return fmt.Sprintf("%dd%dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	case hours > 0:
		if minutes > 0 {
			return fmt.Sprintf("%dh%dm", hours, minutes)
		}
		return fmt.Sprintf("%dh", hours)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	default:
		return fmt.Sprintf("%ds", total)
	}
}

// AgeStr 暴露 age 计算（节点详情等直接用原始 creationTimestamp 拼装）。
func AgeStr(v any) string { return age(v) }

// hpaMetricsStr 把 spec.metrics 压成 "cpu 80%, memory 70%" 式短串；
// Resource 型带目标值，其余（External/Pods/Object 等）只列类型名。
func hpaMetricsStr(v any) string {
	list, _ := v.([]any)
	parts := make([]string, 0, len(list))
	for _, m := range list {
		mm, _ := m.(map[string]any)
		switch t := str(mm["type"]); t {
		case "Resource":
			res, _ := mm["resource"].(map[string]any)
			name := str(res["name"])
			tgt, _ := res["target"].(map[string]any)
			switch {
			case toInt(tgt["averageUtilization"]) != 0:
				parts = append(parts, fmt.Sprintf("%s %d%%", name, toInt(tgt["averageUtilization"])))
			case str(tgt["averageValue"]) != "":
				parts = append(parts, name+" "+str(tgt["averageValue"]))
			default:
				parts = append(parts, name)
			}
		case "":
		default:
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// triggerTypes 把 KEDA triggers 压成 "kafka, cron" 式短串。
func triggerTypes(v any) string {
	list, _ := v.([]any)
	parts := make([]string, 0, len(list))
	for _, t := range list {
		tm, _ := t.(map[string]any)
		if s := str(tm["type"]); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// durationStr 计算起止时间差并按 kubectl 风格时长输出；任一端缺失返回 "-"。
// 用于 Job 的 DURATION 列（startTime → completionTime）。
func durationStr(startV, endV any) string {
	start, end := parseTS(startV), parseTS(endV)
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return "-"
	}
	total := int(end.Sub(start).Seconds())
	if total < 0 {
		return "-"
	}
	days := total / 86400
	hours := (total % 86400) / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60
	switch {
	case days > 0:
		if hours > 0 {
			return fmt.Sprintf("%dd%dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	case hours > 0:
		if minutes > 0 {
			return fmt.Sprintf("%dh%dm", hours, minutes)
		}
		return fmt.Sprintf("%dh", hours)
	case minutes > 0:
		if seconds > 0 {
			return fmt.Sprintf("%dm%ds", minutes, seconds)
		}
		return fmt.Sprintf("%dm", minutes)
	default:
		return fmt.Sprintf("%ds", total)
	}
}

// truncate 按 rune 截断（Python 的 [:200] 是按字符切的，Go 直接切 byte 会切出坏 UTF-8）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
