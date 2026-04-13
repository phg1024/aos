# C++ Runner

Use this skill when you need to compile and run small C++ programs from AOS.

## Rule

Use the `run` tool, not a special compiler tool.

## Preferred compilers

Try `g++` first unless the environment or project already prefers `clang++`.

Common compile patterns:

```sh
g++ -std=c++17 /workspace/main.cpp -O2 -o /workspace/main
```

```sh
clang++ -std=c++20 /workspace/main.cpp -O2 -o /workspace/main
```

## Run pattern

After a successful compile, run the produced binary:

```sh
/workspace/main
```

## Safer file-oriented pattern

When the program is larger:

1. write a `.cpp` file with `write_file`
2. compile it with `run`
3. inspect stdout, stderr, and exit code
4. run the produced binary with `run`
5. revise if needed

## Single-command pattern

For quick experiments, compile and run in one shell command:

```sh
g++ -std=c++17 /workspace/main.cpp -O2 -o /workspace/main && /workspace/main
```

## Expectations

- Treat compiler errors and non-zero exit codes as failed attempts.
- If compilation fails, revise the source or flags instead of repeating the same command unchanged.
- Prefer writing the binary into the mounted workspace, not into system directories.
- Use mounted workspace paths only.
- If the code depends on newer language features, switch from `-std=c++17` to `-std=c++20` or later deliberately.
