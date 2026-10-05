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

// DegradeBoxInput 单个箱的降级判定覆盖参数（可选）。
// 不填则沿用批次生产时间与整单剩余配送距离。
type DegradeBoxInput struct {
	BoxNo           string   `json:"box_no"`
	ProducedAt      *string  `json:"produced_at,omitempty"`       // RFC3339，覆盖批次生产时间
	RemainingDistKm *float64 `json:"remaining_dist_km,omitempty"` // 该箱拟配送站点的剩余距离
}

// DegradeInput 冷链车晚到后的现场降级判定输入。
type DegradeInput struct {
	SiteTempC       float64           `json:"site_temp_c"`       // 现场（车厢/交接点）实测温度
	RemainingDistKm float64           `json:"remaining_dist_km"` // 默认剩余配送距离（公里）
	Note            string            `json:"note"`
	Boxes           []DegradeBoxInput `json:"boxes"` // 逐箱覆盖（如不同站点距离）
}

// DegradeTruckLate 冷链车晚到降级处理：
// 依据生产时间、现场温度、剩余配送距离逐箱判定——不可捐赠转报损，可捐赠生成加急发放；
// 写入降级凭证与 late_degradation 异常，通知已预约机构，并为公示数量调整提供依据。
// 按批次幂等（同一批次仅一次降级，重复调用返回首次结果）。
func (s *Service) DegradeTruckLate(ctx context.Context, u *domain.User, batchID int64,
	in DegradeInput) (*domain.Degradation, bool, error) {
	if in.RemainingDistKm < 0 {
		return nil, false, domain.ErrBadRequest("剩余配送距离不能为负")
	}
	overrides := map[string]DegradeBoxInput{}
	for _, b := range in.Boxes {
		if b.BoxNo != "" {
			overrides[b.BoxNo] = b
		}
	}

	var deg *domain.Degradation
	idempotent := false
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		r, err := lockActiveReservation(ctx, tx, batchID)
		if err != nil {
			return err
		}
		// 仅预约机构可发起降级（其车辆晚到）；管理员可代为处理
		if err := RequireCharity(u, r.CharityID); err != nil {
			return err
		}
		if b.Status != domain.BatchConfirmed && b.Status != domain.BatchHandover {
			return domain.ErrConflict(fmt.Sprintf("批次状态为 %s，仅在交接前后可做迟到降级判定", b.Status))
		}

		// 幂等：已存在降级单直接返回
		if existing, eerr := s.getDegradation(ctx, tx, batchID); eerr == nil {
			deg = existing
			idempotent = true
			return nil
		} else if !errors.Is(eerr, pgx.ErrNoRows) {
			return eerr
		}

		storageMax := in.SiteTempC
		if b.StorageTempMaxC != nil {
			storageMax = *b.StorageTempMaxC
		}
		now := time.Now()

		boxes, err := listBoxesTx(ctx, tx, batchID)
		if err != nil {
			return err
		}
		var pending []domain.Box
		pending = append(pending, pendingOrScanned(boxes)...)
		if len(pending) == 0 {
			return domain.ErrConflict("批次已无可判定的在途箱（可能已完成交接或已报损）")
		}

		var degID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO degradations(batch_id, reservation_id, site_temp_c, remaining_dist_km, notice, created_by)
			VALUES($1,$2,$3,$4,$5,$6) RETURNING id, created_at`,
			batchID, r.ID, in.SiteTempC, in.RemainingDistKm, in.Note, u.ID).
			Scan(&degID, &now); err != nil {
			return err
		}

		writtenOff, expedited := []string{}, []string{}
		for _, bx := range pending {
			produced := bx.ProducedAt
			dist := in.RemainingDistKm
			if ov, ok := overrides[bx.BoxNo]; ok {
				if ov.ProducedAt != nil {
					if t, perr := time.Parse(time.RFC3339, *ov.ProducedAt); perr == nil {
						produced = t
					}
				}
				if ov.RemainingDistKm != nil {
					dist = *ov.RemainingDistKm
				}
			}
			d := domain.EvaluateLateBox(domain.LateBoxCheckInput{
				ProducedAt: produced, ExpiresAt: b.ExpiresAt, Now: now,
				SiteTempC: in.SiteTempC, StorageTempMaxC: storageMax,
				RemainingDistKm: dist, AvgSpeedKmH: s.Cfg.DegradeAvgSpeedKmh,
				TempPenaltyPerC: s.Cfg.DegradeTempPenaltyPerC, SafetyMargin: s.Cfg.DegradeSafetyMargin,
			})
			decision := domain.DegradationExpedited
			if !d.Donatable {
				decision = domain.DegradationWrittenOff
				if _, err := tx.Exec(ctx,
					`UPDATE boxes SET status=$1, expedited=FALSE WHERE id=$2`,
					domain.BoxWrittenOff, bx.ID); err != nil {
					return err
				}
				writtenOff = append(writtenOff, bx.BoxNo)
			} else {
				if _, err := tx.Exec(ctx,
					`UPDATE boxes SET expedited=$1 WHERE id=$2`, true, bx.ID); err != nil {
					return err
				}
				expedited = append(expedited, bx.BoxNo)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO degradation_items(degradation_id, box_id, box_no, decision, reason,
				    produced_at, expires_at, safe_until, expedited)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				degID, bx.ID, bx.BoxNo, decision, d.Reason, produced, b.ExpiresAt,
				d.SafeUntil, d.Donatable); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE degradations SET written_off_boxes=$1, expedited_boxes=$2 WHERE id=$3`,
			len(writtenOff), len(expedited), degID); err != nil {
			return err
		}

		// 降级凭证（逐箱判定依据入哈希链）
		if err := insertVoucher(ctx, tx, batchID, &r.ID, domain.VoucherDegradation, map[string]any{
			"degradation_id":    degID,
			"site_temp_c":       in.SiteTempC,
			"remaining_dist_km": in.RemainingDistKm,
			"written_off_boxes": writtenOff,
			"expedited_boxes":   expedited,
			"note":              in.Note,
		}); err != nil {
			return err
		}

		// late_degradation 异常留痕（open，待销毁凭证补齐/机构确认后可关闭）
		detail := map[string]any{
			"degradation_id":    degID,
			"site_temp_c":       in.SiteTempC,
			"remaining_dist_km": in.RemainingDistKm,
			"written_off_boxes": writtenOff,
			"expedited_boxes":   expedited,
			"reported_by":       u.ID,
		}
		exID, err := insertException(ctx, tx, &batchID, &r.ID, domain.ExLateDegradation, detail, &u.ID)
		if err != nil {
			return err
		}
		if err := s.emitExceptionEvent(ctx, tx, exID, &batchID, &r.ID, domain.ExLateDegradation, detail); err != nil {
			return err
		}

		// 通知已预约机构：哪些箱仍可接收、哪些已报损
		title := fmt.Sprintf("批次 %s 冷链迟到降级结果", b.BatchNo)
		body := fmt.Sprintf("可加急接收 %d 箱（%v）；报损 %d 箱（%v）。报损箱不计入捐赠数量，加急箱送达后须补报实际送达时间。",
			len(expedited), expedited, len(writtenOff), writtenOff)
		if _, err := tx.Exec(ctx, `
			INSERT INTO notifications(charity_id, batch_id, reservation_id, kind, title, body,
			    written_off_boxes, expedited_boxes)
			VALUES($1,$2,$3,'degradation_notice',$4,$5,$6,$7)`,
			r.CharityID, batchID, r.ID, title, body, writtenOff, expedited); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "batch.degraded", map[string]any{
			"batch_id": batchID, "reservation_id": r.ID, "degradation_id": degID,
			"written_off_boxes": writtenOff, "expedited_boxes": expedited,
		}); err != nil {
			return err
		}

		deg, err = s.getDegradation(ctx, tx, batchID)
		return err
	})
	return deg, idempotent, err
}

// UploadDisposalEvidence 门店为报损箱上传销毁照片或内部处理记录。
func (s *Service) UploadDisposalEvidence(ctx context.Context, u *domain.User, batchID int64,
	boxNo, evidenceType, evidenceRef, note string) (*domain.BoxDisposal, error) {
	if boxNo == "" {
		return nil, domain.ErrBadRequest("箱号必填")
	}
	if evidenceType != "destruction_photo" && evidenceType != "internal_record" {
		return nil, domain.ErrBadRequest("凭证类型取值: destruction_photo(销毁照片)/internal_record(内部处理记录)")
	}
	if evidenceRef == "" {
		return nil, domain.ErrBadRequest("销毁照片地址或内部处理记录编号必填")
	}
	var out *domain.BoxDisposal
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if err := RequireStore(u, b.StoreID); err != nil {
			return err
		}
		var boxID int64
		var status string
		err = tx.QueryRow(ctx, `SELECT id, status FROM boxes WHERE batch_id=$1 AND box_no=$2 FOR UPDATE`,
			batchID, boxNo).Scan(&boxID, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != domain.BoxWrittenOff {
			return domain.ErrConflict(fmt.Sprintf("箱 %s 当前状态 %s，非报损箱，无需销毁凭证", boxNo, status))
		}
		var id int64
		var createdAt time.Time
		err = tx.QueryRow(ctx, `
			INSERT INTO box_disposals(batch_id, box_id, box_no, evidence_type, evidence_ref, note, uploaded_by)
			VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at`,
			batchID, boxID, boxNo, evidenceType, evidenceRef, note, u.ID).Scan(&id, &createdAt)
		if isUniqueViolation(err) {
			// 幂等：返回已上传凭证
			d, derr := s.getDisposalByBox(ctx, tx, boxID)
			if derr != nil {
				return derr
			}
			out = d
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE degradation_items SET evidence_ok=TRUE WHERE box_id=$1`, boxID); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "box.disposal_evidence", map[string]any{
			"batch_id": batchID, "box_no": boxNo, "evidence_type": evidenceType,
		}); err != nil {
			return err
		}
		out, err = s.getDisposalByBox(ctx, tx, boxID)
		return err
	})
	return out, err
}

// pendingOrScanned 过滤出尚未交接完成、可参与降级判定的箱。
func pendingOrScanned(boxes []domain.Box) []domain.Box {
	var out []domain.Box
	for _, b := range boxes {
		if b.Status == domain.BoxPending || b.Status == domain.BoxScanned {
			out = append(out, b)
		}
	}
	return out
}

func listBoxesTx(ctx context.Context, q txQuerier, batchID int64) ([]domain.Box, error) {
	rows, err := q.Query(ctx,
		`SELECT id, batch_id, box_no, status, expedited, produced_at, created_at
		 FROM boxes WHERE batch_id=$1 ORDER BY id`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Box
	for rows.Next() {
		var b domain.Box
		if err := rows.Scan(&b.ID, &b.BatchID, &b.BoxNo, &b.Status, &b.Expedited,
			&b.ProducedAt, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func lockActiveReservation(ctx context.Context, tx pgx.Tx, batchID int64) (*domain.Reservation, error) {
	var rid int64
	err := tx.QueryRow(ctx, `
		SELECT id FROM reservations
		WHERE batch_id=$1 AND status IN ('pending','confirmed')
		ORDER BY id DESC LIMIT 1 FOR UPDATE`, batchID).Scan(&rid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrConflict("当前无生效预约，无法做迟到降级判定")
	}
	if err != nil {
		return nil, err
	}
	return lockReservation(ctx, tx, rid)
}

func (s *Service) getDegradation(ctx context.Context, q txQuerier, batchID int64) (*domain.Degradation, error) {
	d := &domain.Degradation{}
	err := q.QueryRow(ctx, `
		SELECT id, batch_id, reservation_id, site_temp_c, remaining_dist_km,
		       written_off_boxes, expedited_boxes, notice, created_by, created_at
		FROM degradations WHERE batch_id=$1`, batchID).Scan(
		&d.ID, &d.BatchID, &d.ReservationID, &d.SiteTempC, &d.RemainingDistKm,
		&d.WrittenOffBoxes, &d.ExpeditedBoxes, &d.Notice, &d.CreatedBy, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT id, degradation_id, box_id, box_no, decision, reason, produced_at,
		       expires_at, safe_until, expedited, evidence_ok, created_at
		FROM degradation_items WHERE degradation_id=$1 ORDER BY id`, d.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it domain.DegradationItem
		if err := rows.Scan(&it.ID, &it.DegradationID, &it.BoxID, &it.BoxNo, &it.Decision,
			&it.Reason, &it.ProducedAt, &it.ExpiresAt, &it.SafeUntil, &it.Expedited,
			&it.EvidenceOK, &it.CreatedAt); err != nil {
			return nil, err
		}
		d.Items = append(d.Items, it)
	}
	return d, rows.Err()
}

// GetDegradation 查询批次的降级处理结果（参与方与监管可见）。
func (s *Service) GetDegradation(ctx context.Context, u *domain.User, batchID int64) (*domain.Degradation, error) {
	d, err := s.getDegradation(ctx, s.DB, batchID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if u.Role == domain.RoleRegulator || u.Role == domain.RoleAdmin {
		return d, nil
	}
	r, rerr := getReservation(ctx, s.DB, d.ReservationID)
	if rerr == nil {
		if RequireCharity(u, r.CharityID) == nil {
			return d, nil
		}
	}
	b, berr := getBatch(ctx, s.DB, batchID)
	if berr == nil && RequireStore(u, b.StoreID) == nil {
		return d, nil
	}
	return nil, domain.ErrForbidden
}

func (s *Service) getDisposalByBox(ctx context.Context, q txQuerier, boxID int64) (*domain.BoxDisposal, error) {
	d := &domain.BoxDisposal{}
	err := q.QueryRow(ctx, `
		SELECT id, batch_id, box_id, box_no, evidence_type, evidence_ref, note, uploaded_by, created_at
		FROM box_disposals WHERE box_id=$1`, boxID).Scan(
		&d.ID, &d.BatchID, &d.BoxID, &d.BoxNo, &d.EvidenceType, &d.EvidenceRef,
		&d.Note, &d.UploadedBy, &d.CreatedAt)
	return d, err
}

// ListDisposals 查询批次报损销毁凭证。
func (s *Service) ListDisposals(ctx context.Context, batchID int64) ([]domain.BoxDisposal, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT id, batch_id, box_id, box_no, evidence_type, evidence_ref, note, uploaded_by, created_at
		FROM box_disposals WHERE batch_id=$1 ORDER BY id`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.BoxDisposal
	for rows.Next() {
		var d domain.BoxDisposal
		if err := rows.Scan(&d.ID, &d.BatchID, &d.BoxID, &d.BoxNo, &d.EvidenceType,
			&d.EvidenceRef, &d.Note, &d.UploadedBy, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------- 通知 ----------

// ListNotifications 机构查看自己的降级通知；监管/管理员可看全部。
func (s *Service) ListNotifications(ctx context.Context, u *domain.User, onlyUnread bool) ([]domain.Notification, error) {
	q := `SELECT id, charity_id, batch_id, reservation_id, kind, title, body,
	             written_off_boxes, expedited_boxes, is_read, created_at, read_at
	      FROM notifications
	      WHERE ($1::bigint IS NULL OR charity_id=$1)
	        AND ($2::boolean IS FALSE OR is_read=FALSE)
	      ORDER BY id DESC LIMIT 200`
	var charityFilter *int64
	if u.Role == domain.RoleCharityStaff {
		if u.CharityID == nil {
			return nil, domain.ErrForbidden
		}
		charityFilter = u.CharityID
	} else if u.Role != domain.RoleRegulator && u.Role != domain.RoleAdmin {
		return nil, domain.ErrForbidden
	}
	rows, err := s.DB.Query(ctx, q, charityFilter, onlyUnread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Notification
	for rows.Next() {
		var n domain.Notification
		if err := rows.Scan(&n.ID, &n.CharityID, &n.BatchID, &n.ReservationID, &n.Kind,
			&n.Title, &n.Body, &n.WrittenOff, &n.Expedited, &n.IsRead, &n.CreatedAt, &n.ReadAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkNotificationRead 标记通知已读（仅所属机构）。
func (s *Service) MarkNotificationRead(ctx context.Context, u *domain.User, id int64) (*domain.Notification, error) {
	var n *domain.Notification
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		var charityID int64
		err := tx.QueryRow(ctx, `SELECT charity_id FROM notifications WHERE id=$1 FOR UPDATE`, id).Scan(&charityID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := RequireCharity(u, charityID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE notifications SET is_read=TRUE, read_at=now() WHERE id=$1 AND is_read=FALSE`, id); err != nil {
			return err
		}
		return s.scanNotification(ctx, tx, id, &n)
	})
	return n, err
}

func (s *Service) scanNotification(ctx context.Context, q txQuerier, id int64, np **domain.Notification) error {
	n := &domain.Notification{}
	err := q.QueryRow(ctx, `
		SELECT id, charity_id, batch_id, reservation_id, kind, title, body,
		       written_off_boxes, expedited_boxes, is_read, created_at, read_at
		FROM notifications WHERE id=$1`, id).Scan(
		&n.ID, &n.CharityID, &n.BatchID, &n.ReservationID, &n.Kind, &n.Title, &n.Body,
		&n.WrittenOff, &n.Expedited, &n.IsRead, &n.CreatedAt, &n.ReadAt)
	if err != nil {
		return err
	}
	*np = n
	return nil
}
