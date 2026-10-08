// liner：REPL 行编辑器 —— 回显 / 退格 / 清行 / 历史（↑↓）/ Ctrl+C / Ctrl+L，
// 以及光标移动（←→/Home/End/Ctrl+A/E/Delete）与 Tab 补全。
//
// 每次键击由 xterm.js 整块送达（方向键是一个完整 \x1b[.. 序列），feed 按
// “块 = 键击”的假设解析，块内再逐字节处理可打印字符与控制字符。
//
// 光标在行中时的编辑（插入 / 退格 / Delete）需要重画后半行并把视觉光标
// 摆回正确列位：CJK 宽字符占两列，移动与擦除都按列宽计算。
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
	pos    int    // 光标位置（buf 字节下标；所有编辑都发生在光标处）
	out    io.Writer

	hist    []string
	histIdx int
	draft   string // 进入历史浏览前的未提交行，↓ 到底时还原
	pending []byte // 跨块未拼完的转义序列缓存

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

// ─── 光标原语 ─────────────────────────────────────────────────

// runeStart 找光标前一个字符的起始字节下标（跳过 UTF-8 续字节）。
func (l *liner) runeStart() int {
	n := l.pos - 1
	for n > 0 && l.buf[n]&0xC0 == 0x80 {
		n--
	}
	return n
}

// segWidth 一段字节占的终端列数（宽字符 2 列）。
func segWidth(b []byte) int {
	w := 0
	for i := 0; i < len(b); {
		r, rn := utf8.DecodeRune(b[i:])
		if rn == 0 {
			break
		}
		if isWide(r) {
			w += 2
		} else {
			w++
		}
		i += rn
	}
	return w
}

func (l *liner) moveLeft(cols int) {
	if cols <= 0 {
		return
	}
	if cols == 1 {
		l.out.Write([]byte("\b"))
		return
	}
	fmt.Fprintf(l.out, "\x1b[%dD", cols)
}

func (l *liner) moveRight(cols int) {
	if cols <= 0 {
		return
	}
	fmt.Fprintf(l.out, "\x1b[%dC", cols)
}

// ─── 编辑操作（全部发生在光标处，尾段重画 + 光标归位）─────────

// insertBytes 在光标处插入字节：回显插入内容 + 重画尾段 + 光标回到插入点后。
func (l *liner) insertBytes(bs []byte) {
	tail := append([]byte{}, l.buf[l.pos:]...)
	l.buf = append(l.buf[:l.pos], append(bs, tail...)...)
	l.out.Write(bs)
	if len(tail) > 0 {
		l.out.Write(tail)
		l.moveLeft(segWidth(tail))
	}
	l.pos += len(bs)
}

// backspace 删除光标前一个字符：左移该字符列宽，重画尾段并用空格擦除
// 残影，光标归位（行尾退格退化为经典的 "\b \b"）。
func (l *liner) backspace() {
	if l.pos == 0 {
		return
	}
	n := l.runeStart()
	r, _ := utf8.DecodeRune(l.buf[n:])
	w := 1
	if isWide(r) {
		w = 2
	}
	tail := append([]byte{}, l.buf[l.pos:]...)
	l.buf = append(l.buf[:n], tail...)
	l.pos = n

	l.moveLeft(w)
	l.out.Write(tail)
	for i := 0; i < w; i++ {
		l.out.Write([]byte(" "))
	}
	l.moveLeft(segWidth(tail) + w)
}

// deleteAtCursor 删除光标处字符（Delete 键），光标位置不动：尾段左移
// 重画，末尾按被删字符的列宽擦残影，再归位。
func (l *liner) deleteAtCursor() {
	if l.pos >= len(l.buf) {
		return
	}
	r, rn := utf8.DecodeRune(l.buf[l.pos:])
	w := 1
	if isWide(r) {
		w = 2
	}
	tail := append([]byte{}, l.buf[l.pos+rn:]...)
	l.buf = append(l.buf[:l.pos], tail...)

	l.out.Write(tail)
	for i := 0; i < w; i++ {
		l.out.Write([]byte(" "))
	}
	l.moveLeft(segWidth(tail) + w)
}

func (l *liner) cursorLeft() {
	if l.pos == 0 {
		return
	}
	n := l.runeStart()
	r, _ := utf8.DecodeRune(l.buf[n:])
	if isWide(r) {
		l.moveLeft(2)
	} else {
		l.moveLeft(1)
	}
	l.pos = n
}

func (l *liner) cursorRight() {
	if l.pos >= len(l.buf) {
		return
	}
	r, rn := utf8.DecodeRune(l.buf[l.pos:])
	if isWide(r) {
		l.moveRight(2)
	} else {
		l.moveRight(1)
	}
	l.pos += rn
}

func (l *liner) cursorHome() {
	l.moveLeft(segWidth(l.buf[:l.pos]))
	l.pos = 0
}

func (l *liner) cursorEnd() {
	l.moveRight(segWidth(l.buf[l.pos:]))
	l.pos = len(l.buf)
}

// ─── 主入口 ───────────────────────────────────────────────────

// feed 消费一块输入。真实场景里块 = 一次键击或一次粘贴，转义序列可能
// 嵌在块中间（粘贴混合内容），也可能被拆在两块之间（极端分片）——按
// 字节流顺序解析：遇 ESC 先拼出完整序列再分派，拼不完的尾巴挂到
// pending 等下一块。
func (l *liner) feed(chunk []byte) {
	if len(l.pending) > 0 {
		chunk = append(l.pending, chunk...)
		l.pending = nil
	}
	i := 0
	for i < len(chunk) {
		if chunk[i] == 0x1b {
			body, n := parseEscape(chunk[i:])
			if n == 0 { // 序列不完整：缓存等待
				l.pending = append([]byte{}, chunk[i:]...)
				return
			}
			if body != "" {
				l.applyEscape(body)
			}
			i += n
			continue
		}
		l.handleByte(chunk[i])
		i++
	}
}

// parseEscape 从 b（以 ESC 开头）解析一条转义序列，返回（序列体, 消费
// 字节数）。n==0 表示序列不完整；body=="" 表示识别出但选择忽略。
func parseEscape(b []byte) (string, int) {
	if len(b) < 2 {
		return "", 0
	}
	switch b[1] {
	case '[': // CSI：参数字节(0x20-0x3F)… 终止字节(0x40-0x7E)
		j := 2
		for j < len(b) && b[j] >= 0x20 && b[j] <= 0x3F {
			j++
		}
		if j >= len(b) {
			return "", 0
		}
		if b[j] < 0x40 || b[j] > 0x7E { // 非法终止字节，整条丢弃
			return "", j + 1
		}
		return string(b[2 : j+1]), j + 1
	case 'O': // SS3：方向键的另一族（\x1bOA…），体统一成 CSI 字母形式
		if len(b) < 3 {
			return "", 0
		}
		c := b[2]
		if c >= 'A' && c <= 'D' {
			return string(c), 3
		}
		return "", 3
	default: // Alt+键 等：ESC + 单字节，忽略
		return "", 2
	}
}

// applyEscape 分派已识别的转义序列体（CSI 去 CSI 前后的体，或 SS3 归一
// 成的单字母）。
func (l *liner) applyEscape(body string) {
	switch body {
	case "A":
		l.historyPrev()
	case "B":
		l.historyNext()
	case "C":
		l.cursorRight()
	case "D":
		l.cursorLeft()
	case "H", "1~", "7~":
		l.cursorHome()
	case "F", "4~", "8~":
		l.cursorEnd()
	case "3~":
		l.deleteAtCursor()
	}
}

func (l *liner) handleByte(b byte) {
	switch b {
	case '\r', '\n':
		// 提交前把视觉光标拨到行尾，保证回显换行后顶格
		l.moveRight(segWidth(l.buf[l.pos:]))
		line := string(l.buf)
		l.buf = l.buf[:0]
		l.pos = 0
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
		l.pos = 0
		l.histIdx = len(l.hist)
		l.draft = ""
		l.redraw()
	case 0x15: // Ctrl+U：清行
		l.buf = l.buf[:0]
		l.pos = 0
		l.redraw()
	case 0x0c: // Ctrl+L：清屏
		l.out.Write([]byte("\x1b[2J\x1b[H"))
		l.redraw()
	case 0x01: // Ctrl+A：行首
		l.cursorHome()
	case 0x05: // Ctrl+E：行尾
		l.cursorEnd()
	case 0x09: // Tab：命令补全（回调由 Session 注入）
		l.tab()
	case 0x04: // Ctrl+D：空行 = EOF 退出；非空行忽略
		if len(l.buf) == 0 && l.onEOF != nil {
			l.onEOF()
		}
	default:
		if b < 0x20 {
			return // 其余控制字符忽略
		}
		l.insertBytes([]byte{b}) // UTF-8 多字节字符的续字节同样落入此处
	}
}

// tab 补全分派：光标在行中时不补全（补全只作用于行尾的词）；唯一候选 /
// 公共前缀 → 插入并回显；多候选 → 换行列出（最多 40 个）后重画提示符与
// 当前行；无候选 → 响铃。
func (l *liner) tab() {
	if l.onTab == nil {
		return
	}
	if l.pos < len(l.buf) {
		l.out.Write([]byte("\a"))
		return
	}
	insert, options := l.onTab(string(l.buf))
	if insert != "" {
		l.insertBytes([]byte(insert))
		return
	}
	if len(options) == 0 {
		l.out.Write([]byte("\a"))
		return
	}
	if len(options) == 1 {
		l.insertBytes([]byte(options[0] + " "))
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

// redraw 重画当前行：回到行首、清整行、提示符 + 内容；光标落在行尾。
func (l *liner) redraw() {
	l.pos = len(l.buf)
	l.out.Write([]byte("\r\x1b[2K"))
	l.out.Write([]byte(l.prompt))
	l.out.Write(l.buf)
}

// isWide 东亚宽字符判定（终端占两列，移动/擦除按两列算）。范围取宽字符的
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
