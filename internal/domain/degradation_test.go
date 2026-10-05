package domain

import (
	"testing"
	"time"
)

func TestEvaluateLateBoxExpiredHardBoundary(t *testing.T) {
	now := time.Now()
	produced := now.Add(-30 * time.Hour)
	expires := now.Add(-time.Hour) // 已过保质期
	d := EvaluateLateBox(LateBoxCheckInput{
		ProducedAt: produced, ExpiresAt: expires, Now: now,
		SiteTempC: 6, StorageTempMaxC: 8, RemainingDistKm: 2,
	})
	if d.Donatable {
		t.Fatal("expired box must not be donatable")
	}
}

func TestEvaluateLateBoxNearSiteDonatable(t *testing.T) {
	// 36h 保质期，已生产 24h（余 12h），温度合规，剩余 3km ≈ 6min 在途 + 2h 窗口 => 可捐赠
	now := time.Now()
	d := EvaluateLateBox(LateBoxCheckInput{
		ProducedAt: now.Add(-24 * time.Hour), ExpiresAt: now.Add(12 * time.Hour), Now: now,
		SiteTempC: 6, StorageTempMaxC: 8, RemainingDistKm: 3,
	})
	if !d.Donatable {
		t.Fatalf("near site, compliant temp should be donatable: %s", d.Reason)
	}
	if d.EffectiveRemain <= d.TravelTime+2*time.Hour {
		t.Fatal("effective remain should cover travel plus safety window")
	}
}

func TestEvaluateLateBoxFarDistanceRejected(t *testing.T) {
	// 余 12h 名义寿命，但温度超标 12℃ 折损后不足，且远距离 40km => 不可捐赠
	now := time.Now()
	d := EvaluateLateBox(LateBoxCheckInput{
		ProducedAt: now.Add(-24 * time.Hour), ExpiresAt: now.Add(12 * time.Hour), Now: now,
		SiteTempC: 20, StorageTempMaxC: 8, RemainingDistKm: 40,
	})
	if d.Donatable {
		t.Fatalf("heat-abused far box must not be donatable: %s", d.Reason)
	}
}

func TestEvaluateLateBoxSameTempDifferentDistance(t *testing.T) {
	// 同一批次（同生产时间/温度）：近距离站可加急，远距离站报损，体现逐箱按剩余距离判定
	now := time.Now()
	in := LateBoxCheckInput{
		ProducedAt: now.Add(-26 * time.Hour), ExpiresAt: now.Add(10 * time.Hour), Now: now,
		SiteTempC: 10, StorageTempMaxC: 8,
	}
	near := in
	near.RemainingDistKm = 2
	far := in
	far.RemainingDistKm = 60
	if !EvaluateLateBox(near).Donatable {
		t.Fatal("near stop should be donatable")
	}
	if EvaluateLateBox(far).Donatable {
		t.Fatal("far stop should be written off")
	}
}
