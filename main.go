// Sailor —— 多集群 Kubernetes 管理桌面端（Wails v2 + Go）。
package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"

	"sailor/internal/execsess"
	"sailor/internal/k8sx"
	"sailor/internal/ksh"
	"sailor/internal/metrics"
	"sailor/internal/store"
	"sailor/internal/syncer"
	"sailor/internal/webui"
)

//go:embed all:frontend/dist
var distFS embed.FS

//go:embed all:internal/webui/templates
var templateFS embed.FS

func main() {
	st, err := store.Open()
	if err != nil {
		log.Fatalf("初始化数据目录失败: %v", err)
	}
	pool := k8sx.NewPool()
	syn := syncer.New(st, pool)
	execMgr := execsess.NewManager(pool, st)
	kshMgr := ksh.NewManager()
	agg := metrics.NewAggregator(pool)
	wsPort, err := execMgr.Start()
	if err != nil {
		log.Fatalf("启动终端网关失败: %v", err)
	}
	// ksh 网关端口只随 open 响应里的 ws_url 下发给前端，这里不直接使用
	if _, err := kshMgr.Start(); err != nil {
		log.Fatalf("启动 kubectl 终端网关失败: %v", err)
	}

	// 静态资源：frontend/dist 下的 /static 子树
	staticFS, err := fs.Sub(distFS, "frontend/dist")
	if err != nil {
		log.Fatalf("静态资源加载失败: %v", err)
	}
	tplFS, err := fs.Sub(templateFS, "internal/webui/templates")
	if err != nil {
		log.Fatalf("模板加载失败: %v", err)
	}
	tpl, err := webui.NewRenderer(tplFS)
	if err != nil {
		log.Fatalf("模板初始化失败: %v", err)
	}
	webui.RegisterPages(tpl)

	srv := &webui.Server{
		Store:   st,
		Pool:    pool,
		Syncer:  syn,
		Agg:     agg,
		Exec:    execMgr,
		Ksh:     kshMgr,
		Tpl:     tpl,
		Assets:  staticFS,
		WSPort:  wsPort,
		Version: appVersion,
	}

	app := NewApp(st, syn, execMgr, kshMgr)

	err = wails.Run(&options.App{
		Title:            "Sailor",
		Width:            1440,
		Height:           900,
		MinWidth:         1080,
		MinHeight:        700,
		BackgroundColour: &options.RGBA{R: 15, G: 15, B: 26, A: 1},
		AssetServer: &assetserver.Options{
			Assets: mustSub(distFS, "frontend/dist"),
			// 完全接管路由：页面与 API 走 Go mux（URL 与 Django 版一致），
			// 静态资源由 mux 内的 /static/ 分支从同一个内嵌 FS 提供。
			// index.html 仅用于满足 asset server 的启动校验，实际不可达。
			Middleware: func(next http.Handler) http.Handler {
				return srv.Handler()
			},
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind:       []interface{}{app},
		Mac: &mac.Options{
			// 去掉原生标题栏（与深色主题不搭）：内容延伸到窗口顶部，
			// 红绿灯悬浮在应用自己的顶栏上（顶栏已留出 pl-20 并设为拖拽区）
			TitleBar: &mac.TitleBar{
				TitlebarAppearsTransparent: true,
				FullSizeContent:            true,
				HideTitle:                  true,
			},
			About: &mac.AboutInfo{
				Title:   "Sailor " + appVersion,
				Message: "一个面板管理你所有的 Kubernetes 集群",
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}

func mustSub(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		log.Fatalf("asset sub %s: %v", dir, err)
	}
	return sub
}

const appVersion = "2.0.0"

// 红绿灯（窗口控制按钮）边距：x 距左缘、y 距顶缘（px），spacing 为相邻按钮间距。
// 默认位置贴着窗口角落，微调后与侧栏顶行的 logo 行对齐更协调。
const (
	trafficLightX       = 14.0
	trafficLightY       = 18.0
	trafficLightSpacing = 24.0
)
