// Package store 提供 Armada 桌面端的本地持久化：集群配置与 K8s 资源缓存。
//
// Django 版用 SQLite（Cluster / K8sResourceCache 两张表），桌面版数据量小、
// 访问模式简单（按 key 读写大 JSON blob），改用纯文件存储，零依赖零 CGO：
//   - config.json：集群列表 + 当前选中集群 + 自增 ID 计数器（整文件原子替换）
//   - cache/c<id>/<kind>.json：每种资源类型一个文件，整轮同步全量替换
//   - cache/c<id>/_error.json：最近一次同步失败的原因（成功即删除）
//
// kubeconfig 与 Django 版一致做加密存储，密钥放在数据目录的 key.bin（0600）。
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Cluster 对应 Django clusters.Cluster。
type Cluster struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	DisplayName   string    `json:"display_name"`
	Status        string    `json:"status"` // online / offline / unknown
	K8sVersion    string    `json:"k8s_version"`
	OSInfo        string    `json:"os_info"`
	APIServer     string    `json:"api_server"`
	NodeCount     int       `json:"node_count"`
	Kubeconfig    []byte    `json:"kubeconfig_encrypted"` // AES-GCM 密文
	PrometheusURL string    `json:"prometheus_url"`
	Description   string    `json:"description"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Display 返回展示名（等价于 Python 的 __str__）。
func (c *Cluster) Display() string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.Name
}

// SetKubeconfig 加密后保存。
func (c *Cluster) SetKubeconfig(raw string, aead cipher.AEAD) error {
	c.Kubeconfig = seal(aead, []byte(raw))
	return nil
}

// GetKubeconfig 解密返回原文。
func (c *Cluster) GetKubeconfig(aead cipher.AEAD) (string, error) {
	plain, err := open(aead, c.Kubeconfig)
	if err != nil {
		return "", fmt.Errorf("kubeconfig 解密失败：密钥不匹配或数据损坏（%w）", err)
	}
	return string(plain), nil
}

type config struct {
	NextID          int64      `json:"next_id"`
	ActiveClusterID int64      `json:"active_cluster_id"`
	Clusters        []*Cluster `json:"clusters"`
}

// CacheEntry 是单个 (cluster, kind) 的资源缓存快照。
type CacheEntry struct {
	SyncedAt time.Time       `json:"synced_at"`
	Data     json.RawMessage `json:"data"` // 序列化后的资源数组
}

// SyncError 记录某集群最近一次同步失败的摘要，列表 API 透传给前端做横幅提示。
type SyncError struct {
	ResourceType string `json:"resource_type"`
	Message      string `json:"message"`
	FailedAt     string `json:"failed_at"`
}

// Store 线程安全的文件存储。
type Store struct {
	dir  string
	aead cipher.AEAD

	mu     sync.Mutex // 保护 config.json 的读改写
	metaMu sync.Mutex // 保护缓存文件写入的串行化（单文件原子替换本身安全，这里只防目录并发创建竞态）
}

// Open 打开（必要时初始化）数据目录。目录可用 ARMADA_DATA_DIR 覆盖，默认
// 走 os.UserConfigDir()/Sailor（macOS 为 ~/Library/Application Support/Sailor；由 Armada 更名而来，旧目录自动迁移）。
func Open() (*Store, error) {
	dir := os.Getenv("ARMADA_DATA_DIR")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, "Sailor")
		// 由 Armada 更名而来：旧数据目录整体迁移（同卷 rename，原子且保留
		// 已导入集群与加密密钥）。新旧目录并存时以新目录为准，不覆盖。
		legacy := filepath.Join(base, "Armada")
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Stat(legacy); err == nil {
				if err := os.Rename(legacy, dir); err != nil {
					return nil, fmt.Errorf("迁移旧数据目录失败：%w", err)
				}
			}
		}
	}
	for _, d := range []string{dir, filepath.Join(dir, "cache")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}

	key, err := loadOrCreateKey(filepath.Join(dir, "key.bin"))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	s := &Store{dir: dir, aead: aead}
	if _, err := s.readConfig(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Dir() string { return s.dir }

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func seal(aead cipher.AEAD, plain []byte) []byte {
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, nil)
}

func open(aead cipher.AEAD, data []byte) ([]byte, error) {
	if len(data) < aead.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], nil)
}

// ─── config.json ──────────────────────────────────────────────

func (s *Store) configFile() string { return filepath.Join(s.dir, "config.json") }

func (s *Store) readConfig() (*config, error) {
	b, err := os.ReadFile(s.configFile())
	if errors.Is(err, os.ErrNotExist) {
		return &config{NextID: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config.json 损坏：%w", err)
	}
	if c.NextID == 0 {
		c.NextID = 1
	}
	// 自愈历史数据：显示名称留空的集群回退到集群名（对齐 Django 版
	// display_name or name 的语义），顶栏与面包屑才不会渲染成空
	for _, cl := range c.Clusters {
		normalizeCluster(cl)
	}
	return &c, nil
}

// normalizeCluster 填补展示字段的空值。
func normalizeCluster(cl *Cluster) {
	if cl.DisplayName == "" {
		cl.DisplayName = cl.Name
	}
}

func (s *Store) writeConfig(c *config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.configFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.configFile())
}

// ListClusters 返回全部集群，按创建时间倒序（对齐 Django Meta.ordering）。
func (s *Store) ListClusters() []*Cluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.readConfig()
	out := append([]*Cluster(nil), c.Clusters...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Store) GetCluster(id int64) (*Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.readConfig()
	for _, cl := range c.Clusters {
		if cl.ID == id {
			return cl, nil
		}
	}
	return nil, fmt.Errorf("cluster %d not found", id)
}

// GetClusterByName 供「集群名称唯一」校验和删除确认使用。
func (s *Store) GetClusterByName(name string) (*Cluster, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.readConfig()
	for _, cl := range c.Clusters {
		if cl.Name == name {
			return cl, true
		}
	}
	return nil, false
}

func (s *Store) CreateCluster(cl *Cluster) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.readConfig()
	now := time.Now()
	cl.ID = c.NextID
	cl.CreatedAt = now
	cl.UpdatedAt = now
	if cl.Status == "" {
		cl.Status = "unknown"
	}
	normalizeCluster(cl)
	c.NextID++
	c.Clusters = append(c.Clusters, cl)
	return s.writeConfig(c)
}

func (s *Store) UpdateCluster(cl *Cluster) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.readConfig()
	for i, e := range c.Clusters {
		if e.ID == cl.ID {
			cl.CreatedAt = e.CreatedAt
			cl.UpdatedAt = time.Now()
			normalizeCluster(cl)
			c.Clusters[i] = cl
			return s.writeConfig(c)
		}
	}
	return fmt.Errorf("cluster %d not found", cl.ID)
}

func (s *Store) DeleteCluster(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.readConfig()
	out := c.Clusters[:0]
	for _, e := range c.Clusters {
		if e.ID != id {
			out = append(out, e)
		}
	}
	c.Clusters = out
	if c.ActiveClusterID == id {
		c.ActiveClusterID = 0
	}
	if err := s.writeConfig(c); err != nil {
		return err
	}
	os.RemoveAll(s.cacheDir(id))
	return nil
}

// ActiveCluster 返回当前选中的集群（可能为 nil）。
func (s *Store) ActiveCluster() *Cluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.readConfig()
	for _, cl := range c.Clusters {
		if cl.ID == c.ActiveClusterID {
			return cl
		}
	}
	return nil
}

func (s *Store) SetActiveCluster(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.readConfig()
	c.ActiveClusterID = id
	_ = s.writeConfig(c)
}

// AEAD 暴露给模型层的加解密（等价于 Django 版的 Fernet）。
func (s *Store) AEAD() cipher.AEAD { return s.aead }

// ─── 资源缓存 ─────────────────────────────────────────────────

func (s *Store) cacheDir(clusterID int64) string {
	return filepath.Join(s.dir, "cache", fmt.Sprintf("c%d", clusterID))
}

func (s *Store) cacheFile(clusterID int64, kind string) string {
	return filepath.Join(s.cacheDir(clusterID), kind+".json")
}

// SaveCache 全量替换某个资源类型的缓存（等价于 update_or_create + 整体覆盖）。
func (s *Store) SaveCache(clusterID int64, kind string, data json.RawMessage) error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if err := os.MkdirAll(s.cacheDir(clusterID), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(CacheEntry{SyncedAt: time.Now(), Data: data})
	if err != nil {
		return err
	}
	tmp := s.cacheFile(clusterID, kind) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cacheFile(clusterID, kind))
}

// GetCache 读取缓存；不存在返回 nil。
func (s *Store) GetCache(clusterID int64, kind string) *CacheEntry {
	b, err := os.ReadFile(s.cacheFile(clusterID, kind))
	if err != nil {
		return nil
	}
	var e CacheEntry
	if json.Unmarshal(b, &e) != nil {
		return nil
	}
	return &e
}

// SetSyncError 记录最近一次同步失败（成功路径不要调它，用 ClearSyncError）。
func (s *Store) SetSyncError(clusterID int64, e *SyncError) {
	b, _ := json.Marshal(e)
	_ = os.MkdirAll(s.cacheDir(clusterID), 0o700)
	_ = os.WriteFile(filepath.Join(s.cacheDir(clusterID), "_error.json"), b, 0o600)
}

// ClearSyncError 清除错误标记。
func (s *Store) ClearSyncError(clusterID int64) {
	_ = os.Remove(filepath.Join(s.cacheDir(clusterID), "_error.json"))
}

func (s *Store) GetSyncError(clusterID int64) *SyncError {
	b, err := os.ReadFile(filepath.Join(s.cacheDir(clusterID), "_error.json"))
	if err != nil {
		return nil
	}
	var e SyncError
	if json.Unmarshal(b, &e) != nil {
		return nil
	}
	return &e
}
