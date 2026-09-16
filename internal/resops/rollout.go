// Rollout 历史 / 回滚：三种工作负载机制不同（这是 Python 版实踩过的坑）：
//
//	Deployment  → ReplicaSet 历史，revision 存在
//	              deployment.kubernetes.io/revision 注解上
//	StatefulSet → ControllerRevision，用 status.updateRevision（完整 CR 名）定位当前
//	DaemonSet   → ControllerRevision，无对应字段，从 Pod 的
//	              controller-revision-hash label 取众数（裸 hash，CR 名是 <名>-<hash>）
//
// 回滚统一用 JSON Merge Patch 只改 spec.template —— 控制器能复用已有
// ReplicaSet，不会每次回滚多出一个。
package resops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"sailor/internal/k8sx"
)

const (
	revisionAnnotation          = "deployment.kubernetes.io/revision"
	changeCauseAnnotation       = "kubernetes.io/change-cause"
	controllerRevisionHashLabel = "controller-revision-hash"
)

// Revision 一条历史版本记录。
type Revision struct {
	Revision    any      `json:"revision"`
	RsName      string   `json:"rs_name,omitempty"`
	CrName      string   `json:"cr_name,omitempty"`
	Images      []string `json:"images"`
	ChangeCause string   `json:"change_cause"`
	Created     string   `json:"created"`
	Replicas    int      `json:"replicas,omitempty"`
	IsCurrent   bool     `json:"is_current"`
}

// RevisionList 历史列表。
type RevisionList struct {
	CurrentRevision any        `json:"current_revision"`
	Revisions       []Revision `json:"revisions"`
}

// ─── Deployment ──────────────────────────────────────────────

// ListDeploymentRevisions 列出 deployment 的 ReplicaSet 历史，按 revision 倒序。
func (o *Ops) ListDeploymentRevisions(ctx context.Context, name, namespace string) (*RevisionList, error) {
	dep, err := o.GetResource(ctx, "deployment", name, namespace)
	if err != nil {
		return nil, fmt.Errorf("Failed to read deployment: %w", err)
	}
	spec, _ := dep.Object["spec"].(map[string]any)
	sel, _ := spec["selector"].(map[string]any)
	ml, _ := sel["matchLabels"].(map[string]any)
	if len(ml) == 0 {
		return &RevisionList{Revisions: []Revision{}}, nil
	}

	dyn, err := o.dyn()
	if err != nil {
		return nil, err
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// 不带 selector 全量拉该 ns 的 RS，让 ownerReferences 过滤更可靠
	//（用户手改过 RS label 时 selector 会漏，owner 由 controller 维护）
	rsList, err := dyn.Resource(k8sx.GVRs["replicaset"]).Namespace(namespace).List(ctx2, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to list replica sets: %w", err)
	}

	meta, _ := dep.Object["metadata"].(map[string]any)
	anns, _ := meta["annotations"].(map[string]any)
	currentRev := str(anns[revisionAnnotation])

	var revisions []Revision
	for _, rs := range rsList.Items {
		if !hasOwner(&rs, "Deployment", name) {
			continue
		}
		rsMeta, _ := rs.Object["metadata"].(map[string]any)
		rsAnns, _ := rsMeta["annotations"].(map[string]any)
		rev := str(rsAnns[revisionAnnotation])
		if rev == "" {
			continue
		}
		rsSpec, _ := rs.Object["spec"].(map[string]any)
		images := templateImages(rsSpec)
		replicas := 0
		if rsSpec != nil {
			replicas = toInt(rsSpec["replicas"])
		}
		revisions = append(revisions, Revision{
			Revision:    rev,
			RsName:      str(rsMeta["name"]),
			Images:      images,
			ChangeCause: str(rsAnns[changeCauseAnnotation]),
			Created:     tsSec(rsMeta["creationTimestamp"]),
			Replicas:    replicas,
			IsCurrent:   rev == currentRev,
		})
	}
	sortRevisions(revisions)
	return &RevisionList{CurrentRevision: json.RawMessage(maybeJSONString(currentRev)), Revisions: revisions}, nil
}

// maybeJSONString 把纯数字字符串包装成 JSON 数值，其余包成 JSON 字符串，
// 让 current_revision 与 revisions[].revision 的类型一致（前端 == 比较）。
func maybeJSONString(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	if _, err := strconv.Atoi(s); err == nil {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// RollbackDeployment 把 spec.template 回滚到目标 revision 的 RS template。
func (o *Ops) RollbackDeployment(ctx context.Context, name, namespace string, targetRevision string) error {
	dep, err := o.GetResource(ctx, "deployment", name, namespace)
	if err != nil {
		return fmt.Errorf("Failed to read deployment: %w", err)
	}
	dyn, err := o.dyn()
	if err != nil {
		return err
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rsList, err := dyn.Resource(k8sx.GVRs["replicaset"]).Namespace(namespace).List(ctx2, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("Failed to list replica sets: %w", err)
	}

	var target *templateAndName
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if !hasOwner(rs, "Deployment", name) {
			continue
		}
		anns, _ := rs.Object["metadata"].(map[string]any)["annotations"].(map[string]any)
		if str(anns[revisionAnnotation]) == targetRevision {
			rsSpec, _ := rs.Object["spec"].(map[string]any)
			tmpl, _ := rsSpec["template"].(map[string]any)
			if tmpl != nil {
				target = &templateAndName{template: deepCopyMap(tmpl), name: str(rs.Object["metadata"].(map[string]any)["name"])}
			}
			break
		}
	}
	if target == nil {
		return fmt.Errorf("Revision %s not found", targetRevision)
	}

	meta, _ := dep.Object["metadata"].(map[string]any)
	anns, _ := meta["annotations"].(map[string]any)
	if str(anns[revisionAnnotation]) == targetRevision {
		return fmt.Errorf("Already at revision %s, no rollback needed", targetRevision)
	}

	return mergePatchTemplate(ctx, dyn, k8sx.GVRs["deployment"], namespace, name, target.template, fmt.Sprintf("Rollback to revision %s", targetRevision))
}

// ─── StatefulSet ─────────────────────────────────────────────

// ListStatefulsetRevisions 列出 ControllerRevision 历史（按 template 去重）。
func (o *Ops) ListStatefulsetRevisions(ctx context.Context, name, namespace string) (*RevisionList, error) {
	sts, err := o.GetResource(ctx, "statefulset", name, namespace)
	if err != nil {
		return nil, fmt.Errorf("Failed to read statefulset: %w", err)
	}
	stsStatus, _ := sts.Object["status"].(map[string]any)
	currentHash := ""
	if stsStatus != nil {
		currentHash = str(stsStatus["updateRevision"])
	}
	return o.listControllerRevisions(ctx, "StatefulSet", name, namespace, currentHash, func(crName string) bool {
		return currentHash != "" && crName == currentHash
	})
}

// ListDaemonsetRevisions DaemonSet 的当前版本只能从 Pod 上的裸 hash 众数推。
func (o *Ops) ListDaemonsetRevisions(ctx context.Context, name, namespace string) (*RevisionList, error) {
	ds, err := o.GetResource(ctx, "daemonset", name, namespace)
	if err != nil {
		return nil, fmt.Errorf("Failed to read daemonset: %w", err)
	}
	currentHash := o.daemonsetCurrentHash(ctx, ds, name, namespace)
	return o.listControllerRevisions(ctx, "DaemonSet", name, namespace, currentHash, func(crName string) bool {
		// Pod 上是裸 hash（如 6fd988788d），CR 名是 "<名>-<hash>"，按后缀匹配
		return currentHash != "" && hasSuffix(crName, "-"+currentHash)
	})
}

func (o *Ops) listControllerRevisions(ctx context.Context, ownerKind, name, namespace, currentHash string, isCurrent func(string) bool) (*RevisionList, error) {
	dyn, err := o.dyn()
	if err != nil {
		return nil, err
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	crList, err := dyn.Resource(k8sx.GVRs["controllerrevision"]).Namespace(namespace).List(ctx2, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to list controller revisions: %w", err)
	}

	// 按 template 去重：同一 template 只留最新 revision。
	// 干扰字段：controller-revision-hash label、restartedAt 注解、$patch 标记。
	type keyed struct {
		rev Revision
		num int64
	}
	templateMap := map[string]keyed{}
	for i := range crList.Items {
		cr := &crList.Items[i]
		if !hasOwner(cr, ownerKind, name) {
			continue
		}
		data, _ := cr.Object["data"].(map[string]any)
		specData, _ := data["spec"].(map[string]any)
		var template map[string]any
		if specData != nil {
			if t, ok := specData["template"].(map[string]any); ok {
				template = deepCopyMap(t)
			}
		}
		key := ""
		if template != nil {
			delete(template, "$patch")
			if meta, ok := template["metadata"].(map[string]any); ok {
				if labels, ok := meta["labels"].(map[string]any); ok {
					delete(labels, controllerRevisionHashLabel)
				}
				if anns, ok := meta["annotations"].(map[string]any); ok {
					delete(anns, "kubectl.kubernetes.io/restartedAt")
					if len(anns) == 0 {
						delete(meta, "annotations")
					}
				}
			}
			b, _ := json.Marshal(template)
			key = string(b)
		}

		crMeta, _ := cr.Object["metadata"].(map[string]any)
		crAnns, _ := crMeta["annotations"].(map[string]any)
		revNum := toInt(cr.Object["revision"])
		images := crImages(data)

		rev := Revision{
			Revision:    revNum,
			CrName:      str(crMeta["name"]),
			Images:      images,
			ChangeCause: str(crAnns[changeCauseAnnotation]),
			Created:     tsSec(crMeta["creationTimestamp"]),
			IsCurrent:   isCurrent(str(crMeta["name"])),
		}
		if existing, ok := templateMap[key]; !ok || int64(revNum) > existing.num {
			templateMap[key] = keyed{rev: rev, num: int64(revNum)}
		}
	}

	revisions := make([]Revision, 0, len(templateMap))
	for _, k := range templateMap {
		revisions = append(revisions, k.rev)
	}
	sortRevisions(revisions)

	var current any
	for _, r := range revisions {
		if r.IsCurrent {
			current = r.Revision
			break
		}
	}
	return &RevisionList{CurrentRevision: current, Revisions: revisions}, nil
}

// daemonsetCurrentHash 从实际运行的 Pod 上取 controller-revision-hash 众数。
func (o *Ops) daemonsetCurrentHash(ctx context.Context, ds *unstructured.Unstructured, name, namespace string) string {
	spec, _ := ds.Object["spec"].(map[string]any)
	var selector map[string]any
	if sel, _ := spec["selector"].(map[string]any); sel != nil {
		selector, _ = sel["matchLabels"].(map[string]any)
	}
	pods := o.listPodsRaw(ctx, namespace, labelsSelector(selector), func(p *unstructured.Unstructured) bool {
		return hasOwner(p, "DaemonSet", name)
	})
	counts := map[string]int{}
	for _, p := range pods {
		labels, _ := p.Object["metadata"].(map[string]any)["labels"].(map[string]any)
		if h := str(labels[controllerRevisionHashLabel]); h != "" {
			counts[h]++
		}
	}
	best, bestN := "", 0
	for h, n := range counts {
		if n > bestN {
			best, bestN = h, n
		}
	}
	return best
}

// RollbackStatefulset / RollbackDaemonset 共用 ControllerRevision 回滚路径。
func (o *Ops) RollbackStatefulset(ctx context.Context, name, namespace string, targetRevision int64) error {
	return o.rollbackControllerRevision(ctx, "statefulset", "StatefulSet", name, namespace, targetRevision)
}

func (o *Ops) RollbackDaemonset(ctx context.Context, name, namespace string, targetRevision int64) error {
	return o.rollbackControllerRevision(ctx, "daemonset", "DaemonSet", name, namespace, targetRevision)
}

func (o *Ops) rollbackControllerRevision(ctx context.Context, kind, ownerKind, name, namespace string, targetRevision int64) error {
	dyn, err := o.dyn()
	if err != nil {
		return err
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	crList, err := dyn.Resource(k8sx.GVRs["controllerrevision"]).Namespace(namespace).List(ctx2, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("Failed to list controller revisions: %w", err)
	}
	var template map[string]any
	for i := range crList.Items {
		cr := &crList.Items[i]
		if !hasOwner(cr, ownerKind, name) {
			continue
		}
		if toInt(cr.Object["revision"]) == int(targetRevision) {
			data, _ := cr.Object["data"].(map[string]any)
			specData, _ := data["spec"].(map[string]any)
			if specData != nil {
				template, _ = specData["template"].(map[string]any)
			}
			break
		}
	}
	if template == nil {
		return fmt.Errorf("Revision %d not found", targetRevision)
	}
	gvr, _ := gvr(kind)
	return mergePatchTemplate(ctx, dyn, gvr, namespace, name, template, fmt.Sprintf("Rollback to revision %d", targetRevision))
}

// mergePatchTemplate JSON Merge Patch 只替换 spec.template + change-cause 注解。
func mergePatchTemplate(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespace, name string, template map[string]any, changeCause string) error {
	// pod-template-hash 是 controller 自己维护的 label，剥掉它才能复用已有 RS
	stripTemplateHash(template)

	patchBody := map[string]any{
		"spec": map[string]any{"template": template},
		"metadata": map[string]any{
			"annotations": map[string]any{changeCauseAnnotation: changeCause},
		},
	}
	b, _ := json.Marshal(patchBody)
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := dyn.Resource(gvr).Namespace(namespace).Patch(ctx2, name, types.MergePatchType, b, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return &ErrNotFound{Kind: gvr.Resource, Name: name}
	}
	return err
}

// ─── 工具 ─────────────────────────────────────────────────────

type templateAndName struct {
	template map[string]any
	name     string
}

// templateImages 从 workload spec.template 提取镜像列表。
func templateImages(spec map[string]any) []string {
	var images []string
	tmpl, _ := spec["template"].(map[string]any)
	if tmpl == nil {
		return images
	}
	podSpec, _ := tmpl["spec"].(map[string]any)
	if podSpec == nil {
		return images
	}
	for _, c := range toList(podSpec["containers"]) {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		if img := str(cm["image"]); img != "" {
			images = append(images, img)
		}
	}
	return images
}

// crImages 从 ControllerRevision data 提取镜像列表。
func crImages(data map[string]any) []string {
	var images []string
	specData, _ := data["spec"].(map[string]any)
	if specData == nil {
		return images
	}
	template, _ := specData["template"].(map[string]any)
	if template == nil {
		return images
	}
	podSpec, _ := template["spec"].(map[string]any)
	if podSpec == nil {
		return images
	}
	for _, c := range toList(podSpec["containers"]) {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		if img := str(cm["image"]); img != "" {
			images = append(images, img)
		}
	}
	return images
}

func sortRevisions(revisions []Revision) {
	sort.Slice(revisions, func(i, j int) bool {
		return revisionNum(revisions[i]) > revisionNum(revisions[j])
	})
}

func revisionNum(r Revision) int {
	switch v := r.Revision.(type) {
	case string:
		n, _ := strconv.Atoi(v)
		return n
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// listPodsRaw 返回未序列化的 Pod 对象（供 hash 统计等）。
func (o *Ops) listPodsRaw(ctx context.Context, namespace, labelSelector string, keep func(*unstructured.Unstructured) bool) []*unstructured.Unstructured {
	dyn, err := o.dyn()
	if err != nil {
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	list, err := dyn.Resource(k8sx.GVRs["pod"]).Namespace(namespace).List(ctx2, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil
	}
	var out []*unstructured.Unstructured
	for i := range list.Items {
		if keep(&list.Items[i]) {
			out = append(out, &list.Items[i])
		}
	}
	return out
}
