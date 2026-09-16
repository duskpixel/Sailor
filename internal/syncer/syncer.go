// Package syncer 移植 Django 版 resources/sync_service.py：
//
//   - 每个集群一个后台 goroutine，60 秒一轮按固定顺序同步 10 类资源
//   - chunked list 分页（limit=500 + continue token），410 Gone 从头重来一次，
//     5 万条熔断 —— 解决 168MB 单响应被截断的 IncompleteRead 问题
//   - 锁粒度是「集群 × 资源类型」：周期同步与写操作触发的立即同步只在同
//     kind 之间互斥，互不拖累
//   - 单类失败不中断整轮，错误落盘 store.SetSyncError，由列表 API 透传前端
package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"sailor/internal/k8sx"
	"sailor/internal/serialize"
	"sailor/internal/store"
)

const (
	interval = 60 * time.Second
	pageSize = 500   // kubectl 默认值，兼容性最稳
	maxItems = 50000 // 保险丝：异常集群不把内存吃穿
)

// SYNC_ORDER 同步优先级：先同步的先被用户看到。
var SyncOrder = []string{
	"namespace", "pod", "deployment", "service",
	"configmap", "secret", "ingress",
	"persistentvolumeclaim", "statefulset", "daemonset",
	"job", "cronjob", "hpa", "scaledobject", "scaledjob",
}

// optionalKinds 是 CRD 提供的资源（KEDA 等）：集群没装对应 CRD 时 list 返回
// 404，这是"未安装"而非"集群异常"，不能进 sync error 否则没装 KEDA 的
// 集群每轮都弹"连接异常"横幅。
var optionalKinds = map[string]bool{"scaledobject": true, "scaledjob": true}

// setOptionalMarker 把可选组件的安装状态落进 marker 缓存，列表 API 读取后
// 透传给前端展示"未检测到 KEDA"空态（marker Data 是 {"installed":bool}）。
func (m *Manager) setOptionalMarker(clusterID int64, name string, installed bool) {
	b, _ := json.Marshal(map[string]bool{"installed": installed})
	_ = m.store.SaveCache(clusterID, name, b)
}

// Manager 管理所有集群的同步 goroutine。
type Manager struct {
	store  *store.Store
	pool   *k8sx.Pool
	metaMu syncMeta
	locks  map[int64]*clusterLocks
}

type clusterLocks struct {
	types map[string]*typeLock
}

type typeLock struct {
	mu      sync.Mutex
	invalid bool // 集群被 stop 后置位，后续同步跳过
}

type syncMeta struct {
	mu    sync.Mutex
	locks map[int64]*clusterLocks
}

// New 创建 Manager。启动时把已有在线集群全部拉起（对应 ResourcesConfig.ready）。
func New(st *store.Store, pool *k8sx.Pool) *Manager {
	return &Manager{
		store:  st,
		pool:   pool,
		locks:  map[int64]*clusterLocks{},
		metaMu: syncMeta{locks: map[int64]*clusterLocks{}},
	}
}

// StartAll 为所有已存在集群启动同步（应用启动时调用）。
func (m *Manager) StartAll() {
	for _, c := range m.store.ListClusters() {
		m.StartForCluster(c)
	}
}

// StartForCluster 启动（或确保已在跑）某集群的同步 goroutine。
func (m *Manager) StartForCluster(c *store.Cluster) {
	m.metaMu.mu.Lock()
	defer m.metaMu.mu.Unlock()
	if _, exists := m.metaMu.locks[c.ID]; exists {
		return // 已在跑
	}
	m.metaMu.locks[c.ID] = &clusterLocks{types: map[string]*typeLock{}}
	go m.loop(c.ID)
}

// StopForCluster 停止某集群的同步（删除集群时调用）。
func (m *Manager) StopForCluster(id int64) {
	m.metaMu.mu.Lock()
	if cl, ok := m.metaMu.locks[id]; ok {
		for _, tl := range cl.types {
			tl.invalid = true
		}
		delete(m.metaMu.locks, id)
	}
	m.metaMu.mu.Unlock()
	m.store.ClearSyncError(id)
}

// lockFor 返回 (cluster, kind) 的锁；集群已 stop 时返回 nil。
func (m *Manager) lockFor(id int64, kind string) *typeLock {
	m.metaMu.mu.Lock()
	defer m.metaMu.mu.Unlock()
	cl, ok := m.metaMu.locks[id]
	if !ok {
		return nil
	}
	tl, ok := cl.types[kind]
	if !ok {
		tl = &typeLock{}
		cl.types[kind] = tl
	}
	return tl
}

func (m *Manager) loop(id int64) {
	for {
		cluster := loadCluster(m.store, id)
		if cluster == nil {
			return // 集群已删除
		}
		m.SyncAll(cluster)
		time.Sleep(interval)
	}
}

func loadCluster(st *store.Store, id int64) *store.Cluster {
	c, err := st.GetCluster(id)
	if err != nil {
		return nil
	}
	return c
}

// SyncAll 周期同步一轮。
func (m *Manager) SyncAll(c *store.Cluster) {
	start := time.Now()
	var firstErrKind string
	var firstErr error

	for _, kind := range SyncOrder {
		tl := m.lockFor(c.ID, kind)
		if tl == nil {
			return // 中途被 stop
		}
		tl.mu.Lock()
		err := m.syncKind(c, kind)
		tl.mu.Unlock()
		if err != nil && firstErrKind == "" {
			firstErrKind, firstErr = kind, err
		}
	}

	if firstErrKind == "" {
		m.store.ClearSyncError(c.ID)
	} else {
		m.store.SetSyncError(c.ID, &store.SyncError{
			ResourceType: firstErrKind,
			Message:      DescribeError(firstErr),
			FailedAt:     time.Now().Format(time.RFC3339),
		})
	}
	log.Printf("[cluster %s] sync cycle completed in %.1fs", c.Name, time.Since(start).Seconds())
}

// TriggerImmediate 写操作后触发单类资源的立即同步。
// wait=true 时阻塞最多 timeout，保证后续 list API 拿到最新数据。
func (m *Manager) TriggerImmediate(c *store.Cluster, kind string, wait bool, timeout time.Duration) {
	tl := m.lockFor(c.ID, kind)
	if tl == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		tl.mu.Lock()
		defer tl.mu.Unlock()
		if err := m.syncKind(c, kind); err != nil {
			m.store.SetSyncError(c.ID, &store.SyncError{
				ResourceType: kind,
				Message:      DescribeError(err),
				FailedAt:     time.Now().Format(time.RFC3339),
			})
		} else if e := m.store.GetSyncError(c.ID); e != nil && e.ResourceType == kind {
			// 这一类成功了就"忘掉"它的错误，让下一轮周期同步重新评估其他类
			m.store.ClearSyncError(c.ID)
		}
		close(done)
	}()
	if wait {
		select {
		case <-done:
		case <-time.After(timeout):
		}
	}
}

func (m *Manager) syncKind(c *store.Cluster, kind string) error {
	start := time.Now()
	cl, err := m.pool.Get(c.ID, func() (string, error) {
		return c.GetKubeconfig(m.store.AEAD())
	})
	if err != nil {
		return err
	}
	items, truncated, err := listPaginated(cl.Dynamic, gvrFor(kind), kind)
	if err != nil {
		// 可选 CRD 未安装：落空缓存 + 未安装标记，等价于"同步成功但没数据"。
		// 装上 CRD 后下一轮周期同步自然恢复。
		if optionalKinds[kind] && apierrors.IsNotFound(err) {
			m.setOptionalMarker(c.ID, "keda", false)
			_ = m.store.SaveCache(c.ID, kind, []byte("[]"))
			return nil
		}
		return err
	}
	if optionalKinds[kind] {
		m.setOptionalMarker(c.ID, "keda", true)
	}

	out := make([]map[string]any, 0, len(items))
	for _, obj := range items {
		out = append(out, serialize.Item(kind, obj))
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err := m.store.SaveCache(c.ID, kind, b); err != nil {
		return err
	}
	if truncated {
		log.Printf("[%s@cluster %d] list truncated at %d items", kind, c.ID, len(items))
	}
	log.Printf("[%s@cluster %d] synced %d items in %.1fs", kind, c.ID, len(out), time.Since(start).Seconds())
	return nil
}

func gvrFor(kind string) schema.GroupVersionResource {
	if gvr, ok := k8sx.GVRs[kind]; ok {
		return gvr
	}
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: kind}
}

// listPaginated 分页拉取全量列表，对应 Python _list_paginated。
func listPaginated(dyn dynamic.Interface, gvr schema.GroupVersionResource, kind string) ([]map[string]any, bool, error) {
	var items []map[string]any
	token := ""
	restarted := false
	truncated := false

	for {
		list, err := dyn.Resource(gvr).List(context.Background(),
			metav1.ListOptions{Limit: pageSize, Continue: token})
		if err != nil {
			// 410 Gone：continue token 过期，从头重来一次
			if apierrors.IsGone(err) && !restarted {
				log.Printf("list %s continue token expired (410), restarting pagination", kind)
				items, token, restarted = nil, "", true
				continue
			}
			return nil, false, err
		}
		for i := range list.Items {
			items = append(items, list.Items[i].Object)
		}
		token = list.GetContinue()
		if token == "" {
			return items, truncated, nil
		}
		if len(items) >= maxItems {
			return items, true, nil
		}
	}
}

// DescribeError 把底层异常翻译成对运维有意义的中文一句话（对应
// Python _describe_sync_error）。
func DescribeError(err error) string {
	msg := lower(err.Error())
	switch {
	case contains(msg, "kubeconfig", "invalid kube-config", "no configuration has been provided"):
		return fmt.Sprintf("kubeconfig 解析失败：%v", err)
	case contains(msg, "unable to connect", "connection refused", "no route to host", "dial tcp"):
		return "无法连接到集群 API Server，请检查网络连通性或 kubeconfig 中的 server 地址"
	case contains(msg, "timed out", "timeout", "context deadline exceeded", "client rate limiter"):
		return "连接集群超时，可能是网络抖动或 API Server 负载过高"
	case contains(msg, "unauthorized", "401"):
		return "kubeconfig 凭证无效或已过期（401 Unauthorized）"
	case contains(msg, "forbidden", "403"):
		return "当前 kubeconfig 没有 list 权限（403 Forbidden）"
	case contains(msg, "no such host", "nodename nor servname", "lookup "):
		return "无法解析集群 API Server 的 DNS，请检查 kubeconfig 中的 server 地址"
	case contains(msg, "certificate", "x509", "tls"):
		return fmt.Sprintf("集群 TLS 证书校验失败：%v", err)
	default:
		return fmt.Sprintf("同步失败：%v", err)
	}
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}
