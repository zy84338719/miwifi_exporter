package profile

import (
	"strconv"
	"strings"
	"time"
)

// 本文件负责「profile 怎么选出来」。
//
// 选法有两条：
//
// 1. 显式：配置里写死 ROUTER_PROFILE=<name>，行为完全可复现。
// 2. 自适应：从 Default 出发，逐个端点试；固件明确说「这个接口不存在」
//    （HTTP 404 / code -1 且非鉴权问题）就把该端点关掉，不再白问。
//
// 为什么值得做自适应：一个不支持 /api/xqnetwork/wifi_list 的老机器，
// 旧行为是每次 scrape 都打一条 WARN 日志、指标恒为 0，而且没人分得清
// 「这个接口本来就没有」和「它坏了」。关掉之后日志干净，指标语义也对。

// recheckInterval 是被禁用端点的复探周期。
//
// 没有复探就会变成「自适应遗忘」：固件升级后新长出来的接口永远采不到。
// 30 分钟是个折中——升级后的第一时间不至于等太久，又不至于每次采集都试。
const recheckInterval = 30 * time.Minute

// unixNow 可在测试里替换。
var unixNow = func() int64 { return time.Now().UnixMilli() }

// RecheckInterval 返回被禁用端点的复探周期，供启动日志展示。
func RecheckInterval() time.Duration { return recheckInterval }

// Select 按名字构造运行期实例。
//
// name 为空或为 auto 时返回 Default 的实例（随后由自适应探测收敛）。
func Select(name string) (*Instance, error) {
	return SelectWith(name, DefaultTimeoutThreshold)
}

// SelectWith 与 Select 相同，但可指定连续超时降级阈值。
func SelectWith(name string, timeoutThreshold int) (*Instance, error) {
	if name == "" || name == Auto {
		return NewInstanceWithTimeoutThreshold(mustLookup(Default), timeoutThreshold), nil
	}
	p, err := Resolve(name)
	if err != nil {
		return nil, err
	}
	return NewInstanceWithTimeoutThreshold(p, timeoutThreshold), nil
}

func mustLookup(name string) Profile {
	p, ok := Lookup(name)
	if !ok {
		panic("profile: 内置 profile " + name + " 未注册")
	}
	return p
}

// RecheckDue 返回距离上次禁用已超过复探间隔、应当重新尝试的端点。
//
// Client 每轮采集前调用它；返回的端点会被重新 Enable 并重新探测。
func (i *Instance) RecheckDue() []string {
	return i.RetryDisabled(recheckInterval.Milliseconds())
}

// DescribeEndpoints 返回一行人类可读的端点清单，用于启动日志。
func (i *Instance) DescribeEndpoints() string {
	enabled := i.EnabledEndpoints()
	disabled := i.DisabledEndpoints()
	if len(disabled) == 0 {
		return "全部 " + strconv.Itoa(len(enabled)) + " 个端点"
	}
	return strconv.Itoa(len(enabled)) + " 个端点（已跳过本机不存在的: " +
		strings.Join(disabled, ", ") + "）"
}
