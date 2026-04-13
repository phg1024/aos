#!/usr/bin/env python3
import argparse
import json
import sys


def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True)
    parser.add_argument("--local-only", action="store_true", default=True)
    args = parser.parse_args()

    try:
        from transformers import AutoTokenizer
    except Exception as exc:
        emit({"error": f"transformers import failed: {exc}"})
        return 1

    try:
        tokenizer = AutoTokenizer.from_pretrained(
            args.model,
            trust_remote_code=True,
            local_files_only=args.local_only,
        )
    except Exception as exc:
        emit({"error": f"tokenizer load failed for {args.model}: {exc}"})
        return 1

    emit({"ready": True})

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
            messages = req.get("messages", [])
            exact = True
            try:
                if hasattr(tokenizer, "apply_chat_template") and tokenizer.chat_template:
                    tokens = tokenizer.apply_chat_template(
                        messages,
                        tokenize=True,
                        add_generation_prompt=False,
                    )
                    count = len(tokens)
                else:
                    exact = False
                    text = "\n".join(
                        f"{message.get('role', '')}: {message.get('content', '')}"
                        for message in messages
                    )
                    count = len(tokenizer.encode(text, add_special_tokens=True))
            except Exception:
                exact = False
                text = "\n".join(
                    f"{message.get('role', '')}: {message.get('content', '')}"
                    for message in messages
                )
                count = len(tokenizer.encode(text, add_special_tokens=True))
            emit({"tokens": count, "exact": exact})
        except Exception as exc:
            emit({"error": str(exc)})
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
