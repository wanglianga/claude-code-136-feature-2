package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"passgate/internal/model"
)

// 转借高频异常冻结：
// 一张年卡一周内在不同入口多次出现人脸不匹配时，综合失败次数、设备位置（入口）、
// 人工放行记录与持卡人申诉判断是否临时冻结。冻结后已预约订单进入待核验；
// 持卡人可提交证件与近期照片申请解冻；若确认转借，剩余权益按规则限制使用。
// 冻结时持卡人已在园区内的，不追溯驱离，只限制新的入园动作。

// ---------- 在园状态 ----------

// markInPark 入园成功后标记在园（按景点记录，当日有效）
func (s *Service) markInPark(ctx context.Context, passID, attraction string) {
	s.St.R.SAdd(ctx, keyInPark(passID), attraction)
	s.St.R.Expire(ctx, keyInPark(passID), 36*time.Hour)
}

func (s *Service) leavePark(ctx context.Context, passID, attraction string) {
	if attraction == "" {
		s.St.R.Del(ctx, keyInPark(passID))
		return
	}
	s.St.R.SRem(ctx, keyInPark(passID), attraction)
}

func (s *Service) isInPark(ctx context.Context, passID string) bool {
	n, _ := s.St.R.SCard(ctx, keyInPark(passID)).Result()
	return n > 0
}

// ---------- 人脸不匹配风险事件 ----------

// RecordFaceRiskEvent 登记一次人脸不匹配事件，并尝试触发高频异常冻结判定。
// 返回自动生成的冻结案件（未触发时为 nil）。
func (s *Service) RecordFaceRiskEvent(ctx context.Context, v *model.Verification) *model.FreezeCase {
	if v == nil || v.PassID == "" || v.Reason != model.ReasonFaceMismatch {
		return nil
	}
	p, err := s.getPass(ctx, v.PassID)
	if err != nil {
		return nil
	}
	// 已冻结/已退款的卡不再重复记录与触发；受限卡继续记录证据但不再自动冻结
	if p.Status == model.PassStatusFrozen || p.Status == model.PassStatusRefunded {
		return nil
	}
	id, _ := s.St.NextID(ctx, "fre", "FRE")
	ev := model.FaceRiskEvent{
		ID: id, PassID: v.PassID, VerificationID: v.ID,
		DeviceID: v.DeviceID, GateID: v.GateID, Attraction: v.Attraction,
		GateLabel: v.GateID, FaceScore: v.FaceScore, CreatedAt: v.CreatedAt,
	}
	if dev, err := s.GetDevice(ctx, v.DeviceID); err == nil {
		ev.GateLabel = dev.Name + "（" + dev.GateID + "）"
	}
	_ = s.St.SetJSON(ctx, keyFaceRiskEvent(id), &ev)
	s.St.R.LPush(ctx, keyFaceRisk(v.PassID), id)
	s.St.R.LTrim(ctx, keyFaceRisk(v.PassID), 0, 499)

	fc := s.evaluateRiskFreeze(ctx, p)
	return fc
}

// listFaceRiskEvents 取窗口内的人脸不匹配事件（按时间倒序存储，返回时转正序）
func (s *Service) listFaceRiskEvents(ctx context.Context, passID string, since time.Time) []model.FaceRiskEvent {
	ids, _ := s.St.R.LRange(ctx, keyFaceRisk(passID), 0, 499).Result()
	out := make([]model.FaceRiskEvent, 0, len(ids))
	for _, id := range ids {
		var ev model.FaceRiskEvent
		if err := s.St.GetJSON(ctx, keyFaceRiskEvent(id), &ev); err == nil {
			if !ev.CreatedAt.Before(since) {
				// 人工放行/拒绝结果回填到事件快照
				if ev.ManualAction == "" {
					if v, _, err := s.GetVerification(ctx, ev.VerificationID); err == nil && v.ManualAction != "" {
						ev.ManualAction = v.ManualAction
					}
				}
				out = append(out, ev)
			}
		}
	}
	// 反转为时间正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// markRiskEventReviewed 人工处理某条不匹配核验后，把结果回填到风险事件
func (s *Service) markRiskEventReviewed(ctx context.Context, v *model.Verification) {
	if v == nil || v.Reason != model.ReasonFaceMismatch {
		return
	}
	ids, _ := s.St.R.LRange(ctx, keyFaceRisk(v.PassID), 0, 499).Result()
	for _, id := range ids {
		var ev model.FaceRiskEvent
		if err := s.St.GetJSON(ctx, keyFaceRiskEvent(id), &ev); err == nil && ev.VerificationID == v.ID {
			ev.ManualAction = v.ManualAction
			_ = s.St.SetJSON(ctx, keyFaceRiskEvent(id), &ev)
			return
		}
	}
}

// ---------- 冻结判定 ----------

// evaluateRiskFreeze 综合一周内：失败次数、设备位置（入口）、人工放行记录、持卡人申诉
func (s *Service) evaluateRiskFreeze(ctx context.Context, p *model.Pass) *model.FreezeCase {
	since := time.Now().AddDate(0, 0, -FaceRiskWindowDays)
	events := s.listFaceRiskEvents(ctx, p.ID, since)
	if len(events) < FaceRiskFreezeMinFails {
		return nil
	}
	gateSet := map[string]bool{}
	var gateLabels []string
	manualReleases := 0
	eventIDs := make([]string, 0, len(events))
	for _, ev := range events {
		eventIDs = append(eventIDs, ev.ID)
		if !gateSet[ev.GateID] {
			gateSet[ev.GateID] = true
			gateLabels = append(gateLabels, ev.GateLabel)
		}
		if ev.ManualAction == "RELEASED" {
			manualReleases++
		}
	}
	if len(gateSet) < FaceRiskFreezeMinGates {
		return nil
	}
	// 持卡人申诉（窗口内）——作为证据材料呈现给客服
	appeals := 0
	if list, err := s.ListAppeals(ctx, ""); err == nil {
		for _, a := range list {
			if a.PassID == p.ID && !a.CreatedAt.Before(since) {
				appeals++
			}
		}
	}
	return s.applyRiskFreeze(ctx, p, model.FreezeCase{
		WindowDays: FaceRiskWindowDays, FailCount: len(events), GateCount: len(gateSet),
		GateLabels: gateLabels, ManualReleases: manualReleases, Appeals: appeals,
		FaceRiskEventIDs: eventIDs,
	})
}

// applyRiskFreeze 加锁执行自动冻结：卡状态/案件/预约待核验/在园标记
func (s *Service) applyRiskFreeze(ctx context.Context, p *model.Pass, snap model.FreezeCase) *model.FreezeCase {
	token, ok, err := s.St.Lock(ctx, "pass:"+p.ID, 5*time.Second)
	if err != nil || !ok {
		return nil
	}
	defer s.St.Unlock(ctx, "pass:"+p.ID, token)

	fresh, err := s.getPass(ctx, p.ID)
	if err != nil || fresh.Status != model.PassStatusActive {
		return nil
	}
	caseID, _ := s.St.NextID(ctx, "fcase", "FC")
	now := time.Now()
	reason := fmt.Sprintf("近 %d 天内在 %d 个入口出现 %d 次人脸不匹配，疑似转借，系统临时冻结待核",
		snap.WindowDays, snap.GateCount, snap.FailCount)
	fc := snap
	fc.ID = caseID
	fc.PassID = fresh.ID
	fc.HolderName = fresh.HolderName
	fc.Status = "FROZEN"
	fc.Reason = reason
	if len(fc.FaceRiskEventIDs) > 0 {
		fc.TriggerEvent = fc.FaceRiskEventIDs[len(fc.FaceRiskEventIDs)-1]
	}
	fc.HolderInPark = s.isInPark(ctx, fresh.ID)
	fc.CreatedAt = now

	fresh.Status = model.PassStatusFrozen
	fresh.FreezeSource = "risk_auto"
	fresh.FreezeCaseID = caseID
	fresh.FreezeReason = reason
	fresh.FrozenAt = &now
	if err := s.savePass(ctx, fresh); err != nil {
		return nil
	}
	// 已预约订单进入待核验（不取消、不核销）
	pending := 0
	if rsvIDs, err := s.St.R.SMembers(ctx, keyPassRsv(fresh.ID)).Result(); err == nil {
		for _, rid := range rsvIDs {
			var r model.Reservation
			if err := s.St.GetJSON(ctx, keyReservation(rid), &r); err == nil && r.Status == "BOOKED" {
				r.Status = "PENDING_VERIFICATION"
				if s.St.SetJSON(ctx, keyReservation(rid), &r) == nil {
					pending++
				}
			}
		}
	}
	_ = s.St.SetJSON(ctx, keyFreezeCase(caseID), &fc)
	s.St.R.LPush(ctx, keyPassFreezeCase(fresh.ID), caseID)
	s.St.R.LPush(ctx, keyFreezeCaseList(), caseID)
	s.statIncr(ctx, "stats:pass:frozen", 1)
	s.statIncr(ctx, "stats:pass:riskfrozen", 1)
	s.addChangeLog(ctx, fresh.ID, "RISK_FREEZE",
		fmt.Sprintf("%s；涉及入口：%s；人工放行记录 %d 次；持卡人申诉 %d 次；在园=%v（在园不驱离）；预约转待核验 %d 单",
			reason, strings.Join(fc.GateLabels, "、"), fc.ManualReleases, fc.Appeals, fc.HolderInPark, pending),
		"risk-engine")
	return &fc
}

// ---------- 案件查询与客服材料 ----------

func (s *Service) GetFreezeCase(ctx context.Context, id string) (*model.FreezeCase, []model.FaceRiskEvent, []model.UnfreezeApplication, error) {
	var fc model.FreezeCase
	if err := s.St.GetJSON(ctx, keyFreezeCase(id), &fc); err != nil {
		return nil, nil, nil, ErrCaseNotFound
	}
	events := make([]model.FaceRiskEvent, 0, len(fc.FaceRiskEventIDs))
	for _, eid := range fc.FaceRiskEventIDs {
		var ev model.FaceRiskEvent
		if err := s.St.GetJSON(ctx, keyFaceRiskEvent(eid), &ev); err == nil {
			events = append(events, ev)
		}
	}
	apps := s.listCaseApps(ctx, id)
	return &fc, events, apps, nil
}

func (s *Service) ListFreezeCases(ctx context.Context, status string) ([]model.FreezeCase, error) {
	ids, err := s.St.R.LRange(ctx, keyFreezeCaseList(), 0, 199).Result()
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

func (s *Service) listCaseApps(ctx context.Context, caseID string) []model.UnfreezeApplication {
	ids, _ := s.St.R.LRange(ctx, keyCaseUnfreezeApps(caseID), 0, 49).Result()
	out := make([]model.UnfreezeApplication, 0, len(ids))
	for _, id := range ids {
		var a model.UnfreezeApplication
		if err := s.St.GetJSON(ctx, keyUnfreezeApp(id), &a); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// ListUnfreezeApplications 全部解冻申请（可按状态过滤）
func (s *Service) ListUnfreezeApplications(ctx context.Context, status string) ([]model.UnfreezeApplication, error) {
	ids, err := s.St.R.LRange(ctx, keyUnfreezeAppList(), 0, 199).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.UnfreezeApplication, 0, len(ids))
	for _, id := range ids {
		var a model.UnfreezeApplication
		if err := s.St.GetJSON(ctx, keyUnfreezeApp(id), &a); err == nil {
			if status == "" || a.Status == status {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// ListPassFreezeCases 一张年卡的冻结案件
func (s *Service) ListPassFreezeCases(ctx context.Context, passID string) ([]model.FreezeCase, error) {
	ids, err := s.St.R.LRange(ctx, keyPassFreezeCase(passID), 0, 49).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.FreezeCase, 0, len(ids))
	for _, id := range ids {
		var fc model.FreezeCase
		if err := s.St.GetJSON(ctx, keyFreezeCase(id), &fc); err == nil {
			out = append(out, fc)
		}
	}
	return out, nil
}

// CSExplanation 客服解释材料（确认/驳回时对客统一口径）
type CSExplanation struct {
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	Evidence    string `json:"evidence"`
	CustomerSay string `json:"customer_say"` // 对客解释话术（电话/在线客服）
	OnsiteSay   string `json:"onsite_say"`   // 现场解释话术（在园不驱离）
}

// BuildCSExplanation 根据案件证据生成客服解释材料
func (s *Service) BuildCSExplanation(ctx context.Context, fc *model.FreezeCase, events []model.FaceRiskEvent) CSExplanation {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("年卡 %s（持卡人 %s）近 %d 天人脸不匹配明细：\n", fc.PassID, fc.HolderName, fc.WindowDays))
	for i, ev := range events {
		b.WriteString(fmt.Sprintf("  %d. %s｜%s｜%s｜人脸分 %.1f",
			i+1, ev.CreatedAt.Format("2006-01-02 15:04"), ev.GateLabel, ev.Attraction, ev.FaceScore))
		if ev.ManualAction != "" {
			who := "人工放行"
			if ev.ManualAction == "REJECTED" {
				who = "人工拒绝"
			}
			b.WriteString("｜" + who)
		}
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("汇总：不匹配 %d 次、覆盖 %d 个入口、窗口内人工放行 %d 次、持卡人申诉 %d 次。\n",
		fc.FailCount, fc.GateCount, fc.ManualReleases, fc.Appeals))
	if fc.HolderInPark {
		b.WriteString("冻结时持卡人在园区内：按规则不追溯驱离，仅限制新的入园动作。\n")
	}

	customerSay := fmt.Sprintf(
		"您好 %s 先生/女士，系统监测到您的年卡（%s）近 %d 天在 %d 个不同入口共出现 %d 次人脸比对不匹配，"+
			"为保障年卡实名权益，卡片已临时冻结。冻结不影响您已有的预约（进入“待核验”状态）。"+
			"请您通过客服通道提交本人有效证件和近期清晰照片，我们将在审核通过后为您解冻；"+
			"若核实存在转借，年卡将恢复但剩余权益会受到限制（每日限本人入园 1 次、不可携同行人、不可新预约）。",
		fc.HolderName, fc.PassID, fc.WindowDays, fc.GateCount, fc.FailCount)

	onsiteSay := fmt.Sprintf(
		"您好，系统监测到这张年卡近 %d 天在多个入口出现人脸比对不匹配，为保障年卡实名使用，卡片已临时冻结。"+
			"您现在已经在园区内，本次游览不受影响，我们不会要求您离开；只是从现在起新的入园动作暂时无法刷闸。"+
			"您可以稍后通过小程序/客服提交本人证件和近期照片申请解冻，核实是本人使用后会立即恢复，给您带来不便非常抱歉。",
		fc.WindowDays)
	if !fc.HolderInPark {
		onsiteSay = fmt.Sprintf(
			"您好，这张年卡近 %d 天在多个入口出现人脸比对不匹配，已临时冻结，本次无法入园。"+
				"请通过客服通道提交本人证件与近期照片申请解冻；核实为本人后立即恢复。", fc.WindowDays)
	}

	title := fmt.Sprintf("年卡 %s 转借高频异常冻结说明", fc.PassID)
	summary := fc.Reason
	return CSExplanation{Title: title, Summary: summary, Evidence: b.String(), CustomerSay: customerSay, OnsiteSay: onsiteSay}
}

// ---------- 解冻申请 ----------

// SubmitUnfreezeApplication 游客提交证件 + 近期照片 + 说明，申请解冻
func (s *Service) SubmitUnfreezeApplication(ctx context.Context, passID, idDocURL, recentPhoto, statement string) (*model.UnfreezeApplication, error) {
	p, err := s.getPass(ctx, passID)
	if err != nil {
		return nil, err
	}
	if p.Status != model.PassStatusFrozen {
		return nil, fmt.Errorf("%w: pass is not frozen", ErrConflict)
	}
	if idDocURL == "" || recentPhoto == "" {
		return nil, fmt.Errorf("%w: id_doc_url / recent_photo required", ErrInvalidInput)
	}
	caseID := p.FreezeCaseID
	if caseID == "" {
		return nil, fmt.Errorf("%w: manual freeze has no risk case, please contact service", ErrConflict)
	}
	// 同一案件只允许一个待审申请
	for _, a := range s.listCaseApps(ctx, caseID) {
		if a.Status == "PENDING" {
			return nil, ErrAppPending
		}
	}
	id, err := s.St.NextID(ctx, "ufapp", "UF")
	if err != nil {
		return nil, err
	}
	a := &model.UnfreezeApplication{
		ID: id, CaseID: caseID, PassID: passID, IDDocURL: idDocURL,
		RecentPhoto: recentPhoto, Statement: statement, Status: "PENDING", CreatedAt: time.Now(),
	}
	if err := s.St.SetJSON(ctx, keyUnfreezeApp(id), a); err != nil {
		return nil, err
	}
	s.St.R.LPush(ctx, keyCaseUnfreezeApps(caseID), id)
	s.St.R.LPush(ctx, keyUnfreezeAppList(), id)
	s.addChangeLog(ctx, passID, "UNFREEZE_APPLY", "游客提交解冻申请（证件+近期照片）："+id, "user")
	return a, nil
}

// ReviewUnfreezeApplication 客服审核解冻申请
func (s *Service) ReviewUnfreezeApplication(ctx context.Context, appID string, approve bool, note, handler string) (*model.UnfreezeApplication, *model.Pass, error) {
	if handler == "" || note == "" {
		return nil, nil, fmt.Errorf("%w: handler / note required", ErrInvalidInput)
	}
	var a model.UnfreezeApplication
	if err := s.St.GetJSON(ctx, keyUnfreezeApp(appID), &a); err != nil {
		return nil, nil, ErrAppNotFound
	}
	if a.Status != "PENDING" {
		return nil, nil, ErrAlreadyProcessed
	}
	token, ok, err := s.St.Lock(ctx, "ufapp:"+appID, 5*time.Second)
	if err != nil || !ok {
		return nil, nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "ufapp:"+appID, token)

	now := time.Now()
	a.ReviewNote = note
	a.Handler = handler
	a.ReviewedAt = &now
	if approve {
		a.Status = "APPROVED"
		if err := s.St.SetJSON(ctx, keyUnfreezeApp(appID), &a); err != nil {
			return nil, nil, err
		}
		p, err := s.unfreezeByCase(ctx, a.PassID, a.CaseID, handler, "解冻申请 "+appID+" 审核通过："+note)
		if err != nil {
			return nil, nil, err
		}
		return &a, p, nil
	}
	a.Status = "REJECTED"
	if err := s.St.SetJSON(ctx, keyUnfreezeApp(appID), &a); err != nil {
		return nil, nil, err
	}
	s.addChangeLog(ctx, a.PassID, "UNFREEZE_REJECT", "解冻申请 "+appID+" 驳回："+note, handler)
	p, _ := s.getPass(ctx, a.PassID)
	return &a, p, nil
}

// unfreezeByCase 风险冻结解冻：恢复 active，案件结案，待核验预约保留（入园时再核销）
func (s *Service) unfreezeByCase(ctx context.Context, passID, caseID, operator, resolution string) (*model.Pass, error) {
	ltoken, ok, err := s.St.Lock(ctx, "pass:"+passID, 5*time.Second)
	if err != nil || !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "pass:"+passID, ltoken)

	p, err := s.getPass(ctx, passID)
	if err != nil {
		return nil, err
	}
	if p.Status != model.PassStatusFrozen {
		return nil, ErrPassNotFrozen
	}
	now := time.Now()
	p.Status = model.PassStatusActive
	p.FreezeReason = ""
	p.FreezeSource = ""
	p.FreezeCaseID = ""
	p.FrozenAt = nil
	if err := s.savePass(ctx, p); err != nil {
		return nil, err
	}
	if caseID != "" {
		var fc model.FreezeCase
		if err := s.St.GetJSON(ctx, keyFreezeCase(caseID), &fc); err == nil && fc.Status == "FROZEN" {
			fc.Status = "UNFROZEN"
			fc.ResolvedAt = &now
			fc.Resolver = operator
			fc.Resolution = resolution
			_ = s.St.SetJSON(ctx, keyFreezeCase(caseID), &fc)
		}
	}
	s.statIncr(ctx, "stats:pass:frozen", -1)
	s.addChangeLog(ctx, passID, "UNFREEZE", resolution, operator)
	return p, nil
}

// ---------- 确认转借：权益限制 ----------

// ConfirmLending 客服核实确认为转借：卡恢复但剩余权益按规则限制使用
func (s *Service) ConfirmLending(ctx context.Context, caseID, note, handler string) (*model.Pass, *model.FreezeCase, error) {
	if handler == "" || note == "" {
		return nil, nil, fmt.Errorf("%w: handler / note required", ErrInvalidInput)
	}
	var fc model.FreezeCase
	if err := s.St.GetJSON(ctx, keyFreezeCase(caseID), &fc); err != nil {
		return nil, nil, ErrCaseNotFound
	}
	if fc.Status != "FROZEN" {
		return nil, nil, fmt.Errorf("%w: case already resolved", ErrConflict)
	}
	token, ok, err := s.St.Lock(ctx, "pass:"+fc.PassID, 5*time.Second)
	if err != nil || !ok {
		return nil, nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "pass:"+fc.PassID, token)

	p, err := s.getPass(ctx, fc.PassID)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	p.Status = model.PassStatusRestricted
	p.RestrictedDailyQuota = RestrictedDailyQuota
	p.RestrictedNote = "确认转借，剩余权益限制：每日限本人入园 1 人次，不可携同行人，不可新预约"
	p.RestrictedAt = &now
	p.FrozenAt = nil
	p.FreezeReason = ""
	p.FreezeCaseID = ""
	p.FreezeSource = ""
	if err := s.savePass(ctx, p); err != nil {
		return nil, nil, err
	}
	// 待核验预约不再可用（限制期不可凭预约入园）
	cancelled := 0
	if rsvIDs, err := s.St.R.SMembers(ctx, keyPassRsv(p.ID)).Result(); err == nil {
		for _, rid := range rsvIDs {
			var r model.Reservation
			if err := s.St.GetJSON(ctx, keyReservation(rid), &r); err == nil && r.Status == "PENDING_VERIFICATION" {
				r.Status = "CANCELLED"
				if s.St.SetJSON(ctx, keyReservation(rid), &r) == nil {
					cancelled++
				}
			}
		}
	}
	fc.Status = "LENDING_CONFIRMED"
	fc.ResolvedAt = &now
	fc.Resolver = handler
	fc.Resolution = note
	_ = s.St.SetJSON(ctx, keyFreezeCase(caseID), &fc)
	s.statIncr(ctx, "stats:pass:frozen", -1)
	s.statIncr(ctx, "stats:pass:restricted", 1)
	s.addChangeLog(ctx, p.ID, "LENDING_CONFIRMED",
		fmt.Sprintf("确认转借：%s；%s；取消待核验预约 %d 单", note, p.RestrictedNote, cancelled), handler)
	return p, &fc, nil
}
