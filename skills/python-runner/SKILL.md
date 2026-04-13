# Python Runner

Use this skill when you need to execute small Python programs from AOS.

## Rule

Use the `run` tool, not `execute_code`.

## Supported pattern

Run Python inline through the shell with `python3 -c`.

Example:

```sh
python3 -c "print('hello from python')"
```

## For multi-line Python

Prefer a triple-quoted string inside `-c` when possible.

Example:

```sh
python3 -c "
from pathlib import Path
text = Path('/workspace/in.txt').read_text()
Path('/workspace/out.txt').write_text(text.upper())
"
```

## Safer file-oriented pattern

When the program is larger:

1. write a `.py` file with `write_file`
2. run it with `run`
3. inspect stdout, stderr, and exit code
4. revise if needed

Example:

```sh
python3 /workspace/script.py
```

## Expectations

- Treat non-zero exit codes as failed attempts.
- If one Python approach fails, revise the script or command instead of repeating it unchanged.
- Use mounted workspace paths only.
