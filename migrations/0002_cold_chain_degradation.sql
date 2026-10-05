-- 0002_cold_chain_degradation.sql 冷链迟到降级处理
-- 1) 扩展既有枚举型 CHECK 约束（箱状态/批次状态/异常类型/凭证类型）
-- 2) 降级单与箱级判定明细
-- 3) 报损食品销毁/内部处理记录
-- 4) 站内通知（告知已预约机构箱号去向）
-- 5) 发放记录补录实际送达时间、公示增加报损箱数

-- ---------- 扩展 CHECK 约束 ----------
ALTER TABLE batches DROP CONSTRAINT IF EXISTS batches_status_check;
ALTER TABLE batches ADD CONSTRAINT batches_status_check CHECK (status IN
    ('reported','reserved','confirmed','handover','handed_over','in_delivery',
     'distributed','closed','cancelled','expired'));

ALTER TABLE boxes DROP CONSTRAINT IF EXISTS boxes_status_check;
ALTER TABLE boxes ADD CONSTRAINT boxes_status_check CHECK (status IN
    ('pending','scanned','handed_over','in_delivery','distributed','spoiled',
     'missing','written_off'));

ALTER TABLE exceptions DROP CONSTRAINT IF EXISTS exceptions_type_check;
ALTER TABLE exceptions ADD CONSTRAINT exceptions_type_check CHECK (type IN
    ('sold_out','expired','truck_late','no_show','wrong_scan','allergen_missing',
     'spoilage_report','regulatory_check','capacity_rejected','temp_breach',
     'cold_chain_degradation'));

ALTER TABLE vouchers DROP CONSTRAINT IF EXISTS vouchers_type_check;
ALTER TABLE vouchers ADD CONSTRAINT vouchers_type_check CHECK (type IN
    ('source','reservation','confirmation','handover','delivery','distribution',
     'exception','degradation','destruction','closure'));

-- ---------- 降级主单 ----------
CREATE TABLE degradations (
    id                   BIGSERIAL PRIMARY KEY,
    batch_id             BIGINT NOT NULL REFERENCES batches(id),
    reservation_id       BIGINT NOT NULL REFERENCES reservations(id),
    exception_id         BIGINT NOT NULL REFERENCES exceptions(id),
    triggered_by         BIGINT REFERENCES users(id),
    observed_temp_c      DOUBLE PRECISION NOT NULL,
    remaining_distance_km DOUBLE PRECISION NOT NULL DEFAULT 0,
    avg_speed_kmh        DOUBLE PRECISION NOT NULL DEFAULT 30,
    late_minutes         INT NOT NULL DEFAULT 0,
    reason               TEXT NOT NULL DEFAULT '',
    donatable_box_nos    TEXT[] NOT NULL DEFAULT '{}',
    urgent_box_nos       TEXT[] NOT NULL DEFAULT '{}',
    written_off_box_nos  TEXT[] NOT NULL DEFAULT '{}',
    status               TEXT NOT NULL DEFAULT 'assessed' CHECK (status IN ('assessed','closed')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at            TIMESTAMPTZ
);
-- 同批次同时仅允许一份未闭环降级单
CREATE UNIQUE INDEX ux_degradations_active_batch
    ON degradations(batch_id) WHERE status='assessed';

-- ---------- 降级箱级判定明细 ----------
CREATE TABLE degradation_items (
    id                        BIGSERIAL PRIMARY KEY,
    degradation_id            BIGINT NOT NULL REFERENCES degradations(id),
    box_no                    TEXT NOT NULL,
    decision                  TEXT NOT NULL CHECK (decision IN ('donate','urgent_donate','write_off')),
    reason                    TEXT NOT NULL DEFAULT '',
    remaining_shelf_hours     DOUBLE PRECISION NOT NULL,
    transit_hours             DOUBLE PRECISION NOT NULL,
    required_hours            DOUBLE PRECISION NOT NULL,
    effective_remaining_hours DOUBLE PRECISION NOT NULL,
    temp_excess_c             DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (degradation_id, box_no)
);
CREATE INDEX ix_degradation_items_deg ON degradation_items(degradation_id);

-- ---------- 报损食品销毁 / 内部处理记录 ----------
CREATE TABLE destruction_records (
    id            BIGSERIAL PRIMARY KEY,
    batch_id      BIGINT NOT NULL REFERENCES batches(id),
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    box_nos       TEXT[] NOT NULL,
    method        TEXT NOT NULL CHECK (method IN ('photo','internal')),
    photo_urls    TEXT[] NOT NULL DEFAULT '{}',
    internal_note TEXT NOT NULL DEFAULT '',
    handler_name  TEXT NOT NULL DEFAULT '',
    witness_name  TEXT NOT NULL DEFAULT '',
    destroyed_at  TIMESTAMPTZ NOT NULL,
    uploaded_by   BIGINT NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT destruction_evidence_chk CHECK (
        (method='photo'    AND COALESCE(array_length(photo_urls,1),0) > 0)
     OR (method='internal' AND length(btrim(internal_note)) > 0))
);
CREATE INDEX ix_destruction_batch ON destruction_records(batch_id);

-- ---------- 站内通知 ----------
CREATE TABLE notifications (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT REFERENCES users(id),
    charity_id BIGINT REFERENCES charities(id),
    store_id   BIGINT REFERENCES stores(id),
    batch_id   BIGINT NOT NULL REFERENCES batches(id),
    category   TEXT NOT NULL CHECK (category IN
        ('degradation_result','urgent_dispatch','destruction_due')),
    title      TEXT NOT NULL,
    content    TEXT NOT NULL DEFAULT '',
    payload    JSONB NOT NULL DEFAULT '{}',
    is_read    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_notifications_charity ON notifications(charity_id, id DESC);
CREATE INDEX ix_notifications_store ON notifications(store_id, id DESC);
CREATE INDEX ix_notifications_user ON notifications(user_id, id DESC);

-- ---------- 既有表补列 ----------
ALTER TABLE distributions ADD COLUMN IF NOT EXISTS actual_delivered_at TIMESTAMPTZ;
ALTER TABLE publicity ADD COLUMN IF NOT EXISTS written_off_boxes INT NOT NULL DEFAULT 0;
