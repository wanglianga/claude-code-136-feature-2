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


# 风险事件按服务器时间统计近 7 天，统一使用运行当天（A 章节已将当天临时移出节假日集合）
RISK_DATE = datetime.datetime.now().astimezone().strftime("%Y-%m-%d")
RISK_NEXT = (datetime.datetime.now().astimezone() + datetime.timedelta(days=1)).strftime("%Y-%m-%d")


def face_fail(pass_id, device, attraction, ts_date=RISK_DATE, req_id=None):
    """制造一次人脸不匹配（显式使用非节假日日期，避免节假日规则提前拦截）"""
    return verify(pass_id=pass_id, device=device, attraction=attraction, face=10.0,
                  req_id=req_id, ts=local_ts(10, 30, date=ts_date))


def rverify(pass_id, attraction, ts_date=RISK_DATE, face=90.0, companions=None, device="dev-east-01"):
    """限制使用期核验：清掉高频/多地/去重窗口，避免既有计数干扰频次断言"""
    redis_cli("DEL", f"hf:{pass_id}", f"loc:{pass_id}", f"dedup:{pass_id}:{attraction}")
    return verify(pass_id=pass_id, device=device, attraction=attraction, face=face,
                  companions=companions, ts=local_ts(10, 30, date=ts_date))


# ============================================================
section("A. 健康检查与种子数据")
st, r = req("GET", "/healthz")
check("健康检查", st == 200 and r.get("status") == "ok")
# OpenAPI 3.x 规范与 Swagger UI（后端项目交付要求，静态资源内嵌可离线）
import urllib.request as _ureq
_spec = _ureq.urlopen(BASE + "/openapi.yaml", timeout=10).read().decode()
check("OpenAPI 3.x 规范可访问且含风控接口",
      _spec.startswith("openapi: 3.0") and "/api/freeze-cases" in _spec and "/api/unfreeze-requests" in _spec)
_html = _ureq.urlopen(BASE + "/docs", timeout=10).read().decode()
check("Swagger UI 页面可访问", "swagger-ui" in _html and "/openapi.yaml" in _html)
_css = _ureq.urlopen(BASE + "/docs/swagger-ui.css", timeout=10).status
_js = _ureq.urlopen(BASE + "/docs/swagger-ui-bundle.js", timeout=10).status
check("Swagger UI 静态资源内嵌可离线加载", _css == 200 and _js == 200)
st, r = req("GET", "/api/devices")
check("种子设备 4 台且在线", st == 200 and len(r["data"]) == 4 and all(d["online"] for d in r["data"]))
st, r = req("GET", "/api/admin/sales-rules")
check("售卖规则 3 类", st == 200 and len(r["data"]) == 3)
st, r = req("GET", "/api/holidays")
check("2026 节假日已配置", st == 200 and "2026-10-01" in r["data"])
# 测试健壮性：若运行当天恰逢节假日，临时将当天从节假日集合移除，保证用例在任意日期可运行
# （节假日专项断言改用固定日期 2026-10-02；run.sh 每次重置 Redis，种子数据不受影响）
_today = local_date()
_holidays = r["data"]
if _today in _holidays:
    _kept = [d for d in _holidays if d != _today]
    req("PUT", "/api/holidays", {"dates": _kept, "operator": "test-runner"})

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

psingle = buy_pass("single")  # 单人卡节假日不可用（固定节假日日期验证规则）
st, r = verify(pass_id=psingle["id"], ts=local_ts(10, 30, date="2026-10-02"))
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

# ============================================================
section("M. 转借高频异常冻结（专题）")

# ---- M1. 一周跨入口多次人脸不匹配：达到 4 次/3 入口前不冻结，达到后自动冻结 ----
pr1 = buy_pass("single")
st, r = face_fail(pr1["id"], "dev-east-01", "attr-lake", req_id="M1-01")
check("M1 首次人脸不匹配转人工", r["data"]["reason"] == "FACE_MISMATCH", r)
st, r = req("GET", f"/api/passes/{pr1['id']}/risk-snapshot")
sn = r["data"]["risk_snapshot"]
check("M1 风险画像记录失败1次/入口1个/未达自动冻结",
      sn["fail_count"] == 1 and sn["gate_count"] == 1 and r["data"]["meets_auto"] is False, r["data"])
face_fail(pr1["id"], "dev-west-01", "attr-summit", req_id="M1-02")
face_fail(pr1["id"], "dev-south-01", "attr-garden", req_id="M1-03")
st, r = req("GET", f"/api/passes/{pr1['id']}")
check("M1 3次不匹配跨2入口仍未冻结(自动门槛4次/3入口)", r["data"]["status"] == "active", r["data"])
st, r = face_fail(pr1["id"], "dev-east-02", "attr-lake", req_id="M1-04")
check("M1 第4次不匹配跨第3入口触发冻结，本次结论提示案件",
      "临时冻结" in r["data"]["detail"], r["data"])
st, r = req("GET", f"/api/passes/{pr1['id']}")
check("M1 卡状态 restricted 并关联案件",
      r["data"]["status"] == "restricted" and r["data"]["freeze_case_id"] != "", r["data"])
case_id = r["data"]["freeze_case_id"]
st, r = req("GET", f"/api/freeze-cases/{case_id}")
check("M1 案件记录失败4次/入口3个/自动触发",
      r["data"]["fail_count"] == 4 and r["data"]["gate_count"] == 3 and r["data"]["trigger"] == "AUTO"
      and r["data"]["status"] == "FROZEN", r["data"])
st, r = req("GET", f"/api/freeze-cases?pass_id={pr1['id']}")
check("M1 按卡查询冻结案件列表", st == 200 and any(c["id"] == case_id for c in r["data"]), r)

# ---- M2. 冻结后限制新入园动作；已预约订单转待核验，且不取消 ----
# 另一张卡：先预约再制造冻结
pr2 = buy_pass("single")
req("POST", "/api/reservations", {"pass_id": pr2["id"], "attraction": "attr-lake",
                                  "date": RISK_DATE, "slot": "10:00-12:00"})
for i, (dv, at) in enumerate([("dev-east-01", "attr-lake"), ("dev-west-01", "attr-summit"),
                              ("dev-south-01", "attr-garden"), ("dev-east-02", "attr-lake")]):
    face_fail(pr2["id"], dv, at, req_id=f"M2-{i+1:02d}")
st, r = req("GET", f"/api/passes/{pr2['id']}")
case2 = r["data"]["freeze_case_id"]
check("M2 第二张卡自动冻结", r["data"]["status"] == "restricted" and case2, r["data"])
st, r = req("GET", f"/api/passes/{pr2['id']}/reservations")
check("M2 已预约订单进入待核验(不取消)", r["data"][0]["status"] == "PENDING_VERIFY", r["data"])
st, r = rverify(pr2["id"], "attr-lake")
check("M2 冻结期间新入园被限制 LENDING_RESTRICTED",
      r["data"]["decision"] == "MANUAL_REVIEW" and r["data"]["reason"] == "LENDING_RESTRICTED", r)
st, r = req("POST", "/api/reservations", {"pass_id": pr2["id"], "attraction": "attr-lake",
                                          "date": RISK_NEXT, "slot": "10:00-12:00"})
check("M2 冻结期间不可新预约", st == 409, r)
# 冻结期间任何人工放行均拒绝
st, rr = rverify(pr2["id"], "attr-lake")
vid_restricted = rr["data"]["verification_id"]
st, r = req("POST", f"/api/verifications/{vid_restricted}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "试图放行冻结卡"})
check("M2 冻结期间人工放行被拒", st == 409, r)

# ---- M3. 在园不追溯驱离：冻结时持卡人已在园内 ----
pr3 = buy_pass("single")
st, r = rverify(pr3["id"], "attr-lake")
assert r["data"]["decision"] == "ALLOW", r
for i, (dv, at) in enumerate([("dev-east-01", "attr-lake"), ("dev-west-01", "attr-summit"),
                              ("dev-south-01", "attr-garden"), ("dev-east-02", "attr-lake")]):
    face_fail(pr3["id"], dv, at, req_id=f"M3-{i+1:02d}")
st, r = req("GET", f"/api/passes/{pr3['id']}")
case3 = r["data"]["freeze_case_id"]
check("M3 在园持卡人被冻结时标记 holder_in_park", r["data"]["holder_in_park"] is True, r["data"])
st, r = req("GET", f"/api/freeze-cases/{case3}/material")
mat = r["data"]
check("M3 现场话术含不驱离表述", "不会要求您离开" in mat["on_site_script"] or "不受影响" in mat["on_site_script"], mat)
check("M3 客服解释材料含证据链(4条)", len(mat["evidence"]) == 4 and mat["holder_name"], mat)
st, r = req("GET", f"/api/attractions/attr-lake/capacity?date={RISK_DATE}")
cap_inpark_day = r["data"]["in_park"]
st, r = rverify(pr3["id"], "attr-garden")
check("M3 在园期间仅限制新入园", r["data"]["reason"] == "LENDING_RESTRICTED", r)
# 出园仍被允许且释放容量
st, r = req("POST", "/api/exit", {"pass_id": pr3["id"], "attraction": "attr-lake", "count": 1, "device_id": "dev-east-01"})
check("M3 在园持卡人可正常出园", st == 200, r)

# ---- M4. 人工放行记录是加权信号：3 次不匹配含 2 次人工放行即冻结 ----
pr4 = buy_pass("single")
# 第1次：东门不匹配并人工放行
st, r = face_fail(pr4["id"], "dev-east-01", "attr-lake", req_id="M4-01")
v1 = r["data"]["verification_id"]
req("POST", f"/api/verifications/{v1}/manual-review",
    {"action": "RELEASE", "operator": "staff-chen", "note": "声称照片旧了，现场放行"})
# 第2次：西门不匹配并人工放行（无去重窗口冲突：不同景点）
st, r = face_fail(pr4["id"], "dev-west-01", "attr-summit", req_id="M4-02")
v2 = r["data"]["verification_id"]
req("POST", f"/api/verifications/{v2}/manual-review",
    {"action": "RELEASE", "operator": "staff-chen", "note": "同行人代刷，放行"})
st, r = req("GET", f"/api/passes/{pr4['id']}")
check("M4 2次不匹配2次人工放行仍 active(门槛3次)", r["data"]["status"] == "active", r["data"])
# 第3次：南门不匹配 → 3 次不匹配 + 2 次人工放行，自动冻结
st, r = face_fail(pr4["id"], "dev-south-01", "attr-garden", req_id="M4-03")
st, r = req("GET", f"/api/passes/{pr4['id']}")
check("M4 3次不匹配含2次人工放行触发自动冻结", r["data"]["status"] == "restricted", r["data"])
st, r = req("GET", f"/api/freeze-cases/{r['data']['freeze_case_id']}")
check("M4 案件记录人工放行2次", r["data"]["manual_count"] == 2, r["data"])

# ---- M5. 持卡人申诉抑制风险事件：申诉成立后证据不足自动解冻，待核验预约恢复 ----
st, r = req("GET", f"/api/passes/{pr2['id']}")
assert r["data"]["status"] == "restricted"
# 找到 M2-01 对应核验（第一次不匹配），持卡人申诉为本人（照片过时）
st, vl = req("GET", f"/api/passes/{pr2['id']}/verifications")
m2verifs = [x for x in vl["data"] if x["reason"] == "FACE_MISMATCH"]
first = [x for x in m2verifs if x["request_id"] == "M2-01"][0]
st, r = req("POST", "/api/appeals", {"pass_id": pr2["id"], "verification_id": first["id"],
                                     "type": "FREEZE_DISPUTE", "description": "近期整容且照片未更新，四次均为本人刷脸"})
a5 = r["data"]["id"]
st, r = req("POST", f"/api/appeals/{a5}/resolve",
            {"approve": True, "resolution": "核实确为本人，照片版本过旧，剔除该次不匹配", "handler": "cs-wang"})
check("M5 申诉通过受理", st == 200, r)
# 自动门槛为 4次/3入口 或 3次/2次人工放行；剔除 1 次后剩 3 次 3 门 0 人工放行 → 证据不足自动解冻
st, r = req("GET", f"/api/passes/{pr2['id']}")
check("M5 剔除风险事件后证据不足自动解冻", r["data"]["status"] == "active", r["data"])
st, r = req("GET", f"/api/freeze-cases/{case2}")
check("M5 案件结案为 UNFROZEN", r["data"]["status"] == "UNFROZEN", r["data"])
st, r = req("GET", f"/api/passes/{pr2['id']}/reservations")
check("M5 待核验预约恢复为 BOOKED", r["data"][0]["status"] == "BOOKED", r["data"])

# ---- M6. 解冻申请：材料不全拒绝；通过后恢复 ----
st, r = req("POST", "/api/unfreeze-requests", {"pass_id": pr1["id"], "photo_url": "https://img.example.com/new.jpg"})
check("M6 解冻申请缺少证件材料被拒", st == 400, r)
st, r = req("POST", "/api/unfreeze-requests",
            {"pass_id": pr1["id"], "id_doc_url": "https://img.example.com/id.jpg",
             "photo_url": "https://img.example.com/recent.jpg", "statement": "本人持卡，申请解冻"})
ureq = r["data"]["id"]
check("M6 解冻申请提交成功待审核", st == 200 and r["data"]["status"] == "PENDING", r)
st, r = req("GET", f"/api/unfreeze-requests?pass_id={pr1['id']}")
check("M6 解冻申请列表可查", any(x["id"] == ureq for x in r["data"]), r)
st, r = req("POST", f"/api/unfreeze-requests/{ureq}/review",
            {"decision": "APPROVED", "note": "证件与近期照片比对为本人，恢复年卡", "handler": "cs-wang"})
check("M6 解冻审核通过", st == 200 and r["data"]["status"] == "APPROVED", r)
st, r = req("GET", f"/api/passes/{pr1['id']}")
check("M6 解冻后卡恢复 active", r["data"]["status"] == "active", r["data"])
st, r = rverify(pr1["id"], "attr-garden")
check("M6 解冻后可正常刷脸入园", r["data"]["decision"] == "ALLOW", r)

# ---- M7. 确认转借：剩余权益限制使用 + 待核验预约取消 ----
# 新卡：先预约，再用 3 次不匹配含 2 次人工放行触发冻结
pr7 = buy_pass("single")
req("POST", "/api/reservations", {"pass_id": pr7["id"], "attraction": "attr-lake",
                                  "date": RISK_DATE, "slot": "10:00-12:00"})
st, r = face_fail(pr7["id"], "dev-east-01", "attr-lake", req_id="M7-01")
req("POST", f"/api/verifications/{r['data']['verification_id']}/manual-review",
    {"action": "RELEASE", "operator": "staff-chen", "note": "声称照片旧了，现场放行"})
st, r = face_fail(pr7["id"], "dev-west-01", "attr-summit", req_id="M7-02")
req("POST", f"/api/verifications/{r['data']['verification_id']}/manual-review",
    {"action": "RELEASE", "operator": "staff-chen", "note": "放行"})
face_fail(pr7["id"], "dev-south-01", "attr-garden", req_id="M7-03")
st, r = req("GET", f"/api/passes/{pr7['id']}")
assert r["data"]["status"] == "restricted", r["data"]
case7 = r["data"]["freeze_case_id"]
st, r = req("GET", f"/api/passes/{pr7['id']}/reservations")
check("M7 冻结时已有预约转待核验", r["data"][0]["status"] == "PENDING_VERIFY", r["data"])
# 对 pr7 提交解冻申请并判定 CONFIRM_LENDING
st, r = req("POST", "/api/unfreeze-requests",
            {"pass_id": pr7["id"], "id_doc_url": "https://img.example.com/id2.jpg",
             "photo_url": "https://img.example.com/recent2.jpg", "statement": "是我本人"})
u7 = r["data"]["id"]
before_remaining = remaining(pr7["id"])  # 两次人工放行已扣 2 人次 → 98
st, r = req("POST", f"/api/unfreeze-requests/{u7}/review",
            {"decision": "CONFIRM_LENDING", "note": "多入口监控比对确认实际使用人与持卡人不符", "handler": "cs-wang"})
check("M7 确认转借审核完成", st == 200 and r["data"]["status"] == "CONFIRM_LENDING", r)
st, r = req("GET", f"/api/passes/{pr7['id']}")
check("M7 卡恢复 active 但附带限制", r["data"]["status"] == "active", r["data"])
st, r = req("GET", f"/api/passes/{pr7['id']}/restriction")
rest = r["data"]
check("M7 限制规则：剩50%/无同行/周2日1/禁人工放行",
      rest["entry_quota_left"] == before_remaining // 2 and rest["companions_allowed"] == 0
      and rest["weekly_entry_limit"] == 2 and rest["daily_entry_limit"] == 1
      and rest["manual_entry_blocked"] is True, r["data"])
check("M7 剩余权益按50%核减", remaining(pr7["id"]) == before_remaining // 2,
      f"{remaining(pr7['id'])} vs {before_remaining // 2}")
st, r = req("GET", f"/api/passes/{pr7['id']}/reservations")
check("M7 确认转借后待核验预约取消", r["data"][0]["status"] == "CANCELLED", r["data"])
st, r = req("GET", f"/api/passes/{pr7['id']}/changes")
check("M7 变更日志记录 LENDING_CONFIRMED", any(c["action"] == "LENDING_CONFIRMED" for c in r["data"]), r)
# 客服解释材料含限制说明
st, r = req("GET", f"/api/freeze-cases/{case7}/material")
check("M7 客服解释材料含权益限制说明", "限制" in r["data"]["restriction_note"] and r["data"]["restriction"], r["data"])
# 限制使用期：本人刷脸可入园（当日第1次）
st, r = rverify(pr7["id"], "attr-lake")
check("M7 限制期本人刷脸首次放行", r["data"]["decision"] == "ALLOW", r)
# 当日第2次：日限1
st, r = rverify(pr7["id"], "attr-summit")
check("M7 限制期当日第二次被频次拦截 LENDING_USAGE_LIMITED",
      r["data"]["reason"] == "LENDING_USAGE_LIMITED", r)
# 人脸不匹配不再支持人工放行
st, r = face_fail(pr7["id"], "dev-east-01", "attr-lake", req_id="M7-FACE")
v_face = r["data"]["verification_id"]
st, r = req("POST", f"/api/verifications/{v_face}/manual-review",
            {"action": "RELEASE", "operator": "staff-chen", "note": "限制期尝试人工放行"})
check("M7 限制期人脸不匹配禁止人工放行", st == 409, r)

# 限制期不可携带同行人：用种子家庭卡（含已绑定成员）直接挂限制规则验证
import datetime as _dt
restriction_obj = {
    "pass_id": "P000001", "freeze_case_id": case7, "entry_quota_left": 100,
    "companions_allowed": 0, "weekly_entry_limit": 2, "daily_entry_limit": 1,
    "manual_entry_blocked": True, "reason": "CONFIRMED_LENDING", "operator": "cs-wang",
    "created_at": _dt.datetime.now().astimezone().isoformat(),
}
redis_set_json("restrict:P000001", restriction_obj)
redis_cli("DEL", "hf:P000001", "loc:P000001", "dedup:P000001:attr-garden")
st, r = verify(idcard="110101199001011234", companions=["110101199203054321"],
               attraction="attr-garden", face=90.0, ts=local_ts(10, 30, date=RISK_DATE))
_risk_week = (_dt.datetime.now().astimezone() - _dt.timedelta(days=1)).isocalendar()
WEEK_KEY = f"{_risk_week[0]:04d}-W{_risk_week[1]:02d}"
check("M7 限制期携带同行人被拒", r["data"]["reason"] == "LENDING_USAGE_LIMITED", r)
redis_cli("DEL", "restrict:P000001", f"rl:day:P000001:{RISK_DATE}", f"rl:week:P000001:{WEEK_KEY}")

# ---- M8. 客服人工风控冻结：证据不足拒绝，达标可冻结；申诉驳回后复评冻结 ----
pr8 = buy_pass("single")
st, r = req("POST", f"/api/passes/{pr8['id']}/risk-freeze",
            {"note": "怀疑转借", "operator": "cs-wang"})
check("M8 证据不足时人工风控冻结被拒", st == 400, r)
face_fail(pr8["id"], "dev-east-01", "attr-lake", req_id="M8-01")
face_fail(pr8["id"], "dev-west-01", "attr-summit", req_id="M8-02")
face_fail(pr8["id"], "dev-east-02", "attr-lake", req_id="M8-03")
st, r = req("POST", f"/api/passes/{pr8['id']}/risk-freeze",
            {"note": "3次不匹配跨2入口，客服核查后冻结", "operator": "cs-wang"})
check("M8 达标后人工风控冻结", st == 200 and r["data"]["trigger"] == "MANUAL" and r["data"]["status"] == "FROZEN", r)
# 直接解冻（客服核实误判）
st, r = req("POST", f"/api/passes/{pr8['id']}/unfreeze", {"operator": "cs-wang"})
check("M8 客服直接解除风控冻结", st == 200 and r["data"]["status"] == "active", r)

# 申诉驳回复评：新卡制造 3 次跨 2 入口不匹配 → 申诉驳回 → 自动冻结
pr9 = buy_pass("single")
face_fail(pr9["id"], "dev-east-01", "attr-lake", req_id="M9-01")
st, r = face_fail(pr9["id"], "dev-west-01", "attr-summit", req_id="M9-02")
v9 = r["data"]["verification_id"]
face_fail(pr9["id"], "dev-east-02", "attr-lake", req_id="M9-03")
st, r = req("GET", f"/api/passes/{pr9['id']}")
check("M9 3次/2入口不满足自动冻结(需4次/3入口)，仍 active", r["data"]["status"] == "active", r["data"])
st, r = req("POST", "/api/appeals", {"pass_id": pr9["id"], "verification_id": v9,
                                     "type": "FREEZE_DISPUTE", "description": "申诉称本人"})
a9 = r["data"]["id"]
st, r = req("POST", f"/api/appeals/{a9}/resolve",
            {"approve": False, "resolution": "无法证明为本人，驳回申诉", "handler": "cs-wang"})
check("M9 申诉驳回受理", st == 200, r)
st, r = req("GET", f"/api/passes/{pr9['id']}")
check("M9 申诉驳回后风险复评触发临时冻结", r["data"]["status"] == "restricted", r["data"])

# ---- M9. 运营总览新增风控字段 ----
st, r = req("GET", "/api/admin/stats/overview")
check("M10 总览含风控冻结与确认转借统计",
      "risk_frozen_passes" in r["data"] and r["data"]["lending_confirmed_total"] >= 1, r["data"])

print(f"\n========================================")
print(f"RESULT: PASS={PASS} FAIL={FAIL}")
print(f"========================================")
sys.exit(1 if FAIL > 0 else 0)
