//go:build darwin

// Package macui 提供少量 Wails 未暴露的 macOS 原生窗口微调能力。
package macui

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#include "lights.h"
*/
import "C"

// RepositionTrafficLights 重定位红绿灯按钮。
// x/y 为按钮组距窗口左缘/顶缘的边距，spacing 为相邻按钮中心间距的增量。
// 返回 false 表示窗口或按钮尚未就绪（启动早期会这样，稍后重试即可）。
func RepositionTrafficLights(x, y, spacing float64) bool {
	return C.sailor_reposition_traffic_lights(C.double(x), C.double(y), C.double(spacing)) == 1
}
