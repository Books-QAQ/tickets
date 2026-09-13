package tools

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 提槽策略（§8.1）：**正则优先**，LLM 只在"提槽未命中时决定调哪个工具"，参数槽位仍由这里填充。
// 所有正则都允许夹在中文句子里（Go 的 RE2 无 lookahead，这里只用普通分组）。

var (
	// 订单号：现有系统 order_no 为 UUID（orders.order_no）
	orderNoRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// 车票号：显式写法才认（"车票 12" / "票号：12"），避免把金额、座位号误当车票号
	ticketIDRe = regexp.MustCompile(`(?:车票|票号|ticket)\s*[:：号]?\s*(\d{1,7})`)
	// 日期：绝对日期 2026-09-15 / 2026年9月15日 / 9月15日
	dateAbsRe = regexp.MustCompile(`(\d{4})[-/年](\d{1,2})[-/月](\d{1,2})`)
	dateMDRe  = regexp.MustCompile(`(\d{1,2})月(\d{1,2})[日号]`)
)

// 相对日期词（确定性，按服务器本地日期推算）
var relativeDays = map[string]int{
	"今天": 0, "今日": 0, "当天": 0,
	"明天": 1, "明日": 1,
	"后天": 2,
	"大后天": 3,
}

// ExtractOrderNo 抽订单号（UUID）
func ExtractOrderNo(q string) string { return orderNoRe.FindString(q) }

// ExtractTicketID 抽车票号
func ExtractTicketID(q string) int32 {
	if m := ticketIDRe.FindStringSubmatch(q); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return int32(n)
		}
	}
	return 0
}

// ExtractDate 抽出发日期；返回 (日期, 原文, 是否命中)
func ExtractDate(q string, now time.Time) (time.Time, string, bool) {
	loc := now.Location()
	for word, delta := range relativeDays {
		if strings.Contains(q, word) {
			d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, delta)
			return d, word, true
		}
	}
	if m := dateAbsRe.FindStringSubmatch(q); len(m) == 4 {
		if d, err := time.ParseInLocation("2006-01-02", pad(m[1])+"-"+pad(m[2])+"-"+pad(m[3]), loc); err == nil {
			return d, m[0], true
		}
	}
	if m := dateMDRe.FindStringSubmatch(q); len(m) == 3 {
		if d, err := time.ParseInLocation("2006-01-02", strconv.Itoa(now.Year())+"-"+pad(m[1])+"-"+pad(m[2]), loc); err == nil {
			// 没写年份且已过：按"下一个该日期"理解（12 月问 1 月 = 明年）
			if d.Before(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)) {
				d = d.AddDate(1, 0, 0)
			}
			return d, m[0], true
		}
	}
	return time.Time{}, "", false
}

func pad(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// StationIndex 站点字典（提槽用）：terminal 名 → id，城市名 → 该城市全部 terminal。
// 由 Store 的 terminals/cities 构建，构建期注入（不做每问查询）。
type StationIndex struct {
	terminals map[string][]int32 // 站名 → terminal ids（同名多站则 >1）
	cities    map[string][]int32 // 城市名 → terminal ids
	names     map[int32]string   // terminal id → 站名
	terminalCity map[int32]string
}

// NewStationIndex 构建站点字典
func NewStationIndex(terminals []TerminalRow, cities []CityRow) *StationIndex {
	idx := &StationIndex{
		terminals:    map[string][]int32{},
		cities:       map[string][]int32{},
		names:        map[int32]string{},
		terminalCity: map[int32]string{},
	}
	cityName := map[int32]string{}
	for _, c := range cities {
		cityName[c.ID] = c.Name
	}
	for _, t := range terminals {
		idx.terminals[t.Name] = append(idx.terminals[t.Name], t.ID)
		idx.names[t.ID] = t.Name
		if cn, ok := cityName[t.CityID]; ok {
			idx.cities[cn] = append(idx.cities[cn], t.ID)
			idx.terminalCity[t.ID] = cn
		}
	}
	return idx
}

// TerminalRow / CityRow 站点字典的最小输入（避免 tools 包依赖 sqlc 行类型）
type TerminalRow struct {
	ID     int32
	Name   string
	CityID int32
}

// CityRow 城市
type CityRow struct {
	ID   int32
	Name string
}

// MatchStation 从问题里找站名/城市名（最长优先，避免"北京"吃掉"北京西站"）。
// 返回命中的原文与候选 terminal ids。
func (s *StationIndex) MatchStation(q string) []StationHit {
	if s == nil {
		return nil
	}
	var hits []StationHit
	// 站名按长度倒序，保证"北京西站"先于"北京"
	names := make([]string, 0, len(s.terminals)+len(s.cities))
	for n := range s.terminals {
		names = append(names, n)
	}
	for n := range s.cities {
		if _, dup := s.terminals[n]; !dup { // 站名与城市名同名时以站名优先，不重复计
			names = append(names, n)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})

	used := map[int]bool{} // 已占用的字符区间，防"北京西站"命中后再命中"北京"
	for _, n := range names {
		pos := strings.Index(q, n)
		if pos < 0 {
			continue
		}
		if overlap(used, pos, pos+len(n)) {
			continue
		}
		ids, isTerminal := s.terminals[n]
		if !isTerminal {
			ids = s.cities[n]
		}
		for i := pos; i < pos+len(n); i++ {
			used[i] = true
		}
		hits = append(hits, StationHit{Text: n, TerminalIDs: ids, IsTerminal: isTerminal, Pos: pos})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Pos < hits[j].Pos })
	return hits
}

// StationHit 一次站名命中
type StationHit struct {
	Text        string
	TerminalIDs []int32
	IsTerminal  bool
	Pos         int
}

func overlap(used map[int]bool, from, to int) bool {
	for i := from; i < to; i++ {
		if used[i] {
			return true
		}
	}
	return false
}

// Names 站点 id 列表 → 站名（用于歧义候选展示）
func (s *StationIndex) Names(ids []int32) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := s.names[id]; ok {
			out = append(out, n)
		}
	}
	return out
}
