package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// HumanGate 构造人在环确认门。
//
// 行为契约：
//
//	y/yes    → 通过（nil）
//	n/no     → 追问一行否决理由；理由作为拒绝错误返回 ——
//	           搭配 Retries≥1 时，runStep 会把理由拼进重试输入：
//	           人否决 → 人给方向 → 模型按方向重做（人指挥重试的闭环）
//	其他输入 → 提示后继续等（不猜、不默认）
//	ctx 取消 → 立即返回 ctx.Err()（Ctrl+C 杀得死等待中的门）
//
// ★ 通道所有权契约（反模式 76 的防线）：本门直接消费 inputCh。
// 安全的前提：唯一的另一读者（CLI 主循环）此刻正阻塞在本门内部
// （同步链）。inputCh 任一时刻恰有一个读者。若链异步化，本门必须
// 改为经主循环路由，否则 y/n 会被主循环抢走。
func HumanGate(inputCh <-chan string, say func(string)) Gate {
	return func(ctx context.Context, output string) error {
		say(output)
		say("── 人工确认：y=通过 / n=否决并给出理由 ──")
		for {
			select {
			case ans, ok := <-inputCh:
				if !ok {
					return errors.New("human gate: input closed")
				}
				switch strings.ToLower(strings.TrimSpace(ans)) {
				case "y", "yes":
					return nil
				case "n", "no":
					say("否决理由（一行，将拼进重试输入）：")
					select {
					case reason, ok := <-inputCh:
						if !ok {
							return errors.New("human gate: input closed")
						}
						if r := strings.TrimSpace(reason); r != "" {
							return fmt.Errorf("用户否决：%s", r)
						}
						return errors.New("用户否决（未给理由）")
					case <-ctx.Done():
						return ctx.Err()
					}
				default:
					say("  请输入 y 或 n")
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}
