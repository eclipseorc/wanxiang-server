# 万象自动化引擎设计(内嵌轻量 RPA)

> 目标:把「影刀」这类 RPA 工具的核心价值——**自动操作界面**——内嵌进万象,
> 复用万象已有的 ADB / scrcpy / Device Owner 能力,让万象自己就能编排、执行、
> 调度「真机自动化」任务,不依赖用户另外安装影刀。
>
> 状态:**待确认设计**(确认后分批复现)

---

## 一、定位与边界

| 维度 | 说明 |
|---|---|
| 控制对象 | **只控安卓真机**(复用 ADB / scrcpy / MDM),不控电脑桌面 |
| 流程定义 | 一期「表单式步骤编排」,二期再上「画布拖拽」 |
| 元素定位 | 坐标 + OCR(本地 OCR,离线免费) |
| 复用什么 | ADB 命令、scrcpy 截帧、MDM 安装/卸载、设备连接管理 |

本质:在已有「设备控制」能力之上,加一层 **「动作原语 + 流程编排 + 调度器」**,
而不是从零造 RPA,也不是去集成影刀本体(影刀是闭源商业软件,无嵌入 SDK)。

---

## 二、整体架构

```
┌─────────────────────────────────────────────┐
│  前端 Web(admin 控制台)                       │
│   + 流程编辑器(一期表单式 / 二期画布拖拽)       │
└──────────────┬──────────────────────────────┘
               │ HTTP
┌──────────────▼──────────────────────────────┐
│  admin-server(新增 automation 模块)           │
│   流程 CRUD · 任务调度 · 结果存储 · 触发执行     │
└──────────────┬──────────────────────────────┘
               │ 内部调用
┌──────────────▼──────────────────────────────┐
│  执行引擎 executor ← 新增                     │
│   解析流程 → 逐步骤执行 → 复用 ADB/scrcpy      │
└──────────────┬──────────────────────────────┘
               │ ADB / scrcpy
┌──────────────▼──────────────────────────────┐
│  安卓真机设备池                               │
└─────────────────────────────────────────────┘
```

执行引擎放在 admin-server 进程内(与现有 adminapi 同进程),通过
`scrcpy.ADB` 直接操作设备,避免新增独立进程的运维成本。

---

## 三、核心数据模型(新增 3 张表)

```sql
-- 流程定义(编排好的自动化流程)
automation_flows(
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  description TEXT,
  steps_json  TEXT NOT NULL,     -- 步骤数组(表单/拖拽编排的结果)
  created_at  TEXT,
  updated_at  TEXT
)

-- 任务(一次具体执行)
automation_jobs(
  id         TEXT PRIMARY KEY,
  flow_id    TEXT NOT NULL,
  device_ids TEXT NOT NULL,      -- 目标设备序列号,逗号分隔或 JSON 数组
  status     TEXT NOT NULL,      -- pending/running/success/failed/partial
  schedule   TEXT,               -- cron 表达式(空 = 手动触发)
  created_at TEXT,
  started_at TEXT,
  finished_at TEXT
)

-- 步骤执行日志
automation_step_logs(
  id              TEXT PRIMARY KEY,
  job_id          TEXT NOT NULL,
  step_index      INTEGER NOT NULL,
  action          TEXT NOT NULL,
  params_json     TEXT,
  status          TEXT NOT NULL, -- success/failed/skipped
  error           TEXT,
  screenshot_path TEXT,          -- screenshot 动作产物 / 失败现场
  started_at      TEXT,
  finished_at     TEXT
)
```

---

## 四、动作原语(一期先做这 8 个)

| 动作 | 说明 | 底层实现 |
|---|---|---|
| `tap` | 点坐标 (x, y) | `adb shell input tap` |
| `swipe` | 滑动 (x1,y1)→(x2,y2) | `adb shell input swipe` |
| `inputText` | 输入文字 | `adb shell input text` |
| `launchApp` | 打开应用 | `adb shell am start` |
| `installApk` | 安装应用 | 复用 MDM / adb install |
| `screenshot` | 截图并保存 | scrcpy 截帧 |
| `wait` | 等待 N 毫秒 | 纯延时 |
| `ocrFind` | OCR 识别文字并返回坐标 | 调本地 OCR 服务 |

每个动作统一签名:

```go
type Step struct {
    Action string            `json:"action"`
    Params map[string]string `json:"params"`
}

type StepResult struct {
    Index      int
    Action     string
    Status     string // success / failed
    Error      string
    Screenshot string // 产物路径(如有)
}
```

---

## 五、流程定义(一期 JSON 形态)

```json
{
  "name": "每日回归:打开 App 并截图",
  "schedule": "0 9 * * *",
  "devices": ["device-001", "device-002"],
  "steps": [
    { "action": "launchApp",  "params": { "pkg": "com.example.app" } },
    { "action": "wait",       "params": { "ms": "3000" } },
    { "action": "screenshot", "params": { "save": "/shots/{{device}}/{{ts}}.png" } },
    { "action": "tap",        "params": { "x": "540", "y": "1600" } },
    { "action": "swipe",      "params": { "x1": "540", "y1": "1600", "x2": "540", "y2": "400" } }
  ]
}
```

---

## 六、OCR 方案(「坐标 + OCR」定位的关键)

- 选用 **RapidOCR / PaddleOCR** 本地部署,理由:离线可用、零成本、数据不出内网、
  中文识别好。
- 部署形态:独立 OCR 服务(HTTP),执行引擎通过 HTTP 调用;或内嵌 ONNX 运行时。
- `ocrFind` 动作流程:截图 → 送 OCR → 返回匹配文字的中心坐标 → 后续步骤用该坐标点击。

一期建议先接一个本地 HTTP OCR 服务(RapidOCR Server),执行引擎通过 `ocrFind`
动作调用它,返回 `{ text, x, y }`。

---

## 七、调度与并发

- 复用现有设备连接能力,增加**任务调度器**:cron 定时 + 手动触发。
- 批量设备**并发执行**,并设置并发上限(顺带修复此前安全审查发现的
  `runOnDevices` 无并发上限问题)。
- 执行日志 + 截图回传,前端展示任务进度与结果。

---

## 八、API 设计(adminapi 新增)

| 方法 | 路径 | 说明 |
|---|---|---|
| GET    | `/api/admin/automation/flows`        | 流程列表 |
| POST   | `/api/admin/automation/flows`        | 创建流程 |
| GET    | `/api/admin/automation/flows/{id}`   | 流程详情 |
| PUT    | `/api/admin/automation/flows/{id}`   | 更新流程 |
| DELETE | `/api/admin/automation/flows/{id}`   | 删除流程 |
| POST   | `/api/admin/automation/flows/{id}/run` | 手动触发执行 |
| GET    | `/api/admin/automation/jobs`         | 任务列表 |
| GET    | `/api/admin/automation/jobs/{id}`    | 任务详情(含步骤日志) |

---

## 九、分期实施

### 一期(先跑通链路,验证价值)
1. 数据模型 + 迁移(3 张表)
2. 动作原语层(8 个动作,复用 ADB/scrcpy)
3. 执行引擎(解析 JSON 流程 → 逐步骤执行 → 落日志)
4. adminapi 新增 automation 路由 + 处理器
5. 前端「表单式步骤编排」页面(添加步骤 → 选动作 → 填参数)
6. 任务列表 + 执行日志展示

### 二期(体验升级)
1. 画布式拖拽编排器(节点 + 连线)
2. 定时调度(cron)
3. OCR 服务接入与 `ocrFind` 落地
4. 并发上限 + 结果可视化

---

## 十、待确认问题

1. 一期是否先「表单式编排」过渡,二期再上画布拖拽?(当前按此排期)
2. OCR 一期是否接入?还是先「坐标 + 人工标定」,OCR 二期再上?(当前按二期接入)
3. 执行引擎放在 admin-server 进程内,还是独立 executor 进程?(当前按进程内)
