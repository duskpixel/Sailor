package main

import (
	"context"
	"log"
	"time"

	"sailor/internal/execsess"
	"sailor/internal/macui"
	"sailor/internal/store"
	"sailor/internal/syncer"
)

// App Wails 生命周期挂钩与少量绑定方法。
type App struct {
	ctx    context.Context
	store  *store.Store
	syncer *syncer.Manager
	exec   *execsess.Manager
}

func NewApp(st *store.Store, syn *syncer.Manager, exec *execsess.Manager) *App {
	return &App{store: st, syncer: syn, exec: exec}
}

// startup 对应 Django ResourcesConfig.ready：为已有集群拉起后台同步。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.syncer.StartAll()
	go a.positionTrafficLights()
}

// positionTrafficLights 等窗口就绪后应用红绿灯自定义边距。
// 窗口创建晚于 OnStartup，因此带重试；成功后再补一次，防初次布局被系统重排。
func (a *App) positionTrafficLights() {
	for i := 0; i < 60; i++ {
		time.Sleep(150 * time.Millisecond)
		if macui.RepositionTrafficLights(trafficLightX, trafficLightY, trafficLightSpacing) {
			time.Sleep(1500 * time.Millisecond)
			if !macui.RepositionTrafficLights(trafficLightX, trafficLightY, trafficLightSpacing) {
				log.Println("traffic lights: reapply skipped (window gone?)")
			}
			return
		}
	}
	log.Println("traffic lights: window never became ready")
}

func (a *App) shutdown(ctx context.Context) {
	a.exec.Stop()
}

// Version 暴露给前端（绑定方法 window.go.main.App.Version）。
func (a *App) Version() string {
	return appVersion
}
