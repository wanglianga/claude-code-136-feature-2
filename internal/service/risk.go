package service

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"passgate/internal/model"
)

// 本文件实现「转借高频异常冻结」专题：
// 单人年卡一周内在不同入口多次人脸不匹配时，综合失败次数、设备位置（入口）、
// 人工放行记录与持卡人申诉判定是否临时冻结；冻结后已预约订单转待核验，游客可
// 提交证件与近期照片申请解冻；确认转借后剩余权益按限制规则使用，并生成客服解释
// 材料。冻结时持卡人若已在园区内，不追溯驱离，仅限制新的入园动作。

// ==================== 在园状态 ====================

// markEntryInPark 入园成功后标记持卡人在园（冻结判定用：在园不追溯驱离）
func (s *Service) markEntryInPark(ctx context.Context, passID string) {
	s.St.R.Set(ctx, keyInPark(passID), time.Now().Unix(), 24*time.Hour)
}

// clearInPark 出园后清除在园标记
func (s *Service) clearInPark(ctx context.Context, passID string) {
	s.St.R.Del(ctx, keyInPark(passID))
}

// IsHolderInPark 持卡人当前是否在园
func (s *Service) IsHolderInPark(ctx context.Context, passID string) bool {
	v, err := s.St.R.Get(ctx, keyInPark(passID)).Int64()
	if err != nil || v == 0 {
		return false
	}
	return time.Since(time.Unix(v, 0)) < 24*time.Hour
}

// ==================== 风险事件（人脸不匹配） ====================

// RecordRiskEvent 记录一次人脸不匹配风险事件；同一核验只记一次
func (s *Service) RecordRiskEvent(ctx context.Context, v *model.Verification) (*model.RiskEvent, error) {
	if v.PassID == "" || v.ID == "" {
		return nil, nil
	}
	if eid, err := s.St.R.Get(ctx, keyRiskEventVerif(v.ID)).Result(); err == nil && eid != "" {
		var e model.RiskEvent
		if err := s.St.GetJSON(ctx, keyRiskEvent(eid), &e); err == nil {
			return &e, nil
		}
	}
	id, err := s.St.NextID(ctx, "revent", "RE")
	if err != nil {
		return nil, err
	}
	e := &model.RiskEvent{
		ID: id, PassID: v.PassID, VerificationID: v.ID,
		DeviceID: v.DeviceID, GateID: v.GateID, Attraction: v.Attraction,
		FaceScore: v.FaceScore, OccurredAt: v.CreatedAt,
	}
	if err := s.St.SetJSON(ctx, keyRiskEvent(id), e); err != nil {
		return nil, err
	}
	s.St.R.SAdd(ctx, keyRiskEvents(v.PassID), id)
	s.St.R.Set(ctx, keyRiskEventVerif(v.ID), id, 0)
	return e, nil
}

// MarkRiskEventManualRelease 人工放行某次人脸不匹配核验后，为对应风险事件打上加权标记
func (s *Service) MarkRiskEventManualRelease(ctx context.Context, verifID string) {
	eid, err := s.St.R.Get(ctx, keyRiskEventVerif(verifID)).Result()
	if err != nil || eid == "" {
		return
	}
	var e model.RiskEvent
	if err := s.St.GetJSON(ctx, keyRiskEvent(eid), &e); err != nil {
		return
	}
	e.ManualRelease = true
	_ = s.St.SetJSON(ctx, keyRiskEvent(eid), &e)
}

// SuppressRiskEvent 申诉核实时，将对应人脸不匹配事件剔除出后续冻结判定
func (s *Service) SuppressRiskEvent(ctx context.Context, verifID, appealID string) {
	eid, err := s.St.R.Get(ctx, keyRiskEventVerif(verifID)).Result()
	if err != nil || eid == "" {
		return
	}
	var e model.RiskEvent
	if err := s.St.GetJSON(ctx, keyRiskEvent(eid), &e); err != nil {
		return
	}
	e.SuppressedBy = appealID
	_ = s.St.SetJSON(ctx, keyRiskEvent(eid), &e)
}

// listActiveRiskEvents 取观察窗口内、未被申诉剔除的人脸不匹配事件（按时间正序）
func (s *Service) listActiveRiskEvents(ctx context.Context, passID string) []model.RiskEvent {
	ids, err := s.St.R.SMembers(ctx, keyRiskEvents(passID)).Result()
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-RiskEventWindow)
	out := make([]model.RiskEvent, 0, len(ids))
	for _, id := range ids {
		var e model.RiskEvent
		if err := s.St.GetJSON(ctx, keyRiskEvent(id), &e); err != nil {
			continue
		}
		if e.SuppressedBy != "" || e.OccurredAt.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].OccurredAt.Before(out[i].OccurredAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ==================== 综合判定 ====================

// RiskSnapshot 一周风险画像：失败次数、入口分布、人工放行、持卡人申诉
type RiskSnapshot struct {
	Events      []model.RiskEvent `json:"events"`
	FailCount   int               `json:"fail_count"`
	GateCount   int               `json:"gate_count"`
	Gates       []string          `json:"gates"`
	ManualCount int               `json:"manual_count"`
	AppealCount int               `json:"appeal_count"`
}

// RiskSnapshotFor 计算某张卡近 7 天的转借风险画像
func (s *Service) RiskSnapshotFor(ctx context.Context, passID string) RiskSnapshot {
	events := s.listActiveRiskEvents(ctx, passID)
	sn := RiskSnapshot{Events: events, FailCount: len(events)}
	gateSeen := map[string]bool{}
	eventVerifs := map[string]bool{}
	for i := range events {
		if events[i].GateID != "" {
			gateSeen[events[i].GateID] = true
		}
		if events[i].ManualRelease {
			sn.ManualCount++
		}
		eventVerifs[events[i].VerificationID] = true
	}
	for g := range gateSeen {
		sn.Gates = append(sn.Gates, g)
	}
	// 稳定顺序
	for i := 0; i < len(sn.Gates); i++ {
		for j := i + 1; j < len(sn.Gates); j++ {
			if sn.Gates[j] < sn.Gates[i] {
				sn.Gates[i], sn.Gates[j] = sn.Gates[j], sn.Gates[i]
			}
		}
	}
	sn.GateCount = len(sn.Gates)
	// 持卡人就这些人脸不匹配事件提起的申诉（未被判为误判剔除者仍计入画像）
	if aids, err := s.St.R.LRange(ctx, keyPassAppeals(passID), 0, 99).Result(); err == nil {
		for _, aid := range aids {
			var a model.Appeal
			if err := s.St.GetJSON(ctx, keyAppeal(aid), &a); err == nil && eventVerifs[a.VerificationID] {
				sn.AppealCount++
			}
		}
	}
	return sn
}

// MeetsAutoFreeze 自动冻结条件：
// 一周内人脸不匹配 ≥4 次且涉及 ≥3 个不同入口；或人脸不匹配 ≥3 次且其中 ≥2 次被人工放行。
func (sn RiskSnapshot) MeetsAutoFreeze() bool {
	if sn.FailCount >= RiskAutoFreezeFails && sn.GateCount >= RiskAutoFreezeGates {
		return true
	}
	if sn.FailCount >= RiskMinFaceFails && sn.ManualCount >= RiskAutoFreezeManuals {
		return true
	}
	return false
}

// MeetsManualFreeze 客服人工风控冻结的建议门槛：≥3 次不匹配，且跨 ≥2 入口，或存在人工放行/申诉争议
func (sn RiskSnapshot) MeetsManualFreeze() bool {
	if sn.FailCount < RiskMinFaceFails {
		return false
	}
	return sn.GateCount >= RiskMinDistinctGates || sn.ManualCount > 0 || sn.AppealCount > 0
}

func (sn RiskSnapshot) summary() string {
	return fmt.Sprintf(
		"近 7 天人脸不匹配 %d 次，涉及 %d 个入园入口 %v；其中现场人工放行 %d 次；持卡人相关申诉 %d 次。",
		sn.FailCount, sn.GateCount, sn.Gates, sn.ManualCount, sn.AppealCount)
}

// ==================== 风控冻结执行 ====================

// applyRiskFreeze 执行风控冻结：卡置 restricted、预约转待核验、生成案件与现场话术
func (s *Service) applyRiskFreeze(ctx context.Context, p *model.Pass, trigger, operator string, sn RiskSnapshot, note string) (*model.FreezeCase, error) {
	token, ok, err := s.St.Lock(ctx, "pass:"+p.ID, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "pass:"+p.ID, token)

	p, err = s.getPass(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if p.Status == model.PassStatusRestricted || p.Status == model.PassStatusFrozen {
		return nil, ErrPassNotActive
	}
	if p.Status == model.PassStatusRefunded {
		return nil, ErrPassNotActive
	}

	id, err := s.St.NextID(ctx, "fcase", "FC")
	if err != nil {
		return nil, err
	}
	eventIDs := make([]string, 0, len(sn.Events))
	for _, e := range sn.Events {
		eventIDs = append(eventIDs, e.ID)
	}
	inPark := s.IsHolderInPark(ctx, p.ID)
	reason := model.FreezeReasonRiskAuto
	if trigger == "MANUAL" {
		reason = model.FreezeReasonRiskManual
	}
	fc := &model.FreezeCase{
		ID: id, PassID: p.ID, Trigger: trigger, Reason: reason,
		Status: model.FreezeCaseFrozen, Operator: operator,
		Summary: sn.summary() + note, EventIDs: eventIDs,
		FailCount: sn.FailCount, GateCount: sn.GateCount,
		ManualCount: sn.ManualCount, AppealCount: sn.AppealCount,
		HolderInPark: inPark, Script: s.buildOnSiteScript(p, sn, inPark),
		CreatedAt: time.Now(),
	}
	if err := s.St.SetJSON(ctx, keyFreezeCase(id), fc); err != nil {
		return nil, err
	}
	s.St.R.LPush(ctx, keyFCaseList(), id)
	s.St.R.LPush(ctx, keyPassFCases(p.ID), id)

	now := time.Now()
	p.Status = model.PassStatusRestricted
	p.FreezeReason = reason
	p.FrozenAt = &now
	p.FreezeCaseID = id
	p.HolderInPark = inPark
	if err := s.savePass(ctx, p); err != nil {
		return nil, err
	}
	// 已预约未入园订单进入待核验（不取消、不核销）
	n := s.transitionReservations(ctx, p.ID, model.ReservationBooked, model.ReservationPendingVerify)
	s.statIncr(ctx, "stats:pass:risk_frozen", 1)
	s.addChangeLog(ctx, p.ID, "RISK_FREEZE",
		fmt.Sprintf("转借高频异常临时冻结（%s）：%s；在园=%v（不追溯驱离）；%d 笔已预约订单转待核验 by %s",
			reason, sn.summary(), inPark, n, operator), operator)
	return fc, nil
}

// EvaluateAndMaybeFreeze 每次新增人脸不匹配事件后调用：达到自动冻结阈值即冻结
func (s *Service) EvaluateAndMaybeFreeze(ctx context.Context, passID string) (*model.FreezeCase, error) {
	p, err := s.getPass(ctx, passID)
	if err != nil {
		return nil, err
	}
	if p.Status != model.PassStatusActive {
		return nil, nil
	}
	// 已确认转借并处于限制使用期的卡，案件已结案，不再重复触发风控冻结
	if r, err := s.GetRestriction(ctx, passID); err == nil && r != nil {
		return nil, nil
	}
	sn := s.RiskSnapshotFor(ctx, passID)
	if !sn.MeetsAutoFreeze() {
		return nil, nil
	}
	return s.applyRiskFreeze(ctx, p, "AUTO", "system", sn, "达到自动冻结阈值，系统临时冻结待持卡人申诉。")
}

// EvaluateAfterAppealReject 持卡人申诉被驳回后的复评：
// 持卡人未能自证 + 近 7 天 ≥3 次不匹配且跨 ≥2 个入口 → 临时冻结。
func (s *Service) EvaluateAfterAppealReject(ctx context.Context, passID, appealID string) (*model.FreezeCase, error) {
	p, err := s.getPass(ctx, passID)
	if err != nil {
		return nil, err
	}
	if p.Status != model.PassStatusActive {
		return nil, nil
	}
	if r, err := s.GetRestriction(ctx, passID); err == nil && r != nil {
		return nil, nil
	}
	sn := s.RiskSnapshotFor(ctx, passID)
	if sn.FailCount < RiskMinFaceFails || sn.GateCount < RiskMinDistinctGates {
		return nil, nil
	}
	return s.applyRiskFreeze(ctx, p, "AUTO", "system", sn,
		fmt.Sprintf("持卡人申诉 %s 被驳回，风险复评仍达临时冻结门槛。", appealID))
}

// MaybeUnfreezeAfterAppeal 申诉核实为本人、剔除对应事件后，若剩余证据不再满足自动冻结门槛则自动解冻
func (s *Service) MaybeUnfreezeAfterAppeal(ctx context.Context, passID, appealID string) bool {
	p, err := s.getPass(ctx, passID)
	if err != nil || p.Status != model.PassStatusRestricted {
		return false
	}
	sn := s.RiskSnapshotFor(ctx, passID)
	if sn.MeetsAutoFreeze() {
		return false
	}
	_ = s.clearRiskFreeze(ctx, passID, model.FreezeCaseUnfrozen, "system",
		fmt.Sprintf("申诉 %s 核实为本人，剔除相关人脸不匹配事件后证据不足，自动解除临时冻结", appealID))
	return true
}

// ManualRiskFreeze 客服根据风险画像人工发起风控冻结（需达到建议门槛）
func (s *Service) ManualRiskFreeze(ctx context.Context, passID, operator, note string) (*model.FreezeCase, error) {
	if note == "" {
		return nil, fmt.Errorf("%w: note required", ErrInvalidInput)
	}
	p, err := s.getPass(ctx, passID)
	if err != nil {
		return nil, err
	}
	if r, err := s.GetRestriction(ctx, passID); err == nil && r != nil {
		return nil, fmt.Errorf("%w: 该卡已确认转借并处于限制使用期，无需重复风控冻结", ErrConflict)
	}
	sn := s.RiskSnapshotFor(ctx, passID)
	if !sn.MeetsManualFreeze() {
		return nil, fmt.Errorf("%w: 风险证据不足（近7天人脸不匹配 %d 次/入口 %d 个），不满足风控冻结条件",
			ErrInvalidInput, sn.FailCount, sn.GateCount)
	}
	return s.applyRiskFreeze(ctx, p, "MANUAL", operator, sn, "客服人工冻结说明："+note)
}

// buildOnSiteScript 现场解释话术：冻结时持卡人若已在园区内，不追溯驱离
func (s *Service) buildOnSiteScript(p *model.Pass, sn RiskSnapshot, inPark bool) string {
	head := fmt.Sprintf(
		"您好，系统监测到 %s 的年卡（卡号 %s）近 7 天在 %d 个入园入口（%v）共出现 %d 次刷脸人与登记信息不一致，"+
			"为保障年卡实名权益，该卡已临时冻结。",
		p.HolderName, p.ID, sn.GateCount, sn.Gates, sn.FailCount)
	if inPark {
		return head + "您目前已经在园区内，本次游玩不受影响，我们不会要求您离开；只是在身份核实完成前，" +
			"该卡不能再刷闸进入新的景点、也不能新增预约。请通过小程序「年卡申诉」上传本人证件与近期照片申请解冻，" +
			"核实无误后会立即恢复，给您带来不便非常抱歉。"
	}
	return head + "在身份核实完成前，该卡暂时无法刷闸入园。请通过小程序「年卡申诉」上传本人证件与近期照片申请解冻，" +
		"核实无误后会立即恢复，给您带来不便非常抱歉。"
}

// transitionReservations 将某状态的预约批量流转到另一状态，返回变更数量
func (s *Service) transitionReservations(ctx context.Context, passID, from, to string) int {
	ids, _ := s.St.R.SMembers(ctx, keyPassRsv(passID)).Result()
	n := 0
	for _, id := range ids {
		var r model.Reservation
		if err := s.St.GetJSON(ctx, keyReservation(id), &r); err != nil {
			continue
		}
		if r.Status == from {
			r.Status = to
			if err := s.St.SetJSON(ctx, keyReservation(id), &r); err == nil {
				n++
			}
		}
	}
	return n
}

// ==================== 案件查询与客服解释材料 ====================

func (s *Service) GetFreezeCase(ctx context.Context, id string) (*model.FreezeCase, error) {
	var fc model.FreezeCase
	if err := s.St.GetJSON(ctx, keyFreezeCase(id), &fc); err != nil {
		return nil, ErrNotFound
	}
	return &fc, nil
}

// ListFreezeCases 风控冻结案件列表（可按卡 / 状态过滤）
func (s *Service) ListFreezeCases(ctx context.Context, passID, status string) ([]model.FreezeCase, error) {
	var ids []string
	var err error
	if passID != "" {
		ids, err = s.St.R.LRange(ctx, keyPassFCases(passID), 0, 99).Result()
	} else {
		ids, err = s.St.R.LRange(ctx, keyFCaseList(), 0, 199).Result()
	}
	if err != nil {
		return nil, err
	}
	out := make([]model.FreezeCase, 0, len(ids))
	for _, id := range ids {
		var fc model.FreezeCase
		if err := s.St.GetJSON(ctx, keyFreezeCase(id), &fc); err == nil {
			if status == "" || fc.Status == status {
				out = append(out, fc)
			}
		}
	}
	return out, nil
}

// EvidenceItem 客服解释材料中的证据条目
type EvidenceItem struct {
	EventID        string    `json:"event_id"`
	OccurredAt     time.Time `json:"occurred_at"`
	GateID         string    `json:"gate_id"`
	DeviceID       string    `json:"device_id"`
	Attraction     string    `json:"attraction"`
	FaceScore      float64   `json:"face_score"`
	ManualRelease  bool      `json:"manual_release"`
	VerificationID string    `json:"verification_id"`
}

// ExplanationMaterial 客服解释材料（含判定依据、证据链、现场话术、权益限制说明）
type ExplanationMaterial struct {
	Case            *model.FreezeCase       `json:"case"`
	PassID          string                  `json:"pass_id"`
	HolderName      string                  `json:"holder_name"`
	PassType        string                  `json:"pass_type"`
	Phone           string                  `json:"phone"`
	Evidence        []EvidenceItem          `json:"evidence"`
	Script          string                  `json:"on_site_script"`
	Restriction     *model.QuotaRestriction `json:"restriction,omitempty"`
	RestrictionNote string                  `json:"restriction_note,omitempty"`
}

// GetExplanationMaterial 生成/获取客服解释材料
func (s *Service) GetExplanationMaterial(ctx context.Context, caseID string) (*ExplanationMaterial, error) {
	fc, err := s.GetFreezeCase(ctx, caseID)
	if err != nil {
		return nil, err
	}
	p, err := s.getPass(ctx, fc.PassID)
	if err != nil {
		return nil, err
	}
	m := &ExplanationMaterial{
		Case: fc, PassID: p.ID, HolderName: p.HolderName,
		PassType: string(p.Type), Phone: p.Phone, Script: fc.Script,
		Evidence: make([]EvidenceItem, 0, len(fc.EventIDs)),
	}
	for _, eid := range fc.EventIDs {
		var e model.RiskEvent
		if err := s.St.GetJSON(ctx, keyRiskEvent(eid), &e); err != nil {
			continue
		}
		m.Evidence = append(m.Evidence, EvidenceItem{
			EventID: e.ID, OccurredAt: e.OccurredAt, GateID: e.GateID, DeviceID: e.DeviceID,
			Attraction: e.Attraction, FaceScore: e.FaceScore, ManualRelease: e.ManualRelease,
			VerificationID: e.VerificationID,
		})
	}
	if r, err := s.GetRestriction(ctx, p.ID); err == nil && r != nil {
		m.Restriction = r
		m.RestrictionNote = fmt.Sprintf(
			"经核实存在转借：剩余权益按 50%% 保留（剩余可入园 %d 次），限制期内不可携带同行人，"+
				"每周入园不超过 %d 次、每日不超过 %d 次，且必须本人刷脸通过，人脸不匹配不再支持人工放行。",
			r.EntryQuotaLeft, r.WeeklyEntryLimit, r.DailyEntryLimit)
	}
	return m, nil
}

// ==================== 解冻申请（证件 + 近期照片） ====================

// SubmitUnfreezeRequest 游客提交解冻申请
func (s *Service) SubmitUnfreezeRequest(ctx context.Context, in UnfreezeInput) (*model.UnfreezeRequest, error) {
	if in.IDDocURL == "" || in.PhotoURL == "" {
		return nil, fmt.Errorf("%w: id_doc_url / photo_url required", ErrInvalidInput)
	}
	p, err := s.getPass(ctx, in.PassID)
	if err != nil {
		return nil, err
	}
	if p.Status != model.PassStatusRestricted || p.FreezeCaseID == "" {
		return nil, fmt.Errorf("%w: pass is not in risk-frozen status", ErrConflict)
	}
	id, err := s.St.NextID(ctx, "ureq", "U")
	if err != nil {
		return nil, err
	}
	u := &model.UnfreezeRequest{
		ID: id, PassID: p.ID, FreezeCaseID: p.FreezeCaseID,
		IDDocURL: in.IDDocURL, PhotoURL: in.PhotoURL, Statement: in.Statement,
		Status: model.UnfreezePending, CreatedAt: time.Now(),
	}
	if err := s.St.SetJSON(ctx, keyUnfreeze(id), u); err != nil {
		return nil, err
	}
	s.St.R.LPush(ctx, keyUnfreezeList(), id)
	s.St.R.LPush(ctx, keyPassUnfreeze(p.ID), id)
	s.addChangeLog(ctx, p.ID, "UNFREEZE_APPLY", "游客提交证件与近期照片申请解冻："+id, "user")
	return u, nil
}

// UnfreezeInput 解冻申请入参
type UnfreezeInput struct {
	PassID    string `json:"pass_id"`
	IDDocURL  string `json:"id_doc_url"`
	PhotoURL  string `json:"photo_url"`
	Statement string `json:"statement"`
}

// ListUnfreezeRequests 解冻申请列表（可按状态 / 卡过滤）
func (s *Service) ListUnfreezeRequests(ctx context.Context, passID, status string) ([]model.UnfreezeRequest, error) {
	var ids []string
	var err error
	if passID != "" {
		ids, err = s.St.R.LRange(ctx, keyPassUnfreeze(passID), 0, 99).Result()
	} else {
		ids, err = s.St.R.LRange(ctx, keyUnfreezeList(), 0, 199).Result()
	}
	if err != nil {
		return nil, err
	}
	out := make([]model.UnfreezeRequest, 0, len(ids))
	for _, id := range ids {
		var u model.UnfreezeRequest
		if err := s.St.GetJSON(ctx, keyUnfreeze(id), &u); err == nil {
			if status == "" || u.Status == status {
				out = append(out, u)
			}
		}
	}
	return out, nil
}

// ReviewUnfreeze 客服审核解冻申请：APPROVE 恢复 / REJECTED 驳回（维持冻结）/ CONFIRM_LENDING 确认转借限制使用
func (s *Service) ReviewUnfreeze(ctx context.Context, id, decision, note, handler string) (*model.UnfreezeRequest, error) {
	if handler == "" || note == "" {
		return nil, fmt.Errorf("%w: handler / note required", ErrInvalidInput)
	}
	if decision != model.UnfreezeApproved && decision != model.UnfreezeRejected && decision != "CONFIRM_LENDING" {
		return nil, fmt.Errorf("%w: decision must be APPROVED / REJECTED / CONFIRM_LENDING", ErrInvalidInput)
	}
	token, ok, err := s.St.Lock(ctx, "ureq:"+id, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "ureq:"+id, token)

	var u model.UnfreezeRequest
	if err := s.St.GetJSON(ctx, keyUnfreeze(id), &u); err != nil {
		return nil, ErrNotFound
	}
	if u.Status != model.UnfreezePending {
		return nil, ErrAlreadyProcessed
	}
	now := time.Now()
	u.ReviewNote, u.Handler, u.ReviewedAt = note, handler, &now

	switch decision {
	case model.UnfreezeApproved:
		if err := s.clearRiskFreeze(ctx, u.PassID, model.FreezeCaseUnfrozen, handler,
			"解冻申请 "+u.ID+" 审核通过：证件与近期照片核验为本人，恢复全部权益。"); err != nil {
			return nil, err
		}
		u.Status = model.UnfreezeApproved
	case model.UnfreezeRejected:
		// 维持冻结，仅记录驳回意见；持卡人可补充材料再次申请
		u.Status = model.UnfreezeRejected
		s.addChangeLog(ctx, u.PassID, "UNFREEZE_REJECT",
			"解冻申请 "+u.ID+" 驳回："+note+" by "+handler, handler)
	case "CONFIRM_LENDING":
		if err := s.applyLendingRestriction(ctx, u.PassID, u.FreezeCaseID, handler,
			"解冻申请 "+u.ID+" 审核：证据确认转借，剩余权益按限制规则使用。"+note); err != nil {
			return nil, err
		}
		u.Status = "CONFIRM_LENDING"
	}
	if err := s.St.SetJSON(ctx, keyUnfreeze(id), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// clearRiskFreeze 解除风控冻结：卡恢复 active，待核验预约恢复为已预约
func (s *Service) clearRiskFreeze(ctx context.Context, passID string, caseStatus, operator, detail string) error {
	token, ok, err := s.St.Lock(ctx, "pass:"+passID, 5*time.Second)
	if err != nil {
		return err
	}
	if !ok {
		return ErrConflict
	}
	defer s.St.Unlock(ctx, "pass:"+passID, token)

	p, err := s.getPass(ctx, passID)
	if err != nil {
		return err
	}
	caseID := p.FreezeCaseID
	p.Status = model.PassStatusActive
	p.FreezeReason = ""
	p.FrozenAt = nil
	p.FreezeCaseID = ""
	p.HolderInPark = false
	if err := s.savePass(ctx, p); err != nil {
		return err
	}
	if caseID != "" {
		if fc, err := s.GetFreezeCase(ctx, caseID); err == nil {
			now := time.Now()
			fc.Status = caseStatus
			fc.ResolvedAt = &now
			_ = s.St.SetJSON(ctx, keyFreezeCase(caseID), fc)
		}
	}
	n := s.transitionReservations(ctx, passID, model.ReservationPendingVerify, model.ReservationBooked)
	s.statIncr(ctx, "stats:pass:risk_frozen", -1)
	s.addChangeLog(ctx, passID, "RISK_UNFREEZE",
		fmt.Sprintf("%s；%d 笔待核验预约恢复为已预约 by %s", detail, n, operator), operator)
	return nil
}

// ==================== 确认转借：剩余权益限制使用 ====================

// GetRestriction 读取转借权益限制（无则 ErrNotFound）
func (s *Service) GetRestriction(ctx context.Context, passID string) (*model.QuotaRestriction, error) {
	var r model.QuotaRestriction
	if err := s.St.GetJSON(ctx, keyRestriction(passID), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// applyLendingRestriction 确认转借：卡恢复可用但剩余权益打折、频次受限、禁止带同行/人工放行
func (s *Service) applyLendingRestriction(ctx context.Context, passID, caseID, operator, detail string) error {
	p, err := s.getPass(ctx, passID)
	if err != nil {
		return err
	}
	remaining, _ := s.remainingQuota(ctx, passID)
	left := int(float64(remaining) * RiskRestrictEntryRate) // 剩余权益保留 50%
	if remaining > 0 && left < 1 {
		left = 1
	}
	r := &model.QuotaRestriction{
		PassID: passID, FreezeCaseID: caseID, EntryQuotaLeft: left,
		CompanionsAllowed: 0, WeeklyEntryLimit: RiskWeeklyEntryLimit,
		DailyEntryLimit: RiskDailyEntryLimit, ManualEntryBlocked: true,
		Reason: "CONFIRMED_LENDING", Operator: operator, CreatedAt: time.Now(),
	}
	if err := s.St.SetJSON(ctx, keyRestriction(passID), r); err != nil {
		return err
	}
	// 剩余额度按比例核减
	if cut := remaining - left; cut > 0 {
		s.St.R.Set(ctx, keyRemaining(passID), left, 0)
		s.St.R.IncrBy(ctx, keyUsed(passID), int64(cut))
		if p != nil {
			s.statIncr(ctx, "stats:quota:used:"+string(p.Type), int64(cut))
		}
	}
	// 解除冻结状态（重新可入园），但携带限制规则
	p.Status = model.PassStatusActive
	p.FreezeReason = ""
	p.FrozenAt = nil
	p.FreezeCaseID = ""
	p.HolderInPark = false
	if err := s.savePass(ctx, p); err != nil {
		return err
	}
	if caseID != "" {
		if fc, err := s.GetFreezeCase(ctx, caseID); err == nil {
			now := time.Now()
			fc.Status = model.FreezeCaseRestricted
			fc.ResolvedAt = &now
			_ = s.St.SetJSON(ctx, keyFreezeCase(caseID), fc)
		}
	}
	// 待核验预约不再保留（确认转借），全部取消
	n := s.transitionReservations(ctx, passID, model.ReservationPendingVerify, model.ReservationCancelled)
	s.statIncr(ctx, "stats:pass:risk_frozen", -1)
	s.statIncr(ctx, "stats:lending:confirmed", 1)
	s.addChangeLog(ctx, passID, "LENDING_CONFIRMED",
		fmt.Sprintf("%s；剩余权益 %d→%d（保留 %.0f%%），不可带同行人，周限 %d 次/日限 %d 次，须本人刷脸；%d 笔待核验预约取消 by %s",
			detail, remaining, left, RiskRestrictEntryRate*100,
			RiskWeeklyEntryLimit, RiskDailyEntryLimit, n, operator), operator)
	return nil
}

// checkRestrictionEntry 限制使用期内的入园频次校验（本人刷脸通过后调用）。
// 返回 "" 放行，或 LENDING_USAGE_LIMITED 类说明。
func (s *Service) checkRestrictionEntry(ctx context.Context, r *model.QuotaRestriction, ts time.Time) string {
	year, wn := ts.ISOWeek()
	weekKey := fmt.Sprintf("%04d-W%02d", year, wn)
	raw, err := restrictEntryScript.Run(ctx, s.St.R, []string{
		keyRestrictDay(r.PassID, dateOf(ts)), keyRestrictWeek(r.PassID, weekKey),
	}, r.DailyEntryLimit, r.WeeklyEntryLimit, 2*24*3600, 8*24*3600).Slice()
	if err != nil {
		return ""
	}
	if len(raw) < 1 {
		return ""
	}
	code, _ := raw[0].(int64)
	switch code {
	case 2:
		return fmt.Sprintf("确认转借限制期内每日最多入园 %d 次，今日额度已用完", r.DailyEntryLimit)
	case 3:
		return fmt.Sprintf("确认转借限制期内每周最多入园 %d 次，本周额度已用完", r.WeeklyEntryLimit)
	}
	return ""
}

// restrictEntryScript 原子占当日/当周入园次数：1=成功 2=日限 3=周限
var restrictEntryScript = redis.NewScript(`
local d = tonumber(redis.call('GET', KEYS[1]) or '0')
local w = tonumber(redis.call('GET', KEYS[2]) or '0')
local dl = tonumber(ARGV[1])
local wl = tonumber(ARGV[2])
if d >= dl then return {2, d, w} end
if w >= wl then return {3, d, w} end
d = redis.call('INCR', KEYS[1])
w = redis.call('INCR', KEYS[2])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
redis.call('EXPIRE', KEYS[2], tonumber(ARGV[4]))
return {1, d, w}
`)

// releaseRestrictionEntry 扣减失败 / 去重命中时回退当日/当周占用次数（不为负）
func (s *Service) releaseRestrictionEntry(ctx context.Context, r *model.QuotaRestriction, ts time.Time) {
	if r == nil {
		return
	}
	year, wn := ts.ISOWeek()
	weekKey := fmt.Sprintf("%04d-W%02d", year, wn)
	release := redis.NewScript(`
local d = tonumber(redis.call('GET', KEYS[1]) or '0')
local w = tonumber(redis.call('GET', KEYS[2]) or '0')
if d > 0 then redis.call('DECR', KEYS[1]) end
if w > 0 then redis.call('DECR', KEYS[2]) end
return 1`)
	_ = release.Run(ctx, s.St.R, []string{keyRestrictDay(r.PassID, dateOf(ts)), keyRestrictWeek(r.PassID, weekKey)}).Err()
}
