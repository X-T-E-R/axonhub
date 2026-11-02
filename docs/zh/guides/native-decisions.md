# 原生决策模型

通过 `/v1/systemone` 或 `/v1/decisions` 向 AxonHub 发送结构化问题。请求使用 AxonHub API key、项目权限、模型关联与渠道模型映射；答案保留原生 JSON，不包装成聊天消息。

## 配置渠道

在**渠道 → 添加渠道**中选择预设。每个渠道填写对应服务商自己的 API key。

| 预设 | 服务商 Base URL | 端点格式 / 路径 | 建议模型 |
| --- | --- | --- | --- |
| TypeSafe Jev | `https://api.typesafe.ai` | `typesafe/systemone` / `/v1/systemone` | `jev-1.14.0` |
| Command Code Jev | `https://api.commandcode.ai/provider` | `commandcode/systemone` / `/v1/systemone` | `typesafe/jev-1.14.0` |
| OpenAI Decisions | `https://api.openai.com/v1` | `openai/decisions` / `/decisions` | `gpt-6-luna` |
| System One 兼容服务 | 已部署服务的根地址，不带 `/v1` | `typesafe/systemone` / `/v1/systemone` | 服务实际模型 ID，例如 `agr-flash` |

预设使用 `openai` 渠道类型承载显式原生端点，不因此开放聊天能力。请手动编辑支持模型；这些预设暂不支持获取模型目录和自动同步。保存后运行渠道/Key 连接测试，再启用渠道。模型关联、API key 与项目 profile 的配置方式与其它渠道相同。

自定义路径和端点专用 Base URL 可在现有端点编辑器中修改。全原生配置替代渠道类型的隐含默认端点；既有配置和显式混合配置保留原有端点合并行为。

## 调用 System One

TypeSafe 与 Command Code 共用 object-map 格式。官方 TypeSafe Python SDK 的 `base_url` 填写**不带 `/v1` 的 AxonHub 根地址**，SDK 会追加 `/v1/systemone`。

```python
import os
from typesafe_sdk import TypeSafeClient, Noul

with TypeSafeClient(
    api_key=os.environ["AXONHUB_API_KEY"],
    base_url="http://localhost:8090",
    model="jev-1.14.0",  # 也可使用配置好的 AxonHub 别名。
    compression=False,
) as client:
    result = client.system_one(
        state={"body": "I was charged twice."},
        questions={"duplicate": Noul(instructions="Is this a duplicate charge?")},
    )
    print(result.nouls["duplicate"].noul)
```

使用 Command 渠道时，选择该渠道配置的模型或别名，例如 `typesafe/jev-1.14.0`。其上游凭据应为 **Command Code Studio API key**，不使用 TypeSafe key。

两类 System One 预设支持文本/JSON 和 `noul`、`choice`、`score` 三类基本题型，明确拒绝实验图片/assets、`box` 题型和 multipart。Command Code 最多 **100 题**，且不接受 TypeSafe 实验 opt-in header；TypeSafe 直连不继承这个题数限制。所有原生决策请求均不使用流式。

向 Command Jev 发送 `x-cmd-zdr: 1` 时，上游文档规定的 `422 / cmd_zdr_no_providers` 会保留。AxonHub 不会删除该 header 来换取非 ZDR 成功。

Agr、Kev、Laya 等已部署服务复用自定义 System One 渠道。`confidence`、`routing`、`truncated`、`action`、`x_*` 和 usage 保持服务自身含义；AxonHub 不重算 confidence，也不假定 output tokens 总是零。

## 调用 OpenAI Decisions

官方 OpenAI SDK 使用**带 `/v1` 的 AxonHub 根地址**，SDK 会追加 `/decisions`。

```python
import os
from openai import OpenAI

client = OpenAI(api_key=os.environ["AXONHUB_API_KEY"], base_url="http://localhost:8090/v1")
result = client.decisions.create(
    model="gpt-6-luna",
    input="The same purchase was billed twice.",
    questions=[{"type": "predicate", "instructions": "Does this report a duplicate charge?"}],
)
print(result.answers[0].probability)
```

OpenAI Decisions 接受 **1–400 题**。有序数组、boolean/string choice 值、可省略的问题名、可为 null 的答案名、部分拒绝、`input_json`、图片 parts/URL、每题 instructions 与缓存字段均保留在原生 JSON 中。AxonHub 不下载或改写图片。System One 与 Decisions 不互转；配置一种协议不会自动提供另一种协议。

## 查看结果与用量

请求详情默认以 JSON 显示原生响应，并按原生端点生成 cURL。Request 与 RequestExecution 记录沿用已有存储策略。

上游完整 input/output 计数进入 token 账本，包括真实的零值。缓存读取与写入 tokens 是 input 的子集，不重复计入 total。缺失或部分 usage 原样保留在响应中，并显示**用量未知**，不解释成零费用。此类响应提供通用 HTTP 转发；AxonHub 不伪造 SDK 必需字段。未报告的缓存或价格字段显示**未报告**。

服务商错误正文、状态码、`Retry-After` 和 request-ID header 保留；仍按既有重试策略执行。本功能不增加跨协议 fallback 或额外请求重放。
