package serialize

import (
	"encoding/json"
	"testing"
)

func mustItem(t *testing.T, kind string, obj map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(Item(kind, obj))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Job 序列化：conditions 决定状态、completions 缺省显示 "-"、duration 取
// startTime→completionTime 差值。
func TestItemJob(t *testing.T) {
	row := mustItem(t, "job", map[string]any{
		"metadata": map[string]any{"name": "pi", "namespace": "default", "creationTimestamp": "2026-09-16T00:00:00Z"},
		"spec": map[string]any{
			"completions": 5,
			"suspend":     false,
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{"image": "perl:5.34"}},
				},
			},
		},
		"status": map[string]any{
			"succeeded":      2,
			"failed":         1,
			"active":         1,
			"startTime":      "2026-09-16T01:00:00Z",
			"completionTime": "2026-09-16T01:30:00Z",
			"conditions":     []any{map[string]any{"type": "Complete"}},
		},
	})
	if row["status_phase"] != "Complete" {
		t.Errorf("status_phase = %v, want Complete", row["status_phase"])
	}
	if row["succeeded"] != float64(2) || row["failed"] != float64(1) {
		t.Errorf("counts = %v/%v", row["succeeded"], row["failed"])
	}
	if row["completions"] != float64(5) {
		t.Errorf("completions = %v, want 5", row["completions"])
	}
	if row["duration"] != "30m" {
		t.Errorf("duration = %v, want 30m", row["duration"])
	}
	if row["image"] != "perl:5.34" {
		t.Errorf("image = %v", row["image"])
	}

	// suspend 且无 conditions → Suspended；completions 缺省 → "-"
	row2 := mustItem(t, "job", map[string]any{
		"metadata": map[string]any{"name": "s", "namespace": "default", "creationTimestamp": "2026-09-16T00:00:00Z"},
		"spec":     map[string]any{"suspend": true},
		"status":   map[string]any{},
	})
	if row2["status_phase"] != "Suspended" {
		t.Errorf("status_phase = %v, want Suspended", row2["status_phase"])
	}
	if row2["completions"] != "-" {
		t.Errorf("completions = %v, want \"-\"", row2["completions"])
	}

	// active>0 且无 conditions → Running
	row3 := mustItem(t, "job", map[string]any{
		"metadata": map[string]any{"name": "r", "namespace": "default", "creationTimestamp": "2026-09-16T00:00:00Z"},
		"spec":     map[string]any{},
		"status":   map[string]any{"active": 2},
	})
	if row3["status_phase"] != "Running" {
		t.Errorf("status_phase = %v, want Running", row3["status_phase"])
	}
}

// CronJob 序列化：schedule/suspend/active/last_schedule 与 jobTemplate 镜像提取。
func TestItemCronjob(t *testing.T) {
	row := mustItem(t, "cronjob", map[string]any{
		"metadata": map[string]any{"name": "batch", "namespace": "default", "creationTimestamp": "2026-09-16T00:00:00Z"},
		"spec": map[string]any{
			"schedule":          "*/5 * * * *",
			"concurrencyPolicy": "Forbid",
			"suspend":           true,
			"jobTemplate": map[string]any{
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{map[string]any{"image": "busybox:1.36"}},
						},
					},
				},
			},
		},
		"status": map[string]any{
			"active":           []any{map[string]any{}, map[string]any{}},
			"lastScheduleTime": "2026-09-16T01:00:00Z",
		},
	})
	if row["schedule"] != "*/5 * * * *" {
		t.Errorf("schedule = %v", row["schedule"])
	}
	if row["suspend"] != true {
		t.Errorf("suspend = %v, want true", row["suspend"])
	}
	if row["active"] != float64(2) {
		t.Errorf("active = %v, want 2", row["active"])
	}
	if row["last_schedule"] == "-" || row["last_schedule"] == "" {
		t.Errorf("last_schedule = %v, want a kubectl-style age", row["last_schedule"])
	}
	if row["image"] != "busybox:1.36" {
		t.Errorf("image = %v, want busybox:1.36 (from jobTemplate)", row["image"])
	}
	if row["concurrency_policy"] != "Forbid" {
		t.Errorf("concurrency_policy = %v", row["concurrency_policy"])
	}

	// suspend 缺省 → false；concurrencyPolicy 缺省 → Allow
	row2 := mustItem(t, "cronjob", map[string]any{
		"metadata": map[string]any{"name": "b2", "namespace": "default", "creationTimestamp": "2026-09-16T00:00:00Z"},
		"spec": map[string]any{
			"schedule":    "@hourly",
			"jobTemplate": map[string]any{"spec": map[string]any{}},
		},
		"status": map[string]any{},
	})
	if row2["suspend"] != false {
		t.Errorf("suspend = %v, want false", row2["suspend"])
	}
	if row2["concurrency_policy"] != "Allow" {
		t.Errorf("concurrency_policy = %v, want Allow", row2["concurrency_policy"])
	}
	if row2["last_schedule"] != "-" {
		t.Errorf("last_schedule = %v, want \"-\"", row2["last_schedule"])
	}
}

// HPA 序列化：目标引用、指标短串、副本数与 ScalingActive 状态。
func TestItemHPA(t *testing.T) {
	row := mustItem(t, "hpa", map[string]any{
		"metadata": map[string]any{"name": "web", "namespace": "default", "creationTimestamp": "2026-09-16T00:00:00Z"},
		"spec": map[string]any{
			"maxReplicas": 10,
			"scaleTargetRef": map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment", "name": "web",
			},
			"metrics": []any{
				map[string]any{
					"type": "Resource",
					"resource": map[string]any{
						"name":   "cpu",
						"target": map[string]any{"type": "Utilization", "averageUtilization": 80},
					},
				},
				map[string]any{
					"type": "External",
					"external": map[string]any{
						"metric": map[string]any{"name": "queue_depth"},
					},
				},
			},
		},
		"status": map[string]any{
			"currentReplicas": 2,
			"desiredReplicas": 4,
			"conditions": []any{
				map[string]any{"type": "ScalingActive", "status": "True"},
			},
		},
	})
	if row["target"] != "Deployment/web" {
		t.Errorf("target = %v, want Deployment/web", row["target"])
	}
	if row["metrics_str"] != "cpu 80%, External" {
		t.Errorf("metrics_str = %v, want \"cpu 80%%, External\"", row["metrics_str"])
	}
	// minReplicas 缺省 → 1
	if row["min_replicas"] != float64(1) {
		t.Errorf("min_replicas = %v, want 1", row["min_replicas"])
	}
	if row["current_replicas"] != float64(2) || row["desired_replicas"] != float64(4) {
		t.Errorf("replicas = %v / %v", row["current_replicas"], row["desired_replicas"])
	}
	if row["status_phase"] != "Active" {
		t.Errorf("status_phase = %v, want Active", row["status_phase"])
	}
}

// ScaledObject / ScaledJob 序列化：triggers 短串、paused 注解、Ready 条件。
func TestItemKeda(t *testing.T) {
	so := mustItem(t, "scaledobject", map[string]any{
		"metadata": map[string]any{
			"name": "so", "namespace": "default", "creationTimestamp": "2026-09-16T00:00:00Z",
			"annotations": map[string]any{"autoscaling.keda.sh/paused-replicas": "0"},
		},
		"spec": map[string]any{
			"scaleTargetRef":  map[string]any{"name": "worker"},
			"minReplicaCount": 2,
			"maxReplicaCount": 20,
			"triggers": []any{
				map[string]any{"type": "kafka"},
				map[string]any{"type": "cron"},
			},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		},
	})
	if so["paused"] != true {
		t.Errorf("paused = %v, want true (annotation present)", so["paused"])
	}
	if so["triggers_str"] != "kafka, cron" {
		t.Errorf("triggers_str = %v", so["triggers_str"])
	}
	if so["target"] != "worker" {
		t.Errorf("target = %v", so["target"])
	}
	if so["min_replicas"] != float64(2) || so["max_replicas"] != float64(20) {
		t.Errorf("replica range = %v - %v", so["min_replicas"], so["max_replicas"])
	}
	if so["status_phase"] != "Ready" {
		t.Errorf("status_phase = %v", so["status_phase"])
	}

	// 无注解 → 未挂起；Ready=False → Error
	sj := mustItem(t, "scaledjob", map[string]any{
		"metadata": map[string]any{"name": "sj", "namespace": "default", "creationTimestamp": "2026-09-16T00:00:00Z"},
		"spec": map[string]any{
			"maxReplicaCount": 5,
			"triggers":        []any{map[string]any{"type": "cron"}},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "False"}},
		},
	})
	if sj["paused"] != false {
		t.Errorf("paused = %v, want false", sj["paused"])
	}
	if sj["status_phase"] != "Error" {
		t.Errorf("status_phase = %v, want Error", sj["status_phase"])
	}
	if sj["max_replica_count"] != float64(5) {
		t.Errorf("max_replica_count = %v", sj["max_replica_count"])
	}
}
