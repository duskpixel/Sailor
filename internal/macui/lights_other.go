//go:build !darwin

package macui

// RepositionTrafficLights 非 darwin 平台无红绿灯，保持空实现。
func RepositionTrafficLights(_, _, _ float64) bool { return true }
