package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "armada-store-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("ARMADA_DATA_DIR", dir)
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCreateClusterDefaultsDisplayName(t *testing.T) {
	st := openTemp(t)
	c := &Cluster{Name: "prod"}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCluster(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "prod" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "prod")
	}
	if got.Display() != "prod" {
		t.Errorf("Display() = %q", got.Display())
	}
}

func TestReadConfigHealsLegacyEmptyDisplayName(t *testing.T) {
	// 模拟修复前的历史数据：display_name 为空
	dir, err := os.MkdirTemp("", "armada-legacy-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("ARMADA_DATA_DIR", dir)

	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(config{
		NextID: 2,
		Clusters: []*Cluster{
			{ID: 1, Name: "test", DisplayName: "", Status: "online"},
		},
	})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	got := st.ListClusters()
	if len(got) != 1 {
		t.Fatalf("clusters = %d, want 1", len(got))
	}
	if got[0].DisplayName != "test" {
		t.Errorf("DisplayName = %q, want healed to %q", got[0].DisplayName, "test")
	}
}

func TestKubeconfigEncryptRoundTrip(t *testing.T) {
	st := openTemp(t)
	c := &Cluster{Name: "enc"}
	raw := "apiVersion: v1\nkind: Config"
	if err := c.SetKubeconfig(raw, st.AEAD()); err != nil {
		t.Fatal(err)
	}
	if string(c.Kubeconfig) == raw {
		t.Fatal("kubeconfig stored in plaintext")
	}
	back, err := c.GetKubeconfig(st.AEAD())
	if err != nil || back != raw {
		t.Errorf("round trip failed: %q %v", back, err)
	}
}
