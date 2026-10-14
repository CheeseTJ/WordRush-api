package api

import (
	"strings"
	"testing"
)

// TestParseChatMarkers 验证模型回复中 [GOALS]/[DONE] 标记的剥离与解析。
func TestParseChatMarkers(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantClean string
		wantGoals []bool
		wantDone  bool
		wantOK    bool
	}{
		{
			name:      "常规标记",
			in:        "Sure, one latte!\n[GOALS:1,0,0]\n[DONE:no]",
			wantClean: "Sure, one latte!",
			wantGoals: []bool{true, false, false},
			wantDone:  false,
			wantOK:    true,
		},
		{
			name:      "全部达成",
			in:        "Perfect, here you go.\n[DONE:yes]\n[GOALS:1,1,1]",
			wantClean: "Perfect, here you go.",
			wantGoals: []bool{true, true, true},
			wantDone:  true,
			wantOK:    true,
		},
		{
			name:      "无标记不炸",
			in:        "Just a plain reply.",
			wantClean: "Just a plain reply.",
			wantGoals: nil,
			wantDone:  false,
			wantOK:    false,
		},
		{
			name:      "多轮标记取最后一个",
			in:        "[GOALS:0,0,0] mid [GOALS:0,1,0]\n[DONE:no]",
			wantClean: "mid",
			wantGoals: []bool{false, true, false},
			wantDone:  false,
			wantOK:    true,
		},
		{
			name:      "标记前后杂文本",
			in:        "Here you are. [GOALS:1,0] Anything else? [DONE:no]",
			wantClean: "Here you are.  Anything else?",
			wantGoals: []bool{true, false},
			wantDone:  false,
			wantOK:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, goals, done, ok := parseChatMarkers(tc.in)
			if clean != tc.wantClean {
				t.Fatalf("clean = %q, want %q", clean, tc.wantClean)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if done != tc.wantDone {
				t.Fatalf("done = %v, want %v", done, tc.wantDone)
			}
			if tc.wantGoals == nil {
				if goals != nil {
					t.Fatalf("goals = %v, want nil", goals)
				}
			} else if len(goals) != len(tc.wantGoals) {
				t.Fatalf("goals = %v, want %v", goals, tc.wantGoals)
			} else {
				for i := range goals {
					if goals[i] != tc.wantGoals[i] {
						t.Fatalf("goals = %v, want %v", goals, tc.wantGoals)
					}
				}
			}
		})
	}
}

// TestScenarioSystemPrompt 验证情景 prompt 拼装：人设、目标、终局指令与输出格式。
func TestScenarioSystemPrompt(t *testing.T) {
	s := chatScenarios["coffee"]
	if s.Name == "" || len(s.Goals) == 0 {
		t.Fatal("coffee 剧本必须注册人设与目标")
	}
	p := scenarioSystemPrompt(s, 1)
	for _, want := range []string{"barista", "order a drink", "[GOALS:1,0,0]", "[DONE:no]", "turn 1"} {
		if !strings.Contains(p, want) {
			t.Fatalf("turn=1 的 prompt 缺少 %q", want)
		}
	}
	final := scenarioSystemPrompt(s, s.Turns)
	if !strings.Contains(final, "FINAL turn") {
		t.Fatal("最后一轮的 prompt 必须包含收尾指令")
	}
}

// TestChatScenariosRegistry 校验前端场景池与后端注册表一致：id 不多不少。
func TestChatScenariosRegistry(t *testing.T) {
	want := []string{"coffee", "airport", "hotel", "restaurant", "shopping", "doctor", "interview", "rent"}
	if len(chatScenarios) != len(want) {
		t.Fatalf("注册表数量 = %d, want %d", len(chatScenarios), len(want))
	}
	for _, id := range want {
		s, ok := chatScenarios[id]
		if !ok {
			t.Fatalf("缺少剧本 %s", id)
		}
		if s.Turns != chatScenarioTurns {
			t.Fatalf("剧本 %s 回合数 = %d, want %d", id, s.Turns, chatScenarioTurns)
		}
	}
}

// TestParseChatSummary 验证结算批改输出的 JSON 抠取与降级。
func TestParseChatSummary(t *testing.T) {
	good := parseChatSummary("blah ```json\n" +
		`{"stars":3,"comment":"很棒！","phrases":[{"en":"That'll be 32 yuan.","zh":"结账报价"}],"fixes":[]}` + "\n``` blah")
	if good.Comment != "很棒！" || len(good.Phrases) != 1 {
		t.Fatalf("合法 JSON 解析失败: %+v", good)
	}
	bad := parseChatSummary("模型抽风了，没有 JSON")
	if bad.Comment == "" || len(bad.Phrases) != 0 {
		t.Fatalf("非法输出应降级为默认结构: %+v", bad)
	}
}
