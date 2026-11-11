#!/usr/bin/env python3
"""Synchronize non-sensitive configuration from .env.prod into Helm values.yaml
and verify zero deployment drift.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path
from typing import Dict, List, Optional, Tuple

import yaml

DEFAULT_ENV_FILE = Path(".env.prod")
DEFAULT_VALUES_FILE = Path("deployments/helm/rag-prompt-registry/values.yaml")
SENSITIVE_KEY_MARKERS = ("PASSWORD", "SECRET", "TOKEN", "KEY", "DSN", "CREDENTIAL", "PRIVATE")


def configure_utf8_stdio() -> None:
    """Ensure UTF-8 encoding for stdout/stderr on Windows consoles."""
    for stream in (sys.stdout, sys.stderr):
        reconfigure = getattr(stream, "reconfigure", None)
        if callable(reconfigure):
            try:
                reconfigure(encoding="utf-8")
            except Exception:
                pass


def is_sensitive_key(key: str) -> bool:
    """Return True if key represents credentials or secrets excluded from values.yaml."""
    upper = key.strip().upper()
    return any(marker in upper for marker in SENSITIVE_KEY_MARKERS)


def parse_env_value(raw: str, line_num: int) -> str:
    """Parse single-quoted, double-quoted, or unquoted .env values."""
    if not raw or raw.startswith("#"):
        return ""

    if raw[0] == "'":
        end_idx = raw.find("'", 1)
        if end_idx < 0:
            raise ValueError(f"unterminated single-quoted value at line {line_num}")
        rest = raw[end_idx + 1 :].strip()
        if rest and not rest.startswith("#"):
            raise ValueError(f"unexpected trailing characters at line {line_num}")
        return raw[1:end_idx]

    if raw[0] == '"':
        chars: List[str] = []
        escaped = False
        closed_at: Optional[int] = None
        for idx in range(1, len(raw)):
            ch = raw[idx]
            if escaped:
                escape_map = {"n": "\n", "r": "\r", "t": "\t", '"': '"', "\\": "\\"}
                chars.append(escape_map.get(ch, "\\" + ch))
                escaped = False
                continue
            if ch == "\\":
                escaped = True
                continue
            if ch == '"':
                closed_at = idx
                break
            chars.append(ch)
        if closed_at is None:
            raise ValueError(f"unterminated double-quoted value at line {line_num}")
        rest = raw[closed_at + 1 :].strip()
        if rest and not rest.startswith("#"):
            raise ValueError(f"unexpected trailing characters at line {line_num}")
        return "".join(chars)

    for idx, ch in enumerate(raw):
        if ch == "#" and idx > 0 and raw[idx - 1] in (" ", "\t"):
            return raw[:idx].strip()
    return raw.strip()


def parse_env_file(env_path: Path) -> Tuple[Dict[str, str], List[str]]:
    """Parse an env file into ordered non-sensitive variables and skipped sensitive key names."""
    if not env_path.is_file():
        raise FileNotFoundError(f"Environment source file not found: {env_path}")

    raw_text = env_path.read_text(encoding="utf-8-sig")
    non_sensitive: Dict[str, str] = {}
    skipped_sensitive: List[str] = []

    for line_num, raw_line in enumerate(raw_text.splitlines(), start=1):
        stripped = raw_line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if stripped.startswith("export ") or stripped.startswith("export\t"):
            stripped = stripped[len("export") :].strip()

        if "=" not in stripped:
            raise ValueError(f"invalid .env syntax at line {line_num} in {env_path}")

        key_part, val_part = stripped.split("=", 1)
        key = key_part.strip()
        if not key or any(c in key for c in (" ", "\t")):
            raise ValueError(f"invalid variable name at line {line_num} in {env_path}")

        value = parse_env_value(val_part.lstrip(" \t"), line_num)
        if is_sensitive_key(key):
            skipped_sensitive.append(key)
            continue
        non_sensitive[key] = value

    return non_sensitive, skipped_sensitive


def normalize_yaml_env_value(val: object) -> Optional[str]:
    """Normalize YAML scalar values into strings comparable with .env string values."""
    if val is None:
        return None
    if isinstance(val, bool):
        return "true" if val else "false"
    return str(val)


def find_block_range(lines: List[str], block_key: str) -> Optional[Tuple[int, int]]:
    """Locate [start_idx, end_idx) of a top-level YAML mapping block while preserving trailing comments."""
    header_re = re.compile(rf"^{re.escape(block_key)}:\s*(?:#.*)?$")
    start_idx: Optional[int] = None
    for idx, line in enumerate(lines):
        if header_re.match(line):
            start_idx = idx
            break
    if start_idx is None:
        return None

    end_idx = start_idx + 1
    last_content_idx = start_idx + 1
    while end_idx < len(lines):
        line = lines[end_idx]
        if line.strip() == "":
            end_idx += 1
            continue
        if line[0] in (" ", "\t"):
            end_idx += 1
            last_content_idx = end_idx
            continue
        break

    return start_idx, last_content_idx


def format_yaml_string(val: str) -> str:
    """Format a scalar string safely as a double-quoted YAML scalar."""
    return json.dumps(val, ensure_ascii=False)


def build_env_block(non_sensitive: Dict[str, str], secret_name: Optional[str]) -> List[str]:
    """Render the Helm values.yaml `env:` block preserving secretName."""
    out = ["env:"]
    if secret_name is None:
        out.append('  secretName: ""')
    else:
        out.append(f"  secretName: {format_yaml_string(secret_name)}")

    for key, val in non_sensitive.items():
        if key == "secretName":
            continue
        out.append(f"  {key}: {format_yaml_string(val)}")
    return out


def check_drift(env_path: Path, values_path: Path) -> int:
    """Compare .env.prod against values.yaml env block. Returns 0 if clean, 1 on drift."""
    non_sensitive, _ = parse_env_file(env_path)
    if not values_path.is_file():
        print(f"ERROR: Helm values file not found: {values_path}", file=sys.stderr)
        return 1

    values_data = yaml.safe_load(values_path.read_text(encoding="utf-8")) or {}
    if not isinstance(values_data, dict):
        print(f"ERROR: {values_path} root must be a YAML mapping", file=sys.stderr)
        return 1

    helm_env_raw = values_data.get("env")
    if not isinstance(helm_env_raw, dict):
        print(
            f"DRIFT DETECTED: '{values_path}' is missing the top-level 'env:' mapping.\n"
            f"Run: python scripts/sync_env_to_deployments.py --sync",
            file=sys.stderr,
        )
        return 1

    helm_env: Dict[str, Optional[str]] = {
        str(k): normalize_yaml_env_value(v) for k, v in helm_env_raw.items() if str(k) != "secretName"
    }

    missing_keys = [k for k in non_sensitive if k not in helm_env]
    extra_keys = [k for k in helm_env if k not in non_sensitive]
    mismatched_keys = [
        k for k, expected in non_sensitive.items() if k in helm_env and helm_env[k] != expected
    ]

    if not missing_keys and not extra_keys and not mismatched_keys:
        print(f"OK: {values_path} is in sync with {env_path} ({len(non_sensitive)} keys).")
        return 0

    print(f"DRIFT DETECTED between {env_path} and {values_path}:", file=sys.stderr)
    for key in missing_keys:
        print(f"  - Missing in values.yaml env: {key}", file=sys.stderr)
    for key in extra_keys:
        print(f"  - Stale key in values.yaml env (absent from {env_path}): {key}", file=sys.stderr)
    for key in mismatched_keys:
        print(
            f"  - Value mismatch for {key}: {env_path}={non_sensitive[key]!r} vs values.yaml={helm_env[key]!r}",
            file=sys.stderr,
        )
    print("\nRun `python scripts/sync_env_to_deployments.py --sync` to synchronize.", file=sys.stderr)
    return 1


def sync_values(env_path: Path, values_path: Path) -> int:
    """Update values.yaml `env:` block from .env.prod while preserving all other Helm blocks."""
    non_sensitive, skipped_sensitive = parse_env_file(env_path)
    if not values_path.is_file():
        raise FileNotFoundError(f"Helm values file not found: {values_path}")

    original_text = values_path.read_text(encoding="utf-8")
    values_data = yaml.safe_load(original_text) or {}
    if not isinstance(values_data, dict):
        raise ValueError(f"{values_path} root must be a YAML mapping")

    existing_secret_name: Optional[str] = None
    existing_env = values_data.get("env")
    if isinstance(existing_env, dict) and "secretName" in existing_env and existing_env["secretName"] is not None:
        existing_secret_name = str(existing_env["secretName"])

    lines = original_text.splitlines()
    block_range = find_block_range(lines, "env") or find_block_range(lines, "config")
    new_env_lines = build_env_block(non_sensitive, existing_secret_name)

    if block_range is not None:
        start_idx, end_idx = block_range
        updated_lines = lines[:start_idx] + new_env_lines + lines[end_idx:]
    else:
        updated_lines = list(lines)
        if updated_lines and updated_lines[-1].strip() != "":
            updated_lines.append("")
        updated_lines.extend(new_env_lines)

    updated_text = "\n".join(updated_lines) + "\n"
    # Validate resulting YAML before persisting.
    yaml.safe_load(updated_text)
    values_path.write_text(updated_text, encoding="utf-8")

    print(
        f"SYNCED: Updated {values_path} from {env_path} "
        f"({len(non_sensitive)} env keys synced, {len(skipped_sensitive)} sensitive keys excluded)."
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    """Construct CLI argument parser."""
    parser = argparse.ArgumentParser(
        description="Sync .env.prod into Helm values.yaml and validate deployment config drift."
    )
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--sync", action="store_true", help="Update Helm values.yaml from .env.prod")
    mode.add_argument(
        "--check-only",
        action="store_true",
        help="Verify zero drift between .env.prod and Helm values.yaml",
    )
    parser.add_argument("--env-file", type=Path, default=DEFAULT_ENV_FILE, help="Source .env.prod path")
    parser.add_argument("--values-file", type=Path, default=DEFAULT_VALUES_FILE, help="Target Helm values.yaml path")
    return parser


def main() -> int:
    """CLI entry point."""
    configure_utf8_stdio()
    args = build_parser().parse_args()
    try:
        if args.sync:
            return sync_values(args.env_file, args.values_file)
        return check_drift(args.env_file, args.values_file)
    except Exception as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())

