"""Per process: every assistant action's notes and tool call (compact), with seq."""
import json, os, re, sys
src, out = sys.argv[1], sys.argv[2]
os.makedirs(out, exist_ok=True)
for fn in sorted(os.listdir(src)):
    if not fn.endswith('.txt') or fn.endswith('.wm.txt'): continue
    text = open(os.path.join(src, fn)).read()
    blocks = re.split(r'\n=== #(\d+) (\w+) \((art-\d+)\)\n', text)
    lines = []
    for i in range(1, len(blocks), 4):
        seq, role, aid, content = blocks[i], blocks[i+1], blocks[i+2], blocks[i+3]
        if role == 'assistant':
            try:
                a = json.loads(content)['action']
            except Exception:
                lines.append(f'#{seq} RAW {content[:600]}'); continue
            tc = a.get('tool_call') or {}
            args = tc.get('args') or {}
            argtxt = json.dumps(args)[:400] if tc.get('name') != 'update_memory' else json.dumps(args)
            lines.append(f'#{seq} [{a.get("type")}] NOTES: {a.get("notes","")}\n    CALL {tc.get("name","")}: {argtxt}' + (f'\n    OUTPUT: {str(a.get("output") or a.get("result") or a.get("message") or "")[:3000]}' if a.get('type')!='tool' else ''))
        elif role in ('tool','user') and ('tool_error' in content[:200] or 'tool_notice' in content[:200]):
            if 'name=backend' in content[:40]: continue
            lines.append(f'#{seq} {role.upper()}: {content[:300]}')
    open(os.path.join(out, fn), 'w').write('\n'.join(lines))
