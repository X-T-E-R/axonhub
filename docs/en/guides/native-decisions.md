# Native decision models

Use `/v1/systemone` or `/v1/decisions` to send structured questions through AxonHub. Requests use your AxonHub API key, project permissions, model associations and channel model mappings. Answers remain native JSON, not chat messages.

## Configure a channel

In **Channels → Add Channel**, select one of these presets. Each channel uses its own provider API key.

| Preset | Provider Base URL | Endpoint format / path | Suggested model |
| --- | --- | --- | --- |
| TypeSafe Jev | `https://api.typesafe.ai` | `typesafe/systemone` / `/v1/systemone` | `jev-1.14.0` |
| Command Code Jev | `https://api.commandcode.ai/provider` | `commandcode/systemone` / `/v1/systemone` | `typesafe/jev-1.14.0` |
| OpenAI Decisions | `https://api.openai.com/v1` | `openai/decisions` / `/decisions` | `gpt-6-luna` |
| System One compatible | Your deployed server root, without `/v1` | `typesafe/systemone` / `/v1/systemone` | Your server's model ID, such as `agr-flash` |

The presets save an `openai` carrier with explicit native endpoints; this does not grant chat capability. Edit the supported models manually. Model discovery and automatic model sync are not supported by these presets. Save, run the channel/key connection test, and enable the channel. Configure model associations and API key/project profiles as for other channels.

Custom paths and per-endpoint Base URLs can be changed in the existing endpoint editor. Native-only configurations replace the carrier's implicit defaults. Existing and explicitly mixed configurations retain the normal endpoint merge behavior.

## Call System One

TypeSafe and Command Code use the same object-map wire format. For the official TypeSafe Python SDK, point `base_url` at the **AxonHub root without `/v1`**; the SDK appends `/v1/systemone`.

```python
import os
from typesafe_sdk import TypeSafeClient, Noul

with TypeSafeClient(
    api_key=os.environ["AXONHUB_API_KEY"],
    base_url="http://localhost:8090",
    model="jev-1.14.0",  # Or your configured AxonHub alias.
    compression=False,
) as client:
    result = client.system_one(
        state={"body": "I was charged twice."},
        questions={"duplicate": Noul(instructions="Is this a duplicate charge?")},
    )
    print(result.nouls["duplicate"].noul)
```

Select the Command channel with its configured model or alias, such as `typesafe/jev-1.14.0`. Its upstream credential must be a **Command Code Studio API key**, not a TypeSafe key.

Both System One presets support text/JSON and the `noul`, `choice` and `score` primitives. Experimental images/assets, `box` questions and multipart are explicitly rejected. Command Code permits at most **100 questions** and rejects TypeSafe experimental opt-in headers. TypeSafe direct does not inherit that question cap. All native decision calls are non-streaming.

If you send `x-cmd-zdr: 1` to Command Jev, its documented `422 / cmd_zdr_no_providers` response is preserved. AxonHub does not remove the header to obtain a non-ZDR success.

Agr, Kev and Laya deployments use the same custom System One channel. Their `confidence`, `routing`, `truncated`, `action`, `x_*` and usage fields retain the serving implementation's meaning; AxonHub does not recalculate confidence or assume that output tokens are always zero.

## Call OpenAI Decisions

For the official OpenAI SDK, use the **AxonHub root with `/v1`**. The SDK appends `/decisions`.

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

OpenAI Decisions accepts **1–400 questions**. Ordered arrays, boolean/string choice values, optional question names, nullable answer names, partial refusals, `input_json`, image parts/URLs, per-question instructions and cache fields remain in the native JSON. AxonHub does not download or rewrite images. System One and Decisions do not convert into each other; configuring one does not provide the other.

## Read results and usage

Request details open native responses as JSON and generate cURL with the native endpoint. The request and execution records use the existing storage policy.

Complete upstream input/output counts enter the token ledger, including real zeros. Cached and cache-write tokens are subsets of input, not additional tokens. Missing or partial usage remains unchanged in the response and is shown as **Usage unknown** instead of a zero-cost call. Such responses provide generic HTTP forwarding; a missing required SDK field is not repaired with fabricated values. Unreported cache or pricing fields are shown as **Not reported**.

Provider error bodies, status codes, `Retry-After` and request-ID headers are preserved. Existing retry policy still applies; this feature does not add cross-protocol fallback or extra request replay.
