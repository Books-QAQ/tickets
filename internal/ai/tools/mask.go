package tools

import (
	"regexp"
	"strings"
)

// 出口脱敏（§8.5.4）：工具结果会进 LLM prompt 也可能被直接展示，**出口统一脱敏**。
// 原则：够用即可 —— 用户核对自己的订单只需认出是哪一单，不需要完整串号；
// 库内/日志内保留完整值，只有"出口"做遮蔽，所以这里是纯函数、不改变数据。

var nameRe = regexp.MustCompile(`^(.{1}).*(.{1})$`)

// MaskOrderNo 订单号遮蔽：保留前 4 后 4（UUID 形如 3f2a...c9d1）
func MaskOrderNo(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:4] + "****" + s[len(s)-4:]
}

// MaskTicketNo 工单号遮蔽：保留前缀与序号（CS20260912-0001 → CS20260912-****）
func MaskTicketNo(s string) string {
	if i := strings.LastIndex(s, "-"); i > 0 && i < len(s)-1 {
		return s[:i+1] + "****"
	}
	return s
}

// MaskName 姓名遮蔽：张三 → 张*，欧阳修 → 欧*修
func MaskName(s string) string {
	r := []rune(strings.TrimSpace(s))
	switch len(r) {
	case 0:
		return ""
	case 1:
		return string(r)
	case 2:
		return string(r[0]) + "*"
	default:
		return string(r[0]) + strings.Repeat("*", len(r)-2) + string(r[len(r)-1])
	}
}

// MaskPhone 手机号遮蔽（未来字段就绪时直接可用）
func MaskPhone(s string) string {
	if len(s) < 7 {
		return s
	}
	return s[:3] + "****" + s[len(s)-4:]
}
