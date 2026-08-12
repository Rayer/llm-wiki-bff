#!/usr/bin/env python3
"""Close the LWC-174 request schema without reserializing Swagger artifacts."""

import argparse
import json
import re
import tempfile
from pathlib import Path


TARGET = "v1.renameProjectRequest"


class PostprocessError(RuntimeError):
    pass


def _no_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise PostprocessError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def _json_value(data, label):
    try:
        return json.loads(data, object_pairs_hook=_no_duplicate_keys)
    except (json.JSONDecodeError, PostprocessError) as exc:
        raise PostprocessError(f"invalid {label}: {exc}") from exc


def _target_json_body(text, label):
    matches = list(re.finditer(r'(?m)^(\s*)"' + re.escape(TARGET) + r'"\s*:\s*\{', text))
    if len(matches) != 1:
        raise PostprocessError(f"{label}: expected one {TARGET} definition, found {len(matches)}")
    match = matches[0]
    start = match.end() - 1
    depth = 0
    in_string = False
    escaped = False
    end = None
    for index in range(start, len(text)):
        char = text[index]
        if in_string:
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                in_string = False
        elif char == '"':
            in_string = True
        elif char == "{":
            depth += 1
        elif char == "}":
            depth -= 1
            if depth == 0:
                end = index + 1
                break
    if end is None:
        raise PostprocessError(f"{label}: unterminated {TARGET} definition")
    return match, start, end, text[start:end]


def _patch_json(text, label, validate_document=True):
    definitions = None
    if validate_document:
        document = _json_value(text, label)
        definitions = document.get("definitions")
        if not isinstance(definitions, dict) or TARGET not in definitions or not isinstance(definitions[TARGET], dict):
            raise PostprocessError(f"{label}: missing {TARGET} definition")
    match, start, end, body = _target_json_body(text, label)
    parsed_body = _json_value(body, f"{label} {TARGET}")
    if not isinstance(parsed_body, dict) or (validate_document and parsed_body != definitions[TARGET]):
        raise PostprocessError(f"{label}: ambiguous {TARGET} definition")

    property_matches = list(re.finditer(r'(?m)^\s*"additionalProperties"\s*:\s*(true|false)\s*,?\s*$', body))
    if len(property_matches) > 1:
        raise PostprocessError(f"{label}: ambiguous additionalProperties in {TARGET}")
    if property_matches:
        if property_matches[0].group(1) == "false":
            return text
        replacement = property_matches[0].group(0).replace("true", "false", 1)
        body = body[:property_matches[0].start()] + replacement + body[property_matches[0].end():]
    else:
        newline = "\n"
        next_line = re.search(r"\n(\s+)\"", body)
        if not next_line:
            raise PostprocessError(f"{label}: cannot anchor additionalProperties in {TARGET}")
        indent = next_line.group(1)
        body = "{" + newline + indent + '"additionalProperties": false,' + body[1:]
    return text[:start] + body + text[end:]


def _patch_yaml(text):
    lines = text.splitlines(keepends=True)
    starts = [index for index, line in enumerate(lines) if re.match(r"^  " + re.escape(TARGET) + r":\s*$", line)]
    if len(starts) != 1:
        raise PostprocessError(f"swagger.yaml: expected one {TARGET} definition, found {len(starts)}")
    start = starts[0]
    end = next((index for index in range(start + 1, len(lines)) if re.match(r"^  \S", lines[index])), len(lines))
    block = lines[start:end]
    property_lines = [index for index, line in enumerate(block) if re.match(r"^    additionalProperties:\s*", line)]
    if len(property_lines) > 1:
        raise PostprocessError(f"swagger.yaml: ambiguous additionalProperties in {TARGET}")
    if property_lines:
        index = property_lines[0]
        if re.fullmatch(r"    additionalProperties:\s*false\s*\n?", block[index]):
            return text
        if not re.fullmatch(r"    additionalProperties:\s*(?:true|false)\s*\n?", block[index]):
            raise PostprocessError(f"swagger.yaml: invalid additionalProperties in {TARGET}")
        block[index] = re.sub(r"(additionalProperties:\s*)(?:true|false)", r"\1false", block[index], count=1)
    else:
        block.insert(1, "    additionalProperties: false\n")
    return "".join(lines[:start] + block + lines[end:])


def _patch_docs_go(text):
    prefix = "const docTemplate = `"
    suffix = "`\n\n// SwaggerInfo holds exported Swagger Info"
    if text.count(prefix) != 1 or text.count(suffix) != 1:
        raise PostprocessError("docs.go: cannot locate unique docTemplate")
    start = text.index(prefix) + len(prefix)
    end = text.index(suffix, start)
    template = text[start:end]
    patched = _patch_json(template, "docs.go", validate_document=False)
    return text if patched == template else text[:start] + patched + text[end:]


def _replace(path, data):
    if path.read_bytes() == data:
        return
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as output:
        output.write(data)
        temporary = Path(output.name)
    temporary.replace(path)


def process(docs_dir):
    json_path = docs_dir / "swagger.json"
    yaml_path = docs_dir / "swagger.yaml"
    go_path = docs_dir / "docs.go"
    try:
        json_data = json_path.read_text()
        yaml_data = yaml_path.read_text()
        go_data = go_path.read_text()
        json_patched = _patch_json(json_data, "swagger.json")
        yaml_patched = _patch_yaml(yaml_data)
        go_patched = _patch_docs_go(go_data)
    except OSError as exc:
        raise PostprocessError(str(exc)) from exc
    _replace(json_path, json_patched.encode())
    _replace(yaml_path, yaml_patched.encode())
    _replace(go_path, go_patched.encode())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--docs-dir", type=Path, default=Path("docs"))
    args = parser.parse_args()
    try:
        process(args.docs_dir)
    except PostprocessError as exc:
        parser.error(str(exc))


if __name__ == "__main__":
    main()
