// liner：REPL 行编辑器 —— 回显 / 退格 / 清行 / 历史（↑↓）/ Ctrl+C / Ctrl+L。
// 每次键击由 xterm.js 整块送达（方向键是一个完整 \x1b[A 序列），feed 按
// “块 = 键击”的假设解析，块内再逐字节处理可打印字符与控制字符。
package ksh

import (
	"fmt"
	"io"
	"unicode/utf8"
)

const maxHistory = 200

type liner struct {
	prompt string
	buf    []byte // 当前编辑行（原始 UTF-8 字节）
	out    io.Writer

	hist    []string
	histIdx int
	draft   string // 进入历史浏览前的未提交行，↓ 到底时还原

	onSubmit func(line string)
	onEOF    func() // 空行 Ctrl+D
	// onTab：补全回调。返回要插入的文本（可为空）与候选列表；
	// 多候选无公共前缀时由 liner 换行列出并重画当前行（bash 行为）。
	onTab func(line string) (insert string, options []string)
}

func newLiner(prompt string, out io.Writer, onSubmit func(string), onEOF func()) *liner {
	return &liner{prompt: prompt, out: out, onSubmit: onSubmit, onEOF: onEOF}
}

func (l *liner) showPrompt() {
	l.out.Write([]byte(l.prompt))
}

// tab 补全分派：唯一候选 / 公共前缀 → 插入并回显；多候选 → 换行列出
// （最多 40 个）后重画提示符与当前行；无候选 → 响铃。
func (l *liner) tab() {
	if l.onTab == nil {
		return
	}
	insert, options := l.onTab(string(l.buf))
	if insert != "" {
		l.buf = append(l.buf, insert...)
		l.out.Write([]byte(insert))
		return
	}
	if len(options) == 0 {
		l.out.Write([]byte("\a"))
		return
	}
	if len(options) == 1 {
		done := options[0] + " "
		l.buf = append(l.buf, done...)
		l.out.Write([]byte(done))
		return
	}
	l.out.Write([]byte("\r\n"))
	shown := options
	if len(shown) > 40 {
		shown = shown[:40]
	}
	for i, opt := range shown {
		if i > 0 {
			l.out.Write([]byte("  "))
		}
		l.out.Write([]byte(opt))
	}
	if len(options) > 40 {
		fmt.Fprintf(l.out, "  …(+%d)", len(options)-40)
	}
	l.out.Write([]byte("\r\n"))
	l.redraw()
}

func (l *liner) feed(chunk []byte) {
	// 方向键 / 功能键是一次性三字节序列
	if len(chunk) == 3 && chunk[0] == 0x1b && chunk[1] == '[' {
		switch chunk[2] {
		case 'A':
			l.historyPrev()
		case 'B':
			l.historyNext()
		}
		return
	}
	if len(chunk) > 0 && chunk[0] == 0x1b {
		return // 左右方向 / Home 等其他转义序列：光标移动不支持，丢弃
	}

	for _, b := range chunk {
		switch b {
		case '\r', '\n':
			line := string(l.buf)
			l.buf = l.buf[:0]
			l.histIdx = len(l.hist)
			l.draft = ""
			l.out.Write([]byte("\r\n"))
			if line != "" {
				l.hist = append(l.hist, line)
				if len(l.hist) > maxHistory {
					l.hist = l.hist[len(l.hist)-maxHistory:]
				}
				l.histIdx = len(l.hist)
			}
			l.onSubmit(line)
		case 0x7f, 0x08: // Backspace / Ctrl+H
			l.backspace()
		case 0x03: // Ctrl+C：丢弃当前行
			l.out.Write([]byte("^C\r\n"))
			l.buf = l.buf[:0]
			l.histIdx = len(l.hist)
			l.draft = ""
			l.redraw()
		case 0x15: // Ctrl+U：清行
			l.buf = l.buf[:0]
			l.redraw()
		case 0x0c: // Ctrl+L：清屏
			l.out.Write([]byte("\x1b[2J\x1b[H"))
			l.redraw()
		case 0x09: // Tab：命令补全（回调由 Session 注入）
			l.tab()
		case 0x04: // Ctrl+D：空行 = EOF 退出；非空行忽略
			if len(l.buf) == 0 && l.onEOF != nil {
				l.onEOF()
			}
		default:
			if b < 0x20 {
				continue // 其余控制字符（含 Tab）忽略
			}
			l.buf = append(l.buf, b)
			l.out.Write([]byte{b}) // 回显：UTF-8 原样透传
		}
	}
}

// backspace 删最后一个字符。CJK 等宽字符占两格，擦除也要发两个退格。
func (l *liner) backspace() {
	if len(l.buf) == 0 {
		return
	}
	n := len(l.buf) - 1
	for n > 0 && l.buf[n]&0xC0 == 0x80 {
		n-- // 跳过 UTF-8 续字节
	}
	r, _ := utf8.DecodeRune(l.buf[n:])
	l.buf = l.buf[:n]
	if isWide(r) {
		l.out.Write([]byte("\b\b  \b\b"))
	} else {
		l.out.Write([]byte("\b \b"))
	}
}

func (l *liner) historyPrev() {
	if len(l.hist) == 0 || l.histIdx == 0 {
		return
	}
	if l.histIdx == len(l.hist) {
		l.draft = string(l.buf)
	}
	l.histIdx--
	l.buf = append(l.buf[:0], l.hist[l.histIdx]...)
	l.redraw()
}

func (l *liner) historyNext() {
	if l.histIdx >= len(l.hist) {
		return
	}
	l.histIdx++
	if l.histIdx == len(l.hist) {
		l.buf = append(l.buf[:0], l.draft...)
	} else {
		l.buf = append(l.buf[:0], l.hist[l.histIdx]...)
	}
	l.redraw()
}

// redraw 重画当前行：回到行首、清整行、提示符 + 内容。
func (l *liner) redraw() {
	l.out.Write([]byte("\r\x1b[2K"))
	l.out.Write([]byte(l.prompt))
	l.out.Write(l.buf)
}

// isWide 东亚宽字符判定（终端占两列，退格要擦两格）。范围取宽字符的
// 主干，不含组合符等边缘情形。
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 谚文字母
		r >= 0x2E80 && r <= 0x303E, // CJK 部首 / 康熙
		r >= 0x3041 && r <= 0x33FF, // 假名 / 注音 / CJK 补充
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x4E00 && r <= 0x9FFF, // CJK 统一表意
		r >= 0xAC00 && r <= 0xD7A3, // 谚文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容
		r >= 0xFE30 && r <= 0xFE4F,
		r >= 0xFF00 && r <= 0xFF60, // 全角形式
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x2FFFD, // CJK 扩展 B+
		r >= 0x30000 && r <= 0x3FFFD:
		return true
	}
	return false
}
