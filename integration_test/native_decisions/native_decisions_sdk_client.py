#!/usr/bin/env python3
"""Task41 official-SDK acceptance client for one loopback Axon root.

Only the Axon root is accepted in argv. ``AXON_NATIVE_TEST_KEY`` supplies the
synthetic Axon key. The Go fixture owns both Axon and its loopback upstream.
TypeSafe responses require ``model``, ``answers`` (question-ID map containing
noul/choice/score unions), and ``usage`` (input/output tokens). OpenAI
Decisions responses require ``model``, ordered predicate/choice/score/refusal
answers, and usage input/cache/output/reasoning/total fields. OpenAI choice
values and nullable answer names are intentionally checked as typed fields.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from importlib import metadata
from pathlib import Path
from typing import Any, Iterable
from urllib.parse import urlsplit

EXPECTED_VERSIONS = {"typesafe-sdk": "0.7.5", "openai": "3.42.1"}


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Run Task41 native SDK checks.")
    parser.add_argument("root", help="http://127.0.0.1:<port> (trailing slash allowed)")
    return parser


def _root(raw: str) -> str:
    try:
        parsed = urlsplit(raw)
        port = parsed.port
    except ValueError as exc:
        raise ValueError(f"invalid Axon root: {exc}") from exc
    if parsed.scheme != "http" or parsed.hostname != "127.0.0.1":
        raise ValueError("root must use http://127.0.0.1:<port>")
    if port is None or not 1 <= port <= 65535:
        raise ValueError("root must include a valid loopback port")
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("root must not contain credentials, query, or fragment")
    if parsed.path not in ("", "/"):
        raise ValueError("root must not contain a path")
    return f"http://127.0.0.1:{port}"


def _versions() -> dict[str, str]:
    result: dict[str, str] = {}
    for distribution, expected in EXPECTED_VERSIONS.items():
        try:
            actual = metadata.version(distribution)
        except metadata.PackageNotFoundError as exc:
            python = Path(__file__).resolve().parents[2] / ".runtime" / "native-sdk" / "Scripts" / "python.exe"
            raise RuntimeError(f"install {distribution}=={expected}; use {python}") from exc
        if actual != expected:
            raise RuntimeError(f"{distribution}={actual}; expected exactly {expected}")
        result[distribution] = actual
    return result


def _check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def _system_questions(typesafe_sdk: Any) -> dict[str, Any]:
    return {
        "id.with.dot": typesafe_sdk.Noul(
            instructions="Does this ticket report a duplicate charge?",
            criteria={"true": "Billed more than once.", "false": "Not billed more than once."},
        ),
        "department": typesafe_sdk.Choice(
            instructions="Which team should handle this ticket?",
            criteria={
                "billing": "Payments, invoices, and refunds.",
                "technical": "Product usage problems.",
                "other": "Anything else.",
            },
        ),
        "rank": typesafe_sdk.Score(
            instructions="How urgent is the request?",
            criteria=["first", "second"],
        ),
    }


def _system_state(alias: str) -> dict[str, Any]:
    return {
        "subject": "Duplicate charge",
        "body": "I was charged twice and need a refund today.",
        "fixture_model": alias,
        "case_id": 9007199254740993,
    }


def _check_system(result: Any, alias: str) -> None:
    _check(result.model == alias, f"TypeSafe {alias}: model={result.model!r}")
    _check(list(result.answers) == ["id.with.dot", "department", "rank"], f"TypeSafe {alias}: answer order/IDs changed")

    noul = result.answers["id.with.dot"]
    _check(noul.type == "noul" and noul.noul == 0, f"TypeSafe {alias}: noul answer changed")

    choice = result.answers["department"]
    _check(choice.type == "choice", f"TypeSafe {alias}: choice answer missing")
    _check(choice.choice == "billing", f"TypeSafe {alias}: choice={choice.choice!r}")
    _check(choice.confidence == 0.85, f"TypeSafe {alias}: choice confidence changed")
    _check(
        choice.probabilities == {"billing": 0.9, "technical": 0.05, "other": 0.05},
        f"TypeSafe {alias}: choice probabilities changed",
    )

    score = result.answers["rank"]
    _check(score.type == "score", f"TypeSafe {alias}: score answer missing")
    _check(score.score == 0.25 and score.confidence == 0, f"TypeSafe {alias}: score values changed")
    _check(score.legend == {0: "first", 1: "second"}, f"TypeSafe {alias}: score legend changed")
    _check(score.probabilities == {0: 0.75, 1: 0.25}, f"TypeSafe {alias}: score probabilities changed")

    _check(result.usage.input_tokens == 83, f"TypeSafe {alias}: input usage changed")
    _check(result.usage.output_tokens == 0, f"TypeSafe {alias}: output usage changed")


def _openai_request() -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    inputs = [
        {
            "role": "user",
            "content": [
                {"type": "input_text", "text": "Evaluate the ticket against the order record."},
                {
                    "type": "input_json",
                    "data": {
                        "ticket": {"body": "I was charged twice and need a refund today."},
                        "order": {"charges": [49, 49]},
                        "case_id": 9007199254740993,
                    },
                },
            ],
        }
    ]
    questions = [
        {"type": "predicate", "instructions": "Does the record support a duplicate charge?"},
        {
            "type": "choice",
            "name": "typed",
            "instructions": "Should a billing reviewer inspect this case?",
            "choices": [{"value": True}, {"value": "true"}],
        },
        {
            "type": "score",
            "instructions": "How urgent is the request?",
            "levels": [{"label": "first"}, {"label": "second"}],
        },
    ]
    return inputs, questions


def _check_openai(result: Any) -> None:
    _check(result.model == "openai", f"OpenAI Decisions: model={result.model!r}")
    _check([answer.type for answer in result.answers] == ["predicate", "choice", "refusal"], "OpenAI answer order changed")
    predicate, choice, refusal = result.answers
    _check(predicate.name is None and predicate.probability == 0, "OpenAI predicate name/probability changed")
    _check(choice.name == "typed" and type(choice.choice) is bool and choice.choice is True, "OpenAI boolean choice lost its type")
    _check(
        [(item.value, item.probability) for item in choice.probabilities]
        == [(True, 0), ("true", 1)],
        "OpenAI typed choice probabilities changed",
    )
    _check(refusal.name is None, "OpenAI refusal name should remain null")

    usage = result.usage
    _check(usage.input_tokens == 2048, "OpenAI input usage changed")
    _check(usage.input_tokens_details.cached_tokens == 1024, "OpenAI cached usage changed")
    _check(usage.input_tokens_details.cache_write_tokens == 512, "OpenAI cache-write usage changed")
    _check(usage.output_tokens == 0, "OpenAI output usage changed")
    _check(usage.output_tokens_details.reasoning_tokens == 0, "OpenAI reasoning usage changed")
    _check(usage.total_tokens == 2048, "OpenAI total usage changed")


def _run(root: str, key: str) -> dict[str, Any]:
    versions = _versions()
    import typesafe_sdk
    from openai import OpenAI

    checks: dict[str, bool] = {}
    for alias in ("direct", "command"):
        with typesafe_sdk.TypeSafeClient(
            api_key=key,
            base_url=root,
            model=alias,
            compression=False,
        ) as client:
            response = client.system_one(
                state=_system_state(alias),
                questions=_system_questions(typesafe_sdk),
            )
        _check_system(response, alias)
        checks[f"typesafe_{alias}"] = True

    inputs, questions = _openai_request()
    with OpenAI(api_key=key, base_url=f"{root}/v1", max_retries=0) as client:
        response = client.decisions.create(
            model="openai",
            input=inputs,
            questions=questions,
            prompt_cache_key="synthetic-billing-v1",
            prompt_cache_options={"mode": "implicit", "ttl": "30m"},
        )
    _check_openai(response)
    checks["openai_decisions"] = True
    return {"checks": checks, "sdk_versions": versions}


def main(argv: Iterable[str] | None = None) -> int:
    try:
        args = _parser().parse_args(argv)
        root = _root(args.root)
        key = os.environ.get("AXON_NATIVE_TEST_KEY")
        if not key:
            raise RuntimeError("AXON_NATIVE_TEST_KEY must be set; it is never accepted in argv")
        print(json.dumps(_run(root, key), sort_keys=True, separators=(",", ":")))
        return 0
    except (AssertionError, RuntimeError, ValueError) as exc:
        print(f"native SDK acceptance failed: {exc}", file=sys.stderr)
        return 1
    except Exception as exc:
        print(f"native SDK request failed: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
