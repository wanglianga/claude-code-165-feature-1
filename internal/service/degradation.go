package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"donation-service/internal/domain"
	"donation-service/internal/store"
)

// DegradeInput 冷链迟到降级评估输入（机构在途上报）。
type DegradeInput struct {
	ObservedTempC       float64            // 默认现场实测温度（℃）
	BoxTemps            map[string]float64 // 逐箱实测温度覆盖（迟到现场逐箱测温；未列出的箱用默认值）
	RemainingDistanceKm float64            // 剩余配送距离（km）
	AvgSpeedKmh         float64            // 预计车辆均速（km/h），缺省 30
	LateMinutes         int                // 车辆晚到分钟数
	Reason              string             // 迟到/现场情况说明
}

// DestructionInput 门店上传报损食品销毁照片或内部处理记录。
type DestructionInput struct {
	BoxNos       []string
	Method       string // photo / internal
	PhotoURLs    []string
	InternalNote string
	HandlerName  string
	WitnessName  string
	DestroyedAt  time.Time
}

// AssessDegradation 冷链迟到降级处理：按生产时间、现场温度、剩余配送距离逐箱判定。
// 可捐赠箱号保留在途（其中临界箱生成加急发放提醒），不可捐赠箱号转报损；
// 通知已预约机构与门店，并即时下调捐赠公示数量。幂等：同批次存在未闭环降级单时直接返回。
func (s *Service) AssessDegradation(ctx context.Context, u *domain.User, batchID int64,
	in DegradeInput) (*domain.Degradation, bool, error) {
	if in.RemainingDistanceKm < 0 {
		return nil, false, domain.ErrBadRequest("剩余配送距离不能为负")
	}
	if in.AvgSpeedKmh == 0 {
		in.AvgSpeedKmh = domain.DefaultVehicleSpeedKmh
	}
	if in.AvgSpeedKmh <= 0 {
		return nil, false, domain.ErrBadRequest("车辆均速必须为正数")
	}
	var deg *domain.Degradation
	idempotent := false
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if b.Status != domain.BatchInDelivery {
			return domain.ErrConflict(fmt.Sprintf("批次当前状态为 %s，仅配送中批次可执行迟到降级评估", b.Status))
		}
		// 生效预约（confirmed；发放完成后为 completed）
		r, err := lockActiveReservation(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if err := RequireCharity(u, r.CharityID); err != nil {
			return err
		}
		// 幂等：已存在未闭环降级单直接返回首次结论
		if existing, _ := s.getActiveDegradation(ctx, tx, batchID); existing != nil {
			deg = existing
			idempotent = true
			return nil
		}
		// 仅评估仍在途箱（已发放箱不参与）
		rows, err := tx.Query(ctx,
			`SELECT box_no FROM boxes WHERE batch_id=$1 AND status=$2 ORDER BY id`,
			batchID, domain.BoxInDelivery)
		if err != nil {
			return err
		}
		var boxNos []string
		for rows.Next() {
			var no string
			if err := rows.Scan(&no); err != nil {
				rows.Close()
				return err
			}
			boxNos = append(boxNos, no)
		}
		rows.Close()
		if len(boxNos) == 0 {
			return domain.ErrConflict("批次已无在途箱，无需降级评估")
		}

		now := time.Now()
		storageMax := 0.0
		if b.StorageTempMaxC != nil {
			storageMax = *b.StorageTempMaxC
		}
		items := make([]domain.DegradationItem, 0, len(boxNos))
		donatable, urgent, writtenOff := []string{}, []string{}, []string{}
		for _, no := range boxNos {
			temp := in.ObservedTempC
			if t, ok := in.BoxTemps[no]; ok {
				temp = t
			}
			res := domain.AssessBox(domain.BoxAssessInput{
				BoxNo: no, ProducedAt: b.ProducedAt, ShelfLifeHours: b.ShelfLifeHours,
				Now: now, ObservedTempC: temp, RequiresColdChain: b.RequiresColdChain,
				StorageTempMaxC: storageMax, RemainingDistanceKm: in.RemainingDistanceKm,
				AvgSpeedKmh: in.AvgSpeedKmh,
			})
			switch res.Decision {
			case domain.DegradeWriteOff:
				writtenOff = append(writtenOff, no)
			case domain.DegradeUrgentDonate:
				urgent = append(urgent, no)
				donatable = append(donatable, no)
			default:
				donatable = append(donatable, no)
			}
			items = append(items, domain.DegradationItem{
				BoxNo: res.BoxNo, Decision: res.Decision, Reason: res.Reason,
				RemainingShelfHours: res.RemainingShelfHours, TransitHours: res.TransitHours,
				RequiredHours: res.RequiredHours, EffectiveRemainingHours: res.EffectiveRemainingHours,
				TempExcessC: res.TempExcessC,
			})
		}

		// 1) 降级异常留痕（独立业务语义，纳入异常台账与凭证链）
		exDetail := map[string]any{
			"observed_temp_c": in.ObservedTempC, "remaining_distance_km": in.RemainingDistanceKm,
			"avg_speed_kmh": in.AvgSpeedKmh, "late_minutes": in.LateMinutes,
			"donatable_box_nos": donatable, "urgent_box_nos": urgent,
			"written_off_box_nos": writtenOff, "reason": in.Reason,
			"reported_by": u.ID,
		}
		exID, err := insertException(ctx, tx, &batchID, &r.ID, domain.ExColdChainDegraded, exDetail, &u.ID)
		if err != nil {
			return err
		}
		if err := s.emitExceptionEvent(ctx, tx, exID, &batchID, &r.ID, domain.ExColdChainDegraded, exDetail); err != nil {
			return err
		}

		// 2) 降级主单 + 箱级明细
		var degID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO degradations(batch_id, reservation_id, exception_id, triggered_by,
			    observed_temp_c, remaining_distance_km, avg_speed_kmh, late_minutes, reason,
			    donatable_box_nos, urgent_box_nos, written_off_box_nos)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id, created_at`,
			batchID, r.ID, exID, u.ID, in.ObservedTempC, in.RemainingDistanceKm, in.AvgSpeedKmh,
			in.LateMinutes, in.Reason, donatable, urgent, writtenOff).
			Scan(&degID, &now); err != nil {
			return err
		}
		for _, it := range items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO degradation_items(degradation_id, box_no, decision, reason,
				    remaining_shelf_hours, transit_hours, required_hours,
				    effective_remaining_hours, temp_excess_c)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				degID, it.BoxNo, it.Decision, it.Reason, it.RemainingShelfHours, it.TransitHours,
				it.RequiredHours, it.EffectiveRemainingHours, it.TempExcessC); err != nil {
				return err
			}
		}

		// 3) 不可捐赠箱号转报损
		if len(writtenOff) > 0 {
			tag, err := tx.Exec(ctx,
				`UPDATE boxes SET status=$1 WHERE batch_id=$2 AND status=$3 AND box_no = ANY($4)`,
				domain.BoxWrittenOff, batchID, domain.BoxInDelivery, writtenOff)
			if err != nil {
				return err
			}
			if int(tag.RowsAffected()) != len(writtenOff) {
				return domain.ErrConflict("箱状态已变化，请刷新后重试降级评估")
			}
		}

		// 4) 降级凭证（箱级判定依据全部入哈希链；用 map 保证键序与 jsonb 规范化一致）
		itemMaps := make([]map[string]any, 0, len(items))
		for _, it := range items {
			itemMaps = append(itemMaps, map[string]any{
				"box_no": it.BoxNo, "decision": it.Decision, "reason": it.Reason,
				"remaining_shelf_hours": it.RemainingShelfHours, "transit_hours": it.TransitHours,
				"required_hours": it.RequiredHours, "effective_remaining_hours": it.EffectiveRemainingHours,
				"temp_excess_c": it.TempExcessC,
			})
		}
		if err := insertVoucher(ctx, tx, batchID, &r.ID, domain.VoucherDegradation, map[string]any{
			"degradation_id": degID, "exception_id": exID,
			"observed_temp_c": in.ObservedTempC, "remaining_distance_km": in.RemainingDistanceKm,
			"avg_speed_kmh": in.AvgSpeedKmh, "late_minutes": in.LateMinutes,
			"donatable_box_nos": donatable, "urgent_box_nos": urgent,
			"written_off_box_nos": writtenOff, "items": itemMaps,
		}); err != nil {
			return err
		}

		// 5) 通知已预约机构（箱号去向 + 加急提醒）与门店（报损待销毁）
		if err := s.notifyOrgTx(ctx, tx, &r.CharityID, nil, batchID, "degradation_result",
			fmt.Sprintf("批次 %s 冷链迟到降级：%d 箱可接收、%d 箱已报损", b.BatchNo, len(donatable), len(writtenOff)),
			fmt.Sprintf("可接收箱号：%v；报损箱号：%v。请按调整后计划发放并补录实际送达时间。", donatable, writtenOff),
			map[string]any{"degradation_id": degID, "donatable_box_nos": donatable,
				"written_off_box_nos": writtenOff, "urgent_box_nos": urgent}); err != nil {
			return err
		}
		if len(urgent) > 0 {
			if err := s.notifyOrgTx(ctx, tx, &r.CharityID, nil, batchID, "urgent_dispatch",
				fmt.Sprintf("批次 %s 有 %d 箱临近保质期边界，请加急发放", b.BatchNo, len(urgent)),
				fmt.Sprintf("加急箱号：%v；送达后须立即补录实际送达时间。", urgent),
				map[string]any{"degradation_id": degID, "urgent_box_nos": urgent}); err != nil {
				return err
			}
		}
		if len(writtenOff) > 0 {
			if err := s.notifyOrgTx(ctx, tx, nil, &b.StoreID, batchID, "destruction_due",
				fmt.Sprintf("批次 %s 有 %d 箱降级报损，待上传销毁凭证", b.BatchNo, len(writtenOff)),
				fmt.Sprintf("报损箱号：%v；请上传销毁照片或内部处理记录。", writtenOff),
				map[string]any{"degradation_id": degID, "written_off_box_nos": writtenOff}); err != nil {
				return err
			}
		}

		// 6) 即时同步捐赠公示数量（报损箱不计入捐赠数量）
		if err := s.adjustPublicityForDegradationTx(ctx, tx, batchID, len(writtenOff)); err != nil {
			return err
		}

		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "degradation.assessed", map[string]any{
			"batch_id": batchID, "reservation_id": r.ID, "degradation_id": degID,
			"donatable_boxes": len(donatable), "urgent_boxes": len(urgent),
			"written_off_boxes": len(writtenOff),
		}); err != nil {
			return err
		}
		deg, err = s.getDegradation(ctx, tx, degID)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return deg, idempotent, nil
}

// SubmitActualDelivery 接收方补录实际送达时间（降级后发放的必填项）。
func (s *Service) SubmitActualDelivery(ctx context.Context, u *domain.User, distID int64,
	actualDeliveredAt time.Time) (*domain.Distribution, error) {
	if actualDeliveredAt.IsZero() {
		return nil, domain.ErrBadRequest("实际送达时间必填")
	}
	if actualDeliveredAt.After(time.Now().Add(time.Minute)) {
		return nil, domain.ErrBadRequest("实际送达时间不能晚于当前时间")
	}
	var dist *domain.Distribution
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		d, err := s.findDistributionByStop(ctx, tx, distID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		r, err := lockReservation(ctx, tx, d.ReservationID)
		if err != nil {
			return err
		}
		if err := RequireCharity(u, r.CharityID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE distributions SET actual_delivered_at=$1 WHERE id=$2`, actualDeliveredAt, distID); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", r.BatchID), "delivery.actual_time", map[string]any{
			"batch_id": r.BatchID, "distribution_id": distID,
			"actual_delivered_at": actualDeliveredAt,
		}); err != nil {
			return err
		}
		// 最后一笔送达时间补录后，若销毁凭证齐全则自动收敛降级闭环
		if _, cerr := s.tryCloseDegradationTx(ctx, tx, r.BatchID); cerr != nil {
			return cerr
		}
		dist, err = s.findDistributionByStop(ctx, tx, distID)
		return err
	})
	return dist, err
}

// SubmitDestruction 门店为报损箱上传销毁照片或内部处理记录。
func (s *Service) SubmitDestruction(ctx context.Context, u *domain.User, batchID int64,
	in DestructionInput) (*domain.DestructionRecord, error) {
	if in.Method != "photo" && in.Method != "internal" {
		return nil, domain.ErrBadRequest("处理方式 method 取值: photo/internal")
	}
	if in.Method == "photo" && len(in.PhotoURLs) == 0 {
		return nil, domain.ErrBadRequest("销毁照片方式至少上传一张照片 URL")
	}
	if in.Method == "internal" && in.InternalNote == "" {
		return nil, domain.ErrBadRequest("内部处理方式必须填写处理记录")
	}
	if in.HandlerName == "" {
		return nil, domain.ErrBadRequest("处理经手人必填")
	}
	if in.DestroyedAt.IsZero() {
		in.DestroyedAt = time.Now()
	}
	var rec *domain.DestructionRecord
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if err := RequireStore(u, b.StoreID); err != nil {
			return err
		}
		deg, _ := s.getActiveDegradation(ctx, tx, batchID)
		if deg == nil {
			return domain.ErrConflict("该批次不存在未闭环的降级单，无需上传销毁凭证")
		}
		// 未指定箱号时覆盖全部报损箱
		targets := in.BoxNos
		if len(targets) == 0 {
			targets = deg.WrittenOffBoxNos
		}
		if len(targets) == 0 {
			return domain.ErrBadRequest("降级单无报损箱号")
		}
		// 校验：均为该批次 written_off 箱，且未重复上传凭证
		rows, err := tx.Query(ctx,
			`SELECT box_no FROM boxes WHERE batch_id=$1 AND status=$2 AND box_no = ANY($3)`,
			batchID, domain.BoxWrittenOff, targets)
		if err != nil {
			return err
		}
		valid := map[string]bool{}
		for rows.Next() {
			var no string
			if err := rows.Scan(&no); err != nil {
				rows.Close()
				return err
			}
			valid[no] = true
		}
		rows.Close()
		for _, no := range targets {
			if !valid[no] {
				return domain.ErrUnprocessable(fmt.Sprintf("箱号 %s 不是本批次待销毁的报损箱", no))
			}
		}
		var covered []string
		crows, err := tx.Query(ctx,
			`SELECT box_nos FROM destruction_records WHERE batch_id=$1`, batchID)
		if err != nil {
			return err
		}
		for crows.Next() {
			var nos []string
			if err := crows.Scan(&nos); err != nil {
				crows.Close()
				return err
			}
			covered = append(covered, nos...)
		}
		crows.Close()
		for _, no := range covered {
			if valid[no] {
				return domain.ErrConflict(fmt.Sprintf("箱号 %s 已上传销毁凭证，请勿重复", no))
			}
		}

		var id int64
		var createdAt time.Time
		photoURLs := in.PhotoURLs
		if photoURLs == nil {
			photoURLs = []string{}
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO destruction_records(batch_id, store_id, box_nos, method, photo_urls,
			    internal_note, handler_name, witness_name, destroyed_at, uploaded_by)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id, created_at`,
			batchID, b.StoreID, targets, in.Method, photoURLs, in.InternalNote,
			in.HandlerName, in.WitnessName, in.DestroyedAt, u.ID).Scan(&id, &createdAt); err != nil {
			return err
		}
		if err := insertVoucher(ctx, tx, batchID, &deg.ReservationID, domain.VoucherDestruction, map[string]any{
			"destruction_id": id, "box_nos": targets, "method": in.Method,
			"photo_count": len(in.PhotoURLs), "handler_name": in.HandlerName,
			"witness_name": in.WitnessName, "destroyed_at": in.DestroyedAt,
		}); err != nil {
			return err
		}
		rd, err := getReservation(ctx, tx, deg.ReservationID)
		if err != nil {
			return err
		}
		if err := s.notifyOrgTx(ctx, tx, &rd.CharityID, nil, batchID, "degradation_result",
			fmt.Sprintf("批次 %s 报损箱销毁凭证已上传", b.BatchNo),
			fmt.Sprintf("门店已完成箱号 %v 的销毁/内部处理记录。", targets),
			map[string]any{"destruction_id": id, "box_nos": targets}); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "destruction.recorded", map[string]any{
			"batch_id": batchID, "destruction_id": id, "boxes": len(targets),
		}); err != nil {
			return err
		}
		rec = &domain.DestructionRecord{
			ID: id, BatchID: batchID, StoreID: b.StoreID, BoxNos: targets, Method: in.Method,
			PhotoURLs: in.PhotoURLs, InternalNote: in.InternalNote, HandlerName: in.HandlerName,
			WitnessName: in.WitnessName, DestroyedAt: in.DestroyedAt, UploadedBy: u.ID, CreatedAt: createdAt,
		}
		// 满足闭环条件则自动闭环
		_, _ = s.tryCloseDegradationTx(ctx, tx, batchID)
		return nil
	})
	return rec, err
}

// CloseDegradation 显式收敛降级闭环（机构/门店/监管均可触发，条件不满足返回 422）。
func (s *Service) CloseDegradation(ctx context.Context, u *domain.User, batchID int64) (*domain.Degradation, error) {
	var deg *domain.Degradation
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		active, err := s.getActiveDegradation(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if active == nil {
			return domain.ErrNotFound
		}
		if u.Role != domain.RoleAdmin && u.Role != domain.RoleRegulator {
			r, rerr := getReservation(ctx, tx, active.ReservationID)
			if rerr != nil {
				return rerr
			}
			if cerr := RequireCharity(u, r.CharityID); cerr != nil {
				if serr := RequireStore(u, b.StoreID); serr != nil {
					return domain.ErrForbidden
				}
			}
		}
		missing, err := s.missingClosureEvidence(ctx, tx, active)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return domain.ErrUnprocessable(fmt.Sprintf("降级闭环条件未满足：%v", missing))
		}
		if err := s.doCloseDegradationTx(ctx, tx, b, active); err != nil {
			return err
		}
		deg, err = s.getDegradation(ctx, tx, active.ID)
		return err
	})
	return deg, err
}

// missingClosureEvidence 返回未满足的闭环条件列表：
// 报损箱销毁凭证齐全 + 全部发放记录已补录实际送达时间 + 无在途箱。
func (s *Service) missingClosureEvidence(ctx context.Context, q txQuerier, deg *domain.Degradation) ([]string, error) {
	var missing []string
	if len(deg.WrittenOffBoxNos) > 0 {
		rows, err := q.Query(ctx,
			`SELECT box_nos FROM destruction_records WHERE batch_id=$1`, deg.BatchID)
		if err != nil {
			return nil, err
		}
		covered := map[string]bool{}
		for rows.Next() {
			var nos []string
			if err := rows.Scan(&nos); err != nil {
				rows.Close()
				return nil, err
			}
			for _, no := range nos {
				covered[no] = true
			}
		}
		rows.Close()
		for _, no := range deg.WrittenOffBoxNos {
			if !covered[no] {
				missing = append(missing, fmt.Sprintf("报损箱 %s 缺少销毁照片或内部处理记录", no))
			}
		}
	}
	var inDelivery int
	if err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM boxes WHERE batch_id=$1 AND status=$2`,
		deg.BatchID, domain.BoxInDelivery).Scan(&inDelivery); err != nil {
		return nil, err
	}
	if inDelivery > 0 {
		missing = append(missing, fmt.Sprintf("仍有 %d 箱在途未发放", inDelivery))
	}
	var noTime int
	if err := q.QueryRow(ctx, `
		SELECT COUNT(*) FROM distributions d
		JOIN reservations r ON r.id = d.reservation_id
		WHERE r.batch_id=$1 AND d.actual_delivered_at IS NULL`, deg.BatchID).Scan(&noTime); err != nil {
		return nil, err
	}
	if noTime > 0 {
		missing = append(missing, fmt.Sprintf("%d 条发放记录未补录实际送达时间", noTime))
	}
	return missing, nil
}

// tryCloseDegradationTx 在销毁凭证上传/最后一笔送达时间补录后尝试自动闭环。
func (s *Service) tryCloseDegradationTx(ctx context.Context, tx pgx.Tx, batchID int64) (bool, error) {
	active, err := s.getActiveDegradation(ctx, tx, batchID)
	if err != nil || active == nil {
		return false, err
	}
	missing, err := s.missingClosureEvidence(ctx, tx, active)
	if err != nil || len(missing) > 0 {
		return false, err
	}
	b, err := lockBatch(ctx, tx, batchID)
	if err != nil {
		return false, err
	}
	return true, s.doCloseDegradationTx(ctx, tx, b, active)
}

// doCloseDegradationTx 执行降级闭环：批次 in_delivery→distributed→closed，
// 预约 completed，公示按"实际捐赠箱数=交接-报损"重算，闭环凭证 + 事件。
func (s *Service) doCloseDegradationTx(ctx context.Context, tx pgx.Tx, b *domain.Batch, deg *domain.Degradation) error {
	if _, err := tx.Exec(ctx,
		`UPDATE boxes SET status=$1 WHERE batch_id=$2 AND status=$3`,
		domain.BoxDistributed, b.ID, domain.BoxInDelivery); err != nil {
		return err
	}
	fromStatus := b.Status
	if fromStatus == domain.BatchInDelivery {
		if err := transitionBatch(ctx, tx, b.ID, domain.BatchInDelivery, domain.BatchDistributed); err != nil {
			return err
		}
		if err := transitionBatch(ctx, tx, b.ID, domain.BatchDistributed, domain.BatchClosed); err != nil {
			return err
		}
	} else if fromStatus == domain.BatchDistributed {
		if err := transitionBatch(ctx, tx, b.ID, domain.BatchDistributed, domain.BatchClosed); err != nil {
			return err
		}
	} else {
		return domain.ErrConflict(fmt.Sprintf("批次状态 %s 不可执行降级闭环", fromStatus))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE reservations SET status=$1, version=version+1, updated_at=now()
		WHERE id=$2`, domain.ResvCompleted, deg.ReservationID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE degradations SET status='closed', closed_at=now() WHERE id=$1`, deg.ID); err != nil {
		return err
	}
	if err := s.projectPublicityTx(ctx, tx, b.ID); err != nil {
		return err
	}
	if err := insertVoucher(ctx, tx, b.ID, &deg.ReservationID, domain.VoucherClosure, map[string]any{
		"degradation_id": deg.ID, "closed_by_degradation": true,
		"total_boxes": b.TotalBoxes, "written_off_boxes": len(deg.WrittenOffBoxNos),
		"donated_boxes": b.TotalBoxes - len(deg.WrittenOffBoxNos),
	}); err != nil {
		return err
	}
	return insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", b.ID), "degradation.closed", map[string]any{
		"batch_id": b.ID, "degradation_id": deg.ID,
		"written_off_boxes": len(deg.WrittenOffBoxNos),
		"donated_boxes":     b.TotalBoxes - len(deg.WrittenOffBoxNos),
	})
}

// adjustPublicityForDegradationTx 降级评估后即时同步公示：报损箱从捐赠数量中剔除。
func (s *Service) adjustPublicityForDegradationTx(ctx context.Context, q txQuerier, batchID int64, writtenOff int) error {
	var batchNo, storeName, charityName, category string
	var totalBoxes int
	err := q.QueryRow(ctx, `
		SELECT b.batch_no, st.name, ch.name, b.category, b.total_boxes
		FROM batches b
		JOIN stores st ON st.id = b.store_id
		JOIN reservations r ON r.batch_id = b.id AND r.status IN ('confirmed','completed')
		JOIN charities ch ON ch.id = r.charity_id
		WHERE b.id=$1 ORDER BY r.id DESC LIMIT 1`, batchID).
		Scan(&batchNo, &storeName, &charityName, &category, &totalBoxes)
	if err != nil {
		return err
	}
	var alreadyDistributed int
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(d.boxes),0)
		FROM distributions d JOIN reservations r ON r.id=d.reservation_id
		WHERE r.batch_id=$1`, batchID).Scan(&alreadyDistributed); err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO publicity(batch_id, batch_no, store_name, charity_name, category, boxes,
		    distributed_boxes, written_off_boxes, recipient_summary, status, published_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'[]','adjusted', now())
		ON CONFLICT (batch_id) DO UPDATE SET
		    distributed_boxes=EXCLUDED.distributed_boxes,
		    written_off_boxes=EXCLUDED.written_off_boxes,
		    status=EXCLUDED.status, published_at=now()`,
		batchID, batchNo, storeName, charityName, category, totalBoxes,
		alreadyDistributed, writtenOff)
	return err
}

// lockActiveReservation 锁定批次的生效/已完成预约（降级发生在交接后，预约为 confirmed/completed）。
func lockActiveReservation(ctx context.Context, tx pgx.Tx, batchID int64) (*domain.Reservation, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		SELECT id FROM reservations
		WHERE batch_id=$1 AND status IN ('confirmed','completed')
		ORDER BY id DESC LIMIT 1 FOR UPDATE`, batchID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrConflict("批次无生效预约，不能执行降级处理")
	}
	if err != nil {
		return nil, err
	}
	return lockReservation(ctx, tx, id)
}

func (s *Service) getActiveDegradation(ctx context.Context, q txQuerier, batchID int64) (*domain.Degradation, error) {
	var id int64
	err := q.QueryRow(ctx,
		`SELECT id FROM degradations WHERE batch_id=$1 AND status='assessed' ORDER BY id DESC LIMIT 1`,
		batchID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.getDegradation(ctx, q, id)
}

func (s *Service) getDegradation(ctx context.Context, q txQuerier, id int64) (*domain.Degradation, error) {
	d := &domain.Degradation{}
	err := q.QueryRow(ctx, `
		SELECT id, batch_id, reservation_id, exception_id, triggered_by, observed_temp_c,
		       remaining_distance_km, avg_speed_kmh, late_minutes, reason,
		       donatable_box_nos, urgent_box_nos, written_off_box_nos, status, created_at, closed_at
		FROM degradations WHERE id=$1`, id).Scan(
		&d.ID, &d.BatchID, &d.ReservationID, &d.ExceptionID, &d.TriggeredBy, &d.ObservedTempC,
		&d.RemainingDistanceKm, &d.AvgSpeedKmh, &d.LateMinutes, &d.Reason,
		&d.DonatableBoxNos, &d.UrgentBoxNos, &d.WrittenOffBoxNos, &d.Status, &d.CreatedAt, &d.ClosedAt)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT id, degradation_id, box_no, decision, reason, remaining_shelf_hours,
		       transit_hours, required_hours, effective_remaining_hours, temp_excess_c, created_at
		FROM degradation_items WHERE degradation_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it domain.DegradationItem
		if err := rows.Scan(&it.ID, &it.DegradationID, &it.BoxNo, &it.Decision, &it.Reason,
			&it.RemainingShelfHours, &it.TransitHours, &it.RequiredHours,
			&it.EffectiveRemainingHours, &it.TempExcessC, &it.CreatedAt); err != nil {
			return nil, err
		}
		d.Items = append(d.Items, it)
	}
	return d, rows.Err()
}

// ListDegradations 按批次查询降级单（参与方与监管可见）。
func (s *Service) ListDegradations(ctx context.Context, batchID *int64) ([]domain.Degradation, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT id, batch_id, reservation_id, exception_id, triggered_by, observed_temp_c,
		       remaining_distance_km, avg_speed_kmh, late_minutes, reason,
		       donatable_box_nos, urgent_box_nos, written_off_box_nos, status, created_at, closed_at
		FROM degradations
		WHERE ($1::bigint IS NULL OR batch_id=$1)
		ORDER BY id DESC LIMIT 200`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Degradation
	for rows.Next() {
		var d domain.Degradation
		if err := rows.Scan(&d.ID, &d.BatchID, &d.ReservationID, &d.ExceptionID, &d.TriggeredBy,
			&d.ObservedTempC, &d.RemainingDistanceKm, &d.AvgSpeedKmh, &d.LateMinutes, &d.Reason,
			&d.DonatableBoxNos, &d.UrgentBoxNos, &d.WrittenOffBoxNos, &d.Status, &d.CreatedAt, &d.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDestructions 按批次查询销毁/内部处理记录（参与方与监管可见）。
func (s *Service) ListDestructions(ctx context.Context, batchID int64) ([]domain.DestructionRecord, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT id, batch_id, store_id, box_nos, method, photo_urls, internal_note,
		       handler_name, witness_name, destroyed_at, uploaded_by, created_at
		FROM destruction_records WHERE batch_id=$1 ORDER BY id`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DestructionRecord
	for rows.Next() {
		var r domain.DestructionRecord
		if err := rows.Scan(&r.ID, &r.BatchID, &r.StoreID, &r.BoxNos, &r.Method, &r.PhotoURLs,
			&r.InternalNote, &r.HandlerName, &r.WitnessName, &r.DestroyedAt, &r.UploadedBy, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// notifyOrgTx 写入一条组织级站内通知（机构或门店；接收方成员可见）。
func (s *Service) notifyOrgTx(ctx context.Context, q txQuerier, charityID, storeID *int64,
	batchID int64, category, title, content string, payload map[string]any) error {
	_, err := q.Exec(ctx, `
		INSERT INTO notifications(charity_id, store_id, batch_id, category, title, content, payload)
		VALUES($1,$2,$3,$4,$5,$6,$7)`,
		charityID, storeID, batchID, category, title, content, domain.MarshalPayload(payload))
	return err
}

// ListNotifications 查询当前用户可见通知（按其所属机构/门店）。
func (s *Service) ListNotifications(ctx context.Context, u *domain.User, unreadOnly bool) ([]domain.AppNotification, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT id, user_id, charity_id, store_id, batch_id, category, title, content, payload, is_read, created_at
		FROM notifications
		WHERE (($1::bigint IS NOT NULL AND charity_id=$1)
		   OR ($2::bigint IS NOT NULL AND store_id=$2)
		   OR user_id=$3)
		  AND ($4 = FALSE OR is_read=FALSE)
		ORDER BY id DESC LIMIT 200`, u.CharityID, u.StoreID, u.ID, unreadOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AppNotification
	for rows.Next() {
		var n domain.AppNotification
		var payload []byte
		if err := rows.Scan(&n.ID, &n.UserID, &n.CharityID, &n.StoreID, &n.BatchID, &n.Category,
			&n.Title, &n.Content, &payload, &n.IsRead, &n.CreatedAt); err != nil {
			return nil, err
		}
		n.Payload = bytesToMap(payload)
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkNotificationRead 标记通知已读（仅本人组织可见范围内）。
func (s *Service) MarkNotificationRead(ctx context.Context, u *domain.User, id int64) error {
	tag, err := s.DB.Exec(ctx, `
		UPDATE notifications SET is_read=TRUE
		WHERE id=$1 AND ((charity_id=$2) OR (store_id=$3) OR user_id=$4)`,
		id, u.CharityID, u.StoreID, u.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
