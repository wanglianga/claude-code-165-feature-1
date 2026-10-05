package domain

import (
	"fmt"
	"time"
)

// LateBoxCheckInput 冷链车迟到后，逐箱判断是否仍可继续捐赠所需的输入。
type LateBoxCheckInput struct {
	ProducedAt      time.Time     // 生产时间
	ExpiresAt       time.Time     // 保质期末（生产时间 + 保质时长）
	Now             time.Time     // 判定时刻（车辆晚到后的现场时刻）
	SiteTempC       float64       // 现场（车厢/交接点）实测温度
	StorageTempMaxC float64       // 批次要求的存储温度上限
	RemainingDistKm float64       // 剩余配送距离（到受助点），公里
	AvgSpeedKmH     float64       // 城区冷链配送平均时速，估算在途时间
	TempPenaltyPerC float64       // 每超存储上限 1℃ 的货架寿命折损系数
	SafetyMargin    time.Duration // 送达受助点后须保留的最短安全食用窗口
}

// LateBoxDecision 逐箱判定结果。
type LateBoxDecision struct {
	Donatable       bool
	SafeUntil       time.Time // 折损后预计的可安全食用截止时间
	EffectiveRemain time.Duration
	TravelTime      time.Duration
	Reason          string
}

// EvaluateLateBox 依据生产时间、现场温度、剩余配送距离判断一箱蛋糕是否可继续捐赠。
//
// 判定模型（确定性、可复算、可审计）：
//  1. 硬边界：已过保质期末直接不可捐赠；
//  2. 温度折损：现场温度每高于存储上限 1℃，已消耗货架寿命按 TempPenaltyPerC 加速，
//     得到“折损后剩余货架寿命”；
//  3. 在途时间：剩余距离 / 平均时速；
//  4. 可捐赠条件：折损后剩余寿命 ≥ 在途时间 + 送达后安全窗口。
func EvaluateLateBox(in LateBoxCheckInput) LateBoxDecision {
	if in.AvgSpeedKmH <= 0 {
		in.AvgSpeedKmH = 30
	}
	if in.TempPenaltyPerC <= 0 {
		in.TempPenaltyPerC = 0.12
	}
	if in.SafetyMargin <= 0 {
		in.SafetyMargin = 2 * time.Hour
	}
	shelfLife := in.ExpiresAt.Sub(in.ProducedAt)
	age := in.Now.Sub(in.ProducedAt)

	// 1) 硬边界
	if !in.Now.Before(in.ExpiresAt) {
		return LateBoxDecision{
			Donatable:       false,
			SafeUntil:       in.ExpiresAt,
			EffectiveRemain: 0,
			TravelTime:      travelDuration(in.RemainingDistKm, in.AvgSpeedKmH),
			Reason:          fmt.Sprintf("已到/超过保质期末 %s，不可继续捐赠", in.ExpiresAt.Format("15:04")),
		}
	}

	// 2) 温度折损：仅对超温部分加速消耗货架寿命
	excursion := in.SiteTempC - in.StorageTempMaxC
	if excursion < 0 {
		excursion = 0
	}
	effectiveAge := float64(age) * (1 + in.TempPenaltyPerC*excursion)
	var effectiveRemain time.Duration
	if shelfLifeF := float64(shelfLife); effectiveAge >= shelfLifeF {
		effectiveRemain = 0
	} else {
		effectiveRemain = time.Duration(shelfLifeF - effectiveAge)
	}
	safeUntil := in.Now.Add(effectiveRemain)
	travel := travelDuration(in.RemainingDistKm, in.AvgSpeedKmH)
	need := travel + in.SafetyMargin

	// 3) 4) 距离/窗口判定
	if effectiveRemain < need {
		reason := fmt.Sprintf(
			"现场温度 %.1f℃（上限 %.1f℃，超 %.1f℃）折损后剩余寿命 %s，不足在途 %s + 安全窗口 %s",
			in.SiteTempC, in.StorageTempMaxC, excursion, effectiveRemain.Round(time.Minute),
			travel.Round(time.Minute), in.SafetyMargin.Round(time.Minute))
		if travel == 0 && in.RemainingDistKm > 0 {
			reason = fmt.Sprintf("剩余距离 %.1fkm 无法估算在途时间，保守判为不可捐赠", in.RemainingDistKm)
		}
		return LateBoxDecision{
			Donatable:       false,
			SafeUntil:       safeUntil,
			EffectiveRemain: effectiveRemain,
			TravelTime:      travel,
			Reason:          reason,
		}
	}
	return LateBoxDecision{
		Donatable:       true,
		SafeUntil:       safeUntil,
		EffectiveRemain: effectiveRemain,
		TravelTime:      travel,
		Reason: fmt.Sprintf(
			"现场温度 %.1f℃，折损后剩余寿命 %s，覆盖在途 %s + 安全窗口 %s，可加急发放",
			in.SiteTempC, effectiveRemain.Round(time.Minute), travel.Round(time.Minute),
			in.SafetyMargin.Round(time.Minute)),
	}
}

func travelDuration(distKm, speedKmH float64) time.Duration {
	if distKm <= 0 || speedKmH <= 0 {
		return 0
	}
	hours := distKm / speedKmH
	return time.Duration(hours * float64(time.Hour))
}
