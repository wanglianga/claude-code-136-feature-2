# passgate — 景区年卡实名入园同行权益与异常核验服务

基于 **Go + Fiber + Redis** 的景区年卡核验后端。覆盖：家庭/单人/亲子年卡售卖与实名信息管理、
闸机入园核验（人脸/证件/二维码/同行人/时段/容量）、异常核验原因可追溯、冻结/退款/升级权益重算、
现场人工放行留痕、客服申诉与权益返还、运营售卖规则与统计分析，以及**转借高频异常临时冻结**：
一周内跨入口多次人脸不匹配的综合判定、自动/人工风控冻结、在园不追溯驱离、证件+近期照片
解冻申请、确认转借后剩余权益限制使用与客服解释材料。

## 快速启动

### 方式一：Docker Compose 一键启动（推荐）

依赖：Docker（含 Compose 插件）。无需本机安装 Go/Redis。

```bash
docker compose up --build
# 服务监听 http://127.0.0.1:8080，首次启动自动写入演示数据
```

### 方式二：本地运行

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

## API 文档（Swagger UI / OpenAPI）

本项目为纯后端服务，随二进制内嵌 OpenAPI 3.x 规范与 Swagger UI 静态资源（离线可用）：

- Swagger UI：<http://127.0.0.1:8080/docs>
- OpenAPI 规范原文：<http://127.0.0.1:8080/openapi.yaml>

## 本地验证方式

```bash
bash test/run.sh        # 一键：构建 → 重置 Redis → 重启服务 → 运行全部端到端断言
# 或服务已启动且为全新种子库时：
python3 test/e2e.py
```

验证结论（实际执行）：`bash test/run.sh` 全量通过，输出 `RESULT: PASS=130 FAIL=0`。覆盖
原有能力（正常放行、幂等重试、抖动去重、20 并发同/不同 request_id 均只扣 1 次、
各失败原因码、人工放行留痕、普通冻结/解冻与预约一致性、申诉返还、退款/升级重算、信息变更、
免刷脸人工确认、出园释放容量、运营统计与日志查询）以及**转借高频异常冻结专题（M 章节 49 项）**：
跨入口人脸不匹配画像与自动冻结阈值、人工放行加权信号、申诉抑制事件与驳回后复评、
已预约订单待核验/恢复/取消、冻结期间限制新入园与禁止人工放行、在园不追溯驱离与现场话术、
解冻申请审核三分支（恢复/驳回/确认转借）、确认转借后剩余权益 50% 保留与频次/同行/刷脸限制、
客服解释材料证据链、风控统计字段。脚本对运行日期健壮（运行当天恰逢节假日时临时移除当天）。

## 演示数据与测试账号

系统不设登录鉴权（演示），操作人通过请求头 `X-Operator: <姓名>` 或请求体 `operator` 字段标识，
所有敏感操作（冻结、风控冻结、人工放行、申诉/解冻处理、规则调整）均记录操作人。建议的演示身份：

| 角色 | 标识 | 用途 |
|---|---|---|
| 现场工作人员 | `staff-chen` | 人工核验放行、设置免刷脸、更新照片 |
| 客服 | `cs-wang` | 冻结/解冻、人工风控冻结、解冻申请审核、退款、升级、申诉处理、换绑 |
| 运营 | `ops-li` | 售卖规则调整、节假日配置 |

种子数据（空库启动自动写入）：

- 景点：`attr-lake` 镜湖(容量5000)、`attr-summit` 云顶山(2000)、`attr-garden` 百花园(3000)
- 闸机：`dev-east-01/02`（东门）、`dev-west-01`（西门）、`dev-south-01`（南门），60 秒无心跳判离线
- 年卡：`P000001` 家庭卡（张三，含配偶/孩子/**免刷脸老人张父**）、`P000002` 单人卡（王五）、`P000003` 亲子卡（赵六+孩子）
- 售卖规则：单人 399 元/100 人次/节假日不可用；亲子 699 元/200 人次/同行 1 人；家庭 1299 元/400 人次/同行 3 人
- 2026 年法定节假日已配置（国庆 2026-10-01~08 等）
- 演示短信验证码固定为 `123456`（换绑手机号时使用）

## 核心 API

### 年卡生命周期
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/passes` | 购卡（实名+人脸照片+证件+手机号+成员；一证一卡；按售卖规则快照发卡） |
| GET | `/api/passes/:id` | 年卡详情（实时剩余额度、成员、二维码 token） |
| GET | `/api/passes?phone=` | 按手机号查卡 |
| POST | `/api/passes/:id/freeze` `/unfreeze` | 普通冻结/解冻（**不影响已预约未入园订单**；`unfreeze` 也兼容解除风控冻结） |
| POST | `/api/passes/:id/refund` | 退款：按剩余有效期 × 80% 折算，权益清零，未用预约取消 |
| POST | `/api/passes/:id/upgrade` | 升级：补差价，**有效期与额度按新卡重新计算** |
| POST | `/api/passes/:id/phone` | 换绑手机号（验证码 123456） |
| POST | `/api/passes/:id/members` / DELETE `.../members/:mid` | 家庭成员临时更换（消耗更换次数） |
| POST | `/api/passes/:id/members/:mid/photo` | 更新人脸照片（孩子长大，版本+1） |
| POST | `/api/passes/:id/members/:mid/face-exempt` | 老人免刷脸标记 |
| POST | `/api/passes/:id/qr-refresh` | 刷新二维码（旧码立即失效） |
| GET | `/api/passes/:id/changes` | 变更日志（全部敏感操作留痕，含 RISK_FREEZE/LENDING_CONFIRMED） |

### 入园核验
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/verify` | 闸机核验，返回 `ALLOW / MANUAL_REVIEW / QUOTA_INSUFFICIENT / SUSPECTED_LENDING` + 原因码 |
| GET | `/api/verifications/:id` | 核验详情（客服查看失败原因与人工处理链路） |
| POST | `/api/verifications/:id/manual-review` | 人工放行/拒绝（必须填操作人与说明，放行走同一原子扣减） |
| POST | `/api/exit` | 出园，释放在园容量并清除在园标记 |

`/api/verify` 请求示例：

```json
{
  "request_id": "gate-east-01-000123",   // 闸机幂等键，重试必须不变
  "device_id": "dev-east-01",
  "qr_token": "QR...",                    // 或 pass_id / id_card（主入园人证件）
  "face_score": 88.5,
  "photo_clear": true,
  "companions": ["110101199203054321"],
  "attraction": "attr-lake",
  "timestamp": "2026-09-25T10:30:00+08:00",
  "offline_sync": false                   // true 表示闸机离线补传
}
```

失败/转人工原因码：`DEVICE_UNKNOWN` `DEVICE_OFFLINE` `PASS_NOT_FOUND` `PASS_FROZEN` `PASS_REFUNDED`
`PASS_EXPIRED` `PASS_NOT_STARTED` `ATTRACTION_NOT_ALLOWED` `HOLIDAY_BLOCKED` `TIMESLOT_BLOCKED`
`CAPACITY_FULL` `COMPANION_EXCEEDED` `COMPANION_NOT_BOUND` `FACE_MISMATCH` `FACE_EXEMPT_REVIEW`
`PHOTO_UNCLEAR` `ID_EXPIRED` `HIGH_FREQUENCY` `MULTI_LOCATION` `QUOTA_EXHAUSTED`（放行去重：`DUPLICATE_ENTRY`）
`LENDING_RESTRICTED`（转借风控冻结期间限制新入园）`LENDING_USAGE_LIMITED`（确认转借后频次/同行受限）。

### 转借高频异常冻结（本专题）
| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/passes/:id/risk-snapshot` | 近 7 天风险画像：人脸不匹配次数、跨入口数、人工放行数、相关申诉数，及是否达到自动/人工冻结门槛 |
| POST | `/api/passes/:id/risk-freeze` | 客服根据画像人工风控冻结（body 必填 `note`） |
| GET | `/api/freeze-cases` `?pass_id=&status=` | 风控冻结案件列表（FROZEN/UNFROZEN/RESTRICTED） |
| GET | `/api/freeze-cases/:id` | 案件详情（判定依据摘要、在园标记、现场话术） |
| GET | `/api/freeze-cases/:id/material` | **客服解释材料**：证据链（每次不匹配的时间/入口/设备/得分/是否人工放行）、现场解释话术、权益限制说明 |
| POST | `/api/unfreeze-requests` | 游客提交解冻申请（必填 `pass_id`、`id_doc_url` 证件、`photo_url` 近期照片） |
| GET | `/api/unfreeze-requests` `?pass_id=&status=` | 解冻申请列表 |
| POST | `/api/unfreeze-requests/:id/review` | 客服审核：`decision=APPROVED`（恢复）/ `REJECTED`（驳回维持冻结）/ `CONFIRM_LENDING`（确认转借） |
| GET | `/api/passes/:id/restriction` | 查询确认转借后的权益限制规则 |

### 设备 / 预约 / 申诉 / 运营
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/devices` `/api/devices/:id/heartbeat` | 设备注册、心跳 |
| GET | `/api/devices` | 设备列表（含在线状态） |
| POST | `/api/reservations` `/api/reservations/:id/cancel` | 预约/取消（冻结卡不可新预约，已有预约保留） |
| POST | `/api/appeals` `/api/appeals/:id/resolve` | 申诉；客服通过自动返还误扣额度 |
| GET/PUT | `/api/holidays` | 节假日配置 |
| GET/PUT | `/api/admin/sales-rules[/:type]` | 售卖规则查询与调整（仅影响新售卡） |
| GET | `/api/admin/stats/overview` `/stats/peaks` `/gate-logs` | 按卡类型售卖/权益使用率/异常率、入园峰值、闸机流水 |

## 关键设计

- **幂等防重（网络抖动/闸机重试）**：`request_id` 先 `SET NX` 占位，重试直接返回首次结果；
  另有 90 秒「同卡同景点放行去重窗口」，重复刷闸按首次放行处理不重复扣次；
  扣减由 **Lua 脚本原子完成**（去重判定+额度+容量+写去重标记），多入口多设备并发安全。
- **疑似转借（实时高频/多地）**：10 分钟窗口内核验 >4 次（`HIGH_FREQUENCY`）或出现在 ≥2 个景点
  （`MULTI_LOCATION`）→ `SUSPECTED_LENDING`；一证一卡源头约束。
- **转借高频异常冻结（一周维度的临时冻结）**：
  - 每次**人脸不匹配**都落风险事件（7 天窗口），按卡聚合失败次数、**不同入口（闸机 gate）数量**、
    **现场人工放行记录**（加权信号，一次放行按强嫌疑计）与**持卡人相关申诉**形成风险画像。
  - **自动冻结**：7 天内不匹配 ≥4 次且跨 ≥3 个入口；或不匹配 ≥3 次且其中 ≥2 次被人工放行。
  - **人工冻结**：客服查画像后发起，门槛为 ≥3 次不匹配且跨 ≥2 入口，或存在人工放行/申诉争议。
  - **申诉联动**：申诉核实为本人（照片过时等）则对应事件剔除判定，剩余证据不达标自动解冻并
    恢复待核验预约；申诉驳回则按最新画像复评（≥3 次/跨 ≥2 入口即冻结）。
  - 冻结将卡置为 `restricted`：已预约未入园订单转为 **PENDING_VERIFY（待核验，不取消）**，
    不可新预约；后续刷闸返回 `LENDING_RESTRICTED`，且**任何人工放行一律拒绝**。
  - **在园不追溯驱离**：冻结时若持卡人已在园内（入园成功标记在园、出园清除），不影响其本次
    游玩，仅限制进入新景点/新预约；案件内置可直接使用的**现场解释话术**区分在园/不在园。
  - **解冻申请**：游客上传证件与近期照片；客服三分支处理——通过则恢复全部权益与待核验预约；
    驳回则维持冻结可补充材料再次申请；**确认转借**则卡恢复可入园但附加权益限制，待核验预约取消。
  - **确认转借限制使用**：剩余可入园次数按 **50% 保留**（原子核减），限制期不可携带同行人、
    每周限 2 次/每日限 1 次（当日/当周计数 Lua 原子占用，扣减失败自动回退）、必须本人刷脸，
    人脸不匹配**不再支持人工放行**；`GET /api/freeze-cases/:id/material` 输出含证据链与限制
    说明的客服解释材料。
- **人工放行可解释**：人工处理强制操作人+说明，写 `ManualReview`、更新核验记录、写闸机日志
  `MANUAL_RELEASE`，客服 `GET /api/verifications/:id` 可见完整链路；同一核验仅可处理一次。
- **权益一致性**：额度以 Redis 计数器为权威；申诉通过自动返还；退款清零；升级按新卡重置并重算有效期；
  普通冻结/解冻只改状态，不触碰预约与额度。
- **离线闸机**：心跳超时判离线，正常请求转人工；离线期间闸机本地放行的记录以 `offline_sync=true`
  补传，凭 `request_id` 幂等去重，不会重复扣次。

## 目录结构

```
main.go                  入口（依赖装配、种子、HTTP 服务）
internal/model/          实体定义（含 FreezeCase/RiskEvent/UnfreezeRequest/QuotaRestriction）
internal/store/          Redis 封装（JSON 存取、ID 序列、分布式锁）
internal/service/        业务层：pass.go 年卡生命周期 / verify.go 核验引擎 /
                         risk.go 转借风控冻结 / ops.go 设备·预约·申诉·统计
internal/handler/        Fiber 路由与错误映射
internal/openapi/        内嵌 OpenAPI 3.x 规范 + Swagger UI 静态资源（/docs）
internal/seed/           演示数据
test/e2e.py              130 项端到端断言（A~L 原有能力 + M 转借冻结专题）
test/run.sh              一键验证（重置环境后全量跑）
Dockerfile               多阶段构建镜像
docker-compose.yml       redis + app 一键编排
```
