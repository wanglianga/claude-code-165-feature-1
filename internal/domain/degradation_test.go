package domain

import (
	"testing"
	"time"
)

func assessBase(now time.Time) BoxAssessInput {
	return BoxAssessInput{
		BoxNo: "B-001", ProducedAt: now.Add(-8 * time.Hour), ShelfLifeHours: 36,
		Now: now, RequiresColdChain: true, StorageTempMaxC: 8,
		ObservedTempC: 6, RemainingDistanceKm: 15, AvgSpeedKmh: 30,
	}
}

func TestAssessBoxNormalDonate(t *testing.T) {
	now := time.Now()
	r := AssessBox(assessBase(now))
	// 剩余保质期 28h，在途 0.5h，远超加急窗口(4.5h)
	if r.Decision != DegradeDonate || !r.Donatable {
		t.Fatalf("expect donate, got %+v", r)
	}
	if r.TransitHours != 0.5 {
		t.Fatalf("transit should be 0.5h, got %v", r.TransitHours)
	}
}

func TestAssessBoxUrgentNearBoundary(t *testing.T) {
	now := time.Now()
	in := assessBase(now)
	// 剩余保质期 6h：在途 0.5h + 加急窗 4h = 4.5h < 6h < ...
	// required(0.5+2=2.5) < 6 -> 可捐；6 < 4.5? 否。调整为剩余 4h：
	in.ProducedAt = now.Add(-32 * time.Hour) // 36-32 = 4h 剩余
	r := AssessBox(in)
	if r.Decision != DegradeUrgentDonate || !r.Donatable || !r.Urgent {
		t.Fatalf("expect urgent_donate, got %+v", r)
	}
	if r.Reason != DegradeReasonUrgentBoundary {
		t.Fatalf("reason=%s", r.Reason)
	}
}

func TestAssessBoxWriteOffShelfInsufficient(t *testing.T) {
	now := time.Now()
	in := assessBase(now)
	in.RemainingDistanceKm = 60              // 在途 2h，required = 4h
	in.ProducedAt = now.Add(-33 * time.Hour) // 剩余 3h < 4h
	r := AssessBox(in)
	if r.Decision != DegradeWriteOff || r.Donatable {
		t.Fatalf("expect write_off, got %+v", r)
	}
	if r.Reason != DegradeReasonShelfInsufficient {
		t.Fatalf("reason=%s", r.Reason)
	}
}

func TestAssessBoxWriteOffTempHardBreach(t *testing.T) {
	now := time.Now()
	in := assessBase(now)
	in.ObservedTempC = 15 // 上限 8，超标 7℃ > 硬限 6℃
	r := AssessBox(in)
	if r.Decision != DegradeWriteOff || r.Reason != DegradeReasonTempHardBreach {
		t.Fatalf("expect temp hard breach write_off, got %+v", r)
	}
}

func TestAssessBoxTempPenaltyEatsMargin(t *testing.T) {
	now := time.Now()
	in := assessBase(now)
	// 超标 4℃（未到硬限），折损 4h；在途 2h，required 4h
	in.RemainingDistanceKm = 60
	in.ObservedTempC = 12
	// 剩余 7h - 4h 惩罚 = 3h < required 4h -> 报损
	in.ProducedAt = now.Add(-29 * time.Hour)
	r := AssessBox(in)
	if r.Decision != DegradeWriteOff {
		t.Fatalf("expect write_off after temp penalty, got %+v", r)
	}
}

func TestAssessBoxNonColdChainIgnoresTemp(t *testing.T) {
	now := time.Now()
	in := assessBase(now)
	in.RequiresColdChain = false
	in.StorageTempMaxC = 0
	in.ObservedTempC = 30
	r := AssessBox(in)
	if r.Decision != DegradeDonate || r.TempExcessC != 0 {
		t.Fatalf("non-cold-chain should ignore temp, got %+v", r)
	}
}

func TestAssessBoxExpired(t *testing.T) {
	now := time.Now()
	in := assessBase(now)
	in.ProducedAt = now.Add(-37 * time.Hour) // 已过保质期
	r := AssessBox(in)
	if r.Decision != DegradeWriteOff || r.Donatable {
		t.Fatalf("expired box must be written off, got %+v", r)
	}
}
