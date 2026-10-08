// 资源页面与资源 API：列表（读缓存）、describe、revisions、scale/restart/
// rollback、YAML 查看/应用/校验、删除、ns 创建/删除/强制完成、终端。
// 对应 Django resources/views.py。
package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"sailor/internal/resops"
	"sailor/internal/serialize"
	"sailor/internal/store"
)

func serializeOne(kind string, obj map[string]interface{}) map[string]interface{} {
	return serialize.Item(kind, obj)
}

var kindPlurals = map[string]string{
	"namespace": "namespaces", "deployment": "deployments", "statefulset": "statefulsets",
	"daemonset": "daemonsets", "pod": "pods", "service": "services", "ingress": "ingresses",
	"configmap": "configmaps", "secret": "secrets", "persistentvolumeclaim": "pvcs",
	"job": "jobs", "cronjob": "cronjobs",
	"hpa": "hpas", "scaledobject": "scaledobjects", "scaledjob": "scaledjobs",
}

// resourceKinds 页面路由里允许的资源 kind。
var resourceKinds = map[string]string{
	"namespaces": "namespace", "deployments": "deployment", "statefulsets": "statefulset",
	"daemonsets": "daemonset", "pods": "pod", "services": "service", "ingresses": "ingress",
	"configmaps": "configmap", "secrets": "secret", "pvcs": "persistentvolumeclaim",
	"jobs": "job", "cronjobs": "cronjob",
	"hpas": "hpa", "scaledobjects": "scaledobject", "scaledjobs": "scaledjob",
}

type resourcePageContent struct {
	Cluster  *store.Cluster
	Kind     string // 内部 kind
	Plural   string
	APIURL   string
	ApplyURL string
}

func (s *Server) pageResourceList(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	plural := r.PathValue("kind")
	kind, ok := resourceKinds[plural]
	if !ok {
		http.NotFound(w, r)
		return
	}
	content := &resourcePageContent{
		Cluster:  c,
		Kind:     kind,
		Plural:   plural,
		APIURL:   fmt.Sprintf("/resources/%d/api/%s/", c.ID, plural),
		ApplyURL: fmt.Sprintf("/resources/%d/apply/", c.ID),
	}
	title := map[string]string{
		"namespaces": "命名空间", "deployments": "Deployments", "statefulsets": "StatefulSets",
		"daemonsets": "DaemonSets", "pods": "Pods", "services": "Services", "ingresses": "Ingresses",
		"configmaps": "ConfigMaps", "secrets": "Secrets", "pvcs": "PersistentVolumeClaims",
		"jobs": "Jobs", "cronjobs": "CronJobs",
		"hpas": "HPA", "scaledobjects": "ScaledObjects (KEDA)", "scaledjobs": "ScaledJobs (KEDA)",
	}[plural]
	s.renderPage(w, r, "resource_"+kind, "", title, content)
}

// ─── 列表 API（读缓存）────────────────────────────────────────

const cacheTTL = 120 * time.Second

var nsCascadeKinds = []string{
	"deployment", "statefulset", "daemonset", "pod",
	"service", "ingress", "configmap", "secret", "persistentvolumeclaim",
	"job", "cronjob", "hpa", "scaledobject", "scaledjob",
}

// purgeNamespaceFromCache 删 ns 后立即剔除该 ns 的资源条目（对齐 Python 版）。
func (s *Server) purgeNamespaceFromCache(clusterID int64, nsName string) {
	for _, kind := range nsCascadeKinds {
		entry := s.Store.GetCache(clusterID, kind)
		if entry == nil {
			continue
		}
		var rows []map[string]interface{}
		if json.Unmarshal(entry.Data, &rows) != nil {
			continue
		}
		out := rows[:0]
		for _, row := range rows {
			if ns, _ := row["namespace"].(string); ns != nsName {
				out = append(out, row)
			}
		}
		if len(out) != len(rows) {
			b, _ := json.Marshal(out)
			_ = s.Store.SaveCache(clusterID, kind, b)
		}
	}
}

func (s *Server) resourceListAPI(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	plural := r.PathValue("kind")
	kind, ok := resourceKinds[plural]
	if !ok {
		JSONError(w, 404, "unknown resource type")
		return
	}
	nsFilter := r.URL.Query().Get("namespace")
	forceRefresh := r.URL.Query().Get("refresh") == "1"

	// KEDA 是 CRD，集群可能没装：syncer 探测后写 marker 缓存，这里读出来
	// 透传给前端展示"未检测到 KEDA"空态（marker 不存在 = 尚未完成首轮同步）
	kedaPlural := plural == "scaledobjects" || plural == "scaledjobs"
	kedaFlag := func() map[string]interface{} {
		if !kedaPlural {
			return nil
		}
		marker := s.Store.GetCache(c.ID, "keda")
		if marker == nil {
			return nil
		}
		var m struct {
			Installed bool `json:"installed"`
		}
		if json.Unmarshal(marker.Data, &m) != nil {
			return nil
		}
		return map[string]interface{}{"optional_installed": m.Installed}
	}

	try := func() {
		if forceRefresh {
			s.Syncer.TriggerImmediate(c, kind, true, 3*time.Second)
		}
		cacheObj := s.Store.GetCache(c.ID, kind)
		nsCache := s.Store.GetCache(c.ID, "namespace")
		syncErr := s.Store.GetSyncError(c.ID)

		terminatingNS := map[string]bool{}
		if nsCache != nil {
			var nsRows []map[string]interface{}
			if json.Unmarshal(nsCache.Data, &nsRows) == nil {
				for _, n := range nsRows {
					if st, _ := n["status"].(string); st == "Terminating" {
						if name, _ := n["name"].(string); name != "" {
							terminatingNS[name] = true
						}
					}
				}
			}
		}

		fresh := cacheObj != nil && time.Since(cacheObj.SyncedAt) < cacheTTL
		if !fresh {
			// 缓存未命中：触发同步并返回 syncing 状态
			go s.Syncer.TriggerImmediate(c, kind, false, 0)
			if nsCache == nil {
				go s.Syncer.TriggerImmediate(c, "namespace", false, 0)
			}
			resp := map[string]interface{}{
				"resources": []interface{}{}, "namespaces": []interface{}{},
				"syncing": true, "cluster_error": syncErr, "never_synced": cacheObj == nil,
			}
			for k, v := range kedaFlag() {
				resp[k] = v
			}
			JSON(w, 200, resp)
			return
		}

		var rows []map[string]interface{}
		json.Unmarshal(cacheObj.Data, &rows)
		if rows == nil {
			rows = []map[string]interface{}{}
		}
		if len(terminatingNS) > 0 {
			filtered := rows[:0]
			for _, row := range rows {
				if ns, _ := row["namespace"].(string); !terminatingNS[ns] {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
		}

		nsSet := map[string]bool{}
		for _, row := range rows {
			if ns, _ := row["namespace"].(string); ns != "" {
				nsSet[ns] = true
			}
		}
		if nsFilter != "" {
			nsSet[nsFilter] = true // 当前选中的 ns 保留在下拉里
		}
		nsList := make([]string, 0, len(nsSet))
		for ns := range nsSet {
			nsList = append(nsList, ns)
		}
		sortStrings(nsList)

		if nsFilter != "" {
			filtered := rows[:0]
			for _, row := range rows {
				if ns, _ := row["namespace"].(string); ns == nsFilter {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
		}

		resp := map[string]interface{}{
			"resources":     rows,
			"namespaces":    nsList,
			"cached":        true,
			"synced_at":     cacheObj.SyncedAt.Format(time.RFC3339),
			"cluster_error": syncErr,
		}
		for k, v := range kedaFlag() {
			resp[k] = v
		}
		JSON(w, 200, resp)
	}
	try()
}

// serializeResource 读最新状态并序列化（写操作返回给前端做乐观更新）。
func (s *Server) serializeResource(o *resops.Ops, kind, name, namespace string) map[string]interface{} {
	obj, err := o.GetResource(context.Background(), kind, name, namespace)
	if err != nil {
		return nil
	}
	return serializeOne(kind, obj.Object)
}

// ─── Namespace 操作 ──────────────────────────────────────────

func (s *Server) namespaceCreate(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	name := trimSpace(r.FormValue("name"))
	if name == "" {
		JSONError(w, 400, "Name is required")
		return
	}
	o := s.opsFor(c.ID, loader)
	ns := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]interface{}{"name": name},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	created, err := o.CreateRaw(ctx, ns)
	if err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	// wait=true 保证 ns cache 立即刷新
	s.Syncer.TriggerImmediate(c, "namespace", true, 10*time.Second)

	createdMeta, _ := created["metadata"].(map[string]any)
	status := "Active"
	if st, ok := created["status"].(map[string]any); ok {
		if p, _ := st["phase"].(string); p != "" {
			status = p
		}
	}
	createdStr := ""
	if ts, _ := createdMeta["creationTimestamp"].(string); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			createdStr = t.Local().Format("2006-01-02 15:04")
		}
	}
	JSON(w, 200, map[string]interface{}{
		"success": true,
		"resource": map[string]interface{}{
			"name": name, "namespace": "", "status": status,
			"created": createdStr, "age": "0s",
		},
	})
}

func (s *Server) namespaceDelete(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	name := r.PathValue("name")
	o := s.opsFor(c.ID, loader)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := o.Delete(ctx, "namespace", name, "", false); err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	s.purgeNamespaceFromCache(c.ID, name)
	s.Syncer.TriggerImmediate(c, "namespace", true, 10*time.Second)
	for _, kind := range nsCascadeKinds {
		go s.Syncer.TriggerImmediate(c, kind, false, 0)
	}
	JSON(w, 200, map[string]interface{}{"success": true})
}

func (s *Server) namespaceForceFinalize(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	name := r.PathValue("name")
	o := s.opsFor(c.ID, loader)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	existed, err := o.ForceFinalizeNamespace(ctx, name)
	if err != nil {
		JSONError(w, 500, fmt.Sprintf("强制完成失败：%v", err))
		return
	}
	s.purgeNamespaceFromCache(c.ID, name)
	s.Syncer.TriggerImmediate(c, "namespace", true, 10*time.Second)
	for _, kind := range nsCascadeKinds {
		go s.Syncer.TriggerImmediate(c, kind, false, 0)
	}
	msg := ""
	if !existed {
		msg = "Namespace 已不存在（K8s 已清理完成），列表即将刷新"
	}
	JSON(w, 200, map[string]interface{}{"success": true, "message": msg})
}

// ─── describe / revisions ────────────────────────────────────

var describeKinds = map[string]string{
	"deployments": "deployment", "statefulsets": "statefulset",
	"daemonsets": "daemonset", "services": "service",
}

func (s *Server) resourceDescribeAPI(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	plural := r.PathValue("kind")
	kind, ok := describeKinds[plural]
	if !ok {
		JSONError(w, 404, "unknown resource type")
		return
	}
	o := s.opsFor(c.ID, loader)
	ns, name := r.PathValue("ns"), r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var d *resops.Describe
	var err error
	switch kind {
	case "deployment":
		d, err = o.DescribeDeployment(ctx, name, ns)
	case "statefulset":
		d, err = o.DescribeStatefulset(ctx, name, ns)
	case "daemonset":
		d, err = o.DescribeDaemonset(ctx, name, ns)
	case "service":
		d, err = o.DescribeService(ctx, name, ns)
	}
	if err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	JSON(w, 200, d)
}

var revisionKinds = map[string]string{
	"deployments": "deployment", "statefulsets": "statefulset", "daemonsets": "daemonset",
}

func (s *Server) resourceRevisionsAPI(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	kind, ok := revisionKinds[r.PathValue("kind")]
	if !ok {
		JSONError(w, 404, "unknown resource type")
		return
	}
	o := s.opsFor(c.ID, loader)
	ns, name := r.PathValue("ns"), r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var list *resops.RevisionList
	var err error
	switch kind {
	case "deployment":
		list, err = o.ListDeploymentRevisions(ctx, name, ns)
	case "statefulset":
		list, err = o.ListStatefulsetRevisions(ctx, name, ns)
	case "daemonset":
		list, err = o.ListDaemonsetRevisions(ctx, name, ns)
	}
	if err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	JSON(w, 200, list)
}

// ─── scale / restart / rollback ──────────────────────────────

var scaleKinds = map[string]bool{"deployments": true, "statefulsets": true}

func (s *Server) resourceScale(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	plural := r.PathValue("kind")
	if !scaleKinds[plural] {
		JSONError(w, 404, "unknown resource type")
		return
	}
	kind := resourceKinds[plural]
	var body struct {
		Replicas int `json:"replicas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		JSONError(w, 400, err.Error())
		return
	}
	o := s.opsFor(c.ID, loader)
	ns, name := r.PathValue("ns"), r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := o.Scale(ctx, kind, name, ns, int32(body.Replicas)); err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	s.Syncer.TriggerImmediate(c, kind, true, 10*time.Second)
	JSON(w, 200, map[string]interface{}{"success": true, "resource": s.serializeResource(o, kind, name, ns)})
}

var restartKinds = map[string]string{"deployments": "deployment", "statefulsets": "statefulset", "daemonsets": "daemonset"}

func (s *Server) resourceRestart(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	kind, ok := restartKinds[r.PathValue("kind")]
	if !ok {
		JSONError(w, 404, "unknown resource type")
		return
	}
	o := s.opsFor(c.ID, loader)
	ns, name := r.PathValue("ns"), r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := o.Restart(ctx, kind, name, ns); err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	s.Syncer.TriggerImmediate(c, kind, true, 10*time.Second)
	go s.Syncer.TriggerImmediate(c, "pod", false, 0)
	JSON(w, 200, map[string]interface{}{"success": true, "resource": s.serializeResource(o, kind, name, ns)})
}

func (s *Server) resourceRollback(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	kind, ok := restartKinds[r.PathValue("kind")]
	if !ok {
		JSONError(w, 404, "unknown resource type")
		return
	}
	var body struct {
		Revision json.RawMessage `json:"revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Revision) == 0 {
		JSONError(w, 400, "revision is required")
		return
	}
	o := s.opsFor(c.ID, loader)
	ns, name := r.PathValue("ns"), r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var err error
	revStr := strings.Trim(string(body.Revision), `"`)
	switch kind {
	case "deployment":
		err = o.RollbackDeployment(ctx, name, ns, revStr)
	case "statefulset":
		var n int64
		fmt.Sscanf(revStr, "%d", &n)
		err = o.RollbackStatefulset(ctx, name, ns, n)
	case "daemonset":
		var n int64
		fmt.Sscanf(revStr, "%d", &n)
		err = o.RollbackDaemonset(ctx, name, ns, n)
	}
	if err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	s.Syncer.TriggerImmediate(c, kind, true, 10*time.Second)
	go s.Syncer.TriggerImmediate(c, "pod", false, 0)
	JSON(w, 200, map[string]interface{}{
		"success":  true,
		"resource": s.serializeResource(o, kind, name, ns),
		"message":  fmt.Sprintf("已回滚到 revision %s", revStr),
	})
}

// ─── cronjob 挂起/恢复 / 立即触发 ─────────────────────────────

func (s *Server) cronjobSuspend(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var body struct {
		Suspend bool `json:"suspend"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		JSONError(w, 400, err.Error())
		return
	}
	o := s.opsFor(c.ID, loader)
	ns, name := r.PathValue("ns"), r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	patch := map[string]any{"spec": map[string]any{"suspend": body.Suspend}}
	if err := o.PatchMerge(ctx, "cronjob", name, ns, patch); err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	s.Syncer.TriggerImmediate(c, "cronjob", true, 10*time.Second)
	JSON(w, 200, map[string]interface{}{"success": true, "resource": s.serializeResource(o, "cronjob", name, ns)})
}

// cronjobTrigger 立即触发一次：拷贝 jobTemplate.spec 生成一次性 Job
// （等价于 kubectl create job --from=cronjob/x），命名加 manual-<5位> 后缀防撞。
func (s *Server) cronjobTrigger(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	o := s.opsFor(c.ID, loader)
	ns, name := r.PathValue("ns"), r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cj, err := o.GetResource(ctx, "cronjob", name, ns)
	if err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	cjSpec, _ := cj.Object["spec"].(map[string]any)
	jt, _ := cjSpec["jobTemplate"].(map[string]any)
	jtSpec, _ := jt["spec"].(map[string]any)
	if jtSpec == nil {
		JSONError(w, 500, "cronjob 缺少 jobTemplate.spec，无法创建 Job")
		return
	}
	jobName := fmt.Sprintf("%s-manual-%05d", name, time.Now().UnixNano()%100000)
	job := map[string]interface{}{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]interface{}{"name": jobName, "namespace": ns},
		"spec":       jtSpec,
	}
	if _, err := o.CreateRaw(ctx, job); err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	s.Syncer.TriggerImmediate(c, "job", true, 10*time.Second)
	go s.Syncer.TriggerImmediate(c, "cronjob", false, 0)
	JSON(w, 200, map[string]interface{}{"success": true, "message": "已创建 Job " + jobName})
}

// ─── KEDA ScaledObject / ScaledJob 挂起与恢复 ─────────────────
// KEDA ≥2.10 的 pause 语义是注解 autoscaling.keda.sh/paused-replicas：
// 存在即挂起（值即挂起期副本数，惯例 0），删除即恢复。恢复用 JSON Merge
// Patch 的 null 删 key（RFC 7386），不用整体替换 annotations。

const kedaPauseAnnotation = "autoscaling.keda.sh/paused-replicas"

func (s *Server) scaledPause(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
		if !ok {
			return
		}
		var body struct {
			Pause bool `json:"pause"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			JSONError(w, 400, err.Error())
			return
		}
		o := s.opsFor(c.ID, loader)
		ns, name := r.PathValue("ns"), r.PathValue("name")
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		var annVal any // nil → JSON null → 删除注解
		if body.Pause {
			annVal = "0"
		}
		patch := map[string]any{
			"metadata": map[string]any{
				"annotations": map[string]any{kedaPauseAnnotation: annVal},
			},
		}
		if err := o.PatchMerge(ctx, kind, name, ns, patch); err != nil {
			JSONError(w, 500, err.Error())
			return
		}
		s.Syncer.TriggerImmediate(c, kind, true, 10*time.Second)
		msg := "已恢复调度"
		if body.Pause {
			msg = "已挂起（副本固定为 0，删除注解后恢复）"
		}
		JSON(w, 200, map[string]interface{}{"success": true, "message": msg, "resource": s.serializeResource(o, kind, name, ns)})
	}
}

// ─── YAML 查看 / 应用 / 校验 / 删除 ───────────────────────────

func (s *Server) resourceYAML(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	ns := r.PathValue("ns") // cluster-scoped 时为空
	o := s.opsFor(c.ID, loader)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	y, err := o.GetResourceYAML(ctx, kind, name, ns)
	if err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	JSON(w, 200, map[string]interface{}{"yaml": y})
}

func (s *Server) resourceValidateAPI(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	o := s.opsFor(c.ID, loader)
	var body struct {
		YAML string `json:"yaml"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body)
	if trimSpace(body.YAML) == "" {
		JSON(w, 200, map[string]interface{}{"success": false, "errors": []map[string]string{{"kind": "-", "name": "-", "message": "YAML 内容不能为空"}}, "warnings": []interface{}{}, "docs": 0})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	JSON(w, 200, o.Validate(ctx, body.YAML))
}

func (s *Server) resourceApplyAPI(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	o := s.opsFor(c.ID, loader)
	var body struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	if trimSpace(body.YAML) == "" {
		JSONError(w, 400, "YAML 内容不能为空")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result := o.Apply(ctx, body.YAML)
	if !result.Success {
		JSON(w, 400, result)
		return
	}
	// 第一个同 kind 用 wait=True，其余异步（对齐 Python 版的 seen_waited 逻辑）
	seenWaited := map[string]bool{}
	for _, action := range result.Actions {
		if action.Kind == "" {
			continue
		}
		if !seenWaited[action.Kind] {
			s.Syncer.TriggerImmediate(c, action.Kind, true, 10*time.Second)
			seenWaited[action.Kind] = true
		} else {
			go s.Syncer.TriggerImmediate(c, action.Kind, false, 0)
		}
	}
	JSON(w, 200, result)
}

var cascadePodKinds = map[string]bool{
	"deployment": true, "statefulset": true, "daemonset": true, "job": true, "cronjob": true,
}

func (s *Server) resourceDelete(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	ns := r.PathValue("ns")
	force := r.URL.Query().Get("force") == "1" || r.FormValue("force") == "1"

	o := s.opsFor(c.ID, loader)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := o.Delete(ctx, kind, name, ns, force); err != nil {
		JSONError(w, 500, err.Error())
		return
	}
	s.Syncer.TriggerImmediate(c, kind, true, 10*time.Second)
	if cascadePodKinds[kind] {
		go s.Syncer.TriggerImmediate(c, "pod", false, 0)
	}
	JSON(w, 200, map[string]interface{}{"success": true})
}

// ─── 终端（真实 WebSocket 直通）──────────────────────────────

func (s *Server) podExecOpen(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	container := r.FormValue("container")
	info, status, err := s.Exec.Open(c.ID, loader, r.PathValue("ns"), r.PathValue("pod"), container)
	if err != nil {
		JSONError(w, status, err.Error())
		return
	}
	JSON(w, 200, map[string]interface{}{
		"success":   true,
		"session":   info.Session,
		"container": info.Container,
		"ws_url":    info.WSTemplate,
	})
}

func (s *Server) podExecClose(w http.ResponseWriter, r *http.Request) {
	// 会话生命周期由 WS 连接断开与空闲回收兜底；保留端点维持前端兼容
	JSON(w, 200, map[string]interface{}{"success": true})
}

// podDebugAdd 注入临时调试容器（ephemeral container）并等待其 Running，
// 返回容器名供前端直接开终端。镜像现场拉取，整体给到 100s。
func (s *Server) podDebugAdd(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	ns, pod := r.PathValue("ns"), r.PathValue("pod")
	var body struct {
		Image  string `json:"image"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || trimSpace(body.Image) == "" {
		JSONError(w, 400, "镜像不能为空")
		return
	}
	image := trimSpace(body.Image)
	if strings.ContainsAny(image, " \t\"'") {
		JSONError(w, 400, "镜像名不合法")
		return
	}

	o := s.opsFor(c.ID, loader)
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()

	name, err := o.AddDebugContainer(ctx, ns, pod, image, trimSpace(body.Target))
	if err != nil {
		JSONError(w, 500, fmt.Sprintf("注入调试容器失败：%v", err))
		return
	}
	if err := o.WaitDebugContainerRunning(ctx, ns, pod, name, 80*time.Second); err != nil {
		// 容器已经写进 Pod（删不掉），把容器名带回去让用户稍后手动 exec
		JSON(w, 500, map[string]interface{}{
			"success":   false,
			"error":     fmt.Sprintf("%v（容器 %s 已注入，可稍后在 kubectl 终端 exec -c %s 重试）", err, name, name),
			"container": name,
		})
		return
	}
	JSON(w, 200, map[string]interface{}{"success": true, "container": name})
}

// kshOpen 打开内置 kubectl 终端会话（返回一次性 token 的 WS 地址）。
func (s *Server) kshOpen(w http.ResponseWriter, r *http.Request) {
	c, loader, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	info, status, err := s.Ksh.Open(c.ID, c.Display(), loader)
	if err != nil {
		JSONError(w, status, err.Error())
		return
	}
	JSON(w, 200, map[string]interface{}{
		"success": true,
		"session": info.Session,
		"cluster": info.Cluster,
		"ws_url":  info.WSTemplate,
	})
}

// kshAttach 页面跳转 / 切换集群后恢复既有会话（会话在 Go 侧保持存活）。
func (s *Server) kshAttach(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.clusterCtx(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var body struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Session == "" {
		JSONError(w, 400, "session is required")
		return
	}
	info, status, err := s.Ksh.Attach(c.ID, body.Session)
	if err != nil {
		JSONError(w, status, err.Error())
		return
	}
	JSON(w, 200, map[string]interface{}{
		"success": true,
		"session": info.Session,
		"cluster": info.Cluster,
		"ws_url":  info.WSTemplate,
	})
}

// kshClose 显式关闭会话（抽屉关闭按钮；WS 断开不会关会话）。
func (s *Server) kshClose(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Session == "" {
		JSONError(w, 400, "session is required")
		return
	}
	s.Ksh.Close(body.Session)
	JSON(w, 200, map[string]interface{}{"success": true})
}
