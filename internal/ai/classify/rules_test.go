package classify

import "testing"

// 7 类闭集 + 前置意图（§5.8）
func TestScoreAndPreIntent(t *testing.T) {
	cases := []struct {
		q        string
		wantTop1 string
		wantPI   string
	}{
		{"退票手续费怎么算", "refund", "none"},
		{"支持微信支付吗", "order", "none"},
		{"页面报错怎么办", "app", "none"},
		{"怎么注册账号", "account", "none"},
		{"北京西站几点停止检票", "travel", "none"},
		{"你好", "", "greet"},
		{"我要投诉你们", "", "complaint"},
		{"转人工", "", "human"},
	}
	for _, c := range cases {
		res := Score(c.q)
		if res.Top1 != c.wantTop1 {
			t.Errorf("%q 期望 top1=%s，得到 %s（scores=%v）", c.q, c.wantTop1, res.Top1, res.Scores)
		}
		if res.PreIntent != c.wantPI {
			t.Errorf("%q 期望 pre_intent=%s，得到 %s", c.q, c.wantPI, res.PreIntent)
		}
	}
}

// 信息类提问不该被"投诉"二字吞掉（M1 实测补充的例外规则）
func TestInformationalComplaintIsNotComplaint(t *testing.T) {
	for _, q := range []string{"投诉渠道是什么", "投诉电话多少", "怎么投诉"} {
		if got := PreIntent(q); got == "complaint" {
			t.Errorf("%q 是问信息，不该判成 complaint（得到 %s）", q, got)
		}
	}
	if got := PreIntent("我要投诉你们"); got != "complaint" {
		t.Errorf("真正的投诉行为应判 complaint，得到 %s", got)
	}
}

// gap 充足性判定（E3/E4 的分界）
func TestEnoughGap(t *testing.T) {
	strong := Score("退票手续费怎么算")
	if !strong.Enough() {
		t.Errorf("强信号应判定为置信度充足: scores=%v gap=%d", strong.Scores, strong.Gap)
	}
	weak := Score("这个东西怎么办")
	if weak.Enough() {
		t.Errorf("无信号不应判定为充足: %v", weak.Scores)
	}
}
