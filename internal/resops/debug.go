// 临时调试容器（ephemeral container）：等价 kubectl debug --image。
//
// 用 Pod 的 ephemeralContainers 子资源注入一个一次性容器（不占用
// spec.containers，不需要重建 Pod），注入后可直接 exec 进去排查。
// 两个 K8s 侧约束决定了实现细节：
//   - ephemeral 容器写进去就删不掉（随 Pod 生命周期），也没法改 —— 注入前
//     生成唯一容器名避免堆积冲突；
//   - 只有被调度到节点后才会启动，镜像要现场拉取 —— 注入后轮询
//     status.ephemeralContainerStatuses 等 Running，拉取失败
//     （ImagePullBackOff 等）快速失败。
package resops

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const debugContainerPrefix = "debugger-"

// AddDebugContainer 注入临时调试容器，返回容器名。
// targetContainer 为空时不共享进程命名空间（与 kubectl debug 不带 --target 一致）。
func (o *Ops) AddDebugContainer(ctx context.Context, namespace, podName, image, targetContainer string) (string, error) {
	cl, err := o.Pool.Get(o.ClusterID, o.Loader)
	if err != nil {
		return "", err
	}

	// UpdateEphemeralContainers 走 resourceVersion 乐观锁，与其他写入方
	// （sidecar 注入器等）撞上 409 时重读重试
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		pod, err := cl.Typed.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		name := debugContainerPrefix + randSuffix(5)
		for _, ec := range pod.Spec.EphemeralContainers {
			if ec.Name == name {
				name = debugContainerPrefix + randSuffix(8)
			}
		}
		ec := corev1.EphemeralContainer{
			EphemeralContainerCommon: corev1.EphemeralContainerCommon{
				Name:                     name,
				Image:                    image,
				ImagePullPolicy:          corev1.PullIfNotPresent,
				Stdin:                    true,
				TTY:                      true,
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			},
		}
		if targetContainer != "" {
			ec.TargetContainerName = targetContainer
		}
		pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, ec)

		_, err = cl.Typed.CoreV1().Pods(namespace).UpdateEphemeralContainers(ctx, podName, pod, metav1.UpdateOptions{})
		if err == nil {
			return name, nil
		}
		lastErr = err
		if !apierrors.IsConflict(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("写入临时容器与并发修改冲突（已重试）：%w", lastErr)
}

// WaitDebugContainerRunning 轮询直到临时容器 Running；拉取失败 / 容器退出
// 立即报错。返回最后一个 Waiting 原因（超时时给用户看）。
func (o *Ops) WaitDebugContainerRunning(ctx context.Context, namespace, podName, container string, timeout time.Duration) error {
	cl, err := o.Pool.Get(o.ClusterID, o.Loader)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var lastWaiting string
	for {
		pod, err := cl.Typed.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		st := findEphemeralStatus(pod, container)
		if st != nil {
			switch {
			case st.State.Running != nil:
				return nil
			case st.State.Terminated != nil:
				return fmt.Errorf("调试容器已退出（%s）：%s", st.State.Terminated.Reason, st.State.Terminated.Message)
			case st.State.Waiting != nil:
				lastWaiting = st.State.Waiting.Reason
				// ImagePullBackOff / ErrImagePull / CreateContainerConfigError
				// 不会自愈，直接失败给出明确原因
				if lastWaiting == "ImagePullBackOff" || lastWaiting == "ErrImagePull" ||
					lastWaiting == "CreateContainerConfigError" || lastWaiting == "InvalidImageName" {
					return fmt.Errorf("调试容器无法启动：%s（%s）", lastWaiting, st.State.Waiting.Message)
				}
			}
		}
		if time.Now().After(deadline) {
			if lastWaiting != "" {
				return fmt.Errorf("等待调试容器启动超时，当前状态：%s", lastWaiting)
			}
			return fmt.Errorf("等待调试容器启动超时")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func findEphemeralStatus(pod *corev1.Pod, container string) *corev1.ContainerStatus {
	for i := range pod.Status.EphemeralContainerStatuses {
		if pod.Status.EphemeralContainerStatuses[i].Name == container {
			return &pod.Status.EphemeralContainerStatuses[i]
		}
	}
	return nil
}

const nameChars = "abcdefghijklmnopqrstuvwxyz0123456789"

func randSuffix(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = nameChars[rand.Intn(len(nameChars))]
	}
	return string(b)
}
