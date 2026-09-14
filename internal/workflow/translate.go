package workflow

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// residueRe 匹配 ≥4 连续字母的 token —— "however" 这类未翻译残留。
// <4 字母（Go、SQL、API）噪音太多，不值得查。
var residueRe = regexp.MustCompile(`[A-Za-z]{4,}`)

// residueAllowDefault 审校门默认白名单：专有名词与技术术语。
var residueAllowDefault = []string{
	"Kubernetes", "gRPC", "HTTP", "JSON", "OpenAI", "Anthropic", "Google", "GitHub",
}

// LengthFloorGate 翻译长度下限门：拦"灾难性丢内容"。
// ★ 量纲换算（反模式 75 的防线）：跨语言不能直接比字符数——
// 200 词原文 ≈1100 字符，合格译文 ≈300-400 汉字，"≥原文80%字符"是
// 永不通过的门。取 0.8 汉字/词下限：只拦灾难，不烦正常波动。
func LengthFloorGate(src string) Gate {
	words := len(strings.Fields(src))
	minChars := words * 4 / 5
	return func(ctx context.Context, output string) error {
		if got := len([]rune(strings.TrimSpace(output))); got < minChars {
			return fmt.Errorf("译文仅 %d 字（源文 %d 词，下限 %d 字），疑似丢内容", got, words, minChars)
		}
		return nil
	}
}

// ResidueGate 未翻译残留门：≥4 字母且不在白名单的 token → 拒绝。
func ResidueGate(allowlist []string) Gate {
	allow := make(map[string]bool, len(allowlist))
	for _, w := range allowlist {
		allow[strings.ToLower(w)] = true
	}
	return func(ctx context.Context, output string) error {
		for _, tok := range residueRe.FindAllString(output, -1) {
			if !allow[strings.ToLower(tok)] {
				return fmt.Errorf("未翻译英文残留：%q", tok)
			}
		}
		return nil
	}
}

// TranslateChain 构造翻译三段链：翻译→（长度门）→审校→（残留门）→润色。
// 分工：翻译门管"量"，审校门管"质"。
// 原文烘焙进审校环节的 system——审校要对照原文，而链上只传译文；
// Stage 是数据、按任务现构，这是组合层的自由度。
func TranslateChain(src string) []ChainStep {
	return []ChainStep{
		{
			Stage: Stage{
				Name: "translate",
				SystemPrompt: "你是专业译者。把用户消息中的英文段落完整翻译成中文，" +
					"保留全部信息，不要摘要、不要解释，只输出译文。",
			},
			Gate:    LengthFloorGate(src),
			Retries: 1,
		},
		{
			Stage: Stage{
				Name: "review",
				SystemPrompt: "你是翻译审校。下面是原文。\n【原文】\n" + src +
					"\n\n用户消息是初译译文。对照原文检查并修正：未翻译的英文残留、" +
					"漏译、错译。输出修正后的完整译文，不要输出其他内容。",
			},
			Gate:    ResidueGate(residueAllowDefault),
			Retries: 1,
		},
		{
			Stage: Stage{
				Name: "polish",
				SystemPrompt: "你是中文润色编辑。在完全保留意思的前提下润色用户消息中的译文，" +
					"使其通顺自然。只输出润色后的译文。",
			},
			// Gate 留空：作业 2 在此处外挂人在环门（见 human.go）

		},
	}
}
