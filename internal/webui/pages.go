package webui

// RegisterPages 页面模板组合登记。base.html 总在最前；ksh_drawer（kubectl
// 终端抽屉）挂在全局侧栏入口上，所有页面都要带上；资源页额外带 base_list 与
// resource_modals 片段（片段内用 {{define}} 包裹）。
func RegisterPages(r *Renderer) {
	// 公共片段：toast + kubectl 终端抽屉；resource_modals 仅资源列表页需要
	toast := "components/toast.html"
	kshDrawer := "components/ksh_drawer.html"
	resourceModals := "components/resource_modals.html"

	r.Register("dashboard", "base.html", toast, kshDrawer, "dashboard/index.html")

	r.Register("cluster_list", "base.html", toast, kshDrawer, "clusters/list.html")
	r.Register("cluster_add", "base.html", toast, kshDrawer, "clusters/add.html")
	r.Register("cluster_edit", "base.html", toast, kshDrawer, "clusters/edit.html")
	r.Register("cluster_detail", "base.html", toast, kshDrawer, "clusters/detail.html")
	r.Register("nodes", "base.html", toast, kshDrawer, "clusters/nodes.html")
	r.Register("node_detail", "base.html", toast, kshDrawer, "clusters/node_detail.html")

	list := func(page string) []string {
		return []string{"base.html", toast, kshDrawer, resourceModals, "resources/base_list.html", page}
	}
	r.Register("resource_namespace", list("resources/namespace_list.html")...)
	r.Register("resource_deployment", list("resources/deployment_list.html")...)
	r.Register("resource_statefulset", list("resources/statefulset_list.html")...)
	r.Register("resource_daemonset", list("resources/daemonset_list.html")...)
	r.Register("resource_job", list("resources/job_list.html")...)
	r.Register("resource_cronjob", list("resources/cronjob_list.html")...)
	r.Register("resource_hpa", list("resources/hpa_list.html")...)
	r.Register("resource_scaledobject", list("resources/scaledobject_list.html")...)
	r.Register("resource_scaledjob", list("resources/scaledjob_list.html")...)
	r.Register("resource_pod", list("resources/pod_list.html")...)
	r.Register("resource_service", list("resources/service_list.html")...)
	r.Register("resource_ingress", list("resources/ingress_list.html")...)
	r.Register("resource_configmap", list("resources/configmap_list.html")...)
	r.Register("resource_secret", list("resources/secret_list.html")...)
	r.Register("resource_persistentvolumeclaim", list("resources/pvc_list.html")...)
}
