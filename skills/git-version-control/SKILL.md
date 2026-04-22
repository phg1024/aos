---
name: git-version-control
description: Use when an agent should inspect repo state, review diffs, preserve version history, create branches or commits non-interactively, and generally use git safely through the run tool.
---

# Git Version Control

Use this skill when working inside a git repository and you need structured version-control behavior.

## Rule

Use the `run` tool with non-interactive git commands.

Do not use destructive git commands unless the user explicitly asked for them.

## Safe inspection pattern

Start by understanding the repo state:

```sh
git status --short
```

```sh
git branch --show-current
```

```sh
git diff --stat
```

Use more detailed inspection only as needed:

```sh
git diff -- path/to/file
```

```sh
git log --oneline -n 10
```

## Safe snapshot pattern

When you want to preserve a meaningful checkpoint:

1. inspect `git status --short`
2. inspect `git diff --stat`
3. stage only the intended files
4. create a concise commit with `git commit -m`

Example:

```sh
git add path/to/file1 path/to/file2
git commit -m "Improve rasterizer benchmark logging"
```

## Branch pattern

If a task needs isolation from the current branch, use a simple non-interactive branch flow:

```sh
git switch -c feature/my-change
```

If the branch already exists:

```sh
git switch feature/my-change
```

## Comparison pattern

When comparing experimental versions:

```sh
git diff -- path/to/file
```

```sh
git show HEAD:path/to/file
```

```sh
git log --oneline -- path/to/file
```

## Expectations

- Prefer `git status --short`, `git diff --stat`, and file-scoped `git diff` before making a commit.
- Stage only the files relevant to the current task.
- Use non-interactive commands only.
- Preserve unrelated user changes; do not revert or overwrite them.
- If the worktree is dirty, understand the existing changes before acting.
- If committing, use a short factual commit message.

## Never do these unless explicitly requested

- `git reset --hard`
- `git checkout -- <file>`
- `git clean -fd`
- history rewrites such as `git rebase -i`
- force pushes

## Good use cases

- record a successful experiment round
- inspect what changed between optimization attempts
- preserve a working implementation before a risky change
- create a branch for a larger refactor
- review file history to recover a prior working version safely
