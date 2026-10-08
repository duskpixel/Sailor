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
