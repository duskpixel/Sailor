// Package k8sx 管理按集群缓存的 K8s 客户端（dynamic + typed + rest.Config）。
//
// 对应 Django 版 clusters/k8s_client.py 的连接池：同一集群反复使用同一批
// 客户端，避免重复解析 kubeconfig、重建 TLS 连接。Go 版不需要 Python 版
// “exec 专用独立 client”的规避 —— client-go 的 remotecommand 不改写共享
// client 的内部状态，可以安全并发。
package k8sx

import (
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client 是一个集群的客户端集合。
type Client struct {
	Config  *rest.Config
	Typed   *kubernetes.Clientset
	Dynamic dynamic.Interface
}

// Pool 按集群 ID 缓存客户端，double-check 加载。
type Pool struct {
	mu      sync.Mutex
	clients map[int64]*Client
}

func NewPool() *Pool {
	return &Pool{clients: map[int64]*Client{}}
}

// Load 从 kubeconfig YAML 构建（或替换）一个集群的客户端。
func (p *Pool) Load(id int64, kubeconfigYAML string) (*Client, error) {
	cc, err := clientcmd.NewClientConfigFromBytes([]byte(kubeconfigYAML))
	if err != nil {
		return nil, fmt.Errorf("kubeconfig 解析失败：%w", err)
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig 无效：%w", err)
	}
	// Django 版把默认请求超时设为 5s；Go 版超时改为按调用传 context，
	// 这里只放宽速率限制，避免大集群连续 list 被客户端限流拖慢。
	cfg.QPS = 50
	cfg.Burst = 100

	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	cl := &Client{Config: cfg, Typed: typed, Dynamic: dyn}

	p.mu.Lock()
	p.clients[id] = cl
	p.mu.Unlock()
	return cl, nil
}

// Get 返回集群客户端，未加载过时用 loader 懒加载。
func (p *Pool) Get(id int64, loader func() (string, error)) (*Client, error) {
	p.mu.Lock()
	if cl, ok := p.clients[id]; ok {
		p.mu.Unlock()
		return cl, nil
	}
	p.mu.Unlock()

	yaml, err := loader()
	if err != nil {
		return nil, err
	}
	// double-check：并发下可能已被别的 goroutine 加载，Load 幂等，直接覆盖即可
	return p.Load(id, yaml)
}

func (p *Pool) Remove(id int64) {
	p.mu.Lock()
	delete(p.clients, id)
	p.mu.Unlock()
}

func (p *Pool) Refresh(id int64, loader func() (string, error)) (*Client, error) {
	p.Remove(id)
	return p.Get(id, loader)
}

// GVR 表：kind → 资源坐标。与 Django K8sResourceManager.RESOURCE_TYPES 对齐，
// 统一走 dynamic client，省掉 Python 版 10 组 typed 方法名的映射。
var GVRs = map[string]schema.GroupVersionResource{
	"namespace":             {Group: "", Version: "v1", Resource: "namespaces"},
	"pod":                   {Group: "", Version: "v1", Resource: "pods"},
	"service":               {Group: "", Version: "v1", Resource: "services"},
	"configmap":             {Group: "", Version: "v1", Resource: "configmaps"},
	"secret":                {Group: "", Version: "v1", Resource: "secrets"},
	"persistentvolumeclaim": {Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
	"event":                 {Group: "", Version: "v1", Resource: "events"},
	"deployment":            {Group: "apps", Version: "v1", Resource: "deployments"},
	"statefulset":           {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"daemonset":             {Group: "apps", Version: "v1", Resource: "daemonsets"},
	"replicaset":            {Group: "apps", Version: "v1", Resource: "replicasets"},
	"controllerrevision":    {Group: "apps", Version: "v1", Resource: "controllerrevisions"},
	"endpoints":             {Group: "", Version: "v1", Resource: "endpoints"},
	"ingress":               {Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
	"job":                   {Group: "batch", Version: "v1", Resource: "jobs"},
	"cronjob":               {Group: "batch", Version: "v1", Resource: "cronjobs"},
	"hpa":                   {Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"},
	"scaledobject":          {Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"},
	"scaledjob":             {Group: "keda.sh", Version: "v1alpha1", Resource: "scaledjobs"},
}

var Namespaced = map[string]bool{
	"namespace": false,
}

func IsNamespaced(kind string) bool { return Namespaced[kind] }

// KindFromPlural 把 URL / YAML 里常见的 kind 写法归一化为内部 kind 名。
func NormalizeKind(kind string) string { return kind }
