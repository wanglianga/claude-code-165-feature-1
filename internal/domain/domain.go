package domain

import (
	"fmt"
	"time"
)

// ---------- 通用错误 ----------

// AppError 是带 HTTP 状态码与业务码的错误，贯穿 service 与 handler 层。
type AppError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *AppError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func NewError(status int, code, msg string) *AppError {
	return &AppError{Status: status, Code: code, Message: msg}
}

var (
	ErrUnauthorized = NewError(401, "UNAUTHORIZED", "未认证或凭证已过期")
	ErrForbidden    = NewError(403, "FORBIDDEN", "无权执行该操作")
	ErrNotFound     = NewError(404, "NOT_FOUND", "资源不存在")
)

func ErrConflict(msg string) *AppError   { return NewError(409, "CONFLICT", msg) }
func ErrBadRequest(msg string) *AppError { return NewError(400, "BAD_REQUEST", msg) }
func ErrUnprocessable(msg string) *AppError {
	return NewError(422, "UNPROCESSABLE", msg)
}

// ---------- 角色 ----------

const (
	RoleStoreStaff   = "store_staff"
	RoleStoreManager = "store_manager"
	RoleCharityStaff = "charity_staff"
	RoleRegulator    = "regulator"
	RoleAdmin        = "admin"
)

// ---------- 批次状态机 ----------

const (
	BatchReported    = "reported"    // 门店已上报，待预约
	BatchReserved    = "reserved"    // 已被公益机构预约，待门店确认
	BatchConfirmed   = "confirmed"   // 门店已确认，待交接
	BatchHandover    = "handover"    // 交接扫码进行中
	BatchHandedOver  = "handed_over" // 交接完成，待配送计划
	BatchInDelivery  = "in_delivery" // 配送中
	BatchDistributed = "distributed" // 全部发放完成
	BatchCancelled   = "cancelled"   // 已取消（如临时售罄）
	BatchExpired     = "expired"     // 超过可捐赠时限
)

// batchTransitions 定义批次状态机的合法迁移。
var batchTransitions = map[string][]string{
	BatchReported:    {BatchReserved, BatchCancelled, BatchExpired},
	BatchReserved:    {BatchConfirmed, BatchReported, BatchCancelled, BatchExpired},
	BatchConfirmed:   {BatchHandover, BatchReported, BatchCancelled, BatchExpired},
	BatchHandover:    {BatchHandedOver, BatchCancelled, BatchExpired},
	BatchHandedOver:  {BatchInDelivery},
	BatchInDelivery:  {BatchDistributed},
	BatchDistributed: {},
	BatchCancelled:   {},
	BatchExpired:     {},
}

// CanTransition 判断批次状态迁移是否合法。
func CanTransition(from, to string) bool {
	for _, s := range batchTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// ---------- 预约状态 ----------

const (
	ResvPending   = "pending"
	ResvConfirmed = "confirmed"
	ResvCancelled = "cancelled"
	ResvExpired   = "expired"
	ResvCompleted = "completed"
)

// ---------- 箱状态 ----------

const (
	BoxPending     = "pending"
	BoxScanned     = "scanned"
	BoxHandedOver  = "handed_over"
	BoxInDelivery  = "in_delivery"
	BoxDistributed = "distributed"
	BoxSpoiled     = "spoiled"
	BoxWrittenOff  = "written_off" // 冷链迟到降级判定不可捐赠，已转报损
)

// ---------- 异常类型 ----------

const (
	ExSoldOut          = "sold_out"          // 门店临时售罄
	ExExpired          = "expired"           // 超过可捐赠时限
	ExTruckLate        = "truck_late"        // 冷链车迟到
	ExNoShow           = "no_show"           // 公益机构爽约
	ExWrongScan        = "wrong_scan"        // 箱号错扫
	ExAllergenMissing  = "allergen_missing"  // 过敏原信息缺失
	ExSpoilageReport   = "spoilage_report"   // 受助点反馈变质
	ExRegulatoryCheck  = "regulatory_check"  // 监管抽查
	ExCapacityRejected = "capacity_rejected" // 接收能力不足被拒
	ExTempBreach       = "temp_breach"       // 温度超标
	ExLateDegradation  = "late_degradation"  // 冷链迟到降级（逐箱判定报损/加急）
)

// ---------- 凭证类型 ----------

const (
	VoucherSource       = "source"       // 来源凭证（上报）
	VoucherReservation  = "reservation"  // 预约凭证
	VoucherConfirmation = "confirmation" // 门店确认凭证
	VoucherHandover     = "handover"     // 交接凭证（温控+封签+身份）
	VoucherDelivery     = "delivery"     // 配送凭证
	VoucherDistribution = "distribution" // 发放凭证
	VoucherException    = "exception"    // 异常凭证
	VoucherDegradation  = "degradation"  // 冷链迟到降级凭证（逐箱报损/加急判定）
	VoucherClosure      = "closure"      // 闭环凭证
)

// ---------- 实体 ----------

type User struct {
	ID          int64     `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	StoreID     *int64    `json:"store_id,omitempty"`
	CharityID   *int64    `json:"charity_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Store struct {
	ID           int64     `json:"id"`
	Code         string    `json:"code"`
	Name         string    `json:"name"`
	Address      string    `json:"address"`
	Lat          float64   `json:"lat"`
	Lng          float64   `json:"lng"`
	ContactPhone string    `json:"contact_phone"`
	CreatedAt    time.Time `json:"created_at"`
}

type Charity struct {
	ID                     int64     `json:"id"`
	Code                   string    `json:"code"`
	Name                   string    `json:"name"`
	Address                string    `json:"address"`
	Lat                    float64   `json:"lat"`
	Lng                    float64   `json:"lng"`
	ServiceRadiusKm        float64   `json:"service_radius_km"`
	DailyCapacityBoxes     int       `json:"daily_capacity_boxes"`
	ColdChainVehicles      int       `json:"cold_chain_vehicles"`
	ColdChainCapacityBoxes int       `json:"cold_chain_capacity_boxes"`
	RecipientTypes         []string  `json:"recipient_types"`
	ContactPhone           string    `json:"contact_phone"`
	CreatedAt              time.Time `json:"created_at"`
}

type Batch struct {
	ID                int64     `json:"id"`
	BatchNo           string    `json:"batch_no"`
	StoreID           int64     `json:"store_id"`
	Status            string    `json:"status"`
	Category          string    `json:"category"`
	Description       string    `json:"description"`
	TotalBoxes        int       `json:"total_boxes"`
	QuantityDesc      string    `json:"quantity_desc"`
	ProducedAt        time.Time `json:"produced_at"`
	ShelfLifeHours    int       `json:"shelf_life_hours"`
	ExpiresAt         time.Time `json:"expires_at"`
	Allergens         []string  `json:"allergens"`
	AllergenConfirmed bool      `json:"allergen_confirmed"`
	RequiresColdChain bool      `json:"requires_cold_chain"`
	StorageTempMaxC   *float64  `json:"storage_temp_max_c,omitempty"`
	Packaging         string    `json:"packaging"`
	Address           string    `json:"address"`
	Lat               float64   `json:"lat"`
	Lng               float64   `json:"lng"`
	LatestPickupAt    time.Time `json:"latest_pickup_at"`
	Notes             string    `json:"notes"`
	Version           int       `json:"version"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type Box struct {
	ID         int64     `json:"id"`
	BatchID    int64     `json:"batch_id"`
	BoxNo      string    `json:"box_no"`
	Status     string    `json:"status"`
	Expedited  bool      `json:"expedited"` // 冷链迟到降级后标记为加急发放
	ProducedAt time.Time `json:"produced_at"`
	CreatedAt  time.Time `json:"created_at"`
}

type Reservation struct {
	ID              int64     `json:"id"`
	ReservationNo   string    `json:"reservation_no"`
	BatchID         int64     `json:"batch_id"`
	CharityID       int64     `json:"charity_id"`
	Status          string    `json:"status"`
	IdempotencyKey  string    `json:"idempotency_key,omitempty"`
	Boxes           int       `json:"boxes"`
	PlannedPickupAt time.Time `json:"planned_pickup_at"`
	VehiclePlate    string    `json:"vehicle_plate"`
	DriverName      string    `json:"driver_name"`
	ColdChain       bool      `json:"cold_chain"`
	ReceiverName    string    `json:"receiver_name"`
	ReceiverPhone   string    `json:"receiver_phone"`
	ReceiverIDNo    string    `json:"receiver_id_no"`
	CancelReason    string    `json:"cancel_reason,omitempty"`
	Version         int       `json:"version"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Handover struct {
	ID               int64      `json:"id"`
	ReservationID    int64      `json:"reservation_id"`
	Status           string     `json:"status"`
	StoreStaffID     *int64     `json:"store_staff_id,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	OverallTempC     *float64   `json:"overall_temp_c,omitempty"`
	SealIntact       *bool      `json:"seal_intact,omitempty"`
	ReceiverVerified bool       `json:"receiver_verified"`
	ReceiverName     string     `json:"receiver_name"`
	ReceiverIDNo     string     `json:"receiver_id_no"`
}

type HandoverScan struct {
	ID         int64     `json:"id"`
	HandoverID int64     `json:"handover_id"`
	BoxID      *int64    `json:"box_id,omitempty"`
	BoxNo      string    `json:"box_no"`
	Result     string    `json:"result"` // ok / wrong_box / duplicate
	TempC      *float64  `json:"temp_c,omitempty"`
	SealIntact *bool     `json:"seal_intact,omitempty"`
	Message    string    `json:"message"`
	ScannedAt  time.Time `json:"scanned_at"`
}

type DeliveryPlan struct {
	ID            int64     `json:"id"`
	ReservationID int64     `json:"reservation_id"`
	VehiclePlate  string    `json:"vehicle_plate"`
	DriverName    string    `json:"driver_name"`
	ColdChain     bool      `json:"cold_chain"`
	SubmittedAt   time.Time `json:"submitted_at"`
}

type DeliveryStop struct {
	ID            int64    `json:"id"`
	PlanID        int64    `json:"plan_id"`
	Seq           int      `json:"seq"`
	RecipientType string   `json:"recipient_type"`
	SiteName      string   `json:"site_name"`
	Address       string   `json:"address"`
	Lat           *float64 `json:"lat,omitempty"`
	Lng           *float64 `json:"lng,omitempty"`
	PlannedBoxes  int      `json:"planned_boxes"`
	Status        string   `json:"status"`
}

type Distribution struct {
	ID                int64      `json:"id"`
	StopID            int64      `json:"stop_id"`
	ReservationID     int64      `json:"reservation_id"`
	Boxes             int        `json:"boxes"`
	ReceiverSign      string     `json:"receiver_sign"`
	DistributedAt     time.Time  `json:"distributed_at"`
	ActualDeliveredAt *time.Time `json:"actual_delivered_at,omitempty"` // 加急箱：接收方补充的实际送达时间
	FeedbackStatus    string     `json:"feedback_status"`
	FeedbackNote      string     `json:"feedback_note,omitempty"`
	FeedbackAt        *time.Time `json:"feedback_at,omitempty"`
}

type Voucher struct {
	ID            int64          `json:"id"`
	BatchID       int64          `json:"batch_id"`
	ReservationID *int64         `json:"reservation_id,omitempty"`
	Type          string         `json:"type"`
	Payload       map[string]any `json:"payload"`
	Hash          string         `json:"hash"`
	PrevHash      string         `json:"prev_hash"`
	CreatedAt     time.Time      `json:"created_at"`
}

type Exception struct {
	ID            int64          `json:"id"`
	ExNo          string         `json:"ex_no"`
	BatchID       *int64         `json:"batch_id,omitempty"`
	ReservationID *int64         `json:"reservation_id,omitempty"`
	Type          string         `json:"type"`
	Detail        map[string]any `json:"detail"`
	Status        string         `json:"status"`
	Resolution    string         `json:"resolution,omitempty"`
	CreatedBy     *int64         `json:"created_by,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	ResolvedAt    *time.Time     `json:"resolved_at,omitempty"`
}

type Publicity struct {
	ID               int64          `json:"id"`
	BatchID          int64          `json:"batch_id"`
	BatchNo          string         `json:"batch_no"`
	StoreName        string         `json:"store_name"`
	CharityName      string         `json:"charity_name"`
	Category         string         `json:"category"`
	Boxes            int            `json:"boxes"`
	DistributedBoxes int            `json:"distributed_boxes"`
	WrittenOffBoxes  int            `json:"written_off_boxes"` // 冷链迟到降级报损箱数（不计入捐赠数量）
	RecipientSummary []RecipientSum `json:"recipient_summary"`
	Status           string         `json:"status"`
	PublishedAt      time.Time      `json:"published_at"`
}

type RecipientSum struct {
	RecipientType string `json:"recipient_type"`
	SiteName      string `json:"site_name"`
	Boxes         int    `json:"boxes"`
}

// ---------- 冷链迟到降级 ----------

const (
	DegradationWrittenOff = "written_off" // 判定不可捐赠，转报损
	DegradationExpedited  = "expedited"   // 仍可捐赠，加急发放
)

// Degradation 记录一次冷链车迟到后的降级处理（按批次维度，幂等单例）。
type Degradation struct {
	ID              int64             `json:"id"`
	BatchID         int64             `json:"batch_id"`
	ReservationID   int64             `json:"reservation_id"`
	SiteTempC       float64           `json:"site_temp_c"`
	RemainingDistKm float64           `json:"remaining_dist_km"`
	WrittenOffBoxes int               `json:"written_off_boxes"`
	ExpeditedBoxes  int               `json:"expedited_boxes"`
	Notice          string            `json:"notice"`
	CreatedBy       int64             `json:"created_by"`
	CreatedAt       time.Time         `json:"created_at"`
	Items           []DegradationItem `json:"items,omitempty"`
}

// DegradationItem 逐箱降级判定结果与依据。
type DegradationItem struct {
	ID            int64     `json:"id"`
	DegradationID int64     `json:"degradation_id"`
	BoxID         int64     `json:"box_id"`
	BoxNo         string    `json:"box_no"`
	Decision      string    `json:"decision"` // written_off / expedited
	Reason        string    `json:"reason"`
	ProducedAt    time.Time `json:"produced_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	SafeUntil     time.Time `json:"safe_until"`
	Expedited     bool      `json:"expedited"`
	EvidenceOK    bool      `json:"evidence_ok"` // 报损箱是否已上传销毁凭证
	CreatedAt     time.Time `json:"created_at"`
}

// BoxDisposal 报损食品的门店销毁凭证（销毁照片 URL 或内部处理记录，二选一）。
type BoxDisposal struct {
	ID           int64     `json:"id"`
	BatchID      int64     `json:"batch_id"`
	BoxID        int64     `json:"box_id"`
	BoxNo        string    `json:"box_no"`
	EvidenceType string    `json:"evidence_type"` // destruction_photo / internal_record
	EvidenceRef  string    `json:"evidence_ref"`  // 照片 URL/对象键 或 内部处理记录编号
	Note         string    `json:"note"`
	UploadedBy   int64     `json:"uploaded_by"`
	CreatedAt    time.Time `json:"created_at"`
}

// Notification 降级处理后通知已预约机构：哪些箱仍可接收、哪些已报损。
type Notification struct {
	ID            int64      `json:"id"`
	CharityID     int64      `json:"charity_id"`
	BatchID       int64      `json:"batch_id"`
	ReservationID int64      `json:"reservation_id"`
	Kind          string     `json:"kind"` // degradation_notice
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	WrittenOff    []string   `json:"written_off_boxes"`
	Expedited     []string   `json:"expedited_boxes"`
	IsRead        bool       `json:"is_read"`
	CreatedAt     time.Time  `json:"created_at"`
	ReadAt        *time.Time `json:"read_at,omitempty"`
}
