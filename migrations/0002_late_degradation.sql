-- 0002_late_degradation.sql 冷链车迟到降级处理
-- 1) 扩展箱状态：written_off（转报损）
ALTER TABLE boxes DROP CONSTRAINT IF EXISTS boxes_status_check;
ALTER TABLE boxes ADD CONSTRAINT boxes_status_check CHECK (status IN
    ('pending','scanned','handed_over','in_delivery','distributed','spoiled','missing','written_off'));

-- 箱维度加急标记与生产时间（生产时间回填自批次）
ALTER TABLE boxes ADD COLUMN IF NOT EXISTS expedited BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE boxes ADD COLUMN IF NOT EXISTS produced_at TIMESTAMPTZ;
UPDATE boxes b SET produced_at = ba.produced_at
FROM batches ba WHERE ba.id = b.batch_id AND b.produced_at IS NULL;
ALTER TABLE boxes ALTER COLUMN produced_at SET NOT NULL;
CREATE INDEX IF NOT EXISTS ix_boxes_status_batch ON boxes(batch_id, status);

-- 2) 异常类型：冷链迟到降级
ALTER TABLE exceptions DROP CONSTRAINT IF EXISTS exceptions_type_check;
ALTER TABLE exceptions ADD CONSTRAINT exceptions_type_check CHECK (type IN
    ('sold_out','expired','truck_late','no_show','wrong_scan','allergen_missing',
     'spoilage_report','regulatory_check','capacity_rejected','temp_breach','late_degradation'));

-- 3) 凭证类型：降级凭证
ALTER TABLE vouchers DROP CONSTRAINT IF EXISTS vouchers_type_check;
ALTER TABLE vouchers ADD CONSTRAINT vouchers_type_check CHECK (type IN
    ('source','reservation','confirmation','handover','delivery','distribution',
     'exception','degradation','closure'));

-- 4) 发放记录：加急箱须由接收方补充实际送达时间
ALTER TABLE distributions ADD COLUMN IF NOT EXISTS actual_delivered_at TIMESTAMPTZ;

-- 5) 公示：同步报损数量（不计入捐赠发放数量）
ALTER TABLE publicity ADD COLUMN IF NOT EXISTS written_off_boxes INT NOT NULL DEFAULT 0;

-- 6) 降级处理主单（按批次幂等单例）
CREATE TABLE IF NOT EXISTS degradations (
    id                BIGSERIAL PRIMARY KEY,
    batch_id          BIGINT NOT NULL UNIQUE REFERENCES batches(id),
    reservation_id    BIGINT NOT NULL REFERENCES reservations(id),
    site_temp_c       DOUBLE PRECISION NOT NULL,
    remaining_dist_km DOUBLE PRECISION NOT NULL DEFAULT 0,
    written_off_boxes INT NOT NULL DEFAULT 0,
    expedited_boxes  INT NOT NULL DEFAULT 0,
    notice            TEXT NOT NULL DEFAULT '',
    created_by        BIGINT REFERENCES users(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 7) 逐箱降级判定结果与依据
CREATE TABLE IF NOT EXISTS degradation_items (
    id             BIGSERIAL PRIMARY KEY,
    degradation_id BIGINT NOT NULL REFERENCES degradations(id),
    box_id         BIGINT NOT NULL REFERENCES boxes(id),
    box_no         TEXT NOT NULL,
    decision       TEXT NOT NULL CHECK (decision IN ('written_off','expedited')),
    reason         TEXT NOT NULL DEFAULT '',
    produced_at    TIMESTAMPTZ NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    safe_until     TIMESTAMPTZ NOT NULL,
    expedited      BOOLEAN NOT NULL DEFAULT FALSE,
    evidence_ok    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (degradation_id, box_id)
);
CREATE INDEX IF NOT EXISTS ix_deg_items_decision ON degradation_items(degradation_id, decision);

-- 8) 报损食品销毁凭证：门店上传销毁照片或内部处理记录（二者至少一项）
CREATE TABLE IF NOT EXISTS box_disposals (
    id            BIGSERIAL PRIMARY KEY,
    batch_id      BIGINT NOT NULL REFERENCES batches(id),
    box_id        BIGINT NOT NULL UNIQUE REFERENCES boxes(id),
    box_no        TEXT NOT NULL,
    evidence_type TEXT NOT NULL CHECK (evidence_type IN ('destruction_photo','internal_record')),
    evidence_ref  TEXT NOT NULL CHECK (length(btrim(evidence_ref)) > 0),
    note          TEXT NOT NULL DEFAULT '',
    uploaded_by   BIGINT REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_disposals_batch ON box_disposals(batch_id);

-- 9) 降级通知：通知已预约机构哪些箱仍可接收、哪些已报损
CREATE TABLE IF NOT EXISTS notifications (
    id             BIGSERIAL PRIMARY KEY,
    charity_id     BIGINT NOT NULL REFERENCES charities(id),
    batch_id       BIGINT NOT NULL REFERENCES batches(id),
    reservation_id BIGINT REFERENCES reservations(id),
    kind           TEXT NOT NULL DEFAULT 'degradation_notice',
    title          TEXT NOT NULL,
    body           TEXT NOT NULL DEFAULT '',
    written_off_boxes TEXT[] NOT NULL DEFAULT '{}',
    expedited_boxes   TEXT[] NOT NULL DEFAULT '{}',
    is_read        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_notifications_charity ON notifications(charity_id, is_read, id DESC);
