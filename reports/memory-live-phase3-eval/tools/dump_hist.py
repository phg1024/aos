"""Dump each process's full history (all checkpoints and segments merged by
message seq) of a run to <out>/<pid>-<agent>.txt, plus its WM versions."""
import json, os, sys
run, out = sys.argv[1], sys.argv[2]
os.makedirs(out, exist_ok=True)
base = os.path.dirname(os.path.dirname(run.rstrip('/'))) if False else os.getcwd()
idx = json.load(open(os.path.join(run, '.aos-state/artifacts/index.json')))
d = json.load(open(os.path.join(run, '.aos-state/kernel/processes.json')))
ps = d if isinstance(d, list) else d.get('processes', d)
ps = list(ps.values()) if isinstance(ps, dict) else ps
agent = {p['pid']: (p.get('metadata') or {}).get('agent_id', '?') for p in ps}
hist = {}; wms = {}
for aid, rec in sorted(idx.items()):
    pid, kind = rec['producer_pid'], rec['type']
    if kind in ('checkpoint', 'memory_segment'):
        data = json.load(open(rec['path']))
        for m in data.get('messages') or []:
            if 'seq' in m:
                hist.setdefault(pid, {})[int(m['seq'])] = (m['role'], m['content'], aid)
    elif kind == 'working_memory':
        wms.setdefault(pid, []).append((aid, rec['created_at'], json.load(open(rec['path']))))
for pid, msgs in hist.items():
    with open(os.path.join(out, f'{pid}-{agent.get(pid,"?")}.txt'), 'w') as f:
        for seq in sorted(msgs):
            role, content, aid = msgs[seq]
            f.write(f'\n=== #{seq} {role} ({aid})\n{content}\n')
for pid, vs in wms.items():
    with open(os.path.join(out, f'{pid}-{agent.get(pid,"?")}.wm.txt'), 'w') as f:
        for aid, t, wm in vs:
            f.write(f'\n=== {aid} {t} v{wm.get("version")}\n')
            for s in wm.get('sections') or []:
                for e in s.get('entries') or []:
                    f.write(f'[{s["name"]}/{e.get("key")}] {e.get("content")}  EVIDENCE={e.get("evidence")}\n')
print({p: len(m) for p, m in hist.items()})
