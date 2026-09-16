// YAML 应用与校验：对应 Python K8sResourceManager.apply_yaml / validate_yaml。
//
// apply：整文档 replace（带 resourceVersion 乐观锁），404 则 create，
// 返回 actions 给前端做乐观更新；validate：server-side dry-run
// （dry_run=All 走完整 admission 链路但不落库），报错按 8 类翻译成中文建议。
package resops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"

	"sailor/internal/serialize"
)

// toYAML 把 unstructured map 转成 YAML 文本。
func toYAML(doc map[string]any) (string, error) {
	b, err := sigsyaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseYAMLDocs 解析多文档 YAML 为 map 列表（空文档跳过）。
func parseYAMLDocs(content string) ([]map[string]any, error) {
	var docs []map[string]any
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(content), 4096)
	for {
		var raw map[string]any
		err := decoder.Decode(&raw)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if raw != nil {
			docs = append(docs, raw)
		}
	}
	return docs, nil
}

// Action 单个文档的 apply 结果，前端据此做乐观更新。
type Action struct {
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Namespace string         `json:"namespace"`
	Action    string         `json:"action"` // created | updated
	Resource  map[string]any `json:"resource,omitempty"`
}

// ApplyResult 是 apply_yaml 的返回结构。
type ApplyResult struct {
	Success  bool     `json:"success"`
	Message  string   `json:"message,omitempty"`
	Error    string   `json:"error,omitempty"`
	Actions  []Action `json:"actions"`
	Warnings []string `json:"warnings,omitempty"`
}

// Apply 应用 YAML（整体替换，不存在则创建）。所有文档串行处理，一个失败
// 返回 error（与 Python 版一致：异常中断整批）。
func (o *Ops) Apply(ctx context.Context, content string) *ApplyResult {
	docs, err := parseYAMLDocs(content)
	if err != nil {
		return &ApplyResult{Success: false, Error: fmt.Sprintf("Invalid YAML: %v", err)}
	}

	dyn, err := o.dyn()
	if err != nil {
		return &ApplyResult{Success: false, Error: err.Error()}
	}

	res := &ApplyResult{Success: true}
	var messages []string
	seenWarn := map[string]bool{}

	for _, doc := range docs {
		if len(doc) == 0 {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		if doc["metadata"] != nil && meta == nil {
			return &ApplyResult{Success: false, Error: fmt.Sprintf(
				"metadata 字段格式错误：解析结果不是 mapping，最常见原因是 \"name: xxx\" 后冒号缺少空格（例如 \"name:xxx\" 应为 \"name: xxx\"）")}
		}
		kind := strings.ToLower(str(doc["kind"]))
		name := ""
		namespace := "default"
		if meta != nil {
			name = str(meta["name"])
			if ns := str(meta["namespace"]); ns != "" {
				namespace = ns
			}
		}
		if kind == "" || name == "" {
			return &ApplyResult{Success: false, Error: "Invalid YAML: missing 'kind' or 'metadata.name'"}
		}
		g, err := gvr(kind)
		if err != nil {
			return &ApplyResult{Success: false, Error: err.Error()}
		}
		ns := kind
		if !namespaced(kind) {
			ns = ""
		}
		_ = ns

		for _, w := range stripServerManagedFields(doc) {
			if !seenWarn[w] {
				seenWarn[w] = true
				res.Warnings = append(res.Warnings, w)
			}
		}

		ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
		obj := &unstructured.Unstructured{Object: doc}
		riable := dyn.Resource(g)
		var ri dynamic.ResourceInterface = riable
		if namespaced(kind) {
			ri = riable.Namespace(namespace)
		}

		existing, err := ri.Get(ctx2, name, metav1.GetOptions{})
		switch {
		case err == nil:
			// 更新：注入 resourceVersion 作为乐观锁
			meta, _ := doc["metadata"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
				doc["metadata"] = meta
			}
			meta["resourceVersion"] = existing.GetResourceVersion()
			obj = &unstructured.Unstructured{Object: doc}
			updated, err := ri.Update(ctx2, obj, metav1.UpdateOptions{})
			cancel()
			if err != nil {
				return &ApplyResult{Success: false, Error: apiErrDetail(err)}
			}
			messages = append(messages, fmt.Sprintf("Updated %s '%s'", kind, name))
			res.Actions = append(res.Actions, Action{
				Kind: kind, Name: name, Namespace: nsOf(namespaced(kind), namespace),
				Action: "updated", Resource: serialize.Item(kind, updated.Object),
			})

		case apierrors.IsNotFound(err):
			created, err := ri.Create(ctx2, obj, metav1.CreateOptions{})
			cancel()
			if err != nil {
				return &ApplyResult{Success: false, Error: apiErrDetail(err)}
			}
			messages = append(messages, fmt.Sprintf("Created %s '%s'", kind, name))
			res.Actions = append(res.Actions, Action{
				Kind: kind, Name: name, Namespace: nsOf(namespaced(kind), namespace),
				Action: "created", Resource: serialize.Item(kind, created.Object),
			})

		default:
			cancel()
			return &ApplyResult{Success: false, Error: apiErrDetail(err)}
		}
	}

	res.Message = strings.Join(messages, "; ")
	return res
}

func nsOf(namespaced bool, ns string) string {
	if namespaced {
		return ns
	}
	return ""
}

// ValidationDoc 单文档校验错误/警告。
type ValidationDoc struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ValidationResult dry-run 校验结果。
type ValidationResult struct {
	Success  bool            `json:"success"`
	Errors   []ValidationDoc `json:"errors"`
	Warnings []ValidationDoc `json:"warnings"`
	Docs     int             `json:"docs"`
}

// Validate 对 YAML 做 server-side dry-run。
func (o *Ops) Validate(ctx context.Context, content string) *ValidationResult {
	res := &ValidationResult{Success: true}
	docs, err := parseYAMLDocs(content)
	if err != nil {
		return &ValidationResult{
			Success: false,
			Errors:  []ValidationDoc{{Kind: "-", Name: "-", Message: fmt.Sprintf("YAML 解析失败：%v", err)}},
		}
	}

	dyn, err := o.dyn()
	if err != nil {
		return &ValidationResult{
			Success: false,
			Errors:  []ValidationDoc{{Kind: "-", Name: "-", Message: err.Error()}},
		}
	}

	for _, doc := range docs {
		if len(doc) == 0 {
			continue
		}
		res.Docs++
		o.validateOne(ctx, dyn, doc, res)
	}
	if len(res.Errors) > 0 {
		res.Success = false
	}
	return res
}

func (o *Ops) validateOne(ctx context.Context, dyn dynamic.Interface, doc map[string]any, res *ValidationResult) {
	if doc["metadata"] != nil {
		if _, ok := doc["metadata"].(map[string]any); !ok {
			res.Errors = append(res.Errors, ValidationDoc{Kind: dfltstr(doc["kind"]), Name: "-",
				Message: "metadata 字段格式错误：解析结果不是 mapping，最常见的原因是 \"name: xxx\" 后冒号缺少空格（例如 \"name:xxx\" 错，应为 \"name: xxx\"）"})
			return
		}
	}
	if doc["spec"] != nil {
		if _, ok := doc["spec"].(map[string]any); !ok {
			if _, isList := doc["spec"].([]any); !isList {
				res.Errors = append(res.Errors, ValidationDoc{Kind: dfltstr(doc["kind"]), Name: "-",
					Message: "spec 字段格式错误：解析结果不是 mapping，通常是缩进或冒号空格问题"})
				return
			}
		}
	}
	kind := strings.ToLower(str(doc["kind"]))
	meta, _ := doc["metadata"].(map[string]any)
	name, namespace := "-", "default"
	if meta != nil {
		if n := str(meta["name"]); n != "" {
			name = n
		}
		if ns := str(meta["namespace"]); ns != "" {
			namespace = ns
		}
	}
	if kind == "" {
		res.Errors = append(res.Errors, ValidationDoc{Kind: "-", Name: name, Message: "缺少 kind 字段（如 Deployment / Service / ...）"})
		return
	}
	if meta == nil || str(meta["name"]) == "" {
		res.Errors = append(res.Errors, ValidationDoc{Kind: kind, Name: "-", Message: "缺少 metadata.name 字段"})
		return
	}
	g, err := gvr(kind)
	if err != nil {
		res.Errors = append(res.Errors, ValidationDoc{Kind: kind, Name: name,
			Message: fmt.Sprintf("不支持的资源类型 %s（仅支持：%s）", kind, supportedKinds())})
		return
	}

	// 先扫一遍"已知会被剥掉"的字段给用户提示（在 strip 之前）
	for _, w := range scanUserFacingDropped(doc) {
		res.Warnings = append(res.Warnings, ValidationDoc{Kind: kind, Name: name, Field: w[0], Reason: w[1]})
	}

	// 备份原始 doc，dry-run 用 strip 过的
	dryrun := deepCopyMap(doc)
	stripServerManagedFields(dryrun)

	riable := dyn.Resource(g)
	var ri dynamic.ResourceInterface = riable
	if namespaced(kind) {
		ri = riable.Namespace(namespace)
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	existing, err := ri.Get(ctx2, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		res.Errors = append(res.Errors, ValidationDoc{Kind: kind, Name: name,
			Message: fmt.Sprintf("查询资源失败：%v", err)})
		return
	}
	if apierrors.IsNotFound(err) {
		existing = nil
	}

	var dryErr error
	if existing == nil {
		_, dryErr = ri.Create(ctx2, &unstructured.Unstructured{Object: dryrun},
			metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	} else {
		meta, _ := dryrun["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			dryrun["metadata"] = meta
		}
		meta["resourceVersion"] = existing.GetResourceVersion()
		_, dryErr = ri.Update(ctx2, &unstructured.Unstructured{Object: dryrun},
			metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
	}
	if dryErr != nil {
		res.Errors = append(res.Errors, ValidationDoc{Kind: kind, Name: name, Message: HumanizeAPIError(dryErr)})
	}
}

func supportedKinds() string {
	var ks []string
	for k := range map[string]bool{
		"namespace": true, "pod": true, "deployment": true, "statefulset": true,
		"daemonset": true, "service": true, "ingress": true, "configmap": true,
		"secret": true, "persistentvolumeclaim": true,
	} {
		ks = append(ks, k)
	}
	sortStrings(ks)
	return strings.Join(ks, ", ")
}

// scanUserFacingDropped 从用户原始 doc 里挑出"我们一定会剥掉、用户应该知道"的
// 字段路径（对应 Python _scan_user_facing_dropped）。
func scanUserFacingDropped(doc map[string]any) [][2]string {
	var out [][2]string
	if _, ok := doc["status"]; ok {
		out = append(out, [2]string{"status",
			"status 是 K8s 控制面维护的只读子资源，对它的修改会被服务端静默忽略。要影响资源运行状态请改 spec/template 触发控制器调谐。"})
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		return out
	}
	readonly := [][2]string{
		{"resourceVersion", "K8s 内部用于乐观锁，由服务端维护，写了也无效"},
		{"uid", "K8s 自动生成的全局唯一 ID，不可写"},
		{"creationTimestamp", "资源创建时间，由服务端写入，不可改"},
		{"generation", "spec 变更次数，由服务端维护"},
		{"managedFields", "server-side apply 的字段所有权记录，不该手工写"},
		{"selfLink", "已弃用字段"},
		{"deletionTimestamp", "资源被删除时由服务端写入，不可手动设置"},
		{"ownerReferences", "由控制器维护（如 ReplicaSet → Pod 的 owner），手工写会被覆盖"},
	}
	for _, kv := range readonly {
		if _, ok := meta[kv[0]]; ok {
			out = append(out, [2]string{"metadata." + kv[0], kv[1]})
		}
	}
	return out
}

// apiErrDetail 把 K8s API 错误压成一段可读文本（对应 Python 的
// f"{e.reason}: {detail[:500]}"）。
func apiErrDetail(err error) string {
	if s := apiErrStatus(err); s != nil {
		detail := s.Message
		if len(detail) > 500 {
			detail = detail[:500]
		}
		if detail != "" {
			return fmt.Sprintf("%s: %s", s.Reason, detail)
		}
	}
	return err.Error()
}

// HumanizeAPIError 把 K8s 拒绝错误翻译成小白友好的说明（对应
// Python _humanize_api_exception + _classify_error）。
func HumanizeAPIError(err error) string {
	s := apiErrStatus(err)
	if s == nil {
		return err.Error()
	}
	code := int(s.Code)
	msg := s.Message

	var b strings.Builder
	fmt.Fprintf(&b, "K8s 拒绝（%d）：%s", code, msg)
	for _, c := range s.Causes {
		field := c.Field
		if field == "" {
			field = c.Reason
		}
		fmt.Fprintf(&b, "\n  • [%s] %s", field, c.Message)
	}
	if hint := classifyError(code, msg); hint != "" {
		fmt.Fprintf(&b, "\n💡 %s", hint)
	}
	return b.String()
}

func classifyError(status int, msg string) string {
	m := strings.ToLower(msg)
	switch {
	case status == 422 || strings.Contains(m, "invalid"):
		switch {
		case strings.Contains(m, "immutable") || strings.Contains(m, "cannot be changed"):
			return ("该字段在资源创建后不可更改（K8s 不可变字段，如 Service.spec.clusterIP、" +
				"PVC.spec.storageClassName、Pod 的大部分 spec 字段等）。如需变更请删除后重建。")
		case strings.Contains(m, "required value") || strings.Contains(m, "must be specified"):
			return "缺少必填字段，请按 K8s 文档补全后重试。"
		case strings.Contains(m, "invalid value"):
			return "某个字段的值不合法（格式 / 取值范围错误），请按上方 [field] 提示修正。"
		default:
			return "资源校验失败，请按上方 [field] 修复"
		}
	case status == 409:
		return "资源版本冲突或已存在。可能是别人/控制器同时改动了，请重新打开 YAML 拿最新版本再试。"
	case status == 403:
		return "当前 kubeconfig 没有这个操作的权限（403 Forbidden）。"
	case status == 404:
		return "关联的对象不存在（如引用了不存在的 namespace / serviceaccount / pvc）。"
	case status == 400 && strings.Contains(m, "unknown field"):
		return "YAML 里有 K8s schema 不认识的字段（拼写错？版本错？）。请检查字段名大小写以及 apiVersion 是否匹配。"
	}
	return ""
}

// jsonStatusCause 是 Status.details.causes 里的单条原因。
type jsonStatusCause struct {
	Reason  string `json:"reason"`
	Type    string `json:"type"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// jsonStatus 是 K8s Status 对象里我们关心的部分。
type jsonStatus struct {
	Code    int               `json:"code"`
	Reason  string            `json:"reason"`
	Message string            `json:"message"`
	Causes  []jsonStatusCause `json:"-"`
}

// apiErrStatus 尽力从错误里解析出 K8s Status。
func apiErrStatus(err error) *jsonStatus {
	// apierrors 的结构化访问优先
	var gi apierrors.APIStatus
	if errors.As(err, &gi) {
		s := gi.Status()
		out := &jsonStatus{Code: int(s.Code), Reason: string(s.Reason), Message: s.Message}
		if s.Details != nil {
			for _, c := range s.Details.Causes {
				out.Causes = append(out.Causes, jsonStatusCause{
					Reason: string(c.Type), Type: string(c.Type), Field: c.Field, Message: c.Message,
				})
			}
		}
		return out
	}
	// 兜底：错误文本里可能嵌着 JSON Status
	if idx := strings.Index(err.Error(), "{"); idx >= 0 {
		var st struct {
			jsonStatus
			Details struct {
				Causes []jsonStatusCause `json:"causes"`
			} `json:"details"`
		}
		if json.Unmarshal([]byte(err.Error()[idx:]), &st) == nil && st.Code != 0 {
			st.Causes = st.Details.Causes
			return &st.jsonStatus
		}
	}
	return nil
}
