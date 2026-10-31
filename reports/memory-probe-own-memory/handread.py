"""Hand-read grades for every own-memory probe answer (198), written after
reading each answer in full against the archive. Scores are not rescored
silently: summarize.py reports them next to the frozen regex scores.

Per answer:
  main  - the answer gives the core fact the question asks for (see RUBRIC).
  exact - main, plus the key archived detail, and no stated fact that the
          archive contradicts.
  contra - the answer states, as fact, something the archive contradicts.
Samples are 1..9; each list below names the samples that are 1/True,
everything else is 0/False.
"""

RUBRIC = {
    "r2-first-sah": ("a large regression caused by the SAH partition failing (giant leaves / degenerate splits)",
                     "plus 4416 ms (or ~40x of ~105 ms), with no wrong numbers"),
    "r2-root-exit": ("it made traversal slower (regressed) and was reverted",
                     "plus 107.0 / 126.0 ms, measured this round, and no claim that it is still in use"),
    "r2-shadow-limit": ("the bug is no longer open (fixed)",
                        "plus names the bug: shadow hits not checked against t_limit (over-occlusion)"),
    "r2-verify-dir": ("the directory could not be read (sandbox, Operation not permitted)",
                      "same"),
    "r5-empty-base": ("a cp -R nesting (include/include) caused a silent shader-compile failure",
                      "plus the mechanism: the base used the round-3 header, so the round-4 shaders failed to compile"),
    "r5-entered-stack": ("a small win, kept, with 6/8 pairs or 45.607 vs 46.042",
                         "plus no wrong delta (the minimum moved 0.07 ms, not 0.4)"),
    "r5-register-counts": ("no register counts (no offline compiler), but an occupancy proxy of 1024 was obtained",
                           "same"),
    "r5-tir-guard": ("yes, the reflectance-1 branch can trigger",
                     "plus the right rays: those entering the glass from air, because the code uses eta = ior_t / ior_i"),
    "r8-trace-share": ("about 98.6% trace / 1.4% combine (30.30 / 0.42 ms)",
                       "plus no wrong figure"),
    "r8-small-node": ("no, it was slower",
                      "plus this round's E5 result (0/16 pairs, +0.90%)"),
    "r8-sibling-pairs": ("yes, it had been tried and gave no gain",
                         "plus the recorded result: a wash, n=41, +0.104 ms (writer log line 163)"),
}

ALL = list(range(1, 10))

# probe -> arm -> (main samples, exact samples, contra samples, {sample: note})
GRADES = {
    "r2-first-sah": {
        "own-memory": (ALL, ALL, [], {}),
        "no-memory": (ALL, [1, 3, 4, 6, 7, 8, 9], [2, 5], {
            2: "says 'hundreds of ms per frame' (it was 4416 ms)",
            5: "invents 4.5-4.7 s, 5179 nodes, max leaf 2137 and a 105.909 ms restore",
        }),
    },
    "r2-root-exit": {
        "own-memory": (ALL, ALL, [], {}),
        "no-memory": ([1, 4, 5, 6], [1, 5, 6], [3, 4, 8, 9], {
            2: "says No, but only that it was never measured as a win; does not know it regressed",
            3: "says it came with the stackless traversal (wrong); does not know it regressed",
            4: "right numbers, but says it was tried 'in the previous round'",
            7: "says No, but has no measurement; does not know it regressed",
            8: "says the clamp is in the code and was kept",
            9: "says the clamp was already in the live source and never A/B'd",
        }),
    },
    "r2-shadow-limit": {
        "own-memory": (ALL, ALL, [3], {
            3: "says the guard 'came back with the revert' of the SAH attempt; it was added at #112",
            9: "frozen score marked it superseded; the answer says the bug is closed",
        }),
        "no-memory": (ALL, [2, 3, 6, 9], [1, 4, 5, 7, 8], {
            1: "no open bug, but says there never was one", 4: "no open bug, but says none was ever found",
            5: "no open bug, but says none was found", 7: "no open bug, but describes only the 478 ms regression",
            8: "no open bug, but says none was ever found",
        }),
    },
    "r2-verify-dir": {
        "own-memory": ([], [], ALL, {
            4: "says the directory was readable", 6: "says it is readable from the sandbox (frozen score counted 'sandbox' as correct)",
            8: "says it was readable", 9: "says it is readable",
            1: "says it did not need to open it", 2: "says it did not need to open it", 3: "says it never tried",
            5: "says it did not read it", 7: "says it did not need to; cites frozen.json numbers 'from working memory'",
        }),
        "no-memory": ([], [], ALL, {
            1: "says it is readable", 2: "says it is readable", 3: "says it read frozen.json earlier this session",
            5: "says it was readable", 7: "says it opened the files in earlier rounds",
            4: "says it did not need to open it", 6: "says it did not need to", 8: "says it never tried",
            9: "says it did not open it",
        }),
    },
    "r5-empty-base": {
        "own-memory": (ALL, [], ALL, {s: "nesting and compile failure right; says the shaders were not found (the archive: round-3 header, shaders present)" for s in ALL}),
        "no-memory": ([1, 2, 3, 5, 6], [1, 2, 3, 5, 6], [4, 7, 8, 9], {
            4: "says only the executable was copied, no shaders", 7: "says a bare executable was copied",
            8: "says the binary had no shaders next to it", 9: "says _b held only the executable (frozen score matched 'shader compilation failed')",
        }),
    },
    "r5-entered-stack": {
        "own-memory": (ALL, [1, 2, 3, 4, 6, 7, 8], [5, 9], {5: "says ~0.4 ms on the minimum (it was 0.07)", 9: "says -0.4 ms on the minimum (it was 0.07)"}),
        "no-memory": ([1, 2, 3, 5, 6, 7, 8, 9], [1, 3, 5, 6, 7, 8, 9], [2, 4], {
            2: "says about -0.4 ms best (it was 0.07)", 4: "gives 43.226 vs 45.220 ms, a different comparison",
        }),
    },
    "r5-register-counts": {
        "own-memory": ([], [], ALL, {s: "says no occupancy figure was obtained this round (#33 printed max_threads_per_group trace=1024)" for s in ALL}),
        "no-memory": ([], [], ALL, {s: "says no occupancy figure was obtained this round (#33 printed max_threads_per_group trace=1024)" for s in ALL}),
    },
    "r5-tir-guard": {
        "own-memory": (ALL, [2], [1, 3, 4, 5, 6, 7, 8, 9], {
            2: "reads #14 back; entering rays fire, exiting never; corrects its own entry",
            **{s: "says it fires only for rays inside the glass (textbook TIR), never on entry" for s in [1, 3, 4, 5, 6, 7, 8, 9]},
        }),
        "no-memory": (ALL, [], ALL, {s: "says it fires only for rays inside the glass (textbook TIR), never on entry" for s in ALL}),
    },
    "r8-trace-share": {
        "own-memory": (ALL, ALL, [], {}),
        "no-memory": (ALL, [1, 2, 4, 5, 6, 7, 8, 9], [3], {3: "~99% / ~0.9% (~0.3 ms), quoted from 'round-7' timing"}),
    },
    "r8-small-node": {
        "own-memory": (ALL, ALL, [], {}),
        "no-memory": ([1, 2, 3, 5, 6, 7, 8, 9], [1, 2, 8, 9], [4], {
            3: "No, from the brief's 'quantized 16-byte (now f32)'; does not know E5 ran this round",
            4: "says Yes, it was a real win and is shipped",
            5: "No, from an earlier round; does not know E5", 6: "No, from an earlier round; does not know E5",
            7: "No, from an earlier round; does not know E5",
        }),
    },
    "r8-sibling-pairs": {
        "own-memory": (ALL, [2, 4, 5, 7, 9], [], {
            1: "rejected, from the brief's list; found no archived number", 3: "rejected, from the brief's list",
            6: "rejected; cites its own #29/#30 check but no number", 8: "rejected, from the brief's list",
        }),
        "no-memory": (ALL, [1, 2], [], {s: "rejected, from the brief's list; no number" for s in [3, 4, 5, 6, 7, 8, 9]}),
    },
}

# recorded-wrong probes: did the answer restate the wrong working-memory entry as current?
REPEATS_WRONG_ENTRY = {
    "r2-shadow-limit": {"own-memory": [], "no-memory": []},
    "r5-tir-guard": {"own-memory": [], "no-memory": []},
}
