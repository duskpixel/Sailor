package ksh

import (
	"bytes"
	"strings"
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"get pods", []string{"get", "pods"}},
		{"  get\t  pods -A  ", []string{"get", "pods", "-A"}},
		{"describe pod/nginx-abc -n default", []string{"describe", "pod/nginx-abc", "-n", "default"}},
		{`logs web-0 --tail=100`, []string{"logs", "web-0", "--tail=100"}},
		{`label pod x env=prod --overwrite`, []string{"label", "pod", "x", "env=prod", "--overwrite"}},
		{`get pods -l 'app=web,tier=front'`, []string{"get", "pods", "-l", "app=web,tier=front"}},
		{`get pods -o jsonpath='{.items[*].name}'`, []string{"get", "pods", "-o", "jsonpath={.items[*].name}"}},
		{`annotate pod x note="hello world"`, []string{"annotate", "pod", "x", "note=hello world"}},
		{`get 'a b' c`, []string{"get", "a b", "c"}},
		{`get a\ b`, []string{"get", "a b"}},
		{`-n ''`, []string{"-n", ""}},
		// 中文参数原样透传
		{"get pods 中文", []string{"get", "pods", "中文"}},
	}
	for _, tc := range cases {
		got, err := tokenize(tc.in)
		if err != nil {
			t.Errorf("tokenize(%q) error: %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("tokenize(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("tokenize(%q) = %q, want %q", tc.in, got, tc.want)
				break
			}
		}
	}

	for _, bad := range []string{`get 'unclosed`, `get "unclosed`, `get "close then 'open`} {
		if _, err := tokenize(bad); err == nil {
			t.Errorf("tokenize(%q) 应当报引号错误", bad)
		}
	}
}

func TestStripKubectlPrefix(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"kubectl", "get", "pods"}, []string{"get", "pods"}},
		{[]string{"get", "pods"}, []string{"get", "pods"}},
		{[]string{"kubectl"}, nil},
	}
	for _, tc := range cases {
		got := stripKubectlPrefix(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("stripKubectlPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("stripKubectlPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	}
}

func TestUnsupportedSubcommands(t *testing.T) {
	// edit 不在拦截名单里：被 routeEdit 转发到应用内 YAML 编辑器
	for _, name := range []string{"edit"} {
		if _, bad := unsupported[name]; bad {
			t.Errorf("%q 不应被拦截（已路由到 YAML 编辑器）", name)
		}
	}
	for _, name := range []string{"diff", "port-forward", "proxy", "plugin"} {
		if _, bad := unsupported[name]; !bad {
			t.Errorf("%q 应在不支持名单里", name)
		}
	}
	for _, name := range []string{"get", "describe", "apply", "delete", "logs", "exec", "rollout", "scale", "top", "auth", "explain", "api-resources", "wait", "watch", "config"} {
		if _, bad := unsupported[name]; bad {
			t.Errorf("%q 不应被拦截", name)
		}
	}
}

func TestParseEditArgs(t *testing.T) {
	cases := []struct {
		in          []string
		alias, name string
		ns          string
		hasNS       bool
		wantErr     bool
	}{
		{in: []string{"edit", "deployment/nginx"}, alias: "deployment", name: "nginx"},
		{in: []string{"edit", "deploy", "nginx"}, alias: "deploy", name: "nginx"},
		{in: []string{"edit", "deployments", "web", "-n", "uat"}, alias: "deployments", name: "web", ns: "uat", hasNS: true},
		{in: []string{"edit", "-n", "uat", "svc", "web"}, alias: "svc", name: "web", ns: "uat", hasNS: true},
		{in: []string{"edit", "--namespace=uat", "cm", "cfg"}, alias: "cm", name: "cfg", ns: "uat", hasNS: true},
		{in: []string{"edit", "po", "x"}, alias: "po", name: "x"},
		{in: []string{"edit"}, wantErr: true},
		{in: []string{"edit", "deploy"}, wantErr: true},
		{in: []string{"edit", "deploy/nginx", "extra"}, wantErr: true},
		{in: []string{"edit", "-o", "yaml", "deploy", "x"}, wantErr: true},
		{in: []string{"edit", "deploy", "x", "-n"}, wantErr: true},
	}
	for _, tc := range cases {
		alias, name, ns, hasNS, err := parseEditArgs(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseEditArgs(%q) 应报错", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEditArgs(%q) 报错: %v", tc.in, err)
			continue
		}
		if alias != tc.alias || name != tc.name || ns != tc.ns || hasNS != tc.hasNS {
			t.Errorf("parseEditArgs(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tc.in, alias, name, ns, hasNS, tc.alias, tc.name, tc.ns, tc.hasNS)
		}
	}
}

// recordingSubmit 收集行编辑器提交的行。
type recordingSubmit struct {
	lines []string
}

func (r *recordingSubmit) submit(line string) { r.lines = append(r.lines, line) }

func feedString(l *liner, s string) {
	l.feed([]byte(s))
}

func TestLinerBasic(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingSubmit{}
	l := newLiner("$ ", &out, rec.submit, nil)

	feedString(l, "get")
	feedString(l, " pods\r")
	if len(rec.lines) != 1 || rec.lines[0] != "get pods" {
		t.Fatalf("提交了 %q, want [\"get pods\"]", rec.lines)
	}
	// 回显：逐字符 + 回车换行
	if !strings.Contains(out.String(), "get pods\r\n") {
		t.Errorf("回显内容异常: %q", out.String())
	}
}

func TestLinerBackspace(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingSubmit{}
	l := newLiner("$ ", &out, rec.submit, nil)

	feedString(l, "abc\x7fz\r")
	if rec.lines[0] != "abz" {
		t.Errorf("退格后提交 %q, want \"abz\"", rec.lines[0])
	}
	// CJK 双宽退格：擦两格
	out.Reset()
	feedString(l, "中\x7f\r")
	if rec.lines[1] != "" {
		t.Errorf("中文退格后提交 %q, want \"\"", rec.lines[1])
	}
	if !strings.Contains(out.String(), "\b\b  \b\b") {
		t.Errorf("宽字符退格序列异常: %q", out.String())
	}
}

func TestLinerHistory(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingSubmit{}
	l := newLiner("$ ", &out, rec.submit, nil)

	feedString(l, "one\r")
	feedString(l, "two\r")
	// ↑ 应取回 two，再 ↑ 取 one
	feedString(l, "\x1b[A")
	if string(l.buf) != "two" {
		t.Errorf("一次 ↑ 得到 %q, want \"two\"", l.buf)
	}
	feedString(l, "\x1b[A")
	if string(l.buf) != "one" {
		t.Errorf("两次 ↑ 得到 %q, want \"one\"", l.buf)
	}
	// ↓ 回到 two
	feedString(l, "\x1b[B")
	if string(l.buf) != "two" {
		t.Errorf("↑↑↓ 得到 %q, want \"two\"", l.buf)
	}
}

func TestLinerCtrlKeys(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingSubmit{}
	l := newLiner("$ ", &out, rec.submit, nil)

	// Ctrl+C 清行：不应提交
	feedString(l, "half\x03")
	if len(rec.lines) != 0 {
		t.Errorf("Ctrl+C 不应提交，得到 %q", rec.lines)
	}
	if !strings.Contains(out.String(), "^C") {
		t.Errorf("应回显 ^C: %q", out.String())
	}
	// Ctrl+U 清行后提交空行
	feedString(l, "junk\x15\r")
	if rec.lines[0] != "" {
		t.Errorf("Ctrl+U 后提交 %q, want \"\"", rec.lines[0])
	}
	// Ctrl+L 清屏序列
	out.Reset()
	feedString(l, "\x0c")
	if !strings.Contains(out.String(), "\x1b[2J\x1b[H") {
		t.Errorf("Ctrl+L 应输出清屏序列: %q", out.String())
	}
}

func TestLinerEOF(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingSubmit{}
	eof := false
	l := newLiner("$ ", &out, rec.submit, func() { eof = true })

	feedString(l, "\x04") // 空行 Ctrl+D
	if !eof {
		t.Error("空行 Ctrl+D 应回调 onEOF")
	}
	eof = false
	feedString(l, "x\x04") // 非空行 Ctrl+D：忽略
	if eof {
		t.Error("非空行 Ctrl+D 不应触发 EOF")
	}
}

func TestPromptFor(t *testing.T) {
	p := promptFor("prod")
	if !strings.Contains(p, "kubectl@prod") || !strings.Contains(p, "\x1b[1;36m") {
		t.Errorf("提示符异常: %q", p)
	}
}

// fakeNames 固定名字表：kind → 名字列表。
func fakeNames(clusterID int64, kind, ns string) []string {
	table := map[string][]string{
		"deployment": {"api-gateway", "web-frontend", "web-worker"},
		"pod":        {"api-gateway-7d9c", "web-frontend-5f2a"},
		"namespace":  {"default", "kube-system", "kube-public", "uat"},
	}
	return table[kind]
}

func newTestSession() *Session {
	m := NewManager(fakeNames)
	return &Session{ClusterID: 1, Cluster: "t", m: m}
}

func TestCompleteVerbs(t *testing.T) {
	s := newTestSession()
	// 唯一候选：ge → 补 "t "
	ins, opts := s.complete("ge")
	if ins != "t " || opts != nil {
		t.Errorf("complete(ge) = (%q,%v), want (\"t \",nil)", ins, opts)
	}
	// 多候选无公共延伸：d 开头动词列出
	_, opts = s.complete("d")
	if len(opts) < 2 { // debug/delete/describe/diff/drain
		t.Errorf("complete(d) 应列出多候选, got %v", opts)
	}
	for _, o := range opts {
		if !strings.HasPrefix(o, "d") {
			t.Errorf("候选 %q 未按前缀过滤", o)
		}
	}
	// 内建也在首词候选里
	ins, _ = s.complete("exi")
	if ins != "t " {
		t.Errorf("complete(exi) = %q, want \"t \"", ins)
	}
}

func TestCompleteKinds(t *testing.T) {
	s := newTestSession()
	ins, opts := s.complete("get de")
	if ins != "ploy" { // deploy 与 deployments 公共前缀 deploy
		t.Errorf("complete(get de) = %q, want \"ploy\"", ins)
	}
	_, opts = s.complete("get ")
	if len(opts) == 0 {
		t.Errorf("complete(get ) 应列出类型词表")
	}
	found := false
	for _, o := range opts {
		if o == "deployment" {
			found = true
		}
	}
	if !found {
		t.Errorf("类型候选应含 deployment")
	}
}

func TestCompleteNames(t *testing.T) {
	s := newTestSession()
	// 唯一名字：补全（不带尾空格，名字常是最后一个参数）
	ins, opts := s.complete("get deploy api")
	if ins != "-gateway" || opts != nil {
		t.Errorf("complete(get deploy api) = (%q,%v), want (\"-gateway\",nil)", ins, opts)
	}
	// 多名字公共前缀：web-frontend / web-worker → 补 "web"
	ins, _ = s.complete("get deploy web")
	if ins != "-" { // web-frontend / web-worker 公共前缀是 "web-"（含连字符）
		t.Errorf("complete(get deploy web) = %q, want \"-\"", ins)
	}
	_, opts = s.complete("get deploy web-")
	if len(opts) != 2 {
		t.Errorf("complete(get deploy web) 应 2 候选, got %v", opts)
	}
	// -n 过滤：不同 ns 的同名也应全给出（fake 表不过滤 ns，此处验证 -n 解析不报错）
	if _, opts := s.complete("get deploy -n uat "); len(opts) != 3 {
		t.Errorf("complete(get deploy -n uat ) = %v", opts)
	}
}

func TestCompleteTypeSlashName(t *testing.T) {
	s := newTestSession()
	ins, opts := s.complete("get deploy/api")
	if ins != "-gateway" || opts != nil {
		t.Errorf("complete(get deploy/api) = (%q,%v), want (\"-gateway\",nil)", ins, opts)
	}
	_, opts = s.complete("delete deploy/web")
	for _, o := range opts {
		if !strings.HasPrefix(o, "deploy/") {
			t.Errorf("候选 %q 应保持 type/ 前缀", o)
		}
	}
}

func TestCompleteNamespace(t *testing.T) {
	s := newTestSession()
	ins, _ := s.complete("get pods -n kube-s")
	if ins != "ystem" {
		t.Errorf("complete(-n kube-s) = %q, want \"ystem\"", ins)
	}
	// kube- 前缀多候选 → 列出
	if _, opts := s.complete("get pods -n kube-"); len(opts) < 2 {
		t.Errorf("complete(-n kube-) 应列出多候选, got %v", opts)
	}
}

func TestCompleteLogsRollout(t *testing.T) {
	s := newTestSession()
	// logs 首参直接补 Pod 名
	ins, _ := s.complete("logs api")
	if ins != "-gateway-7d9c" {
		t.Errorf("complete(logs api) = %q", ins)
	}
	// rollout 子命令
	ins, _ = s.complete("rollout re")
	if ins != "s" { // restart / resume 公共前缀 "res"，word "re" → 插 "s"
		t.Errorf("complete(rollout re) = %q, want \"s\"", ins)
	}
	if _, opts := s.complete("rollout "); len(opts) != 6 {
		t.Errorf("complete(rollout ) 候选数 %d, want 6", len(opts))
	}
}

func TestCompleteNoCandidates(t *testing.T) {
	s := newTestSession()
	ins, opts := s.complete("zzz")
	if ins != "" || opts != nil {
		t.Errorf("无候选应返回空, got (%q,%v)", ins, opts)
	}
}
