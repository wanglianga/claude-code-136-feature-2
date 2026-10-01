package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"passgate/internal/model"
	"passgate/internal/store"
)

// MemberInput 购卡/添加成员入参
type MemberInput struct {
	Name         string `json:"name"`
	IDCard       string `json:"id_card"`
	IDCardExpiry string `json:"id_card_expiry"`
	Relation     string `json:"relation"`
	PhotoURL     string `json:"photo_url"`
	FaceExempt   bool   `json:"face_exempt"`
}

// CreatePassInput 购卡入参
type CreatePassInput struct {
	Type         model.PassType `json:"type"`
	HolderName   string         `json:"holder_name"`
	HolderIDCard string         `json:"holder_id_card"`
	IDCardExpiry string         `json:"id_card_expiry"`
	Phone        string         `json:"phone"`
	PhotoURL     string         `json:"photo_url"`
	Members      []MemberInput  `json:"members"`
}

func newQRToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "QR" + hex.EncodeToString(b)
}

// CreatePass 购卡：校验售卖规则 → 一证一卡 → 写卡/成员/索引/额度计数
func (s *Service) CreatePass(ctx context.Context, in CreatePassInput) (*model.PassView, error) {
	if in.HolderName == "" || in.HolderIDCard == "" || in.Phone == "" {
		return nil, fmt.Errorf("%w: holder_name / holder_id_card / phone required", ErrInvalidInput)
	}
	if !validDate(in.IDCardExpiry) {
		return nil, fmt.Errorf("%w: id_card_expiry must be YYYY-MM-DD", ErrInvalidInput)
	}
	rule, err := s.GetSalesRule(ctx, in.Type)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown pass type %q", ErrInvalidInput, in.Type)
	}
	if !rule.OnSale {
		return nil, ErrOffSale
	}
	today := dateOf(time.Now())
	if rule.DailyLimit > 0 {
		sold, _ := s.St.R.Get(ctx, "stats:sale:"+today+":"+string(in.Type)).Int()
		if sold >= rule.DailyLimit {
			return nil, ErrDailyLimit
		}
	}
	// 一证一卡：同一证件已有有效年卡则拒绝（防转借基础约束）
	if existing, err := s.St.R.Get(ctx, keyIdxHolder(in.HolderIDCard)).Result(); err == nil && existing != "" {
		if p, err := s.getPass(ctx, existing); err == nil && p.Status != model.PassStatusRefunded {
			return nil, ErrDuplicateHolder
		}
	}
	for _, m := range in.Members {
		if m.IDCard == "" || m.Name == "" {
			return nil, fmt.Errorf("%w: member name/id_card required", ErrInvalidInput)
		}
		if m.IDCard == in.HolderIDCard {
			return nil, fmt.Errorf("%w: member id_card duplicates holder", ErrInvalidInput)
		}
	}

	id, err := s.St.NextID(ctx, "pass", "P")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	until := now.AddDate(0, 0, rule.ValidDays)
	p := &model.Pass{
		ID:               id,
		Type:             in.Type,
		Status:           model.PassStatusActive,
		HolderName:       in.HolderName,
		HolderIDCard:     in.HolderIDCard,
		Phone:            in.Phone,
		ValidFrom:        dateOf(now),
		ValidUntil:       dateOf(until),
		TotalQuota:       rule.TotalQuota,
		CompanionLimit:   rule.CompanionLimit,
		Attractions:      rule.Attractions,
		HolidayBlocked:   rule.HolidayBlocked,
		AllowedSlots:     rule.AllowedSlots,
		MemberChangeLeft: rule.MemberChangeLimit,
		PaidAmount:       rule.Price,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.St.SetJSON(ctx, keyPass(id), p); err != nil {
		return nil, err
	}
	// 额度计数器（权威值，原子扣减的对象）
	s.St.R.Set(ctx, keyRemaining(id), rule.TotalQuota, 0)
	s.St.R.Set(ctx, keyUsed(id), 0, 0)
	// 索引
	s.St.R.Set(ctx, keyIdxHolder(in.HolderIDCard), id, 0)
	s.St.R.SAdd(ctx, keyIdxPhone(in.Phone), id)
	token := newQRToken()
	s.St.R.Set(ctx, keyIdxQR(token), id, 0)
	s.St.R.Set(ctx, keyQRToken(id), token, 0)
	// 持卡人成员
	holder := model.Member{
		ID: id + "-M0", PassID: id, Name: in.HolderName, IDCard: in.HolderIDCard,
		IDCardExpiry: in.IDCardExpiry, Role: "holder", Relation: "self",
		PhotoURL: in.PhotoURL, PhotoVersion: 1, CreatedAt: now,
	}
	if err := s.St.SetJSON(ctx, keyMember(holder.ID), &holder); err != nil {
		return nil, err
	}
	s.St.R.SAdd(ctx, keyPassMembers(id), holder.ID)
	s.St.R.Set(ctx, keyIdxMember(in.HolderIDCard), holder.ID, 0)
	// 同行成员
	for i, mi := range in.Members {
		m := model.Member{
			ID: fmt.Sprintf("%s-M%d", id, i+1), PassID: id, Name: mi.Name, IDCard: mi.IDCard,
			IDCardExpiry: mi.IDCardExpiry, Role: "companion", Relation: mi.Relation,
			PhotoURL: mi.PhotoURL, PhotoVersion: 1, FaceExempt: mi.FaceExempt, CreatedAt: now,
		}
		if m.IDCardExpiry == "" {
			m.IDCardExpiry = in.IDCardExpiry
		}
		if err := s.St.SetJSON(ctx, keyMember(m.ID), &m); err != nil {
			return nil, err
		}
		s.St.R.SAdd(ctx, keyPassMembers(id), m.ID)
		s.St.R.Set(ctx, keyIdxMember(m.IDCard), m.ID, 0)
	}
	// 统计
	s.statIncr(ctx, "stats:sale:total:"+string(in.Type), 1)
	s.statIncr(ctx, "stats:sale:"+today+":"+string(in.Type), 1)
	s.statIncr(ctx, "stats:quota:total:"+string(in.Type), int64(rule.TotalQuota))
	s.addChangeLog(ctx, id, "CREATE", fmt.Sprintf("购买%s年卡，实付 %.2f 元，额度 %d", in.Type, rule.Price, rule.TotalQuota), "system")
	return s.GetPassView(ctx, id)
}

// GetPassView 年卡详情（含实时额度与成员）
func (s *Service) GetPassView(ctx context.Context, id string) (*model.PassView, error) {
	p, err := s.getPass(ctx, id)
	if err != nil {
		return nil, err
	}
	members, err := s.listMembers(ctx, id)
	if err != nil {
		return nil, err
	}
	remaining, _ := s.remainingQuota(ctx, id)
	used, _ := s.usedQuota(ctx, id)
	token, _ := s.St.R.Get(ctx, keyQRToken(id)).Result()
	return &model.PassView{Pass: *p, RemainingQuota: remaining, UsedQuota: used, Members: members, QRToken: token}, nil
}

// FindPassesByPhone 按手机号查卡
func (s *Service) FindPassesByPhone(ctx context.Context, phone string) ([]string, error) {
	return s.St.R.SMembers(ctx, keyIdxPhone(phone)).Result()
}

// Freeze 冻结年卡：不影响已预约未入园订单（预约记录保持 BOOKED）
func (s *Service) Freeze(ctx context.Context, id, reason, operator string) (*model.Pass, error) {
	token, ok, err := s.St.Lock(ctx, "pass:"+id, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "pass:"+id, token)

	p, err := s.getPass(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Status != model.PassStatusActive {
		return nil, ErrPassNotActive
	}
	now := time.Now()
	p.Status = model.PassStatusFrozen
	p.FreezeReason = reason
	p.FrozenAt = &now
	if err := s.savePass(ctx, p); err != nil {
		return nil, err
	}
	s.statIncr(ctx, "stats:pass:frozen", 1)
	s.addChangeLog(ctx, id, "FREEZE", "冻结原因："+reason, operator)
	return p, nil
}

// Unfreeze 解冻：恢复 active，已预约订单继续有效。
// 转借风控冻结（restricted）走风控解冻路径，待核验预约一并恢复。
func (s *Service) Unfreeze(ctx context.Context, id, operator string) (*model.Pass, error) {
	p, err := s.getPass(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Status == model.PassStatusRestricted {
		if err := s.clearRiskFreeze(ctx, id, model.FreezeCaseUnfrozen, operator,
			"客服直接解除转借风控冻结，恢复全部权益"); err != nil {
			return nil, err
		}
		return s.getPass(ctx, id)
	}

	token, ok, err := s.St.Lock(ctx, "pass:"+id, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "pass:"+id, token)

	p, err = s.getPass(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Status != model.PassStatusFrozen {
		return nil, ErrPassNotFrozen
	}
	p.Status = model.PassStatusActive
	p.FreezeReason = ""
	p.FrozenAt = nil
	if err := s.savePass(ctx, p); err != nil {
		return nil, err
	}
	s.statIncr(ctx, "stats:pass:frozen", -1)
	s.addChangeLog(ctx, id, "UNFREEZE", "解冻，恢复入园权益", operator)
	return p, nil
}

// Refund 退款：按剩余有效期折算退款，权益清零，未使用预约取消
func (s *Service) Refund(ctx context.Context, id, operator string) (*model.Pass, error) {
	token, ok, err := s.St.Lock(ctx, "pass:"+id, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "pass:"+id, token)

	p, err := s.getPass(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Status == model.PassStatusRefunded {
		return nil, fmt.Errorf("%w: pass already refunded", ErrConflict)
	}
	from, _ := time.Parse("2006-01-02", p.ValidFrom)
	until, _ := time.Parse("2006-01-02", p.ValidUntil)
	totalDays := int(until.Sub(from).Hours() / 24)
	if totalDays <= 0 {
		totalDays = 1
	}
	remainDays := int(until.Sub(time.Now()).Hours()/24) + 1
	if remainDays < 0 {
		remainDays = 0
	}
	refund := math.Round(p.PaidAmount*float64(remainDays)/float64(totalDays)*RefundRate*100) / 100
	wasRiskFrozen := p.Status == model.PassStatusRestricted
	now := time.Now()
	p.Status = model.PassStatusRefunded
	p.RefundedAt = &now
	p.RefundAmount = refund
	p.FreezeReason = ""
	p.FrozenAt = nil
	if wasRiskFrozen {
		p.FreezeCaseID = ""
		p.HolderInPark = false
	}
	if err := s.savePass(ctx, p); err != nil {
		return nil, err
	}
	if wasRiskFrozen {
		s.statIncr(ctx, "stats:pass:risk_frozen", -1)
	}
	// 权益清零
	s.St.R.Set(ctx, keyRemaining(id), 0, 0)
	// 未使用预约取消（含风控冻结转待核验的预约）
	rsvIDs, _ := s.St.R.SMembers(ctx, keyPassRsv(id)).Result()
	for _, rid := range rsvIDs {
		var r model.Reservation
		if err := s.St.GetJSON(ctx, keyReservation(rid), &r); err == nil &&
			(r.Status == "BOOKED" || r.Status == model.ReservationPendingVerify) {
			r.Status = "CANCELLED"
			_ = s.St.SetJSON(ctx, keyReservation(rid), &r)
		}
	}
	s.addChangeLog(ctx, id, "REFUND",
		fmt.Sprintf("退款 %.2f 元（剩余 %d/%d 天 × %.0f%%），权益清零，未使用预约已取消", refund, remainDays, totalDays, RefundRate*100), operator)
	return p, nil
}

// Upgrade 升级年卡：重新计算有效期（自升级日起按新卡时长）与权益额度
func (s *Service) Upgrade(ctx context.Context, id string, toType model.PassType, operator string) (*model.PassView, float64, error) {
	token, ok, err := s.St.Lock(ctx, "pass:"+id, 5*time.Second)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, ErrConflict
	}
	defer s.St.Unlock(ctx, "pass:"+id, token)

	p, err := s.getPass(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if p.Status != model.PassStatusActive {
		return nil, 0, ErrPassNotActive
	}
	if p.Type == toType {
		return nil, 0, fmt.Errorf("%w: already %s", ErrInvalidInput, toType)
	}
	rule, err := s.GetSalesRule(ctx, toType)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: unknown pass type %q", ErrInvalidInput, toType)
	}
	// 补差价 = 新卡价格 - 旧卡剩余价值（剩余价值按剩余天数折算，不为负）
	from, _ := time.Parse("2006-01-02", p.ValidFrom)
	until, _ := time.Parse("2006-01-02", p.ValidUntil)
	totalDays := int(until.Sub(from).Hours() / 24)
	if totalDays <= 0 {
		totalDays = 1
	}
	remainDays := int(until.Sub(time.Now()).Hours()/24) + 1
	if remainDays < 0 {
		remainDays = 0
	}
	oldValue := p.PaidAmount * float64(remainDays) / float64(totalDays)
	diff := math.Round((rule.Price-oldValue)*100) / 100
	if diff < 0 {
		diff = 0
	}
	oldType := p.Type
	now := time.Now()
	// 重新计算：类型 / 有效期 / 额度 / 同行规则
	p.Type = toType
	p.ValidFrom = dateOf(now)
	p.ValidUntil = dateOf(now.AddDate(0, 0, rule.ValidDays))
	p.TotalQuota = rule.TotalQuota
	p.CompanionLimit = rule.CompanionLimit
	p.Attractions = rule.Attractions
	p.HolidayBlocked = rule.HolidayBlocked
	p.AllowedSlots = rule.AllowedSlots
	p.MemberChangeLeft = rule.MemberChangeLimit
	p.PaidAmount = math.Round((p.PaidAmount+diff)*100) / 100
	p.UpgradedFrom = string(oldType)
	if err := s.savePass(ctx, p); err != nil {
		return nil, 0, err
	}
	// 额度按新卡重新给足（有效期已重算为新周期）
	s.St.R.Set(ctx, keyRemaining(id), rule.TotalQuota, 0)
	s.St.R.Set(ctx, keyUsed(id), 0, 0)
	s.statIncr(ctx, "stats:quota:total:"+string(toType), int64(rule.TotalQuota))
	s.addChangeLog(ctx, id, "UPGRADE",
		fmt.Sprintf("%s → %s，补差价 %.2f 元，有效期重算为 %s ~ %s，额度重置为 %d",
			oldType, toType, diff, p.ValidFrom, p.ValidUntil, rule.TotalQuota), operator)
	view, err := s.GetPassView(ctx, id)
	return view, diff, err
}

// ChangePhone 换绑手机号（模拟短信验证码，演示环境固定 123456）
func (s *Service) ChangePhone(ctx context.Context, id, oldPhone, newPhone, code, operator string) (*model.Pass, error) {
	if code != VerifyCodeMock {
		return nil, ErrVerifyCode
	}
	if newPhone == "" {
		return nil, fmt.Errorf("%w: new_phone required", ErrInvalidInput)
	}
	p, err := s.getPass(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Phone != oldPhone {
		return nil, fmt.Errorf("%w: old_phone mismatch", ErrConflict)
	}
	s.St.R.SRem(ctx, keyIdxPhone(oldPhone), id)
	s.St.R.SAdd(ctx, keyIdxPhone(newPhone), id)
	p.Phone = newPhone
	if err := s.savePass(ctx, p); err != nil {
		return nil, err
	}
	s.addChangeLog(ctx, id, "CHANGE_PHONE", fmt.Sprintf("手机号 %s → %s", oldPhone, newPhone), operator)
	return p, nil
}

// AddMember 添加同行成员（消耗一次成员更换额度）
func (s *Service) AddMember(ctx context.Context, passID string, in MemberInput, operator string) (*model.Member, error) {
	token, ok, err := s.St.Lock(ctx, "pass:"+passID, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	defer s.St.Unlock(ctx, "pass:"+passID, token)

	p, err := s.getPass(ctx, passID)
	if err != nil {
		return nil, err
	}
	if p.Status == model.PassStatusRefunded {
		return nil, ErrPassNotActive
	}
	if p.MemberChangeLeft <= 0 {
		return nil, ErrMemberChangeNone
	}
	if in.Name == "" || in.IDCard == "" {
		return nil, fmt.Errorf("%w: member name/id_card required", ErrInvalidInput)
	}
	if _, err := s.St.R.Get(ctx, keyIdxMember(in.IDCard)).Result(); err == nil {
		return nil, fmt.Errorf("%w: id_card already bound to a member", ErrConflict)
	}
	members, _ := s.listMembers(ctx, passID)
	m := model.Member{
		ID: fmt.Sprintf("%s-M%d", passID, len(members)), PassID: passID, Name: in.Name, IDCard: in.IDCard,
		IDCardExpiry: in.IDCardExpiry, Role: "companion", Relation: in.Relation,
		PhotoURL: in.PhotoURL, PhotoVersion: 1, FaceExempt: in.FaceExempt, CreatedAt: time.Now(),
	}
	if err := s.St.SetJSON(ctx, keyMember(m.ID), &m); err != nil {
		return nil, err
	}
	s.St.R.SAdd(ctx, keyPassMembers(passID), m.ID)
	s.St.R.Set(ctx, keyIdxMember(m.IDCard), m.ID, 0)
	p.MemberChangeLeft--
	_ = s.savePass(ctx, p)
	s.addChangeLog(ctx, passID, "ADD_MEMBER", fmt.Sprintf("添加成员 %s（%s），剩余更换次数 %d", in.Name, in.IDCard, p.MemberChangeLeft), operator)
	return &m, nil
}

// RemoveMember 移除同行成员（不消耗更换次数）
func (s *Service) RemoveMember(ctx context.Context, passID, memberID, operator string) error {
	p, err := s.getPass(ctx, passID)
	if err != nil {
		return err
	}
	var m model.Member
	if err := s.St.GetJSON(ctx, keyMember(memberID), &m); err != nil {
		return ErrNotFound
	}
	if m.PassID != passID {
		return ErrNotFound
	}
	if m.Role == "holder" {
		return fmt.Errorf("%w: holder cannot be removed", ErrInvalidInput)
	}
	s.St.R.SRem(ctx, keyPassMembers(passID), memberID)
	s.St.R.Del(ctx, keyIdxMember(m.IDCard))
	s.St.R.Del(ctx, keyMember(memberID))
	s.addChangeLog(ctx, passID, "REMOVE_MEMBER", fmt.Sprintf("移除成员 %s（%s）", m.Name, m.IDCard), operator)
	_ = p
	return nil
}

// UpdateMemberPhoto 更新成员人脸照片（孩子长大照片变化），版本号递增
func (s *Service) UpdateMemberPhoto(ctx context.Context, passID, memberID, photoURL, operator string) (*model.Member, error) {
	if photoURL == "" {
		return nil, fmt.Errorf("%w: photo_url required", ErrInvalidInput)
	}
	var m model.Member
	if err := s.St.GetJSON(ctx, keyMember(memberID), &m); err != nil || m.PassID != passID {
		return nil, ErrNotFound
	}
	m.PhotoURL = photoURL
	m.PhotoVersion++
	if err := s.St.SetJSON(ctx, keyMember(memberID), &m); err != nil {
		return nil, err
	}
	s.addChangeLog(ctx, passID, "UPDATE_PHOTO", fmt.Sprintf("成员 %s 更新人脸照片（v%d）", m.Name, m.PhotoVersion), operator)
	return &m, nil
}

// SetFaceExempt 标记/取消成员免刷脸（老人无法刷脸场景，需工作人员操作）
func (s *Service) SetFaceExempt(ctx context.Context, passID, memberID string, exempt bool, operator string) (*model.Member, error) {
	var m model.Member
	if err := s.St.GetJSON(ctx, keyMember(memberID), &m); err != nil || m.PassID != passID {
		return nil, ErrNotFound
	}
	m.FaceExempt = exempt
	if err := s.St.SetJSON(ctx, keyMember(memberID), &m); err != nil {
		return nil, err
	}
	s.addChangeLog(ctx, passID, "FACE_EXEMPT", fmt.Sprintf("成员 %s 免刷脸标记=%v", m.Name, exempt), operator)
	return &m, nil
}

// RefreshQRToken 刷新入园二维码（换手机/疑似泄露时）
func (s *Service) RefreshQRToken(ctx context.Context, passID, operator string) (string, error) {
	p, err := s.getPass(ctx, passID)
	if err != nil {
		return "", err
	}
	if p.Status == model.PassStatusRefunded {
		return "", ErrPassNotActive
	}
	old, _ := s.St.R.Get(ctx, keyQRToken(passID)).Result()
	if old != "" {
		s.St.R.Del(ctx, keyIdxQR(old))
	}
	token := newQRToken()
	s.St.R.Set(ctx, keyIdxQR(token), passID, 0)
	s.St.R.Set(ctx, keyQRToken(passID), token, 0)
	s.addChangeLog(ctx, passID, "REFRESH_QR", "刷新入园二维码", operator)
	return token, nil
}

// ListChangeLogs 年卡变更日志
func (s *Service) ListChangeLogs(ctx context.Context, passID string) ([]model.ChangeLog, error) {
	ids, err := s.St.R.LRange(ctx, keyPassChanges(passID), 0, 99).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.ChangeLog, 0, len(ids))
	for _, id := range ids {
		var cl model.ChangeLog
		if err := s.St.GetJSON(ctx, "changelog:"+id, &cl); err == nil {
			out = append(out, cl)
		}
	}
	return out, nil
}

// findPassByVerifyKeys 按核验请求定位年卡：pass_id > qr_token > id_card（成员证件索引）
func (s *Service) findPassByVerifyKeys(ctx context.Context, passID, qrToken, idCard string) (*model.Pass, error) {
	if passID != "" {
		return s.getPass(ctx, passID)
	}
	if qrToken != "" {
		id, err := s.St.R.Get(ctx, keyIdxQR(qrToken)).Result()
		if err != nil {
			return nil, ErrNotFound
		}
		return s.getPass(ctx, id)
	}
	if idCard != "" {
		memberID, err := s.St.R.Get(ctx, keyIdxMember(idCard)).Result()
		if err != nil {
			return nil, ErrNotFound
		}
		var m model.Member
		if err := s.St.GetJSON(ctx, keyMember(memberID), &m); err != nil {
			return nil, ErrNotFound
		}
		return s.getPass(ctx, m.PassID)
	}
	return nil, ErrInvalidInput
}

var _ = store.ErrNotFound
