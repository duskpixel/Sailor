package resops

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestFindEphemeralStatus(t *testing.T) {
	pod := &corev1.Pod{}
	// 普通容器状态在另一个列表里，不应被临时容器查找命中
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app"}}
	pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{
		{Name: "debugger-abc12", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}
	if st := findEphemeralStatus(pod, "debugger-abc12"); st == nil || st.State.Running == nil {
		t.Error("应找到 Running 的临时容器")
	}
	if st := findEphemeralStatus(pod, "app"); st != nil {
		t.Error("不应匹配普通容器状态（临时容器状态列表里才有）")
	}
	if st := findEphemeralStatus(pod, "debugger-none"); st != nil {
		t.Error("不存在的容器应返回 nil")
	}
}

func TestRandSuffix(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := randSuffix(5)
		if len(s) != 5 {
			t.Fatalf("长度错误: %q", s)
		}
		if !strings.HasPrefix(debugContainerPrefix, "debugger-") {
			t.Fatal("前缀常量错误")
		}
		for _, c := range s {
			if !strings.ContainsRune(nameChars, c) {
				t.Fatalf("非法字符 %q in %q", c, s)
			}
		}
	}
}
