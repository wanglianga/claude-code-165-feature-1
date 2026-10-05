package domain

import (
	"math"
	"time"
)

// 冷链迟到降级判定参数（集中管理，便于监管复核与策略调参）。
const (
	// DegradationSafeMarginHours 箱到达受助点后必须保留的最短可发放时间窗（小时）。
	DegradationSafeMarginHours = 2.0
	// DegradationUrgentMarginHours 有效剩余保质期短于"在途时间+该窗口"时，
	// 虽可继续捐赠但必须生成加急发放提醒。
	DegradationUrgentMarginHours = 4.0
	// DegradationTempHardBreachC 现场温度超出冷链存储上限多少℃判定为脱冷硬超标（直接报损）。
	DegradationTempHardBreachC = 6.0
	// DegradationTempPenaltyHoursPerC 现场温度每超上限 1℃ 折损的保质期裕度（小时）。
	DegradationTempPenaltyHoursPerC = 1.0
	// DefaultVehicleSpeedKmh 剩余配送距离折算在途时间时使用的默认冷藏车城区均速。
	DefaultVehicleSpeedKmh = 30.0
)

// 降级判定结论。
const (
	DegradeDonate       = "donate"        // 可继续捐赠，正常发放
	DegradeUrgentDonate = "urgent_donate" // 可继续捐赠，但须加急发放
	DegradeWriteOff     = "write_off"     // 不可捐赠，转报损
)

// 降级判定原因。
const (
	DegradeReasonOK                = "ok"
	DegradeReasonUrgentBoundary    = "urgent_shelf_boundary"
	DegradeReasonShelfInsufficient = "shelf_life_insufficient"
	DegradeReasonTempHardBreach    = "temp_hard_breach"
)

// BoxAssessInput 单箱降级评估输入：生产时间/保质期（时间维度）、
// 现场温度（温控维度）、剩余配送距离与车速（在途维度）。
type BoxAssessInput struct {
	BoxNo               string
	ProducedAt          time.Time
	ShelfLifeHours      int
	Now                 time.Time
	ObservedTempC       float64 // 现场实测温度（℃）；非冷链批次传 0 且 RequiresColdChain=false
	RequiresColdChain   bool
	StorageTempMaxC     float64
	RemainingDistanceKm float64
	AvgSpeedKmh         float64
}

// BoxAssessResult 单箱评估结论与判定依据（全部落库，供审计解释）。
type BoxAssessResult struct {
	BoxNo                   string  `json:"box_no"`
	Decision                string  `json:"decision"`
	Reason                  string  `json:"reason"`
	Donatable               bool    `json:"donatable"`
	Urgent                  bool    `json:"urgent"`
	RemainingShelfHours     float64 `json:"remaining_shelf_hours"`
	TransitHours            float64 `json:"transit_hours"`
	RequiredHours           float64 `json:"required_hours"`
	EffectiveRemainingHours float64 `json:"effective_remaining_hours"`
	TempExcessC             float64 `json:"temp_excess_c"`
}

// AssessBox 冷链迟到场景下的箱级降级判定。
//
// 判定逻辑：
//  1. 在途时间 = 剩余配送距离 / 车辆均速；送达后须保留 SafeMargin 小时发放窗口，
//     故所需剩余保质期 required = transit + safe；
//  2. 现场温度超过冷链上限 HardBreach℃（脱冷硬超标）-> 直接报损；
//  3. 每超 1℃ 折损 PenaltyHoursPerC 小时保质期裕度，得到有效剩余保质期；
//  4. 有效剩余 < 所需 -> 报损（剩余保质期不足以覆盖在途+安全发放窗口）；
//  5. 有效剩余 < 在途+加急窗口 -> 可捐赠但生成加急发放提醒；否则正常捐赠。
func AssessBox(in BoxAssessInput) BoxAssessResult {
	speed := in.AvgSpeedKmh
	if speed <= 0 {
		speed = DefaultVehicleSpeedKmh
	}
	transit := 0.0
	if in.RemainingDistanceKm > 0 {
		transit = in.RemainingDistanceKm / speed
	}
	required := transit + DegradationSafeMarginHours
	remaining := in.ProducedAt.Add(time.Duration(in.ShelfLifeHours) * time.Hour).Sub(in.Now).Hours()

	r := BoxAssessResult{
		BoxNo:               in.BoxNo,
		Decision:            DegradeDonate,
		Reason:              DegradeReasonOK,
		Donatable:           true,
		RemainingShelfHours: round1(remaining),
		TransitHours:        round1(transit),
		RequiredHours:       round1(required),
	}

	if in.RequiresColdChain && in.StorageTempMaxC > 0 && in.ObservedTempC > in.StorageTempMaxC {
		r.TempExcessC = round1(in.ObservedTempC - in.StorageTempMaxC)
		if r.TempExcessC > DegradationTempHardBreachC {
			r.Decision = DegradeWriteOff
			r.Reason = DegradeReasonTempHardBreach
			r.Donatable = false
			r.EffectiveRemainingHours = round1(remaining - r.TempExcessC*DegradationTempPenaltyHoursPerC)
			return r
		}
	}
	effective := remaining - r.TempExcessC*DegradationTempPenaltyHoursPerC
	r.EffectiveRemainingHours = round1(effective)
	if effective < required {
		r.Decision = DegradeWriteOff
		r.Reason = DegradeReasonShelfInsufficient
		r.Donatable = false
		return r
	}
	if effective < transit+DegradationUrgentMarginHours {
		r.Decision = DegradeUrgentDonate
		r.Reason = DegradeReasonUrgentBoundary
		r.Urgent = true
	}
	return r
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
