package service

import (
	"context"
	"fmt"
	"time"

	"passgate/internal/model"
)

// ---------- 设备 ----------

func (s *Service) RegisterDevice(ctx context.Context, d model.Device) (*model.Device, error) {
	if d.ID == "" || d.GateID == "" || d.Attraction == "" {
		return nil, fmt.Errorf("%w: device id / gate_id / attraction required", ErrInvalidInput)
	}
	d.RegisteredAt = time.Now()
	d.LastHeartbeat = time.Now()
	if err := s.St.SetJSON(ctx, keyDevice(d.ID), &d); err != nil {
		return nil, err
	}
	s.St.R.SAdd(ctx, keyDeviceList(), d.ID)
	return &d, nil
}

func (s *Service) Heartbeat(ctx context.Context, deviceID string) error {
	var d model.Device
	if err := s.St.GetJSON(ctx, keyDevice(deviceID), &d); err != nil {
		return ErrNotFound
	}
	d.LastHeartbeat = time.Now()
	return s.St.SetJSON(ctx, keyDevice(deviceID), &d)
}

func (s *Service) GetDevice(ctx context.Context, id string) (*model.Device, error) {
	var d model.Device
	if err := s.St.GetJSON(ctx, keyDevice(id), &d); err != nil {
		return nil, ErrNotFound
	}
	return &d, nil
}

// DeviceView 设备 + 在线状态
type DeviceView struct {
	model.Device
	Online bool `json:"online"`
}

func (s *Service) ListDevices(ctx context.Context) ([]DeviceView, error) {
	ids, err := s.St.R.SMembers(ctx, keyDeviceList()).Result()
	if err != nil {
		return nil, err
	}
	out := make([]DeviceView, 0, len(ids))
	for _, id := range ids {
		var d model.Device
		if err := s.St.GetJSON(ctx, keyDevice(id), &d); err == nil {
			out = append(out, DeviceView{Device: d, Online: time.Since(d.LastHeartbeat) <= DeviceOfflineAfter})
		}
	}
	return out, nil
}

// ---------- 预约 ----------

// CreateReservation 预约。冻结卡不可新预约；已冻结卡的既有预约不受冻结/解冻影响。
func (s *Service) CreateReservation(ctx context.Context, passID, attraction, date, slot string) (*model.Reservation, error) {
	p, err := s.getPass(ctx, passID)
	if err != nil {
		return nil, err
	}
	if p.Status != model.PassStatusActive {
		return nil, ErrPassNotActive
	}
	if !validDate(date) || attraction == "" {
		return nil, fmt.Errorf("%w: attraction / date(YYYY-MM-DD) required", ErrInvalidInput)
	}
	if !contains(p.Attractions, attraction) {
		return nil, fmt.Errorf("%w: attraction not covered by pass", ErrInvalidInput)
	}
	id, err := s.St.NextID(ctx, "rsv", "R")
	if err != nil {
		return nil, err
	}
	r := &model.Reservation{
		ID: id, PassID: passID, Attraction: attraction, Date: date,
		Slot: slot, Status: "BOOKED", CreatedAt: time.Now(),
	}
	if err := s.St.SetJSON(ctx, keyReservation(id), r); err != nil {
		return nil, err
	}
	s.St.R.SAdd(ctx, keyPassRsv(passID), id)
	s.addChangeLog(ctx, passID, "RESERVE", fmt.Sprintf("预约 %s %s %s", attraction, date, slot), "user")
	return r, nil
}

func (s *Service) CancelReservation(ctx context.Context, id string) (*model.Reservation, error) {
	var r model.Reservation
	if err := s.St.GetJSON(ctx, keyReservation(id), &r); err != nil {
		return nil, ErrNotFound
	}
	if r.Status != "BOOKED" {
		return nil, fmt.Errorf("%w: reservation not in BOOKED status", ErrConflict)
	}
	r.Status = "CANCELLED"
	if err := s.St.SetJSON(ctx, keyReservation(id), &r); err != nil {
		return nil, err
	}
	s.addChangeLog(ctx, r.PassID, "CANCEL_RESERVE", "取消预约 "+id, "user")
	return &r, nil
}

func (s *Service) ListReservations(ctx context.Context, passID string) ([]model.Reservation, error) {
	ids, err := s.St.R.SMembers(ctx, keyPassRsv(passID)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.Reservation, 0, len(ids))
	for _, id := range ids {
		var r model.Reservation
		if err := s.St.GetJSON(ctx, keyReservation(id), &r); err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// ---------- 申诉 ----------

// CreateAppeal 游客申诉
func (s *Service) CreateAppeal(ctx context.Context, passID, verifID, typ, description string) (*model.Appeal, error) {
	if _, err := s.getPass(ctx, passID); err != nil {
		return nil, err
	}
	if description == "" {
		return nil, fmt.Errorf("%w: description required", ErrInvalidInput)
	}
	if verifID != "" {
		if _, _, err := s.GetVerification(ctx, verifID); err != nil {
			return nil, fmt.Errorf("%w: verification not found", ErrInvalidInput)
		}
	}
	id, err := s.St.NextID(ctx, "appeal", "A")
	if err != nil {
		return nil, err
	}
	a := &model.Appeal{
		ID: id, PassID: passID, VerificationID: verifID, Type: typ,
		Description: description, Status: "PENDING", CreatedAt: time.Now(),
	}
	if a.Type == "" {
		a.Type = "OTHER"
	}
	if err := s.St.SetJSON(ctx, keyAppeal(id), a); err != nil {
		return nil, err
	}
	s.St.R.LPush(ctx, keyAppealList(), id)
	s.St.R.LPush(ctx, keyPassAppeals(passID), id)
	return a, nil
}

// ResolveAppeal 客服处理申诉：通过可返还权益额度，保持与闸机扣次一致
func (s *Service) ResolveAppeal(ctx context.Context, id string, approve bool, resolution string, compensateQuota int, handler string) (*model.Appeal, error) {
	if handler == "" || resolution == "" {
		return nil, fmt.Errorf("%w: handler / resolution required", ErrInvalidInput)
	}
	token, ok, err := s.St.Lock(ctx, "appeal:"+id, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "appeal:"+id, token)

	var a model.Appeal
	if err := s.St.GetJSON(ctx, keyAppeal(id), &a); err != nil {
		return nil, ErrNotFound
	}
	if a.Status != "PENDING" {
		return nil, ErrAlreadyProcessed
	}
	now := time.Now()
	a.ResolvedAt = &now
	a.Resolution = resolution
	a.Handler = handler
	if approve {
		a.Status = "APPROVED"
		// 误扣申诉且未指定补偿额时，按该次核验实际扣减返还
		if compensateQuota <= 0 && a.VerificationID != "" {
			if v, _, err := s.GetVerification(ctx, a.VerificationID); err == nil {
				compensateQuota = v.ChargedQuota
			}
		}
		if compensateQuota > 0 {
			a.CompensateQuota = compensateQuota
			s.St.R.IncrBy(ctx, keyRemaining(a.PassID), int64(compensateQuota))
			// used 不为负
			used, _ := s.usedQuota(ctx, a.PassID)
			back := int64(compensateQuota)
			if int64(used)-back < 0 {
				back = int64(used)
			}
			if back > 0 {
				s.St.R.DecrBy(ctx, keyUsed(a.PassID), back)
			}
			if p, err := s.getPass(ctx, a.PassID); err == nil {
				s.statIncr(ctx, "stats:quota:used:"+string(p.Type), -int64(compensateQuota))
			}
		}
		// 申诉核实为本人（照片过时等）：对应人脸不匹配事件剔除出转借冻结判定
		if a.VerificationID != "" {
			s.SuppressRiskEvent(ctx, a.VerificationID, a.ID)
		}
		// 剔除后剩余证据不再达标的风控冻结自动解除
		if s.MaybeUnfreezeAfterAppeal(ctx, a.PassID, a.ID) {
			a.Resolution = resolution + "；相关风险事件剔除后证据不足，已自动解除临时冻结"
		}
	} else {
		a.Status = "REJECTED"
		// 持卡人申诉被驳回：按最新风险画像复评，证据达到门槛（≥3次不匹配/跨≥2入口）时立即临时冻结
		if p, err := s.getPass(ctx, a.PassID); err == nil && p.Status == model.PassStatusActive {
			if fc, ferr := s.EvaluateAfterAppealReject(ctx, a.PassID, a.ID); ferr == nil && fc != nil {
				a.Resolution = resolution + fmt.Sprintf("；申诉驳回后风险复评触发临时冻结（案件 %s）", fc.ID)
			}
		}
	}
	if err := s.St.SetJSON(ctx, keyAppeal(id), &a); err != nil {
		return nil, err
	}
	s.addChangeLog(ctx, a.PassID, "APPEAL_"+a.Status,
		fmt.Sprintf("申诉 %s 处理：%s（补偿额度 %d）by %s", a.ID, resolution, a.CompensateQuota, handler), handler)
	return &a, nil
}

func (s *Service) ListAppeals(ctx context.Context, status string) ([]model.Appeal, error) {
	ids, err := s.St.R.LRange(ctx, keyAppealList(), 0, 499).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.Appeal, 0, len(ids))
	for _, id := range ids {
		var a model.Appeal
		if err := s.St.GetJSON(ctx, keyAppeal(id), &a); err == nil {
			if status == "" || a.Status == status {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// ---------- 景点 / 节假日 / 售卖规则 ----------

func (s *Service) CreateAttraction(ctx context.Context, a model.Attraction) (*model.Attraction, error) {
	if a.ID == "" || a.Name == "" {
		return nil, fmt.Errorf("%w: id / name required", ErrInvalidInput)
	}
	if err := s.St.SetJSON(ctx, keyAttraction(a.ID), &a); err != nil {
		return nil, err
	}
	s.St.R.SAdd(ctx, keyAttractionList(), a.ID)
	return &a, nil
}

func (s *Service) GetAttraction(ctx context.Context, id string) (*model.Attraction, error) {
	var a model.Attraction
	if err := s.St.GetJSON(ctx, keyAttraction(id), &a); err != nil {
		return nil, ErrNotFound
	}
	return &a, nil
}

func (s *Service) ListAttractions(ctx context.Context) ([]model.Attraction, error) {
	ids, err := s.St.R.SMembers(ctx, keyAttractionList()).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.Attraction, 0, len(ids))
	for _, id := range ids {
		var a model.Attraction
		if err := s.St.GetJSON(ctx, keyAttraction(id), &a); err == nil {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *Service) SetHolidays(ctx context.Context, dates []string, operator string) error {
	for _, d := range dates {
		if !validDate(d) {
			return fmt.Errorf("%w: bad date %q", ErrInvalidInput, d)
		}
	}
	pipe := s.St.R.TxPipeline()
	pipe.Del(ctx, keyHolidays())
	if len(dates) > 0 {
		members := make([]interface{}, len(dates))
		for i, d := range dates {
			members[i] = d
		}
		pipe.SAdd(ctx, keyHolidays(), members...)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Service) ListHolidays(ctx context.Context) ([]string, error) {
	return s.St.R.SMembers(ctx, keyHolidays()).Result()
}

func (s *Service) GetSalesRule(ctx context.Context, t model.PassType) (*model.SalesRule, error) {
	var r model.SalesRule
	if err := s.St.GetJSON(ctx, keySalesRule(t), &r); err != nil {
		return nil, ErrNotFound
	}
	return &r, nil
}

func (s *Service) ListSalesRules(ctx context.Context) ([]model.SalesRule, error) {
	out := make([]model.SalesRule, 0, 3)
	for _, t := range []model.PassType{model.PassTypeSingle, model.PassTypeParentChild, model.PassTypeFamily} {
		if r, err := s.GetSalesRule(ctx, t); err == nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

// UpdateSalesRule 运营调整售卖规则（仅影响调整后新售出的卡）
func (s *Service) UpdateSalesRule(ctx context.Context, t model.PassType, in model.SalesRule, operator string) (*model.SalesRule, error) {
	rule, err := s.GetSalesRule(ctx, t)
	if err != nil {
		return nil, err
	}
	// 仅更新传入的非零字段之外的整体替换过于粗暴，这里整体替换但保留类型
	in.PassType = t
	in.UpdatedAt = time.Now()
	in.UpdatedBy = operator
	if in.ValidDays <= 0 {
		in.ValidDays = rule.ValidDays
	}
	if err := s.St.SetJSON(ctx, keySalesRule(t), &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// ---------- 运营统计 ----------

// TypeStats 单类型统计
type TypeStats struct {
	PassType       model.PassType `json:"pass_type"`
	SoldTotal      int64          `json:"sold_total"`
	QuotaTotal     int64          `json:"quota_total"`
	QuotaUsed      int64          `json:"quota_used"`
	UsageRate      float64        `json:"usage_rate"` // 权益使用率
	EntriesToday   int64          `json:"entries_today"`
	AnomaliesToday int64          `json:"anomalies_today"`
}

// OverviewStats 运营总览
type OverviewStats struct {
	Date             string           `json:"date"`
	EntriesTotal     int64            `json:"entries_total"`
	AnomalyTotal     int64            `json:"anomaly_total"`
	ManualReleases   int64            `json:"manual_releases"`
	FrozenPasses     int64            `json:"frozen_passes"`
	RiskFrozenPasses int64            `json:"risk_frozen_passes"`
	LendingConfirmed int64            `json:"lending_confirmed_total"`
	ByType           []TypeStats      `json:"by_type"`
	AnomalyByReason  map[string]int64 `json:"anomaly_by_reason"`
}

func (s *Service) getInt64(ctx context.Context, key string) int64 {
	n, _ := s.St.R.Get(ctx, key).Int64()
	return n
}

func (s *Service) StatsOverview(ctx context.Context, date string) (*OverviewStats, error) {
	if date == "" {
		date = dateOf(time.Now())
	}
	o := &OverviewStats{Date: date, AnomalyByReason: map[string]int64{}}
	o.EntriesTotal = s.getInt64(ctx, "stats:entry:"+date)
	o.AnomalyTotal = s.getInt64(ctx, "stats:anomaly:total:"+date)
	o.ManualReleases = s.getInt64(ctx, "stats:manual:"+date)
	o.FrozenPasses = s.getInt64(ctx, "stats:pass:frozen")
	o.RiskFrozenPasses = s.getInt64(ctx, "stats:pass:risk_frozen")
	o.LendingConfirmed = s.getInt64(ctx, "stats:lending:confirmed")
	for _, t := range []model.PassType{model.PassTypeSingle, model.PassTypeParentChild, model.PassTypeFamily} {
		ts := TypeStats{
			PassType:       t,
			SoldTotal:      s.getInt64(ctx, "stats:sale:total:"+string(t)),
			QuotaTotal:     s.getInt64(ctx, "stats:quota:total:"+string(t)),
			QuotaUsed:      s.getInt64(ctx, "stats:quota:used:"+string(t)),
			EntriesToday:   s.getInt64(ctx, "stats:entry:type:"+date+":"+string(t)),
			AnomaliesToday: s.getInt64(ctx, "stats:anomaly:type:"+date+":"+string(t)),
		}
		if ts.QuotaTotal > 0 {
			ts.UsageRate = float64(ts.QuotaUsed) / float64(ts.QuotaTotal)
		}
		o.ByType = append(o.ByType, ts)
	}
	// 异常原因分布
	keys, _ := s.St.R.Keys(ctx, "stats:anomaly:"+date+":*").Result()
	for _, k := range keys {
		var reason string
		if _, err := fmt.Sscanf(k, "stats:anomaly:"+date+":%s", &reason); err == nil && reason != "" {
			o.AnomalyByReason[reason] = s.getInt64(ctx, k)
		}
	}
	return o, nil
}

// PeakStats 入园峰值（按小时 + 按景点）
type PeakStats struct {
	Date         string           `json:"date"`
	ByHour       map[string]int64 `json:"by_hour"`
	PeakHour     string           `json:"peak_hour"`
	ByAttraction map[string]int64 `json:"by_attraction"`
}

func (s *Service) StatsPeaks(ctx context.Context, date string) (*PeakStats, error) {
	if date == "" {
		date = dateOf(time.Now())
	}
	p := &PeakStats{Date: date, ByHour: map[string]int64{}, ByAttraction: map[string]int64{}}
	var max int64
	for h := 0; h < 24; h++ {
		key := fmt.Sprintf("stats:entry:%s:%02d", date, h)
		n := s.getInt64(ctx, key)
		p.ByHour[fmt.Sprintf("%02d", h)] = n
		if n > max {
			max = n
			p.PeakHour = fmt.Sprintf("%02d", h)
		}
	}
	keys, _ := s.St.R.Keys(ctx, "stats:entry:attr:"+date+":*").Result()
	for _, k := range keys {
		attr := k[len("stats:entry:attr:"+date+":"):]
		p.ByAttraction[attr] = s.getInt64(ctx, k)
	}
	return p, nil
}

// ListGateLogs 闸机日志（可按景点/卡过滤）
func (s *Service) ListGateLogs(ctx context.Context, passID string, limit int) ([]model.GateLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var ids []string
	var err error
	if passID != "" {
		ids, err = s.St.R.LRange(ctx, keyPassGateLogs(passID), 0, int64(limit-1)).Result()
	} else {
		ids, err = s.St.R.LRange(ctx, keyGateLogList(), 0, int64(limit-1)).Result()
	}
	if err != nil {
		return nil, err
	}
	out := make([]model.GateLog, 0, len(ids))
	for _, id := range ids {
		var gl model.GateLog
		if err := s.St.GetJSON(ctx, keyGateLog(id), &gl); err == nil {
			out = append(out, gl)
		}
	}
	return out, nil
}

// CapacityView 景点在园人数
func (s *Service) CapacityNow(ctx context.Context, attraction, date string) (int64, error) {
	if date == "" {
		date = dateOf(time.Now())
	}
	return s.getInt64(ctx, keyCap(attraction, date)), nil
}
