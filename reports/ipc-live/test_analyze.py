#!/usr/bin/env python3
"""Tests analyze.py on made-up state directories, and on live2's own state where it is here. Usage: python3 test_analyze.py"""
import hashlib, json, os, shutil, subprocess, sys, tempfile, unittest

ANALYZE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "analyze.py")
FOREIGN = "/home/elsewhere/aos-ipc-live/reports/ipc-live/runs/gone/state/artifacts/payloads"
PAYLOADS = {
    "art-000001": ("tool_result", {"tool": "message_agent", "output": {"to": "pid-000002", "body": "the numbers", "artifacts": "art-000002"}}),
    "art-000002": ("published.raw_timings", {"body": "twelve bytes"}),
    "art-000003": ("tool_result", {"tool": "read_artifact", "output": {"artifact_id": "art-000002", "offset": 0, "next_offset": 0}}),
    "art-000004": ("verification", {"status": "pass", "reason": "the numbers hold"}),
}


def write_state(root, index_dir=FOREIGN):
    for sub in ("artifacts/payloads", "kernel", "run"):
        os.makedirs(os.path.join(root, sub))
    index = {}
    for artifact, (kind, payload) in PAYLOADS.items():
        with open(os.path.join(root, "artifacts", "payloads", artifact + ".json"), "w") as f:
            json.dump(payload, f)
        index[artifact] = {"id": artifact, "type": kind, "producer_pid": "pid-000001", "created_at": "2026-11-06T07:00:0" + artifact[-1] + "Z",
                           "path": os.path.join(index_dir, artifact + ".json")}
    with open(os.path.join(root, "artifacts", "index.json"), "w") as f:
        json.dump(index, f)
    processes = [{"pid": "pid-000001", "metadata": {"agent_id": "writer"}}, {"pid": "pid-000002", "metadata": {"agent_id": "verifier"}}]
    with open(os.path.join(root, "kernel", "processes.json"), "w") as f:
        json.dump(processes, f)
    with open(os.path.join(root, "kernel", "events.json"), "w") as f:
        json.dump([], f)


def run(root):
    return subprocess.run([sys.executable, "-I", ANALYZE, root], capture_output=True, text=True)


class Copied(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir)
        self.state = os.path.join(self.dir, "state")
        write_state(self.state)

    def test_payloads_come_from_the_given_directory(self):
        result = run(self.state)
        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertIn("art-000002 (published.raw_timings) from writer (pid-000001) to verifier (pid-000002)", result.stdout)
        self.assertIn("published: art-000002 published.raw_timings by writer (pid-000001), 12 bytes", result.stdout)
        self.assertIn("verdict art-000004: pass: the numbers hold", result.stdout)
        self.assertIn("exercised 1      a handle attached to mail", result.stdout)
        self.assertNotIn("no handle was attached", result.stdout)

    def test_the_same_files_give_the_same_answer_anywhere(self):
        moved = os.path.join(self.dir, "somewhere", "else")
        os.makedirs(os.path.dirname(moved))
        shutil.copytree(self.state, moved)
        here, there = run(self.state), run(moved)
        self.assertEqual(here.returncode, 3, here.stderr)
        self.assertEqual(here.stdout.split("\n", 1)[1], there.stdout.split("\n", 1)[1])

    def test_a_path_that_exists_elsewhere_is_not_read(self):
        other = os.path.join(self.dir, "other-run")
        os.makedirs(other)
        with open(os.path.join(other, "art-000004.json"), "w") as f:
            json.dump({"status": "fail", "reason": "another run's verdict"}, f)
        shutil.rmtree(self.state)
        write_state(self.state, other)
        self.assertIn("verdict art-000004: pass", run(self.state).stdout)

    def test_a_missing_payload_is_an_error_naming_it(self):
        os.remove(os.path.join(self.state, "artifacts", "payloads", "art-000002.json"))
        result = run(self.state)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("art-000002", result.stderr)
        self.assertNotIn("Traceback", result.stderr)
        self.assertNotIn("Contract situations", result.stdout)

    def test_an_unreadable_payload_is_an_error_naming_it(self):
        with open(os.path.join(self.state, "artifacts", "payloads", "art-000004.json"), "w") as f:
            f.write('{"status": "pa')
        result = run(self.state)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("art-000004", result.stderr)
        self.assertNotIn("Traceback", result.stderr)


RECORD = "render_ms_best=2.64186\n" * 4
SHA = hashlib.sha256(RECORD.encode()).hexdigest()
PATH = "/home/elsewhere/runs/gone/cuda-bench/raw_record.txt"
PROCESSES = [
    {"pid": "pid-000001", "metadata": {"agent_id": "coordinator"}},
    {"pid": "pid-000002", "parent_pid": "pid-000001", "metadata": {"agent_id": "benchmarker"}},
    {"pid": "pid-000003", "parent_pid": "pid-000001", "metadata": {"agent_id": "verifier"}},
    {"pid": "pid-000004", "parent_pid": "pid-000001", "metadata": {"agent_id": "verifier"}},
]


def page(artifact, offset, next_offset=None, pid="pid-000004"):
    output = {"artifact_id": artifact, "offset": str(offset), "total_bytes": "92", "body": "x"}
    if next_offset:
        output["next_offset"] = str(next_offset)
    return ("tool_result", pid, {"tool": "read_artifact", "output": output}, {})


FILE_META = {"kind": "published", "source": "file", "source_path": PATH, "source_sha256": SHA}
RECORD_ART = ("published.raw_timings", "pid-000002", {"body": RECORD}, FILE_META)
TYPED_ART = ("published.raw_timings", "pid-000002", {"body": RECORD}, {"kind": "published"})
VERDICT = ("verification", "pid-000001", {"status": "pass", "reason": "held"}, {"verifier_pid": "pid-000004"})


class Evidence(unittest.TestCase):
    def state(self, records, report=None, on_disk=RECORD, calls=()):
        """records: (type, producer, payload, metadata), numbered in order."""
        run_dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, run_dir)
        root = os.path.join(run_dir, "state")
        for sub in ("artifacts/payloads", "kernel", "run"):
            os.makedirs(os.path.join(root, sub))
        index = {}
        for number, (kind, producer, payload, metadata) in enumerate(records, 1):
            artifact = f"art-{number:06d}"
            with open(os.path.join(root, "artifacts", "payloads", artifact + ".json"), "w") as f:
                json.dump(payload, f)
            index[artifact] = {"id": artifact, "type": kind, "producer_pid": producer, "metadata": metadata, "created_at": f"2026-11-06T07:{number:02d}:00Z"}
        with open(os.path.join(root, "artifacts", "index.json"), "w") as f:
            json.dump(index, f)
        with open(os.path.join(root, "kernel", "processes.json"), "w") as f:
            json.dump(PROCESSES, f)
        with open(os.path.join(root, "kernel", "events.json"), "w") as f:
            json.dump([], f)
        with open(os.path.join(root, "run", "calls.jsonl"), "w") as f:
            for call in calls:
                f.write(json.dumps(call) + "\n")
        if on_disk is not None:
            os.makedirs(os.path.join(run_dir, "cuda-bench"))
            with open(os.path.join(run_dir, "cuda-bench", "raw_record.txt"), "w") as f:
                f.write(on_disk)
        if report is not None:
            os.makedirs(os.path.join(run_dir, "cuda-verification"))
            with open(os.path.join(run_dir, "cuda-verification", "cuda_verification.txt"), "w") as f:
                f.write(report)
        return root

    def unclean(self, records, text, **kw):
        result = run(self.state(records, **kw))
        self.assertEqual(result.returncode, 3, result.stdout + result.stderr)
        self.assertTrue([line for line in result.stdout.splitlines() if line.startswith("UNCLEAN PASS: ") and text in line], result.stdout)
        self.assertNotIn("\nCLEAN:", result.stdout)

    def test_a_whole_read_of_a_files_bytes_is_clean(self):
        result = run(self.state([RECORD_ART, page("art-000001", 0, 60), page("art-000001", 60), VERDICT]))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("CLEAN: the judged verifier", result.stdout)
        self.assertNotIn("UNCLEAN", result.stdout)
        self.assertIn("on disk at the end: the same bytes", result.stdout)
        self.assertIn("exercised 1      the verifier reading evidence to its end", result.stdout)

    def test_never_read(self):
        self.unclean([RECORD_ART, VERDICT], "the judged verifier never read art-000001")

    def test_the_first_page_alone(self):
        self.unclean([RECORD_ART, page("art-000001", 0, 60), VERDICT], "did not read art-000001 to its end")

    def test_the_last_page_alone(self):
        self.unclean([RECORD_ART, page("art-000001", 60), VERDICT], "did not read art-000001 to its end")

    def test_a_gap_between_pages(self):
        self.unclean([RECORD_ART, page("art-000001", 0, 30), page("art-000001", 60), VERDICT], "did not read art-000001 to its end")

    def test_another_verifiers_read_does_not_count(self):
        self.unclean([RECORD_ART, page("art-000001", 0, pid="pid-000003"), VERDICT], "the judged verifier never read art-000001")
        result = run(self.state([RECORD_ART, page("art-000001", 0, pid="pid-000003"), VERDICT]))
        self.assertIn("NOT EXERCISED    the verifier reading evidence to its end", result.stdout)

    def test_a_typed_record(self):
        self.unclean([TYPED_ART, page("art-000001", 0), VERDICT], "art-000001 is typed")

    def test_the_older_record_does_not_count(self):
        self.unclean([RECORD_ART, page("art-000001", 0), RECORD_ART, VERDICT], "the judged verifier never read art-000003")

    def test_a_record_read_before_it_was_published(self):
        records = [page("art-000002", 0), RECORD_ART, VERDICT]
        self.unclean(records, "before it was published")

    def test_no_record_at_all(self):
        self.unclean([VERDICT], "the benchmarker published no record")

    def test_a_file_typed_with_write_file(self):
        written = ("tool_result", "pid-000002", {"tool": "write_file", "output": {"path": PATH}}, {})
        self.unclean([written, RECORD_ART, page("art-000002", 0), VERDICT], "last written by write_file")

    def test_a_file_a_command_made_is_named(self):
        made = ("tool_result", "pid-000002", {"tool": "run", "output": {"command": "bash trials.sh"}, "changes": {"root": "/home/elsewhere/runs/gone/cuda-bench", "created": ["raw_record.txt"]}}, {})
        result = run(self.state([made, RECORD_ART, page("art-000002", 0), VERDICT]))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("last changed by: art-000001 run 'bash trials.sh'", result.stdout)

    def test_a_report_naming_what_was_not_read(self):
        other = ("published.benchmark_json", "pid-000002", {"body": "{}"}, FILE_META)
        self.unclean([other, RECORD_ART, page("art-000002", 0), VERDICT], "its report names art-000001, which it never read", report="Read art-000001 and art-000002.")

    def test_a_file_changed_after_the_publish_is_said_and_not_judged(self):
        result = run(self.state([RECORD_ART, page("art-000001", 0), VERDICT], on_disk="other"))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("CHANGED after the publish", result.stdout)

    def test_a_copy_without_the_file_says_so(self):
        result = run(self.state([RECORD_ART, page("art-000001", 0), VERDICT], on_disk=None))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("not in this copy", result.stdout)

    def test_a_failed_verdict_is_no_pass_to_look_behind(self):
        failed = ("verification", "pid-000001", {"status": "fail", "reason": "no"}, {"verifier_pid": "pid-000004"})
        result = run(self.state([failed]))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("no pass to look behind", result.stdout)
        self.assertNotIn("UNCLEAN", result.stdout)

    def test_no_verdict(self):
        result = run(self.state([RECORD_ART]))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertNotIn("UNCLEAN", result.stdout)

    def test_a_publish_and_a_topic_publish_are_counted_apart(self):
        out = run(self.state([RECORD_ART])).stdout
        self.assertIn("exercised 1      an artifact published", out)
        self.assertIn("NOT EXERCISED    a topic publish", out)
        out = run(self.state([("mailbox_publish", "pid-000001", [], {})])).stdout
        self.assertIn("NOT EXERCISED    an artifact published", out)
        self.assertIn("exercised 1      a topic publish", out)
        self.assertIn("of 26 exercised", out)

    def test_a_wait_comes_from_the_engines_record(self):
        parked = ("agent_run", "pid-000004", {"question": "I need the record"}, {"status": "waiting"})
        mail = ("tool_result", "pid-000001", {"tool": "message_agent", "output": {"to": "pid-000004", "body": "here", "artifacts": "art-000001"}}, {})
        out = run(self.state([RECORD_ART, parked, mail])).stdout
        self.assertIn("exercised 1      the verifier pausing with a wait", out)
        self.assertIn("waited 1 time(s): 'I need the record'", out)
        self.assertIn("mail to a parked process: 1", out)
        self.assertIn("NOT EXERCISED    mail held for a process not running", out)

    def test_a_reply_missing_its_closing_braces_is_still_an_action(self):
        calls = [{"phase": "outcome", "time": "2026-11-06T07:00:00Z", "pid": "pid-000004", "response": '{"action":{"type":"tool","tool_call":{"name":"run","args":{"command":"echo 2.64186"}}'},
                 {"phase": "outcome", "time": "2026-11-06T07:00:01Z", "pid": "pid-000004", "response": 'no json here'}]
        out = run(self.state([RECORD_ART], calls=calls)).stdout
        self.assertIn("1 replies not parsed here", out)
        self.assertIn("a verifier retyped into a command or a file: 1 of 1", out)

    def test_an_attempt_on_the_private_directory_is_listed(self):
        tried = ("tool_result", "pid-000004", {"tool": "run", "output": {"command": "cat /x/cuda-bench/raw_record.txt", "exit_code": "1", "stderr": "Permission denied"}}, {})
        own = ("tool_result", "pid-000002", {"tool": "run", "output": {"command": "cat /x/cuda-bench/raw_record.txt", "exit_code": "0"}}, {})
        out = run(self.state([tried, own])).stdout
        self.assertEqual(out.count("attempt: "), 1, out)
        self.assertIn("attempt: verifier (pid-000004) run, exit 1", out)

    def test_numbers_pasted_into_mail_are_counted(self):
        mail = ("tool_result", "pid-000001", {"tool": "message_agent", "output": {"to": "pid-000004", "body": "render_ms_best=1 render_ms_best=2"}}, {})
        self.assertIn("in mail bodies to a verifier: 2", run(self.state([mail])).stdout)


class Declared(unittest.TestCase):
    """What analyze.py says when a workflow declares evidence. A Go test holds it to the kernel on real records."""
    def state(self, verdict, reads=(), source="file", grant=False, other=None, replies=()):
        root = os.path.join(tempfile.mkdtemp(), "state")
        self.addCleanup(shutil.rmtree, os.path.dirname(root))
        for sub in ("artifacts/payloads", "kernel", "run"):
            os.makedirs(os.path.join(root, sub))
        payloads = {
            "art-000001": ("run_spec", "pid-000001", {"spec": {"verification": {"evidence": [{"publisher": "bench", "type": "raw_record", "from_file": True}]}}}, {}),
            "art-000002": ("published.raw_record", "pid-000002", {"body": "bytes"}, {"source": source}),
        }
        for n, output in enumerate(reads):
            payloads[f"art-00001{n}"] = ("tool_result", "pid-000003", {"tool": "read_artifact", "output": dict(output, artifact_id="art-000002")}, {"tool": "read_artifact"})
        if grant:
            payloads["art-000020"] = ("delegation_handles", "pid-000003", {}, {"delegator": "pid-000004", "artifacts": "art-000002"})
        if other:
            payloads["art-000025"] = ("published.raw_record", other, {"body": "not the publisher's"}, {"source": "file"})
        payloads["art-000030"] = ("verification", "pid-000001", verdict, {"verifier_pid": "pid-000003"})
        index = {}
        for artifact, (kind, producer, payload, metadata) in payloads.items():
            with open(os.path.join(root, "artifacts", "payloads", artifact + ".json"), "w") as f:
                json.dump(payload, f)
            index[artifact] = {"id": artifact, "type": kind, "producer_pid": producer, "created_at": "2026-11-06T07:00:00Z", "metadata": metadata, "path": artifact + ".json"}
        with open(os.path.join(root, "artifacts", "index.json"), "w") as f:
            json.dump(index, f)
        processes = [{"pid": "pid-000001"}, {"pid": "pid-000004", "parent_pid": "pid-000001", "metadata": {"agent_id": "coordinator"}},
                     {"pid": "pid-000002", "parent_pid": "pid-000004", "metadata": {"agent_id": "bench"}}, {"pid": "pid-000003", "parent_pid": "pid-000004", "metadata": {"agent_id": "verifier"}},
                     {"pid": "pid-000005", "parent_pid": "pid-000004", "metadata": {"agent_id": "writer"}}, {"pid": "pid-000006", "metadata": {"agent_id": "root"}},
                     {"pid": "pid-000007", "parent_pid": "pid-000006", "metadata": {"agent_id": "bench"}}]
        with open(os.path.join(root, "kernel", "processes.json"), "w") as f:
            json.dump(processes, f)
        with open(os.path.join(root, "kernel", "events.json"), "w") as f:
            json.dump([{"id": "evt-000001", "type": "verification.exit_refused", "process_id": "pid-000003", "timestamp": "t", "data": {"unmet": "art-000002 was not read"}}], f)
        with open(os.path.join(root, "run", "calls.jsonl"), "w") as f:
            for seq, reply in enumerate(replies, 1):
                action, pid, when = reply if isinstance(reply, tuple) else (reply, "pid-000004", "2026-11-06T06:59:00Z")
                f.write(json.dumps({"seq": seq, "phase": "outcome", "time": when, "pid": pid, "response": json.dumps({"action": action})}) + "\n")
        return root

    def verdict(self, status, check, **more):
        return dict({"status": status, "reason": "why", "verifier_pid": "pid-000003",
                     "evidence": [{"publisher": "bench", "type": "raw_record", "artifact_id": "art-000002", "status": check}]}, **more)

    def test_a_kernel_pass_on_what_these_records_say_was_not_read_disagrees(self):
        result = run(self.state(self.verdict("pass", "read")))
        self.assertEqual(result.returncode, 3)
        self.assertIn("declared raw_record by bench: art-000002: unread", result.stdout)
        self.assertIn("DISAGREE", result.stdout)

    def test_a_verdict_that_says_pass_with_unread_evidence_disagrees(self):
        result = run(self.state(self.verdict("pass", "unread")))
        self.assertEqual(result.returncode, 3)
        self.assertIn("DISAGREE: the verdict is a pass", result.stdout)

    def test_any_difference_from_the_verdict_disagrees(self):
        result = run(self.state(self.verdict("unverified", "partial")))
        self.assertEqual(result.returncode, 3)
        self.assertIn("DISAGREE: the verdict says", result.stdout)

    def test_only_the_publisher_in_this_run_counts(self):
        for other in ("pid-000005", "pid-000007"):
            result = run(self.state(self.verdict("unverified", "unread"), other=other))
            self.assertIn("declared raw_record by bench: art-000002: unread", result.stdout)
            self.assertNotIn("DISAGREE", result.stdout)

    def test_unverified_is_said_and_is_no_unclean_pass(self):
        result = run(self.state(self.verdict("unverified", "unread")))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("UNVERIFIED: why", result.stdout)
        self.assertNotIn("UNCLEAN", result.stdout)
        self.assertIn("exits refused for evidence: 1", result.stdout)
        self.assertIn("exercised 1      an exit 0 refused for unread evidence", result.stdout)
        self.assertIn("exercised 1      a verdict withheld as unverified", result.stdout)

    def test_a_handle_at_delegation_has_its_line_and_counts_apart_from_mail(self):
        result = run(self.state(self.verdict("pass", "read"), reads=[{"offset": 0}], grant=True))
        self.assertIn("art-000002 (published.raw_record) attached at delegation by coordinator (pid-000004) to verifier (pid-000003)", result.stdout)
        self.assertIn("read 1 pages, to the end", result.stdout)
        self.assertIn("exercised 1      a handle attached at a delegation", result.stdout)
        self.assertIn("NOT EXERCISED    a handle attached to mail", result.stdout)
        self.assertIn("of 26 exercised", result.stdout)
        self.assertIn("of 4 of the delegation and evidence situations exercised", result.stdout)
        self.assertIn("CLEAN", result.stdout)

    def test_an_attachment_in_a_reply_that_nothing_granted_has_its_line(self):
        delegate = {"agent_id": "verifier", "goal": "check the attached record"}
        for name, action in {
            "at the action level": {"type": "delegate", "artifacts": "art-000002", "delegate": delegate},
            "as a list": {"type": "delegate", "artifacts": ["art-000002"], "delegate": delegate},
            "inside the delegate": {"type": "delegate", "delegate": dict(delegate, artifacts="art-000002")},
            "in spawn_agent's args": {"type": "tool", "tool_call": {"name": "spawn_agent", "args": dict(delegate, artifacts="art-000002")}},
            "beside spawn_agent's args": {"type": "tool", "tool_call": {"name": "spawn_agent", "args": delegate, "artifacts": "art-000002"}},
        }.items():
            with self.subTest(name):
                result = run(self.state(self.verdict("unverified", "unread"), replies=[action]))
                self.assertIn("NOT GRANTED: coordinator (pid-000004) at 2026-11-06T06:59:00Z (seq 1) attached art-000002", result.stdout)
                self.assertIn("an attachment in a delegate reply not granted: 1", result.stdout)
                granted = run(self.state(self.verdict("pass", "read"), reads=[{"offset": 0}], grant=True, replies=[action]))
                self.assertNotIn("NOT GRANTED", granted.stdout)
                self.assertIn("an attachment in a delegate reply not granted: 0", granted.stdout)

    def test_a_reply_that_attached_nothing_is_not_counted(self):
        replies = [{"type": "delegate", "delegate": {"agent_id": "verifier", "goal": "it is attached"}},
                   {"type": "tool", "tool_call": {"name": "message_agent", "args": {"to": "pid-000003", "body": "b", "artifacts": "art-000002"}}}]
        result = run(self.state(self.verdict("unverified", "unread"), replies=replies))
        self.assertNotIn("NOT GRANTED", result.stdout)
        self.assertIn("an attachment in a delegate reply not granted: 0", result.stdout)

    def test_a_grant_matches_only_its_delegators_later_reply(self):
        action = {"type": "delegate", "artifacts": "art-000002", "delegate": {"agent_id": "verifier", "goal": "g"}}
        for name, reply in {"another delegator": (action, "pid-000005", "2026-11-06T06:59:00Z"), "after the grant": (action, "pid-000004", "2026-11-06T07:01:00Z")}.items():
            with self.subTest(name):
                result = run(self.state(self.verdict("pass", "read"), reads=[{"offset": 0}], grant=True, replies=[reply]))
                self.assertIn("an attachment in a delegate reply not granted: 1", result.stdout)

    def test_one_grant_matches_one_reply(self):
        action = {"type": "delegate", "artifacts": "art-000002", "delegate": {"agent_id": "verifier", "goal": "g"}}
        result = run(self.state(self.verdict("pass", "read"), reads=[{"offset": 0}], grant=True, replies=[action, action]))
        self.assertIn("(seq 2) attached art-000002", result.stdout)
        self.assertIn("an attachment in a delegate reply not granted: 1", result.stdout)

    def test_typed_when_a_file_is_declared(self):
        result = run(self.state(self.verdict("unverified", "not_from_file"), reads=[{"offset": 0}], source=""))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("declared raw_record by bench: art-000002: not_from_file", result.stdout)
        self.assertNotIn("DISAGREE", result.stdout)


class Live3(unittest.TestCase):
    """live3 declared nothing, so what is said of it must not move. Its state stays where it ran."""
    STATE = os.path.join(os.path.dirname(ANALYZE), "runs", "live3", "state")

    def test_live3_is_still_an_unclean_pass_on_the_same_count(self):
        if not os.path.isdir(self.STATE):
            self.skipTest("live3's state is not here")
        result = run(self.STATE)
        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertIn("UNCLEAN PASS: the judged verifier never read art-000172", result.stdout)
        self.assertIn("7 of 26 exercised", result.stdout)
        self.assertIn("0 of 4 of the delegation and evidence situations exercised", result.stdout)
        self.assertNotIn("exits refused for evidence", result.stdout)
        self.assertIn("an attachment in a delegate reply not granted: 0", result.stdout)


class Live4(unittest.TestCase):
    """live4's coordinator put a delegation's artifacts at the action level twice, and both were dropped unsaid."""
    STATE = os.path.join(os.path.dirname(ANALYZE), "runs", "live4", "state")

    def test_live4s_two_dropped_attachments_are_named(self):
        if not os.path.isdir(self.STATE):
            self.skipTest("live4's state is not here")
        result = run(self.STATE)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("NOT GRANTED: coordinator (pid-000002) at 2026-11-07T06:40:02.884842391Z (seq 186) attached art-000167 in the action of its reply", result.stdout)
        self.assertIn("(seq 504) attached art-000465 in the action of its reply", result.stdout)
        self.assertIn("an attachment in a delegate reply not granted: 2", result.stdout)
        self.assertIn("8 of 26 exercised", result.stdout)
        self.assertIn("0 of 4 of the delegation and evidence situations exercised", result.stdout)


class Live2(unittest.TestCase):
    """live2's real state, which stays where it ran and is not in git."""
    STATE = os.path.join(os.path.dirname(ANALYZE), "runs", "live2", "state")

    def test_live2_is_an_unclean_pass(self):
        if not os.path.isdir(self.STATE):
            self.skipTest("live2's state is not here")
        result = run(self.STATE)
        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertIn("UNCLEAN PASS: the judged verifier never read art-000319", result.stdout)
        self.assertIn("UNCLEAN PASS: art-000319 is typed", result.stdout)
        self.assertIn("3 of 26 exercised", result.stdout)
        self.assertIn("exercised 1      an artifact published", result.stdout)
        self.assertIn("exercised 3      a chain closed by a re-delegation", result.stdout)
        self.assertIn("exercised 1      a backend retry", result.stdout)
        self.assertIn("3 replies not parsed", result.stdout)


unittest.main()
