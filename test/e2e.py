#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""passgate 端到端验证脚本：覆盖购卡、核验决策、幂等并发、冻结/退款/升级、申诉、运营统计。

前置：服务已在 127.0.0.1:8080 运行（空库启动会自动写入种子数据）。
运行：python3 test/e2e.py
"""
import datetime
import json
import subprocess
import sys
import time
import urllib.request
import urllib.error
import uuid
from concurrent.futures import ThreadPoolExecutor

BASE = "http://127.0.0.1:8080"
PASS = 0
FAIL = 0


def check(name, cond, extra=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print(f"  ✓ {name}")
    else:
        FAIL += 1
        print(f"  ✗ {name}  {extra}")


def section(title):
    print(f"\n== {title} ==")


def req(method, path, body=None, headers=None):
    url = BASE + path
    data = None
    h = {"Content-Type": "application/json"}
    if headers:
        h.update(headers)
    if body is not None:
        data = json.dumps(body).encode()
    r = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(r, timeout=15) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode() or "{}")
        except Exception:
            return e.code, {}


def redis_cli(*args, stdin=None):
    cmd = ["redis-cli"] + list(args)
    res = subprocess.run(cmd, input=stdin, capture_output=True, text=True)
    return res.stdout.strip()


def redis_set_json(key, obj):
    redis_cli("-x", "SET", key, stdin=json.dumps(obj))


def redis_get_json(key):
    out = redis_cli("GET", key)
    return json.loads(out) if out else None


def local_date():
    return datetime.datetime.now().astimezone().strftime("%Y-%m-%d")


def local_ts(hour=10, minute=30, date=None):
    now = datetime.datetime.now().astimezone()
    tz = now.strftime("%z")
    tz = tz[:3] + ":" + tz[3:]
    d = date or now.strftime("%Y-%m-%d")
    return f"{d}T{hour:02d}:{minute:02d}:00{tz}"


_uid = 0


def new_idcard():
    global _uid
    _uid += 1
    return "ID" + uuid.uuid4().hex[:14] + str(_uid)


def buy_pass(ptype="single", members=None, expiry="2032-01-01", phone=None):
    body = {
        "type": ptype,
        "holder_name": "测试用户" + str(_uid),
        "holder_id_card": new_idcard(),
        "id_card_expiry": expiry,
        "phone": phone or ("139" + str(uuid.uuid4().int)[:8]),
        "photo_url": "https://img.example.com/p.jpg",
    }
    if members:
        body["members"] = members
    st, r = req("POST", "/api/passes", body)
    assert st == 200, f"buy_pass failed: {r}"
    return r["data"]


def verify(pass_id=None, qr=None, idcard=None, device="dev-east-01", attraction="attr-lake",
           face=90.0, companions=None, req_id=None, ts=None, photo_clear=None, offline_sync=False):
    body = {
        "request_id": req_id or ("REQ-" + uuid.uuid4().hex[:12]),
        "device_id": device,
        "attraction": attraction,
        "face_score": face,
        "offline_sync": offline_sync,
    }
    if pass_id:
        body["pass_id"] = pass_id
    if qr:
        body["qr_token"] = qr
    if idcard:
        body["id_card"] = idcard
    if companions:
        body["companions"] = companions
    if ts:
        body["timestamp"] = ts
    if photo_clear is not None:
        body["photo_clear"] = photo_clear
    st, r = req("POST", "/api/verify", body)
    return st, r


def remaining(pass_id):
    st, r = req("GET", f"/api/passes/{pass_id}")
    return r["data"]["remaining_quota"]


# ============================================================
section("A. 健康检查与种子数据")
st, r = req("GET", "/healthz")
check("健康检查", st == 200 and r.get("status") == "ok")
st, r = req("GET", "/api/devices")
check("种子设备 4 台且在线", st == 200 and len(r["data"]) == 4 and all(d["online"] for d in r["data"]))
st, r = req("GET", "/api/admin/sales-rules")
check("售卖规则 3 类", st == 200 and len(r["data"]) == 3)
st, r = req("GET", "/api/holidays")
check("2026 节假日已配置", st == 200 and "2026-10-01" in r["data"])
# 若测试当天恰为配置中的节假日（如在 2026-10-01 当天跑测试），临时移除当日配置，
# 使依赖“今天可正常入园”的用例与运行日期无关；节假日专项用例改用另一个节假日日期。
_today = local_date()
if _today in r["data"]:
    _adjusted = [d for d in r["data"] if d != _today]
    st2, _ = req("PUT", "/api/holidays", {"dates": _adjusted, "operator": "test-runner"})
    check("测试当天为节假日：临时移除当日节假日配置", st2 == 200)

def a_holiday_date():
    st, h = req("GET", "/api/holidays")
    for d in h["data"]:
        if d != local_date():
            return d
    return "2026-01-01"

section("B. 购卡与售卖规则")
p1 = buy_pass("single")
check("购单人卡成功", p1["id"].startswith("P") and p1["remaining_quota"] == 100 and p1["qr_token"].startswith("QR"))
st, r = req("POST", "/api/passes", {
    "type": "single", "holder_name": "重复", "holder_id_card": p1["holder_id_card"],
    "id_card_expiry": "2032-01-01", "phone": "13911112222"})
check("一证一卡：同证件重复购卡被拒", st == 409, r)
# 停售后禁售
st, rules = req("GET", "/api/admin/sales-rules")
rule_single = [x for x in rules["data"] if x["pass_type"] == "single"][0]
rule_off = dict(rule_single, on_sale=False)
st, r = req("PUT", "/api/admin/sales-rules/single", rule_off, {"X-Operator": "ops-li"})
check("运营停售单人卡", st == 200 and r["data"]["on_sale"] is False)
st, r = req("POST", "/api/passes", {
    "type": "single", "holder_name": "买不到", "holder_id_card": new_idcard(),
    "id_card_expiry": "2032-01-01", "phone": "13933334444"})
check("停售后购卡被拒", st == 409, r)
st, r = req("PUT", "/api/admin/sales-rules/single", rule_single, {"X-Operator": "ops-li"})
check("恢复售卖", st == 200 and r["data"]["on_sale"] is True)

section("C. 核验放行 / 幂等 / 并发去重")
pc = buy_pass("single")
st, r = verify(pass_id=pc["id"], req_id="C-0001", ts=local_ts(10, 30))
check("正常放行", st == 200 and r["data"]["decision"] == "ALLOW" and r["data"]["charged_quota"] == 1, r)
v_id = r["data"]["verification_id"]
st, r2 = verify(pass_id=pc["id"], req_id="C-0001", ts=local_ts(10, 30))
check("同 request_id 重试返回首次结果不重复扣", r2["data"]["verification_id"] == v_id and remaining(pc["id"]) == 99, r2)
st, r3 = verify(pass_id=pc["id"], req_id="C-0002", ts=local_ts(10, 31))
check("抖动重刷(不同request_id)按重复入园处理", r3["data"]["decision"] == "ALLOW" and r3["data"]["reason"] == "DUPLICATE_ENTRY"
      and r3["data"]["charged_quota"] == 0 and remaining(pc["id"]) == 99, r3)

# 并发：20 个相同 request_id
pc2 = buy_pass("single")
def concurrent_same(_):
    return verify(pass_id=pc2["id"], req_id="C-CONC", ts=local_ts(10, 30))
with ThreadPoolExecutor(max_workers=20) as ex:
    results = list(ex.map(concurrent_same, range(20)))
charged = sum(x[1]["data"]["charged_quota"] for x in results if x[0] == 200)
check("20 并发同 request_id 仅扣 1 次", charged == 1 and remaining(pc2["id"]) == 99,
      f"charged_sum={charged} remaining={remaining(pc2['id'])}")

# 并发：20 个不同 request_id 同景点（模拟闸机重试风暴）
pc3 = buy_pass("single")
def concurrent_diff(i):
    return verify(pass_id=pc3["id"], req_id=f"C-CONC-{i}", ts=local_ts(10, 30))
with ThreadPoolExecutor(max_workers=20) as ex:
    results = list(ex.map(concurrent_diff, range(20)))
charged = sum(x[1]["data"]["charged_quota"] for x in results if x[0] == 200)
check("20 并发不同 request_id 同景点仅扣 1 次(去重窗口)", charged == 1 and remaining(pc3["id"]) == 99,
      f"charged_sum={charged} remaining={remaining(pc3['id'])}")

# 二维码核验
fam = None
st, r = req("GET", "/api/passes/P000001")
if st == 200:
    fam = r["data"]
st, r = verify(qr=fam["qr_token"], face=88.0, ts=local_ts(10, 30))
check("二维码核验放行", st == 200 and r["data"]["decision"] == "ALLOW", r)
# 刷证件（成员证件定位年卡）；清掉高频计数避免误判，换景点避免去重窗口
redis_cli("DEL", "hf:P000001", "loc:P000001")
st, r = verify(idcard="110101199203054321", face=80.0, attraction="attr-garden", ts=local_ts(10, 32))
check("刷成员证件核验放行", st == 200 and r["data"]["decision"] == "ALLOW" and r["data"]["charged_quota"] == 1, r)

section("D. 核验失败原因覆盖")
st, r = verify(pass_id=pc["id"], device="dev-not-exist", ts=local_ts(10, 30))
check("未注册设备 DEVICE_UNKNOWN", r["data"]["reason"] == "DEVICE_UNKNOWN", r)

# 设备离线：注册新设备并把心跳改到 5 分钟前
st, r = req("POST", "/api/devices", {"id": "dev-offline-01", "name": "离线闸机", "gate_id": "gate-east", "attraction": "attr-lake"})
dev = redis_get_json("device:dev-offline-01")
dev["last_heartbeat"] = (datetime.datetime.now() - datetime.timedelta(minutes=5)).astimezone().isoformat()
redis_set_json("device:dev-offline-01", dev)
st, r = verify(pass_id=pc["id"], device="dev-offline-01", ts=local_ts(10, 30))
check("设备离线 DEVICE_OFFLINE", r["data"]["decision"] == "MANUAL_REVIEW" and r["data"]["reason"] == "DEVICE_OFFLINE", r)
# 离线补传用全新卡，避免去重窗口/高频计数干扰
pc_off = buy_pass("single")
st, r = verify(pass_id=pc_off["id"], device="dev-offline-01", ts=local_ts(10, 30), offline_sync=True, req_id="C-OFFLINE-SYNC")
check("离线补传(offline_sync)正常放行", r["data"]["decision"] == "ALLOW" and r["data"]["charged_quota"] == 1, r)
st, r = verify(pass_id=pc_off["id"], device="dev-offline-01", ts=local_ts(10, 30), offline_sync=True, req_id="C-OFFLINE-SYNC")
check("离线补传重复提交不重复扣次", remaining(pc_off["id"]) == 99, r)

st, r = verify(pass_id="P999999", ts=local_ts(10, 30))
check("年卡不存在 PASS_NOT_FOUND", r["data"]["reason"] == "PASS_NOT_FOUND", r)

pfrozen = buy_pass("single")
req("POST", f"/api/passes/{pfrozen['id']}/freeze", {"reason": "失主挂失", "operator": "cs-wang"})
st, r = verify(pass_id=pfrozen["id"], ts=local_ts(10, 30))
check("冻结卡 PASS_FROZEN", r["data"]["decision"] == "MANUAL_REVIEW" and r["data"]["reason"] == "PASS_FROZEN", r)

pexp = buy_pass("single")
pj = redis_get_json(f"pass:{pexp['id']}")
pj["valid_until"] = "2020-01-01"
redis_set_json(f"pass:{pexp['id']}", pj)
st, r = verify(pass_id=pexp["id"], ts=local_ts(10, 30))
check("年卡过期 PASS_EXPIRED", r["data"]["decision"] == "QUOTA_INSUFFICIENT" and r["data"]["reason"] == "PASS_EXPIRED", r)

st, r = req("GET", "/api/passes/P000003")  # 亲子卡不含 attr-summit
pcard = r["data"]
st, r = verify(pass_id=pcard["id"], attraction="attr-summit", ts=local_ts(10, 30))
check("景点不适用 ATTRACTION_NOT_ALLOWED", r["data"]["reason"] == "ATTRACTION_NOT_ALLOWED", r)

psingle = buy_pass("single")  # 单人卡节假日不可用
st, r = verify(pass_id=psingle["id"], ts=local_ts(10, 30, date=a_holiday_date()))
check("节假日限制 HOLIDAY_BLOCKED", r["data"]["reason"] == "HOLIDAY_BLOCKED", r)
st, r = verify(pass_id=psingle["id"], ts=local_ts(23, 30))
check("时段限制 TIMESLOT_BLOCKED", r["data"]["reason"] == "TIMESLOT_BLOCKED", r)

st, r = verify(pass_id=fam["id"], companions=["ID_NOT_BOUND_1"], ts=local_ts(10, 33))
check("同行人未绑定 COMPANION_NOT_BOUND", r["data"]["reason"] == "COMPANION_NOT_BOUND", r)
st, r = verify(pass_id=pcard["id"], companions=["440101201909096666", "ID_X"], ts=local_ts(10, 30))
check("同行超限 COMPANION_EXCEEDED", r["data"]["reason"] == "COMPANION_EXCEEDED", r)

pphoto = buy_pass("single")
st, r = verify(pass_id=pphoto["id"], photo_clear=False, ts=local_ts(10, 30))
check("照片不清 PHOTO_UNCLEAR", r["data"]["reason"] == "PHOTO_UNCLEAR", r)
st, r = verify(pass_id=pphoto["id"], face=30.0, ts=local_ts(10, 30))
check("人脸低分 FACE_MISMATCH", r["data"]["reason"] == "FACE_MISMATCH", r)

pold = buy_pass("single", expiry="2020-06-01")
st, r = verify(pass_id=pold["id"], ts=local_ts(10, 30))
check("证件过期 ID_EXPIRED", r["data"]["reason"] == "ID_EXPIRED", r)

phf = buy_pass("single")
redis_cli("SET", f"hf:{phf['id']}", "4")
st, r = verify(pass_id=phf["id"], ts=local_ts(10, 30))
check("高频异常 SUSPECTED_LENDING/HIGH_FREQUENCY",
      r["data"]["decision"] == "SUSPECTED_LENDING" and r["data"]["reason"] == "HIGH_FREQUENCY", r)

pml = buy_pass("family")
st, r = verify(pass_id=pml["id"], attraction="attr-lake", ts=local_ts(10, 30))
assert r["data"]["decision"] == "ALLOW", r
st, r = verify(pass_id=pml["id"], attraction="attr-garden", ts=local_ts(10, 35))
check("多地异常 SUSPECTED_LENDING/MULTI_LOCATION",
      r["data"]["decision"] == "SUSPECTED_LENDING" and r["data"]["reason"] == "MULTI_LOCATION", r)

pcap = buy_pass("family")
today = local_date()
redis_cli("SET", f"cap:attr-garden:{today}", "3000")  # 百花园容量 3000
st, r = verify(pass_id=pcap["id"], attraction="attr-garden", ts=local_ts(10, 30))
check("容量超限 CAPACITY_FULL", r["data"]["reason"] == "CAPACITY_FULL", r)
redis_cli("DEL", f"cap:attr-garden:{today}")

pquota = buy_pass("single")
redis_cli("SET", f"pass:{pquota['id']}:remaining", "0")
st, r = verify(pass_id=pquota["id"], ts=local_ts(10, 30))
check("权益耗尽 QUOTA_EXHAUSTED", r["data"]["decision"] == "QUOTA_INSUFFICIENT" and r["data"]["reason"] == "QUOTA_EXHAUSTED", r)

section("E. 人工放行与客服可解释")
pm = buy_pass("single")
st, r = verify(pass_id=pm["id"], photo_clear=False, ts=local_ts(10, 30))
vid = r["data"]["verification_id"]
check("制造人工核验(PHOTO_UNCLEAR)", r["data"]["decision"] == "MANUAL_REVIEW", r)
st, r = req("POST", f"/api/verifications/{vid}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "现场比对身份证照片一致，人工放行"})
check("人工放行成功并扣减额度", st == 200 and r["data"]["charged_quota"] == 1 and remaining(pm["id"]) == 99, r)
st, r = req("POST", f"/api/verifications/{vid}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "重复处理"})
check("重复人工处理被拒", st == 409, r)
st, r = req("GET", f"/api/verifications/{vid}")
check("客服可查完整链路(原因+人工处理记录)",
      r["data"]["manual_action"] == "RELEASED" and r["manual_review"]["operator"] == "staff-chen"
      and r["manual_review"]["note"].startswith("现场比对"), r)
st, r = req("GET", f"/api/passes/{pm['id']}/gate-logs")
check("闸机日志含 MANUAL_RELEASE", any(g["action"] == "MANUAL_RELEASE" for g in r["data"]), r)

section("F. 冻结/解冻与预约一致性")
pr = buy_pass("single")
st, r = req("POST", "/api/reservations", {"pass_id": pr["id"], "attraction": "attr-lake",
                                          "date": local_date(), "slot": "10:00-12:00"})
rsv_id = r["data"]["id"]
check("预约成功", st == 200 and r["data"]["status"] == "BOOKED", r)
req("POST", f"/api/passes/{pr['id']}/freeze", {"reason": "涉嫌转借核查", "operator": "cs-wang"})
st, r = req("GET", f"/api/passes/{pr['id']}/reservations")
check("冻结不影响已预约未入园订单", r["data"][0]["status"] == "BOOKED", r)
st, r = req("POST", "/api/reservations", {"pass_id": pr["id"], "attraction": "attr-lake",
                                          "date": local_date(), "slot": "14:00-16:00"})
check("冻结期间不可新预约", st == 409, r)
req("POST", f"/api/passes/{pr['id']}/unfreeze", {"operator": "cs-wang"})
st, r = verify(pass_id=pr["id"], ts=local_ts(10, 30))
check("解冻后正常放行", r["data"]["decision"] == "ALLOW", r)
st, r = req("GET", f"/api/passes/{pr['id']}/reservations")
check("入园后预约核销为 USED", r["data"][0]["status"] == "USED", r)

section("G. 申诉与权益返还")
pa = buy_pass("single")
st, r = verify(pass_id=pa["id"], req_id="G-0001", ts=local_ts(10, 30))
vid = r["data"]["verification_id"]
check("扣次成功(剩99)", remaining(pa["id"]) == 99)
st, r = req("POST", "/api/appeals", {"pass_id": pa["id"], "verification_id": vid,
                                     "type": "WRONG_DEDUCTION", "description": "闸机离线重复扣次，申请返还"})
aid = r["data"]["id"]
check("申诉创建", st == 200 and r["data"]["status"] == "PENDING", r)
st, r = req("POST", f"/api/appeals/{aid}/resolve",
            {"approve": True, "resolution": "核实为闸机抖动误扣，返还 1 人次", "handler": "cs-wang"})
check("申诉通过自动返还额度", st == 200 and r["data"]["compensate_quota"] == 1 and remaining(pa["id"]) == 100, r)
st, r = req("POST", f"/api/appeals/{aid}/resolve",
            {"approve": True, "resolution": "重复处理", "handler": "cs-wang"})
check("申诉重复处理被拒", st == 409, r)

section("H. 退款与升级重算")
pref = buy_pass("single")
req("POST", "/api/reservations", {"pass_id": pref["id"], "attraction": "attr-lake",
                                  "date": local_date(), "slot": "10:00-12:00"})
st, r = req("POST", f"/api/passes/{pref['id']}/refund", {"operator": "cs-wang"})
check("退款：状态+退款金额", st == 200 and r["data"]["status"] == "refunded" and r["data"]["refund_amount"] > 0, r)
check("退款后权益清零", remaining(pref["id"]) == 0)
st, r = req("GET", f"/api/passes/{pref['id']}/reservations")
check("退款后未使用预约取消", r["data"][0]["status"] == "CANCELLED", r)
st, r = verify(pass_id=pref["id"], ts=local_ts(10, 30))
check("退款卡核验 PASS_REFUNDED", r["data"]["reason"] == "PASS_REFUNDED", r)

pup = buy_pass("single")
# 先把有效期改短（模拟已用 60 天、只剩 100 天的旧卡），升级后应重算为完整新周期
pj = redis_get_json(f"pass:{pup['id']}")
d60 = (datetime.datetime.now() - datetime.timedelta(days=60)).strftime("%Y-%m-%d")
d100 = (datetime.datetime.now() + datetime.timedelta(days=100)).strftime("%Y-%m-%d")
pj["valid_from"] = d60
pj["valid_until"] = d100
redis_set_json(f"pass:{pup['id']}", pj)
expect_until = (datetime.datetime.now() + datetime.timedelta(days=365)).strftime("%Y-%m-%d")
st, r = req("POST", f"/api/passes/{pup['id']}/upgrade", {"to_type": "family", "operator": "cs-wang"})
check("升级：类型变更+补差价", st == 200 and r["data"]["type"] == "family" and r["price_diff"] >= 0, r)
check("升级：有效期重算为新卡完整周期", r["data"]["valid_from"] == local_date() and r["data"]["valid_until"] == expect_until, r["data"])
check("升级：额度按新卡重置", r["data"]["remaining_quota"] == 400, r["data"])
st, r2 = verify(pass_id=pup["id"], attraction="attr-summit", ts=local_ts(10, 30))
check("升级后可进家庭卡景点", r2["data"]["decision"] == "ALLOW", r2)

section("I. 信息变更")
ph = buy_pass("single", phone="13700001111")
st, r = req("POST", f"/api/passes/{ph['id']}/phone",
            {"old_phone": "13700001111", "new_phone": "13700009999", "code": "000000"})
check("换手机号：错误验证码被拒", st == 403, r)
st, r = req("POST", f"/api/passes/{ph['id']}/phone",
            {"old_phone": "13700001111", "new_phone": "13700009999", "code": "123456"})
check("换手机号成功", st == 200 and r["data"]["phone"] == "13700009999", r)
st, r = req("GET", "/api/passes?phone=13700009999")
check("新手机号可查到卡", any(x["id"] == ph["id"] for x in r["data"]), r)
st, r = req("GET", "/api/passes?phone=13700001111")
check("旧手机号查不到卡", len(r["data"]) == 0, r)

pmem = buy_pass("parent_child", members=[{"name": "孩子", "id_card": new_idcard(), "relation": "child"}])
st, r = req("POST", f"/api/passes/{pmem['id']}/members",
            {"name": "外婆", "id_card": new_idcard(), "relation": "parent", "operator": "staff-chen"})
check("添加成员成功(消耗更换次数)", st == 200, r)
st, r = req("POST", f"/api/passes/{pmem['id']}/members",
            {"name": "外公", "id_card": new_idcard(), "relation": "parent", "operator": "staff-chen"})
check("更换次数用尽被拒", st == 409, r)
st, r = req("GET", f"/api/passes/{pmem['id']}")
mid = [m for m in r["data"]["members"] if m["name"] == "外婆"][0]["id"]
st, r = req("DELETE", f"/api/passes/{pmem['id']}/members/{mid}")
check("移除成员", st == 200, r)
st, r = req("GET", f"/api/passes/{pmem['id']}")
child = [m for m in r["data"]["members"] if m["role"] == "companion"][0]
st, r = req("POST", f"/api/passes/{pmem['id']}/members/{child['id']}/photo",
            {"photo_url": "https://img.example.com/new-photo.jpg", "operator": "staff-chen"})
check("孩子长大更新照片(版本+1)", st == 200 and r["data"]["photo_version"] == 2, r)

pq = buy_pass("single")
old_qr = pq["qr_token"]
st, r = req("POST", f"/api/passes/{pq['id']}/qr-refresh", {"operator": "cs-wang"})
new_qr = r["data"]["qr_token"]
st, r = verify(qr=old_qr, ts=local_ts(10, 30))
check("旧二维码失效", r["data"]["reason"] == "PASS_NOT_FOUND", r)
st, r = verify(qr=new_qr, ts=local_ts(10, 30))
check("新二维码放行", r["data"]["decision"] == "ALLOW", r)

section("J. 老人免刷脸（人工确认放行）")
# 种子家庭卡成员 张父(P000001-M3) 为免刷脸；换 attr-summit 避开去重窗口
st, r = verify(idcard="110101195501011239", face=-1.0, attraction="attr-summit", ts=local_ts(10, 40))
check("免刷脸成员转人工 FACE_EXEMPT_REVIEW",
      r["data"]["decision"] == "MANUAL_REVIEW" and r["data"]["reason"] == "FACE_EXEMPT_REVIEW", r)
vid = r["data"]["verification_id"]
st, r = req("POST", f"/api/verifications/{vid}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "老人证件人证一致，人工放行"})
check("人工确认放行老人", st == 200 and r["data"]["charged_quota"] == 1, r)
# 工作人员设置/取消免刷脸
st, r = req("GET", "/api/passes/P000001")
zhangfu = [m for m in r["data"]["members"] if m["name"] == "张父"][0]
st, r = req("POST", f"/api/passes/P000001/members/{zhangfu['id']}/face-exempt",
            {"exempt": False, "operator": "staff-chen"})
check("取消免刷脸标记", st == 200 and r["data"]["face_exempt"] is False, r)
req("POST", f"/api/passes/P000001/members/{zhangfu['id']}/face-exempt",
    {"exempt": True, "operator": "staff-chen"})

section("K. 出园与容量")
pk = buy_pass("family")
st, r = verify(pass_id=pk["id"], attraction="attr-lake", ts=local_ts(11, 0))
assert r["data"]["decision"] == "ALLOW", r
st, r = req("GET", "/api/attractions/attr-lake/capacity")
cap_after_enter = r["data"]["in_park"]
st, r = req("POST", "/api/exit", {"pass_id": pk["id"], "attraction": "attr-lake", "count": 1, "device_id": "dev-east-01"})
st, r = req("GET", "/api/attractions/attr-lake/capacity")
check("出园释放在园容量", r["data"]["in_park"] == cap_after_enter - 1, r)

section("L. 运营统计与日志")
st, r = req("GET", "/api/admin/stats/overview")
d = r["data"]
check("总览：按卡类型统计", st == 200 and len(d["by_type"]) == 3 and d["entries_total"] > 0, d)
check("总览：异常原因分布", "PHOTO_UNCLEAR" in d["anomaly_by_reason"] or len(d["anomaly_by_reason"]) > 0, d)
check("总览：人工放行计数", d["manual_releases"] >= 2, d)
st, r = req("GET", "/api/admin/stats/peaks")
check("峰值：24 小时分布", st == 200 and len(r["data"]["by_hour"]) == 24, r)
st, r = req("GET", "/api/admin/gate-logs?limit=50")
check("闸机日志查询", st == 200 and len(r["data"]) > 0, r)
st, r = req("GET", f"/api/passes/{pm['id']}/changes")
check("变更日志留痕", st == 200 and any(c["action"] == "CREATE" for c in r["data"]), r)
st, r = req("GET", "/api/appeals?status=APPROVED")
check("申诉列表查询", st == 200 and len(r["data"]) >= 1, r)

section("M. 转借高频异常冻结（自动冻结/解冻申请/在园不驱离/确认转借）")

def face_fail(pass_id, device, attraction, tag):
    return verify(pass_id=pass_id, device=device, attraction=attraction, face=20.0, req_id=f"RISK-{tag}")

# M1. 一周内 3 个入口 3 次人脸不匹配 → 自动冻结；预约转待核验；申诉计入证据
prisk = buy_pass("single")
st, r = req("POST", "/api/reservations", {"pass_id": prisk["id"], "attraction": "attr-lake",
                                          "date": local_date(), "slot": "10:00-12:00"})
check("风控场景：预先预约成功", st == 200 and r["data"]["status"] == "BOOKED", r)
st, r = req("POST", "/api/appeals", {"pass_id": prisk["id"], "type": "FREEZE_DISPUTE",
                                     "description": "本人脸受伤近期容貌变化，不是转借"})
check("风控场景：冻结前持卡人申诉", st == 200, r)
st, r = face_fail(prisk["id"], "dev-east-01", "attr-lake", "M1-1")
check("不匹配第1次：不冻结", r["data"]["reason"] == "FACE_MISMATCH" and "auto_freeze_case_id" not in r["data"], r)
st, r = face_fail(prisk["id"], "dev-west-01", "attr-summit", "M1-2")
check("不匹配第2次：仍不冻结", r["data"]["reason"] == "FACE_MISMATCH" and "auto_freeze_case_id" not in r["data"], r)
st, r = face_fail(prisk["id"], "dev-south-01", "attr-garden", "M1-3")
case_id = r["data"].get("auto_freeze_case_id")
check("不匹配第3次(跨3入口)：触发自动冻结", r["data"]["reason"] == "FACE_MISMATCH" and case_id, r)
st, r = req("GET", f"/api/passes/{prisk['id']}")
pv = r["data"]
check("卡状态=risk_auto 冻结", pv["status"] == "frozen" and pv["freeze_source"] == "risk_auto"
      and pv["freeze_case_id"] == case_id, r["data"])
check("待核验预约数=1", pv["pending_verification_count"] == 1, r["data"])
st, r = req("GET", f"/api/passes/{prisk['id']}/reservations")
check("已预约订单转待核验 PENDING_VERIFICATION",
      r["data"][0]["attraction"] == "attr-lake" and r["data"][0]["status"] == "PENDING_VERIFICATION", r)

# M2. 案件证据与客服解释材料
st, r = req("GET", f"/api/risk/freeze-cases/{case_id}")
fc, evs, apps, expl = r["data"], r["face_risk_events"], r["unfreeze_applications"], r["explanation"]
check("案件：3 次不匹配/3 个入口/1 次申诉",
      fc["status"] == "FROZEN" and fc["fail_count"] == 3 and fc["gate_count"] == 3
      and fc["appeals"] == 1 and fc["holder_in_park"] is False and len(evs) == 3, r)
check("案件：客服解释材料与现场话术(不在园版本)",
      "人脸不匹配" in expl["evidence"] and "解冻" in expl["customer_say"]
      and "本次无法入园" in expl["onsite_say"] and "不会要求您离开" not in expl["onsite_say"], r)
st, r = req("GET", "/api/risk/freeze-cases?status=FROZEN")
check("冻结案件列表可按状态过滤", any(c["id"] == case_id for c in r["data"]), r)
st, r = req("GET", f"/api/passes/{prisk['id']}/changes")
check("变更日志留痕 RISK_FREEZE", any(c["action"] == "RISK_FREEZE" for c in r["data"]), r)

# M3. 冻结只限制新的入园动作，人工放行也被阻断
st, r = verify(pass_id=prisk["id"], device="dev-east-01", attraction="attr-lake",
               face=95.0, req_id="RISK-M1-BLOCKED")
blocked_vid = r["data"]["verification_id"]
check("冻结卡刷闸 PASS_FROZEN",
      r["data"]["decision"] == "MANUAL_REVIEW" and r["data"]["reason"] == "PASS_FROZEN"
      and "在园区内" not in r["data"]["detail"], r)
st, r = req("POST", f"/api/verifications/{blocked_vid}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "想强行放行"})
check("冻结卡禁止人工放行新入园", st == 409, r)
st, r = req("POST", "/api/reservations", {"pass_id": prisk["id"], "attraction": "attr-summit",
                                          "date": local_date(), "slot": "14:00-16:00"})
check("冻结期间不可新预约", st == 409, r)

# M4. 解冻申请：缺材料 400；重复待审 409；驳回后可再次申请
st, r = req("POST", f"/api/passes/{prisk['id']}/unfreeze-application",
            {"id_doc_url": "", "recent_photo": "", "statement": "材料忘带"})
check("解冻申请缺证件/照片被拒", st == 400, r)
st, r = req("POST", f"/api/passes/{prisk['id']}/unfreeze-application",
            {"id_doc_url": "https://img.example.com/id.jpg",
             "recent_photo": "https://img.example.com/recent.jpg", "statement": "是本人，近期受伤消肿后变样"})
app_id = r["data"]["id"]
check("解冻申请提交成功(PENDING)", st == 200 and r["data"]["status"] == "PENDING", r)
st, r = req("POST", f"/api/passes/{prisk['id']}/unfreeze-application",
            {"id_doc_url": "https://img.example.com/id.jpg", "recent_photo": "https://img.example.com/recent.jpg"})
check("同一案件重复待审申请被拒", st == 409, r)
st, r = req("GET", "/api/unfreeze-applications?status=PENDING")
check("解冻申请列表", any(a["id"] == app_id for a in r["data"]), r)
st, r = req("POST", f"/api/unfreeze-applications/{app_id}/review",
            {"approve": False, "note": "照片模糊无法比对，请重新提交", "handler": "cs-wang"})
check("解冻申请驳回", st == 200 and r["data"]["status"] == "REJECTED", r)
st, r = req("GET", f"/api/passes/{prisk['id']}")
check("驳回后卡仍冻结", r["data"]["status"] == "frozen", r)
st, r = req("POST", f"/api/passes/{prisk['id']}/unfreeze-application",
            {"id_doc_url": "https://img.example.com/id2.jpg",
             "recent_photo": "https://img.example.com/recent2.jpg", "statement": "重新拍了清晰照片"})
app_id2 = r["data"]["id"]
check("驳回后可再次提交申请", st == 200 and r["data"]["status"] == "PENDING", r)
st, r = req("POST", f"/api/unfreeze-applications/{app_id2}/review",
            {"approve": True, "note": "证件与近期照片比对一致，确为本人，解冻", "handler": "cs-wang"})
check("解冻申请通过：卡恢复 active", st == 200 and r["pass"]["status"] == "active", r)
st, r = req("GET", f"/api/risk/freeze-cases/{case_id}")
check("案件结案 UNFROZEN", r["data"]["status"] == "UNFROZEN" and r["data"]["resolver"] == "cs-wang", r)
st, r = verify(pass_id=prisk["id"], device="dev-east-01", attraction="attr-lake",
               face=95.0, req_id="RISK-M1-UNFROZEN")
check("解冻后正常放行", r["data"]["decision"] == "ALLOW" and r["data"]["charged_quota"] == 1, r)
st, r = req("GET", f"/api/passes/{prisk['id']}/reservations")
check("解冻入园后待核验预约补核销为 USED",
      any(x["attraction"] == "attr-lake" and x["status"] == "USED" for x in r["data"]), r)

# M5. 在园持卡人触发冻结：不追溯驱离，现场话术不同
pinpark = buy_pass("single")
st, r = verify(pass_id=pinpark["id"], device="dev-east-01", attraction="attr-lake",
               face=95.0, req_id="RISK-M5-IN")
check("在园场景：先正常入园", r["data"]["decision"] == "ALLOW", r)
st, r = req("GET", f"/api/passes/{pinpark['id']}")
check("在园标记 in_park=true", r["data"]["in_park"] is True, r)
face_fail(pinpark["id"], "dev-east-01", "attr-lake", "M5-1")
face_fail(pinpark["id"], "dev-west-01", "attr-summit", "M5-2")
st, r = face_fail(pinpark["id"], "dev-south-01", "attr-garden", "M5-3")
inpark_case = r["data"].get("auto_freeze_case_id")
check("在园期间触发自动冻结", bool(inpark_case), r)
st, r = req("GET", f"/api/risk/freeze-cases/{inpark_case}")
check("案件记录冻结时在园 holder_in_park=true", r["data"]["holder_in_park"] is True, r)
check("现场话术：已在园不驱离+限制新入园",
      "不会要求您离开" in r["explanation"]["onsite_say"] and "新的入园" in r["explanation"]["onsite_say"], r)
st, r = verify(pass_id=pinpark["id"], device="dev-west-01", attraction="attr-summit",
               face=95.0, req_id="RISK-M5-BLOCKED")
check("冻结后新入园被拦且提示在园不驱离",
      r["data"]["reason"] == "PASS_FROZEN" and "不追溯驱离" in r["data"]["detail"], r)
st, r = req("POST", "/api/exit", {"pass_id": pinpark["id"], "attraction": "attr-lake",
                                  "count": 1, "device_id": "dev-east-01"})
st, r = req("GET", f"/api/passes/{pinpark['id']}")
check("出园后在园标记清除", st == 200 and r["data"]["in_park"] is False, r)

# M6. 人工放行记录计入冻结证据
pmanual = buy_pass("single")
st, r = req("POST", "/api/reservations", {"pass_id": pmanual["id"], "attraction": "attr-garden",
                                          "date": local_date(), "slot": "14:00-16:00"})
check("证据场景：百花园预约成功", st == 200 and r["data"]["status"] == "BOOKED", r)
st, r = face_fail(pmanual["id"], "dev-east-01", "attr-lake", "M6-1")
vid = r["data"]["verification_id"]
st, r = req("POST", f"/api/verifications/{vid}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "称卡主本人，人工放行"})
check("证据场景：第1次不匹配人工放行", st == 200 and r["data"]["charged_quota"] == 1, r)
st, r = face_fail(pmanual["id"], "dev-west-01", "attr-summit", "M6-2")
vid = r["data"]["verification_id"]
req("POST", f"/api/verifications/{vid}/manual-review",
    {"action": "RELEASE", "operator": "staff-chen", "note": "又解释为本人，放行"})
st, r = face_fail(pmanual["id"], "dev-south-01", "attr-garden", "M6-3")
manual_case = r["data"].get("auto_freeze_case_id")
st, cr = req("GET", f"/api/risk/freeze-cases/{manual_case}")
check("案件含 2 次人工放行记录", st == 200 and cr["data"]["manual_releases"] == 2, cr)

# M7. 确认转借 → 受限：每日 1 次本人入园、不可同行、不可预约、待核验预约取消
st, r = req("POST", f"/api/risk/freeze-cases/{manual_case}/confirm-lending", {"note": ""})
check("确认转借缺少说明被拒", st == 400, r)
# 清掉 M6 人工放行写入的 90 秒放行去重标记，避免干扰限制期每日次数判断
_dkeys = redis_cli("--scan", "--pattern", f"dedup:{pmanual['id']}:*").split()
if _dkeys:
    redis_cli("DEL", *_dkeys)
st, r = req("POST", f"/api/risk/freeze-cases/{manual_case}/confirm-lending",
            {"note": "现场承认将卡借予同事使用，证据确凿", "handler": "cs-wang"})
check("确认转借：卡转 restricted", st == 200 and r["data"]["status"] == "restricted"
      and r["data"]["restricted_daily_quota"] == 1, r)
st, r = req("GET", f"/api/passes/{pmanual['id']}/reservations")
check("确认转借：待核验预约被取消",
      any(x["attraction"] == "attr-garden" and x["status"] == "CANCELLED" for x in r["data"]), r)
st, r = req("GET", f"/api/risk/freeze-cases/{manual_case}")
check("案件状态 LENDING_CONFIRMED", r["data"]["status"] == "LENDING_CONFIRMED", r)
st, r = req("POST", "/api/reservations", {"pass_id": pmanual["id"], "attraction": "attr-lake",
                                          "date": local_date(), "slot": "10:00-12:00"})
check("受限卡不可新预约", st == 409, r)
st, r = verify(pass_id=pmanual["id"], device="dev-east-01", attraction="attr-lake",
               face=95.0, companions=["ID_NOT_BOUND_1"], req_id="RISK-M7-COMP")
check("受限卡携同行被拦 PASS_RESTRICTED", r["data"]["reason"] == "PASS_RESTRICTED", r)
st, r = verify(pass_id=pmanual["id"], device="dev-south-01", attraction="attr-garden",
               face=95.0, req_id="RISK-M7-OK1")
check("受限卡本人当日首次入园放行", r["data"]["decision"] == "ALLOW" and r["data"]["charged_quota"] == 1, r)
st, r = verify(pass_id=pmanual["id"], device="dev-west-01", attraction="attr-summit",
               face=95.0, req_id="RISK-M7-OK2")
check("受限卡当日第二次入园被拦 PASS_RESTRICTED",
      r["data"]["decision"] == "MANUAL_REVIEW" and r["data"]["reason"] == "PASS_RESTRICTED", r)
st, r = req("POST", f"/api/verifications/{r['data']['verification_id']}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "想再放一次"})
check("受限卡超限人工放行被拒", st == 409, r)
st, r = req("GET", f"/api/passes/{pmanual['id']}/changes")
check("变更日志留痕 LENDING_CONFIRMED", any(c["action"] == "LENDING_CONFIRMED" for c in r["data"]), r)

# M8. 反例：次数不足 / 单一入口不触发冻结
pno = buy_pass("single")
face_fail(pno["id"], "dev-east-01", "attr-lake", "M8-1")
st, r = face_fail(pno["id"], "dev-west-01", "attr-summit", "M8-2")
check("反例：2 次不匹配不冻结", "auto_freeze_case_id" not in r["data"], r)
st, r = req("GET", f"/api/passes/{pno['id']}")
check("反例：卡仍 active", r["data"]["status"] == "active", r)
psame = buy_pass("single")
face_fail(psame["id"], "dev-east-01", "attr-lake", "M8-3")
face_fail(psame["id"], "dev-east-02", "attr-lake", "M8-4")
st, r = face_fail(psame["id"], "dev-east-01", "attr-lake", "M8-5")
check("反例：3 次不匹配但同一入口(同景点)不冻结", "auto_freeze_case_id" not in r["data"], r)
st, r = req("GET", f"/api/passes/{psame['id']}")
check("反例：卡仍 active(2)", r["data"]["status"] == "active", r)

# M9. 人工冻结（非风控）不允许提交解冻申请，走人工解冻
pman = buy_pass("single")
req("POST", f"/api/passes/{pman['id']}/freeze", {"reason": "失主挂失", "operator": "cs-wang"})
st, r = req("POST", f"/api/passes/{pman['id']}/unfreeze-application",
            {"id_doc_url": "https://img.example.com/id.jpg", "recent_photo": "https://img.example.com/r.jpg"})
check("人工冻结无风控案件，不走线上解冻申请", st == 409, r)
st, r = req("POST", f"/api/passes/{pman['id']}/unfreeze", {"operator": "cs-wang"})
check("人工解冻恢复", st == 200 and r["data"]["status"] == "active", r)

section("N. OpenAPI 3.x 文档与 Swagger UI")
import urllib.request as _u
spec = _u.urlopen(BASE + "/openapi.yaml", timeout=10).read().decode()
check("OpenAPI 规范可访问且版本 3.x、含风控接口",
      spec.startswith("openapi: 3.0") and "/api/risk/freeze-cases" in spec
      and "/api/passes/{id}/unfreeze-application" in spec)
page = _u.urlopen(BASE + "/static/swagger-ui/index.html", timeout=10).read().decode()
check("Swagger UI 页面本地化可访问", "swagger-ui-bundle.js" in page)
js = _u.urlopen(BASE + "/static/vendor/swagger-ui-bundle.js", timeout=10).status
check("Swagger UI 资源随服务提供", js == 200)
# /docs 会 302 重定向到 Swagger UI（禁止自动跟随以断言重定向）
import urllib.request as _u2
class _NoRedirect(_u2.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None
_opnr = _u2.build_opener(_NoRedirect())
try:
    _opnr.open(BASE + "/docs", timeout=10)
    _docs_code = 200
except urllib.error.HTTPError as _e:
    _docs_code = _e.code
check("/docs 重定向到 Swagger UI", _docs_code in (301, 302), _docs_code)

print(f"\n========================================")
print(f"RESULT: PASS={PASS} FAIL={FAIL}")
print(f"========================================")
sys.exit(1 if FAIL > 0 else 0)
