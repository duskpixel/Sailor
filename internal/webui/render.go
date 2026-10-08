// Package webui：页面渲染与 HTTP API。URL 结构与 Django 版一一对应，
// 前端 JS 的 fetch 路径无需改动。
package webui

import (
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

// PageData 所有页面共享的渲染上下文。
type PageData struct {
	PageID string
	Title  string
	// SidebarActive 侧栏当前高亮段（dashboard/clusters/nodes/资源 kind 复数），
	// 由路径推导 —— 整页跳转后侧栏重渲染，高亮跟随让"选中不丢"。
	SidebarActive string
	CurrentPath   string
	Flash         *Flash
	AllClusters   interface{} // []*store.Cluster
	ActiveCluster interface{} // *store.Cluster 或 nil（nil 必须是无类型 nil，模板判空才正确）
	WSPort        int
	Version       string
	Content       interface{} // 页面自己的上下文
	KEDAInstalled bool        // 侧栏 KEDA 入口可见性：同步器探测到集群未安装 KEDA 时收起
}

// Flash 等价于 Django messages 的单条闪现消息（通过 query 参数传递）。
type Flash struct {
	Level string // success / error / info / warning
	Text  string
}

// Renderer 按页面组装 Go 模板集合（base + 公共片段 + 页面自身）。
type Renderer struct {
	funcs template.FuncMap
	pages map[string][]string // page name -> template files
	fsys  fs.FS
}

// NewRenderer: templateFS 里必须含 base.html 与 components/；pages 描述每页的组合。
func NewRenderer(templateFS fs.FS) (*Renderer, error) {
	r := &Renderer{
		funcs: template.FuncMap{
			// dflt 对应 Django 的 |default 过滤器（仅空字符串场景）
			"dflt": func(d string, v string) string {
				if v == "" {
					return d
				}
				return v
			},
			"statusDisplay": func(status string) string {
				switch status {
				case "online":
					return "在线"
				case "offline":
					return "离线"
				default:
					return "未知"
				}
			},
		},
		pages: map[string][]string{},
		fsys:  templateFS,
	}
	return r, nil
}

// Register 登记一个页面模板组合。
func (r *Renderer) Register(name string, files ...string) {
	r.pages[name] = files
}

// Render 渲染页面到 w。
func (r *Renderer) Render(w io.Writer, page string, data *PageData) error {
	files, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("unknown page %q", page)
	}
	// 根模板以空名为锚点：base.html 解析进根，其余文件以路径为名挂进同一集合。
	// 关联模板共享函数表，后解析页面的 {{define "xxx"}} 覆盖 base 的 {{block "xxx"}} 默认内容。
	t := template.New("").Funcs(r.funcs)
	for _, f := range files {
		b, err := fs.ReadFile(r.fsys, f)
		if err != nil {
			return fmt.Errorf("read template %s: %w", f, err)
		}
		if f == files[0] {
			if _, err := t.Parse(string(b)); err != nil {
				return fmt.Errorf("parse %s: %w", f, err)
			}
			continue
		}
		if _, err := t.New(f).Parse(string(b)); err != nil {
			return fmt.Errorf("parse %s: %w", f, err)
		}
	}
	return t.Execute(w, data)
}

// JSON 写一个 JSON 响应。
func JSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, v)
}

// JSONError 写一个 {"error": ...} 响应。
func JSONError(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]interface{}{"error": msg})
}

func flashFromQuery(r *http.Request) *Flash {
	msg := r.URL.Query().Get("flash")
	if msg == "" {
		return nil
	}
	level := r.URL.Query().Get("flash_level")
	if level == "" {
		level = "success"
	}
	return &Flash{Level: level, Text: msg}
}

func redirectWithFlash(w http.ResponseWriter, r *http.Request, url, msg, level string) {
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	if msg != "" {
		url = url + sep + "flash=" + queryEscape(msg) + "&flash_level=" + level
	}
	redirectViaJS(w, url)
}

// redirectViaJS 通过 200 + JS location.replace 完成页面跳转。
//
// 为什么不用 302：macOS 的 WKWebView 对自定义 scheme（wails://）的 scheme-task
// 响应【不跟随 3xx Location】—— 表单提交后页面会停在原地、选择集群后跳到
// 一片空白。这是 WKURLSchemeHandler 的已知限制，所以页面级跳转一律返回
// 200 的极简 HTML，用 JS 跳走（meta refresh 作为无 JS 兜底）。
// replace() 不留历史记录，保持 POST/redirect/GET 语义。
func redirectViaJS(w http.ResponseWriter, target string) {
	// 单引号/双引号先编码掉，target 才能同时安全放进 JS 字符串与 HTML 属性
	safe := strings.NewReplacer("'", "%27", `"`, "%22").Replace(target)
	attr := strings.ReplaceAll(safe, "&", "&amp;")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	page := "<!DOCTYPE html><html style=\"background:#0f0f1a\"><head><meta charset=\"utf-8\">" +
		"<meta http-equiv=\"refresh\" content=\"0;url=" + attr + "\">" +
		"<script>location.replace(\"" + safe + "\");</script></head><body></body></html>"
	fmt.Fprint(w, page)
}

func queryEscape(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~':
			b = append(b, c)
		case c == ' ':
			b = append(b, "%20"...)
		default:
			// UTF-8 多字节逐字节编码
			b = append(b, '%')
			const hex = "0123456789ABCDEF"
			b = append(b, hex[c>>4], hex[c&0xf])
		}
	}
	return string(b)
}
