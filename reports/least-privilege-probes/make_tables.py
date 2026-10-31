"""Renders the probe records (results/P*.json) as HTML tables for
../least-privilege-probes.html: one row per attempt with what was tried,
what the agent saw (paths shortened to <root>, long text cut), the disk
change and the records. Prints the tables to stdout, one block per run,
each preceded by a line "<!-- run: NAME -->".

Usage, from this folder: python3 -I make_tables.py > tables.html, or
python3 -I make_tables.py --build, which fills report.src.html and writes
../least-privilege-probes.html: "<!-- TABLE: NAME -->" with that run's
table from results-283a92e/ (the first run), "<!-- RETEST: NAME -->" from
results/ (the b3c53b0 re-test) and "<!-- COMPARE -->" with every attempt's
outcome in both.
"""

import html
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = re.compile(r"/private/var/folders/[^ /]+/[^ /]+/T/Test\w+/\d+")


def short(text, limit=360):
    text = ROOT.sub("<root>", text)
    if len(text) > limit:
        text = text[:limit] + f" … [{len(text) - limit} more bytes]"
    return text


def yes(value):
    return '<span class="yes">yes</span>' if value else "no"


def args(record):
    a = dict(record.get("args") or {})
    if "content" in a and len(a["content"]) > 40:
        a["content"] = f"<{len(a['content'])} bytes>"
    return short(" ".join(f"{k}={v}" for k, v in sorted(a.items())), 240)


def table(run):
    rows = [f'<!-- run: {run["name"]} -->',
            f'<p class="grants"><b>{html.escape(run["name"])}</b>. Grants: {html.escape(short(run["grants"]))}. '
            f'Exit {run["exit"]["code"]} ({html.escape(run["exit"].get("reason", ""))}); '
            f'capability.denied events in the kernel log: {run["denied_events"]}.</p>',
            '<div class="wrap"><table><tr><th>#</th><th>Attempt</th><th>Agent saw (exact)</th><th>Disk</th>'
            '<th>tool_error</th><th>tool_result</th><th>capability.denied</th></tr>']
    for r in run["records"]:
        disk = "<br>".join(html.escape(short(c, 200)) for c in r.get("disk") or []) or "unchanged"
        denied = r.get("capability_denied") or []
        den = "<br>".join(html.escape(f'{d.get("capability")}: {d.get("reason")}') for d in denied)
        extra = r.get("extra") or {}
        extra_text = "".join(f"<br><i>{html.escape(k)}: {html.escape(short(v, 120))}</i>" for k, v in extra.items())
        landed = r.get("disk") and not denied
        cls = ' class="landed"' if landed and "control" not in r["attempt"] and not r["attempt"].startswith("legal") and "legal" not in r["attempt"] else ""
        rows.append(f'<tr{cls}><td>{html.escape(r["id"])}</td><td>{html.escape(r["attempt"])}<br><code>{html.escape(r["tool"])} {html.escape(args(r))}</code>'
                    f'<br><span class="note">expected: {html.escape(r["expect"])}</span>{extra_text}</td>'
                    f'<td><code class="saw">{html.escape(short(r["saw"]))}</code></td><td>{disk}</td>'
                    f'<td>{yes(r.get("tool_error_artifact"))}</td><td>{yes(r.get("tool_result_artifact"))}</td>'
                    f'<td>{("yes<br>" + den) if denied else "no"}</td></tr>')
    rows.append("</table></div>")
    return "\n".join(rows)


def outcome(record):
    """One line: what the attempt came to."""
    denied = record.get("capability_denied") or []
    if record.get("tool_error_artifact"):
        reason = "; ".join(d.get("reason", "") for d in denied)
        text = f"refused: tool_error + capability.denied ({reason})" if denied else "refused: tool_error, no capability.denied"
        return re.sub(r":\d{4,5}/", ":<port>/", text)
    saw = record.get("saw", "")
    if record.get("tool") == "run":
        inner = re.findall(r"\w+_exit=(\d+)", saw)
        code = re.search(r"exit_code:(\d+)", saw).group(1)
        blocked = "Operation not permitted" in saw or "Could not resolve" in saw or "bind:" in saw
        status = f"exit {inner[0] if inner else code}"
        text = f"run: {'sandbox refused, ' if blocked else ''}{status}"
    else:
        text = "tool_result"
    if record.get("disk"):
        text += ", disk changed"
    return re.sub(r":\d{4,5}/", ":<port>/", text + (", capability.denied" if denied else ""))


def compare(before_dir, after_dir):
    """Rows of every attempt in both result sets: before, after."""
    def load(folder):
        out = {}
        for probe in ("P1", "P2", "P3", "P4"):
            data = json.load(open(os.path.join(HERE, folder, probe + ".json")))
            for run in (data if isinstance(data, list) else data["runs"]):
                for r in run["records"]:
                    out[(run["name"], r["id"])] = r
        return out
    before, after = load(before_dir), load(after_dir)
    rows = ['<div class="wrap"><table><tr><th>Run</th><th>#</th><th>Attempt</th><th>283a92e</th><th>b3c53b0</th></tr>']
    for key in list(before) + [k for k in after if k not in before]:
        b, a = before.get(key), after.get(key)
        ob, oa = (outcome(b) if b else "not run"), (outcome(a) if a else "not run")
        cls = ' class="changed"' if ob != oa else ""
        attempt = (a or b)["attempt"]
        rows.append(f'<tr{cls}><td>{html.escape(key[0])}</td><td>{html.escape(key[1])}</td><td>{html.escape(attempt)}</td>'
                    f'<td>{html.escape(ob)}</td><td>{html.escape(oa)}</td></tr>')
    rows.append("</table></div>")
    return "\n".join(rows)


def tables_of(folder):
    tables = {}
    for probe in ("P1", "P2", "P3", "P4"):
        data = json.load(open(os.path.join(HERE, folder, probe + ".json")))
        for run in (data if isinstance(data, list) else data["runs"]):
            tables[run["name"]] = table(run)
    return tables


def main():
    if "--build" not in sys.argv:
        print("\n".join(tables_of("results").values()))
        return
    page = open(os.path.join(HERE, "report.src.html")).read()
    for kind, folder in (("TABLE", "results-283a92e"), ("RETEST", "results")):
        for name, block in tables_of(folder).items():
            page = page.replace(f"<!-- {kind}: {name} -->", block)
    page = page.replace("<!-- COMPARE -->", compare("results-283a92e", "results"))
    left = re.findall(r"<!-- (?:TABLE|RETEST|COMPARE)[^>]*-->", page)
    if left:
        raise SystemExit(f"unfilled markers: {left}")
    open(os.path.join(HERE, "..", "least-privilege-probes.html"), "w").write(page)


if __name__ == "__main__":
    main()
