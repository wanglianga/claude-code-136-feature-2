# passgate — 景区年卡实名入园同行权益与异常核验服务

基于 **Go + Fiber + Redis** 的景区年卡核验后端。覆盖：家庭/单人/亲子年卡售卖与实名信息管理、
闸机入园核验（人脸/证件/二维码/同行人/时段/容量）、异常核验原因可追溯、冻结/退款/升级权益重算、
现场人工放行留痕、客服申诉与权益返还、运营售卖规则与统计分析，以及 **转借高频异常冻结**
（多入口人脸不匹配自动临时冻结、游客证件+近期照片申请解冻、确认转借后权益限制、
在园不驱离与客服解释材料/现场话术）。

## 快速启动

### 方式一：Docker Compose 一键启动（推荐）

依赖：Docker + Docker Compose。无需本机 Go / Redis。

```bash
docker compose up --build
# 服务：http://127.0.0.1:8080   Swagger UI：http://127.0.0.1:8080/docs
```

Compose 同时启动 app 与 redis:7（含健康检查、AOF 数据卷），空库自动写入演示数据。

### 方式二：本地启动

依赖：Go 1.19+、Redis 5+（本机已在 127.0.0.1:6379 运行 Redis 7）。

```bash
# 1. 启动 Redis（若未运行）
redis-server --daemonize yes

# 2. 启动服务（默认端口 8080，空库自动写入演示数据）
go run .
# 或
go build -o passgate . && ./passgate

# 环境变量：PORT（默认 8080）、REDIS_ADDR（默认 127.0.0.1:6379）
```

启动后可见种子日志：示例年卡 家庭卡=P000001 单人卡=P000002 亲子卡=P000003。

### API 文档（OpenAPI 3.x + Swagger UI）

后端服务自带随二进制内嵌（`go:embed`）的 OpenAPI 3.x 规范与本地化 Swagger UI：

- Swagger UI：http://127.0.0.1:8080/docs （自动跳转到静态页面，无需外网 CDN）
- OpenAPI 规范：http://127.0.0.1:8080/openapi.yaml
- 静态资源：`static/openapi.yaml`、`static/swagger-ui/`、`static/vendor/`

## 本地验证方式

```bash
# 一键：构建 → 重置 Redis → 重启服务 → 运行全部 135 项端到端断言
bash test/run.sh
# 或服务已启动且为全新种子库时：
python3 test/e2e.py
```

验证结论（实际执行）：`bash test/run.sh` 全量通过，输出 `RESULT: PASS=135 FAIL=0`。覆盖
原有 78 项（正常放行、幂等重试、抖动去重、20 并发同/不同 request_id 均只扣 1 次、16 类失败原因、
人工放行留痕、冻结/解冻与预约一致性、申诉返还、退款/升级重算、信息变更、免刷脸人工确认、
出园释放容量、运营统计与日志查询）与新增 57 项转借高频异常冻结场景：

- 一周内 3 个入口 3 次人脸不匹配自动临时冻结；次数不足 / 单一入口等反例不冻结；
- 冻结证据汇总（失败次数、入口分布、人工放行记录、持卡人申诉）；
- 冻结后已预约订单转 `PENDING_VERIFICATION`，不可新预约、不可刷闸、不可人工放行新入园；
- 游客提交证件+近期照片申请解冻（缺材料 400、重复待审 409、驳回可重申、通过自动解冻结案）；
- 解冻入园后待核验预约补核销为 USED；
- 冻结时持卡人已在园 → 不追溯驱离、仅限制新入园，生成差异化现场解释话术；
- 客服确认转借 → 卡转 `restricted`：每日限本人入园 1 人次、不可携同行、不可新预约、
  待核验预约取消，并生成对客解释材料；
- OpenAPI 3.x 规范与本地化 Swagger UI 可访问。

> 说明：测试脚本对运行日期免疫——若测试当天恰好是种子配置的节假日（如 2026-10-01 国庆），
> 会临时移除当日节假日配置（节假日专项用例改用其他节假日日期），避免“单人卡节假日不可用”
> 干扰与日期无关的断言。

## 演示数据与测试账号

系统不设登录鉴权（演示），操作人通过请求头 `X-Operator: <姓名>` 或请求体 `operator` 字段标识，
所有敏感操作（冻结、人工放行、申诉处理、解冻审核、转借确认、规则调整）均记录操作人。
建议的演示身份：

| 角色 | 标识 | 用途 |
|---|---|---|
| 现场工作人员 | `staff-chen` | 人工核验放行、设置免刷脸、更新照片 |
| 客服 | `cs-wang` | 冻结/解冻、解冻申请审核、确认转借、退款、升级、申诉处理、换绑 |
| 运营 | `ops-li` | 售卖规则调整、节假日配置 |

种子数据（空库启动自动写入）：

- 景点：`attr-lake` 镜湖(容量5000)、`attr-summit` 云顶山(2000)、`attr-garden` 百花园(3000)
- 闸机：`dev-east-01/02`（东门）、`dev-west-01`（西门）、`dev-south-01`（南门），60 秒无心跳判离线
- 年卡：`P000001` 家庭卡（张三，含配偶/孩子/**免刷脸老人张父**）、`P000002` 单人卡（王五）、`P000003` 亲子卡（赵六+孩子）
- 售卖规则：单人 399 元/100 人次/节假日不可用；亲子 699 元/200 人次/同行 1 人；家庭 1299 元/400 人次/同行 3 人
- 2026 年法定节假日已配置（国庆 2026-10-01~08 等）
- 演示短信验证码固定为 `123456`（换绑手机号时使用）

## 核心 API

### 转借高频异常冻结（本次新增）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/risk/freeze-cases?status=` | 冻结案件列表（FROZEN/UNFROZEN/LENDING_CONFIRMED） |
| GET | `/api/risk/freeze-cases/:id` | 案件详情：证据事件、解冻申请、**客服解释材料+现场话术** |
| POST | `/api/risk/freeze-cases/:id/confirm-lending` | 客服确认转借：卡转 restricted，剩余权益限制 |
| GET | `/api/passes/:id/freeze-cases` | 该年卡的冻结案件 |
| POST | `/api/passes/:id/unfreeze-application` | 游客提交证件 `id_doc_url`+近期照片 `recent_photo`+说明申请解冻 |
| GET | `/api/unfreeze-applications?status=` | 解冻申请列表 |
| POST | `/api/unfreeze-applications/:id/review` | 客服审核（approve/note/handler），通过自动解冻结案 |

规则与行为：

- **触发条件**：7 天窗口内同一持卡人人脸不匹配（`FACE_MISMATCH`）≥3 次，且覆盖 ≥2 个入口
  （按闸机 gate/入口归并）；判定同时汇总**人工放行记录数**与**持卡人申诉数**作为案件证据。
- **冻结后**：卡 `status=frozen`、`freeze_source=risk_auto`；已预约订单 `BOOKED → PENDING_VERIFICATION`
  （不取消、不核销）；冻结期间不可刷闸入园、不可新预约、现场也不可人工放行新入园。
- **在园不驱离**：冻结时若持卡人已在园区内（`holder_in_park=true`），不追溯驱离，
  只限制新的入园动作，核验返回与案件详情均给出在园版现场解释话术。
- **解冻**：游客提交证件与近期照片 → 客服审核；通过后卡恢复 active、案件结案，
  待核验预约保留，持卡人实际入园时补核销为 USED。
- **确认转借**：卡转 `restricted`，剩余权益按规则限制——每日仅本人入园 1 人次、
  不可携带同行人、不可新预约，待核验预约取消；核验返回原因码 `PASS_RESTRICTED`。
- 人工冻结（`/freeze`，失主挂失等）不生成风控案件，不走线上解冻申请，仍由客服 `/unfreeze` 处理。

### 年卡生命周期
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/passes` | 购卡（实名+人脸照片+证件+手机号+成员；一证一卡；按售卖规则快照发卡） |
| GET | `/api/passes/:id` | 年卡详情（实时剩余额度、成员、二维码、`in_park` 在园标记、待核验预约数） |
| GET | `/api/passes?phone=` | 按手机号查卡 |
| POST | `/api/passes/:id/freeze` `/unfreeze` | 人工冻结/解冻（风控冻结解冻时同步结案；待核验预约保留） |
| POST | `/api/passes/:id/refund` | 退款：按剩余有效期 × 80% 折算，权益清零，未用/待核验预约取消 |
| POST | `/api/passes/:id/upgrade` | 升级：补差价，**有效期与额度按新卡重新计算** |
| POST | `/api/passes/:id/phone` | 换绑手机号（验证码 123456） |
| POST | `/api/passes/:id/members` / DELETE `.../members/:mid` | 家庭成员临时更换（消耗更换次数） |
| POST | `/api/passes/:id/members/:mid/photo` | 更新人脸照片（孩子长大，版本+1） |
| POST | `/api/passes/:id/members/:mid/face-exempt` | 老人免刷脸标记 |
| POST | `/api/passes/:id/qr-refresh` | 刷新二维码（旧码立即失效） |
| GET | `/api/passes/:id/changes` | 变更日志（RISK_FREEZE/UNFREEZE/LENDING_CONFIRMED 等全部留痕） |

### 入园核验
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/verify` | 闸机核验，返回 `ALLOW / MANUAL_REVIEW / QUOTA_INSUFFICIENT / SUSPECTED_LENDING` + 原因码；人脸不匹配触发自动冻结时返回 `auto_freeze_case_id` |
| GET | `/api/verifications/:id` | 核验详情（客服查看失败原因与人工处理链路） |
| POST | `/api/verifications/:id/manual-review` | 人工放行/拒绝（必须填操作人与说明；冻结/受限卡受规则阻断） |
| POST | `/api/exit` | 出园，释放在园容量并清除在园标记 |

`/api/verify` 请求示例：

```json
{
  "request_id": "gate-east-01-000123",
  "device_id": "dev-east-01",
  "qr_token": "QR...",
  "face_score": 88.5,
  "photo_clear": true,
  "companions": ["110101199203054321"],
  "attraction": "attr-lake",
  "timestamp": "2026-09-25T10:30:00+08:00",
  "offline_sync": false
}
```

失败/转人工原因码：`DEVICE_UNKNOWN` `DEVICE_OFFLINE` `PASS_NOT_FOUND` `PASS_FROZEN` `PASS_REFUNDED`
`PASS_EXPIRED` `PASS_NOT_STARTED` `PASS_RESTRICTED` `ATTRACTION_NOT_ALLOWED` `HOLIDAY_BLOCKED`
`TIMESLOT_BLOCKED` `CAPACITY_FULL` `COMPANION_EXCEEDED` `COMPANION_NOT_BOUND` `FACE_MISMATCH`
`FACE_EXEMPT_REVIEW` `PHOTO_UNCLEAR` `ID_EXPIRED` `HIGH_FREQUENCY` `MULTI_LOCATION`
`QUOTA_EXHAUSTED`（放行去重：`DUPLICATE_ENTRY`）。

### 设备 / 预约 / 申诉 / 运营
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/devices` `/api/devices/:id/heartbeat` | 设备注册、心跳 |
| GET | `/api/devices` | 设备列表（含在线状态） |
| POST | `/api/reservations` `/api/reservations/:id/cancel` | 预约/取消（冻结卡、受限卡不可新预约） |
| POST | `/api/appeals` `/api/appeals/:id/resolve` | 申诉（亦作为冻结判定证据）；客服通过自动返还误扣额度 |
| GET/PUT | `/api/holidays` | 节假日配置 |
| GET/PUT | `/api/admin/sales-rules[/:type]` | 售卖规则查询与调整（仅影响新售卡） |
| GET | `/api/admin/stats/overview` `/stats/peaks` `/gate-logs` | 按卡类型售卖/权益使用率/异常率、入园峰值、闸机流水 |

## 关键设计

- **幂等防重（网络抖动/闸机重试）**：`request_id` 先 `SET NX` 占位，重试直接返回首次结果；
  另有 90 秒「同卡同景点放行去重窗口」，重复刷闸按首次放行处理不重复扣次；
  扣减由 **Lua 脚本原子完成**（去重判定+额度+容量+写去重标记），多入口多设备并发安全。
- **转借高频异常冻结**：人脸不匹配逐次登记风险事件（含入口/设备/时间/分数/事后人工处理结果），
  7 天窗口 + 多入口 + 多次失败触发系统自动临时冻结；冻结案件固化证据快照
  （失败次数、入口分布、人工放行数、申诉数、在园标记），全链路写入年卡变更日志；
  客服解释材料区分对客话术与现场（在园/不在园）话术。
- **疑似转借（原有实时风控）**：10 分钟窗口内核验 >4 次（`HIGH_FREQUENCY`）或出现在 ≥2 个
  景点（`MULTI_LOCATION`）→ `SUSPECTED_LENDING`；一证一卡源头约束。
- **人工放行可解释**：人工处理强制操作人+说明，写 `ManualReview`、更新核验记录、写闸机日志
  `MANUAL_RELEASE`，客服 `GET /api/verifications/:id` 可见完整链路；同一核验仅可处理一次。
- **权益一致性**：额度以 Redis 计数器为权威；申诉通过自动返还；退款清零；升级按新卡重置并重算
  有效期；冻结/解冻只改状态，不触碰预约与额度；确认转借不改额度，仅加每日 1 人次等使用限制。
- **在园状态**：入园成功写 `inpark:<pass>`（按景点集合，36h TTL），出园按景点移除；
  冻结据此判定是否在园，实现“在园不驱离、只限制新入园”。
- **离线闸机**：心跳超时判离线，正常请求转人工；离线期间闸机本地放行的记录以 `offline_sync=true`
  补传，凭 `request_id` 幂等去重，不会重复扣次。

## 目录结构

```
main.go                  入口（依赖装配、种子、HTTP 服务、内嵌 OpenAPI/Swagger 静态资源）
static/
  openapi.yaml           OpenAPI 3.x 规范（含全部风控接口）
  swagger-ui/            本地化 Swagger UI 页面
  vendor/                swagger-ui-dist 资源（离线可用）
internal/model/          实体定义（FreezeCase/FaceRiskEvent/UnfreezeApplication 等）
internal/store/          Redis 封装（JSON 存取、ID 序列、分布式锁）
internal/service/        业务层：pass.go 年卡生命周期 / verify.go 核验引擎 /
                         risk.go 转借高频异常冻结 / ops.go 设备·预约·申诉·统计
internal/handler/        Fiber 路由与错误映射
internal/seed/           演示数据
test/e2e.py              135 项端到端断言
test/run.sh              一键验证（重置环境后全量跑）
Dockerfile / docker-compose.yml   一键容器化启动（app + redis 7）
```
