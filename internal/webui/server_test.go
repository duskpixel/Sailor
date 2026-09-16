package webui

import (
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"sailor/internal/execsess"
	"sailor/internal/k8sx"
	"sailor/internal/store"
	"sailor/internal/syncer"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "armada-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("ARMADA_DATA_DIR", dir)
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func testServer(t *testing.T) *Server {
	st := testStore(t)
	c := &store.Cluster{
		Name: "test-cluster", DisplayName: "测试集群", Status: "online",
		K8sVersion: "v1.30.0", APIServer: "https://api.example.com:6443", NodeCount: 3,
		Description: "测试描述",
	}
	c.SetKubeconfig("apiVersion: v1\nkind: Config", st.AEAD())
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	st.SetActiveCluster(c.ID)

	tpl, err := NewRenderer(os.DirFS("templates"))
	if err != nil {
		t.Fatal(err)
	}
	RegisterPages(tpl)

	pool := k8sx.NewPool()
	return &Server{
		Store:   st,
		Pool:    pool,
		Syncer:  syncer.New(st, pool),
		Exec:    execsess.NewManager(pool, st),
		Tpl:     tpl,
		Version: "2.0.0-test",
		WSPort:  9999,
	}
}

func TestRenderAllPages(t *testing.T) {
	srv := testServer(t)

	cases := []struct {
		path string
	}{
		{"/"},
		{"/clusters/"},
		{"/clusters/add/"},
		{"/clusters/1/"},
		{"/clusters/1/edit/"},
		{"/clusters/1/nodes/manage/"},
		{"/clusters/1/node/node-abc/"},
		{"/resources/1/namespaces/"},
		{"/resources/1/deployments/"},
		{"/resources/1/statefulsets/"},
		{"/resources/1/daemonsets/"},
		{"/resources/1/jobs/"},
		{"/resources/1/cronjobs/"},
		{"/resources/1/hpas/"},
		{"/resources/1/scaledobjects/"},
		{"/resources/1/scaledjobs/"},
		{"/resources/1/pods/"},
		{"/resources/1/services/"},
		{"/resources/1/ingresses/"},
		{"/resources/1/configmaps/"},
		{"/resources/1/secrets/"},
		{"/resources/1/pvcs/"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != 200 {
			body := rec.Body.String()
			if len(body) > 500 {
				body = body[:500]
			}
			t.Errorf("%s: status %d\nbody: %s", tc.path, rec.Code, body)
			continue
		}
		body := rec.Body.String()
		for _, forbidden := range []string{"{%", "{#", "csrfmiddlewaretoken", "«URL", "«PK", "«RES"} {
			if containsSubstr(body, forbidden) {
				t.Errorf("%s: rendered output contains %q", tc.path, forbidden)
			}
		}
	}
}

func TestRenderAllPagesWithoutCluster(t *testing.T) {
	// 空数据目录：未导入任何集群、ActiveCluster 为 nil 的首次启动场景
	st := testStore(t)
	tpl, err := NewRenderer(os.DirFS("templates"))
	if err != nil {
		t.Fatal(err)
	}
	RegisterPages(tpl)
	srv := &Server{Store: st, Tpl: tpl, Version: "test", WSPort: 1}

	for _, path := range []string{"/", "/clusters/", "/clusters/add/"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("%s: status %d", path, rec.Code)
		}
	}
}

func TestClusterSelectRedirect(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest("GET", "/clusters/1/select/?next=/resources/1/pods/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	// Wails 的 WKWebView 不跟随 302，跳转必须是 200 + JS location.replace
	if rec.Code != 200 {
		t.Fatalf("expect 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !containsSubstr(body, "location.replace") || !containsSubstr(body, `/resources/1/pods/`) {
		t.Errorf("expected JS redirect to /resources/1/pods/, got: %s", body)
	}
	if srv.Store.ActiveCluster() == nil || srv.Store.ActiveCluster().ID != 1 {
		t.Errorf("active cluster not set")
	}
}

func TestClusterSelectRedirectRewritesClusterID(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest("GET", "/clusters/1/select/?next=/resources/99/deployments/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	// 旧集群 ID 99 应被改写为当前集群 1
	if !containsSubstr(body, `/resources/1/deployments/`) || containsSubstr(body, "/resources/99/") {
		t.Errorf("cluster id not rewritten: %s", body)
	}
}

func TestClusterAddPostFlow(t *testing.T) {
	srv := testServer(t)
	form := url.Values{}
	form.Set("name", "prod-cluster")
	form.Set("display_name", "生产集群")
	form.Set("kubeconfig_text", "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://api.example.com:6443\n  name: prod\nusers: []\ncontexts: []\n")

	req := httptest.NewRequest("POST", "/clusters/add/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// 表单提交后同样是 200 + JS 跳转（而非 302）
	if rec.Code != 200 {
		t.Fatalf("expect 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !containsSubstr(rec.Body.String(), "location.replace") {
		t.Errorf("expected JS redirect, got: %s", rec.Body.String())
	}
	c, ok := srv.Store.GetClusterByName("prod-cluster")
	if !ok {
		t.Fatal("cluster not created")
	}
	if c.APIServer != "https://api.example.com:6443" {
		t.Errorf("api server not parsed: %q", c.APIServer)
	}
	kube, err := c.GetKubeconfig(srv.Store.AEAD())
	if err != nil || !containsSubstr(kube, "prod") {
		t.Errorf("kubeconfig not stored encrypted/readable: %v", err)
	}
}

func TestResourceListAPIEmptyCache(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest("GET", "/resources/1/api/pods/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expect 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// 未同步过：应返回 syncing=true + never_synced=true
	if !containsSubstr(rec.Body.String(), `"syncing":true`) {
		t.Errorf("expected syncing=true in %s", rec.Body.String())
	}
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
