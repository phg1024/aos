"""Renders the probe records (results/P*.json) as HTML tables for
../least-privilege-probes.html: one row per attempt with what was tried,
what the agent saw (paths shortened to <root>, long text cut), the disk
change and the records. Prints the tables to stdout, one block per run,
each preceded by a line "<!-- run: NAME -->".

Usage, from this folder: python3 -I make_tables.py > tables.html, or
python3 -I make_tables.py --build, which fills each "<!-- TABLE: NAME -->"
in report.src.html with that run's table and writes
../least-privilege-probes.html.
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


def main():
    tables = {}
    for probe in ("P1", "P2", "P3", "P4"):
        data = json.load(open(os.path.join(HERE, "results", probe + ".json")))
        runs = data if isinstance(data, list) else data["runs"]
        for run in runs:
            tables[run["name"]] = table(run)
    if "--build" not in sys.argv:
        print("\n".join(tables.values()))
        return
    page = open(os.path.join(HERE, "report.src.html")).read()
    for name, block in tables.items():
        marker = f"<!-- TABLE: {name} -->"
        if marker not in page:
            raise SystemExit(f"no marker for {name}")
        page = page.replace(marker, block)
    open(os.path.join(HERE, "..", "least-privilege-probes.html"), "w").write(page)


if __name__ == "__main__":
    main()
