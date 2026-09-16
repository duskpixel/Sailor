// Package resops 移植 Django 版 resources/k8s_resources.py 的资源操作：
// 读 YAML / apply / dry-run 校验 / 删除 / 扩缩 / 重启 / 回滚 / describe。
//
// 全部走 dynamic client（unstructured），一张 GVR 表对应 Python 版的
// RESOURCE_TYPES 字典；JSON Merge Patch 回滚等语义逐一对齐。
package resops

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"sailor/internal/k8sx"
	"sailor/internal/serialize"
)

// ErrNotFound 表示资源不存在（404），HTTP 层映射为特定提示。
type ErrNotFound struct{ Kind, Name string }

func (e *ErrNotFound) Error() string {
	return fmt.Sprintf("%s '%s' not found", strings.Title(e.Kind), e.Name)
}

// Ops 绑定一个集群的资源操作集合。
type Ops struct {
	ClusterID int64
	Pool      *k8sx.Pool
	Loader    func() (string, error) // kubeconfig 懒加载
}

func (o *Ops) dyn() (dynamic.Interface, error) {
	cl, err := o.Pool.Get(o.ClusterID, o.Loader)
	if err != nil {
		return nil, err
	}
	return cl.Dynamic, nil
}

func gvr(kind string) (schema.GroupVersionResource, error) {
	if g, ok := k8sx.GVRs[kind]; ok {
		return g, nil
	}
	return schema.GroupVersionResource{}, fmt.Errorf("unsupported resource type: %s", kind)
}

func namespaced(kind string) bool {
	return kind != "namespace"
}

// ─── 单资源读取 ───────────────────────────────────────────────

func (o *Ops) GetResource(ctx context.Context, kind, name, namespace string) (*unstructured.Unstructured, error) {
	g, err := gvr(kind)
	if err != nil {
		return nil, err
	}
	dyn, err := o.dyn()
	if err != nil {
		return nil, err
	}
	var obj *unstructured.Unstructured
	var err2 error
	if namespaced(kind) {
		obj, err2 = dyn.Resource(g).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	} else {
		obj, err2 = dyn.Resource(g).Get(ctx, name, metav1.GetOptions{})
	}
	if apierrors.IsNotFound(err2) {
		return nil, &ErrNotFound{Kind: kind, Name: name}
	}
	return obj, err2
}

// 已剥离的 metadata 只读字段集合（YAML 查看 / apply / 校验共用）。
var serverManagedMeta = []string{
	"resourceVersion", "uid", "creationTimestamp", "generation",
	"managedFields", "selfLink", "deletionTimestamp",
	"deletionGracePeriodSeconds", "ownerReferences",
}

const podTemplateHashLabel = "pod-template-hash"

// GetResourceYAML 返回面向用户编辑的 YAML：剥掉 status 与服务端维护的
// metadata 字段、last-applied 注解、controller 自加的 pod-template-hash
// （等价于 kubectl edit 的处理，详见 Python 版注释）。
func (o *Ops) GetResourceYAML(ctx context.Context, kind, name, namespace string) (string, error) {
	obj, err := o.GetResource(ctx, kind, name, namespace)
	if err != nil {
		return "", err
	}
	doc := obj.UnstructuredContent()
	doc = deepCopyMap(doc)

	delete(doc, "status")
	if meta, ok := doc["metadata"].(map[string]any); ok {
		for _, k := range serverManagedMeta {
			delete(meta, k)
		}
		if anns, ok := meta["annotations"].(map[string]any); ok {
			delete(anns, "kubectl.kubernetes.io/last-applied-configuration")
			if len(anns) == 0 {
				delete(meta, "annotations")
			}
		}
	}
	if spec, ok := doc["spec"].(map[string]any); ok {
		if tmpl, ok := spec["template"].(map[string]any); ok {
			stripTemplateHash(tmpl)
		}
		if sel, ok := spec["selector"].(map[string]any); ok {
			if ml, ok := sel["matchLabels"].(map[string]any); ok {
				delete(ml, podTemplateHashLabel)
			}
		}
	}
	return toYAML(doc)
}

func stripTemplateHash(tmpl map[string]any) {
	if meta, ok := tmpl["metadata"].(map[string]any); ok {
		if labels, ok := meta["labels"].(map[string]any); ok {
			delete(labels, podTemplateHashLabel)
		}
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// stripServerManagedFields 移除服务端只读字段，返回给用户的警告（对应
// Python _strip_server_managed_fields）。
func stripServerManagedFields(doc map[string]any) []string {
	var warnings []string
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		doc["metadata"] = meta
	}
	for _, k := range serverManagedMeta {
		delete(meta, k)
	}
	if _, has := doc["status"]; has {
		warnings = append(warnings, "status 字段是 K8s 控制面维护的只读子资源，对它的修改不会生效，已自动忽略")
		delete(doc, "status")
	}
	if spec, ok := doc["spec"].(map[string]any); ok {
		if tmpl, ok := spec["template"].(map[string]any); ok {
			stripTemplateHash(tmpl)
		}
		if sel, ok := spec["selector"].(map[string]any); ok {
			if ml, ok := sel["matchLabels"].(map[string]any); ok {
				delete(ml, podTemplateHashLabel)
			}
		}
	}
	return warnings
}

// ─── 删除 ────────────────────────────────────────────────────

// Delete 删除资源。force 仅对 Pod 生效：跳过优雅退出期立即清除。
func (o *Ops) Delete(ctx context.Context, kind, name, namespace string, force bool) error {
	g, err := gvr(kind)
	if err != nil {
		return err
	}
	dyn, err := o.dyn()
	if err != nil {
		return err
	}
	opts := metav1.DeleteOptions{}
	if force && kind == "pod" {
		grace := int64(0)
		opts.GracePeriodSeconds = &grace
		opts.PropagationPolicy = func() *metav1.DeletionPropagation {
			p := metav1.DeletePropagationBackground
			return &p
		}()
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var err2 error
	if namespaced(kind) {
		err2 = dyn.Resource(g).Namespace(namespace).Delete(ctx2, name, opts)
	} else {
		err2 = dyn.Resource(g).Delete(ctx2, name, opts)
	}
	if apierrors.IsNotFound(err2) {
		return &ErrNotFound{Kind: kind, Name: name}
	}
	return err2
}

// ─── 扩缩容 / 重启 ────────────────────────────────────────────

// Scale 通过 scale 子资源扩缩容 Deployment / StatefulSet。
func (o *Ops) Scale(ctx context.Context, kind, name, namespace string, replicas int32) error {
	g, err := gvr(kind)
	if err != nil {
		return err
	}
	dyn, err := o.dyn()
	if err != nil {
		return err
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// dynamic client 的子资源 Update 走 "scale"
	scaleObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "autoscaling/v1",
			"kind":       "Scale",
			"metadata":   map[string]any{"name": name, "namespace": namespace},
			"spec":       map[string]any{"replicas": replicas},
		},
	}
	// dynamic client 的 Update 支持 scale 子资源 → PUT .../deployments/{name}/scale
	_, err = dyn.Resource(g).Namespace(namespace).Update(ctx2, scaleObj, metav1.UpdateOptions{}, "scale")
	if apierrors.IsNotFound(err) {
		return &ErrNotFound{Kind: kind, Name: name}
	}
	return err
}

func restartedAtPatch() []byte {
	body := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						"kubectl.kubernetes.io/restartedAt": time.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
					},
				},
			},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

// PatchMerge 对任意支持的资源做 JSON Merge Patch（cronjob 挂起/恢复等场景）。
func (o *Ops) PatchMerge(ctx context.Context, kind, name, namespace string, patch map[string]any) error {
	g, err := gvr(kind)
	if err != nil {
		return err
	}
	dyn, err := o.dyn()
	if err != nil {
		return err
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var ri dynamic.ResourceInterface = dyn.Resource(g)
	if namespaced(kind) {
		ri = dyn.Resource(g).Namespace(namespace)
	}
	_, err = ri.Patch(ctx2, name, types.MergePatchType, b, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return &ErrNotFound{Kind: kind, Name: name}
	}
	return err
}

// Restart 通过打 restartedAt 注解触发滚动更新。
func (o *Ops) Restart(ctx context.Context, kind, name, namespace string) error {
	g, err := gvr(kind)
	if err != nil {
		return err
	}
	dyn, err := o.dyn()
	if err != nil {
		return err
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = dyn.Resource(g).Namespace(namespace).Patch(ctx2, name, types.StrategicMergePatchType, restartedAtPatch(), metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return &ErrNotFound{Kind: kind, Name: name}
	}
	return err
}

// ─── Namespace 应急操作 ───────────────────────────────────────

// ForceFinalizeNamespace 清空 spec.finalizers 让 K8s 立即从 etcd 删除 namespace。
// 返回 (existed, error)：K8s 里已不存在时 existed=false。
func (o *Ops) ForceFinalizeNamespace(ctx context.Context, name string) (bool, error) {
	dyn, err := o.dyn()
	if err != nil {
		return false, err
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	ns, err := dyn.Resource(k8sx.GVRs["namespace"]).Get(ctx2, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	spec, _ := ns.Object["spec"].(map[string]any)
	if spec == nil || spec["finalizers"] == nil {
		return true, nil // 没有 finalizers，K8s 应该很快自己删掉
	}
	spec["finalizers"] = []any{}
	_, err = dyn.Resource(k8sx.GVRs["namespace"]).Update(ctx2, ns, metav1.UpdateOptions{}, "finalize")
	return true, err
}

// ─── Describe / 关联 Pods / Events ────────────────────────────

// listEventsForTree 聚合「控制器自身 + 关联 Pod」的 events，按时间倒序。
func (o *Ops) listEventsForTree(ctx context.Context, namespace, kind, name string, podNames []string) []map[string]any {
	dyn, err := o.dyn()
	if err != nil {
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	evs, err := dyn.Resource(k8sx.GVRs["event"]).Namespace(namespace).List(ctx2, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	podSet := map[string]bool{}
	for _, p := range podNames {
		podSet[p] = true
	}
	type evOut struct {
		m    map[string]any
		sort time.Time
	}
	var out []evOut
	for _, e := range evs.Items {
		io, _ := e.Object["involvedObject"].(map[string]any)
		if io == nil {
			continue
		}
		ioKind, ioName := str(io["kind"]), str(io["name"])
		var obj string
		if ioKind == kind && ioName == name {
			obj = kind + "/" + name
		} else if ioKind == "Pod" && podSet[ioName] {
			obj = "Pod/" + ioName
		} else {
			continue
		}
		lastTS := firstNonEmpty(
			e.Object["lastTimestamp"], e.Object["eventTime"],
			e.Object["firstTimestamp"], e.Object["creationTimestamp"],
		)
		cnt := 1
		if c, ok := e.Object["count"].(float64); ok {
			cnt = int(c)
		}
		source := "-"
		if src, ok := e.Object["source"].(map[string]any); ok {
			if s := str(src["component"]); s != "" {
				source = s
			}
		}
		out = append(out, evOut{
			sort: parseTS(lastTS),
			m: map[string]any{
				"type":           dfltstr(e.Object["type"]),
				"reason":         dfltstr(e.Object["reason"]),
				"message":        str(e.Object["message"]),
				"source":         source,
				"object":         obj,
				"count":          cnt,
				"last_timestamp": tsSec(lastTS),
			},
		})
	}
	// 按时间倒序
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].sort.After(out[j-1].sort); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	res := make([]map[string]any, len(out))
	for i, e := range out {
		res[i] = e.m
	}
	return res
}

// listPodsBySelector 按 label selector 拉取 Pod 并序列化。
func (o *Ops) listPodsBySelector(ctx context.Context, namespace string, matchLabels map[string]any) []map[string]any {
	if len(matchLabels) == 0 {
		return nil
	}
	return o.listPods(ctx, namespace, labelsSelector(matchLabels), "", func(p *unstructured.Unstructured) bool { return true })
}

// listPodsByOwner 按 ownerReference 过滤（StatefulSet / DaemonSet），先用
// label selector 在 API 层过滤，再用 owner 二次过滤保证准确。
func (o *Ops) listPodsByOwner(ctx context.Context, namespace, ownerKind, ownerName string, matchLabels map[string]any) []map[string]any {
	return o.listPods(ctx, namespace, labelsSelector(matchLabels), "", func(p *unstructured.Unstructured) bool {
		return hasOwner(p, ownerKind, ownerName)
	})
}

// listPodsForDeployment Deployment → ReplicaSet → Pod 两级 owner 过滤。
func (o *Ops) listPodsForDeployment(ctx context.Context, namespace, deployName string, matchLabels map[string]any) []map[string]any {
	dyn, err := o.dyn()
	if err != nil {
		return nil
	}
	sel := labelsSelector(matchLabels)
	if sel == "" {
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	rsList, err := dyn.Resource(k8sx.GVRs["replicaset"]).Namespace(namespace).List(ctx2, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	ownedRS := map[string]bool{}
	for _, rs := range rsList.Items {
		if hasOwner(&rs, "Deployment", deployName) {
			ownedRS[rs.GetName()] = true
		}
	}
	return o.listPods(ctx, namespace, sel, "", func(p *unstructured.Unstructured) bool {
		return hasOwner(p, "ReplicaSet", "") && ownedRS[ownerNameOf(p, "ReplicaSet")]
	})
}

// listPods 通用 Pod 拉取：label/field selector + 谓词过滤。
func (o *Ops) listPods(ctx context.Context, namespace, labelSelector, fieldSelector string, keep func(*unstructured.Unstructured) bool) []map[string]any {
	dyn, err := o.dyn()
	if err != nil {
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	opts := metav1.ListOptions{LabelSelector: labelSelector, FieldSelector: fieldSelector}
	list, err := dyn.Resource(k8sx.GVRs["pod"]).Namespace(namespace).List(ctx2, opts)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for i := range list.Items {
		if keep(&list.Items[i]) {
			out = append(out, serialize.Item("pod", list.Items[i].Object))
		}
	}
	return out
}

func labelsSelector(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+fmt.Sprintf("%v", v))
	}
	sortStrings(parts)
	return strings.Join(parts, ",")
}

func hasOwner(obj *unstructured.Unstructured, kind, name string) bool {
	refs, _ := obj.Object["metadata"].(map[string]any)["ownerReferences"].([]any)
	for _, r := range refs {
		rm, _ := r.(map[string]any)
		if rm == nil || str(rm["kind"]) != kind {
			continue
		}
		if name == "" || str(rm["name"]) == name {
			return true
		}
	}
	return false
}

func ownerNameOf(obj *unstructured.Unstructured, kind string) string {
	refs, _ := obj.Object["metadata"].(map[string]any)["ownerReferences"].([]any)
	for _, r := range refs {
		rm, _ := r.(map[string]any)
		if rm != nil && str(rm["kind"]) == kind {
			return str(rm["name"])
		}
	}
	return ""
}

// CreateRaw 通过 unstructured 文档创建任意支持的资源（ns 创建等场景）。
func (o *Ops) CreateRaw(ctx context.Context, doc map[string]any) (map[string]any, error) {
	kind := strings.ToLower(str(doc["kind"]))
	g, err := gvr(kind)
	if err != nil {
		return nil, err
	}
	dyn, err := o.dyn()
	if err != nil {
		return nil, err
	}
	meta, _ := doc["metadata"].(map[string]any)
	namespace := "default"
	if meta != nil {
		if ns, _ := meta["namespace"].(string); ns != "" && namespaced(kind) {
			namespace = ns
		}
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	riable := dyn.Resource(g)
	var ri dynamic.ResourceInterface = riable
	if namespaced(kind) {
		ri = riable.Namespace(namespace)
	}
	created, err := ri.Create(ctx2, &unstructured.Unstructured{Object: doc}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return created.Object, nil
}

// ─── 小工具 ───────────────────────────────────────────────────

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

func dfltstr(v any) string {
	if s := str(v); s != "" {
		return s
	}
	return "-"
}

func firstNonEmpty(vals ...any) any {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" && s != "null" {
			return v
		}
	}
	return nil
}

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

func tsSec(v any) string {
	t := parseTS(v)
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
