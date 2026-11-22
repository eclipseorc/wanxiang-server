# 万象自动化引擎设计(内嵌轻量 RPA)

> 目标:把「影刀」这类 RPA 工具的核心价值——**自动操作界面**——内嵌进万象,
> 复用万象已有的 ADB / scrcpy 能力,让万象自己就能编排、执行、
> 调度「真机自动化」任务,不依赖用户另外安装影刀。
>
> 状态:**待确认设计**(确认后分批复现)

---

## 一、定位与边界

| 维度 | 说明 |
|---|---|
| 控制对象 | **只控安卓真机**(复用 ADB / scrcpy),不控电脑桌面 |
| 流程定义 | 一期「表单式步骤编排」,二期再上「画布拖拽」 |
| 元素定位 | 坐标 + OCR(本地 OCR,离线免费) |
| 复用什么 | ADB 命令、scrcpy 截帧、APK 上传安装、设备连接管理 |

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
| `installApk` | 安装应用 | 复用 APK 上传 + adb install |
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

最终选定 **本地 Tesseract(经 gosseract 绑定)**,与已有 adb/scrcpy 属于同一量级的
「系统依赖」,而非独立服务。理由:离线可用、零成本、数据不出内网、中文识别可用
(需 `chi_sim.traineddata`)。

- 实现:`internal/automation/ocr.go` 定义 `TesseractOCR`,用
  `github.com/otiai10/gosseract/v2` 调 Tesseract C 库,`GetBoundingBoxes(RIL_WORD)`
  拿到每个词的框,匹配目标文字后返回中心坐标。
- **编译隔离(build tag)**:gosseract 是 cgo 库,需 CGO + tesseract 开发库。
  为保持项目默认「纯 Go 一键 `go build`」,OCR 实现放在 `//go:build ocr` 标签下,
  未启用时用 `ocr_stub.go` 的占位实现(返回「OCR 未启用」明确错误)。
  - 默认:`go build ./...`(纯 Go,零 CGO,不依赖 tesseract)
  - 启用:`CGO_ENABLED=1 go build -tags ocr`(需装 tesseract + 中文语言包)
- **运行期开关**:通过环境变量 `AUTOMATION_OCR=1` 启用 OCR 客户端(即使已
  用 `-tags ocr` 编译,未设该变量时 `buildOCR()` 仍返回 nil,`ocrFind` 报「未配置」)。
  可选 `AUTOMATION_OCR_LANGS`(默认 `chi_sim+eng`)、`AUTOMATION_OCR_TESSDATA`
  (语言包目录)。
- `ocrFind` 动作流程:截图 → 送 Tesseract 识别 → 匹配目标文字 → 返回中心坐标,
  供后续 `tap` 等步骤使用。

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
1. ✅ 画布式拖拽编排器(节点 + 连线)—— 已完成,用滴滴开源 **LogicFlow**
   (Apache-2.0,UMD 单文件,下载到 `web/lib/`,符合 CSP `'self'`)。画布是线性
   `steps` 数组的可视化:节点=动作、连线=顺序,拖拽节点重排顺序,保存时仍导出
   线性数组(后端零改动)。与一期「表单式编排」通过视图切换共存。
2. ✅ 定时调度(cron)—— 已完成,自研轻量 cron 解析器(5 字段,`*`/`N`/`N-M`/`*/N`,
   分钟级精度,零第三方依赖),进程内 Scheduler 每 30s tick、到分钟边界触发。
3. ✅ OCR 服务接入与 `ocrFind` 落地 —— 已完成(见第六节,build tag + 环境变量双重开关)
4. ✅ 并发上限配置化 —— 已完成,`AUTOMATION_MAX_CONCURRENCY` 环境变量(默认 8)
5. ✅ 任务详情面板化 —— 已完成,从 alert 升级为抽屉面板,按设备分组展示步骤日志/错误/截图
6. ✅ 画布节点参数内联编辑 —— 已完成,点击画布节点在画布内弹参数编辑面板

### 定时调度设计要点
- 流程新增 `device_ids` 列(默认目标设备,逗号分隔;空 = 触发时跑全部在线设备)。
- 调度器为进程内单实例(admin-server),不做多副本去重——本系统单实例部署满足需求。
- 触发即创建任务并异步执行,复用 Executor 的并发控制。

---

## 十、待确认问题

1. ✅ 一期是否先「表单式编排」过渡,二期再上画布拖拽?—— 已确认,一期表单式、二期画布
2. ✅ OCR 一期是否接入?—— 已确认二期接入,且选定 Tesseract(gosseract)而非 RapidOCR
3. ✅ 执行引擎放在 admin-server 进程内,还是独立 executor 进程?—— 已确认进程内
