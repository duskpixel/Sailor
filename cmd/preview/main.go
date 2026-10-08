// preview —— 独立浏览器预览服务（调 UI 主题用，不依赖 Wails / 真实集群）。
// 用法：go run ./cmd/preview，然后打开 http://127.0.0.1:8787
// 数据写在系统临时目录，退出即弃，不碰真实数据目录。
package main

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"

	"sailor/internal/execsess"
	"sailor/internal/k8sx"
	"sailor/internal/ksh"
	"sailor/internal/store"
	"sailor/internal/syncer"
	"sailor/internal/webui"
)

func main() {
	dir, err := os.MkdirTemp("", "sailor-preview-*")
	if err != nil {
		log.Fatal(err)
	}
	os.Setenv("ARMADA_DATA_DIR", dir)
	st, err := store.Open()
	if err != nil {
		log.Fatal(err)
	}

	c := &store.Cluster{
		Name: "preview-cluster", DisplayName: "预览集群", Status: "online",
		K8sVersion: "v1.30.0", APIServer: "https://api.example.com:6443", NodeCount: 3,
		Description: "主题预览用假集群",
	}
	c.SetKubeconfig("apiVersion: v1\nkind: Config", st.AEAD())
	if err := st.CreateCluster(c); err != nil {
		log.Fatal(err)
	}
	st.SetActiveCluster(c.ID)

	staticFS, err := fs.Sub(os.DirFS("frontend/dist"), ".")
	if err != nil {
		log.Fatal(err)
	}
	tplFS, err := fs.Sub(os.DirFS("internal/webui/templates"), ".")
	if err != nil {
		log.Fatal(err)
	}
	tpl, err := webui.NewRenderer(tplFS)
	if err != nil {
		log.Fatal(err)
	}
	webui.RegisterPages(tpl)

	pool := k8sx.NewPool()
	kshMgr := ksh.NewManager(webui.ClusterNameLister(st))
	if _, err := kshMgr.Start(); err != nil {
		log.Fatal(err)
	}
	srv := &webui.Server{
		Store:   st,
		Pool:    pool,
		Syncer:  syncer.New(st, pool),
		Exec:    execsess.NewManager(pool, st),
		Ksh:     kshMgr,
		Tpl:     tpl,
		Assets:  staticFS,
		Version: "preview",
		WSPort:  1,
	}

	addr := "127.0.0.1:8787"
	if p := os.Getenv("PREVIEW_PORT"); p != "" {
		addr = "127.0.0.1:" + p
	}
	fmt.Println("预览地址: http://" + addr + "  (数据目录: " + dir + ")")
	// 静态资源禁缓存：调样式时改完刷新即可见，不用跟浏览器启发式缓存较劲。
	log.Fatal(http.ListenAndServe(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		srv.Handler().ServeHTTP(w, r)
	})))
}
