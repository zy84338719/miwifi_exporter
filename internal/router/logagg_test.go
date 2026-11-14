package router

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/logx"
)

// TestFailureLogAggregation 同质失败只记一次，失败形态变了立刻再记。
//
// 背景：一台路由器长时间不可达时，每一轮采集都会产生一条内容完全相同的
// 「采集部分失败」WARN（实测 .71 上 1 小时 132 行，每行还把七个端点名
// 重复一遍）。真出现这种故障时人只需要知道「它还坏着、坏了多久」，
// 不需要每 30 秒被同一句话刷一次。
func TestFailureLogAggregation(t *testing.T) {
	c := &Client{logger: logx.Delegating("test")}

	// 首次失败必须记录。
	if !c.shouldLogFailure("sigA") {
		t.Fatal("首次失败应当被记录")
	}
	// 窗口内的同质失败不再记录。
	if c.shouldLogFailure("sigA") {
		t.Error("聚合窗口内的同质失败不应重复记录")
	}
	// 失败形态变了（新端点开始失败/原因变了）是新信息，必须立刻记录。
	if !c.shouldLogFailure("sigB") {
		t.Error("失败形态变化应立即记录")
	}
	// 持续时长从最近一次形态变化起算。
	if d := c.failureDuration(); d <= 0 {
		t.Errorf("failureDuration = %v，期望 > 0", d)
	}
}

// TestRecoveredResetsFailureState 恢复后，同一种失败再次出现要算「新故障」。
//
// 恢复日志本身（「采集已恢复」）的价值在于：没有它，一次持续三小时的故障
// 在日志里只有「坏了、坏了、坏了」，没有「好了」——
// 排查的人无法确认链路是否已经恢复。
func TestRecoveredResetsFailureState(t *testing.T) {
	c := &Client{logger: logx.Delegating("test")}
	ctx := context.Background()

	c.shouldLogFailure("sigA")
	c.noteRecovered(ctx)

	// 恢复后再出现同样的失败，应当被当成一次新的故障记录。
	if !c.shouldLogFailure("sigA") {
		t.Error("恢复后再次出现同样失败应重新记录")
	}
	// 持续时长也应当从零重新起算。
	if d := c.failureDuration(); d > time.Second {
		t.Errorf("恢复后 failureDuration 应重置，实际 %v", d)
	}
}

// TestNoRecoveryLogWithoutFailure 没有故障过就不该打「已恢复」。
func TestNoRecoveryLogWithoutFailure(t *testing.T) {
	c := &Client{logger: logx.Delegating("test")}

	c.noteRecovered(context.Background())

	if d := c.failureDuration(); d != 0 {
		t.Errorf("未发生故障时 failureDuration 应为 0，实际 %v", d)
	}
	// 状态必须是干净的：下一轮失败仍然按「首次」处理。
	if !c.shouldLogFailure("sigA") {
		t.Error("无故障历史时首次失败应被记录")
	}
}
