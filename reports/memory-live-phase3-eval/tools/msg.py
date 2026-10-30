"""Print messages of a dumped history: msg.py <file> <seq>[-<seq>] [maxchars] [grep]"""
import re,sys
t=open(sys.argv[1]).read(); r=sys.argv[2]; n=int(sys.argv[3]) if len(sys.argv)>3 else 1500; g=sys.argv[4] if len(sys.argv)>4 else None
a,b=(int(x) for x in r.split('-')) if '-' in r else (int(r),int(r))
for seq,role,aid,c in re.findall(r'\n=== #(\d+) (\w+) \((art-\d+)\)\n(.*?)(?=\n=== #|\Z)',t,re.S):
    if a<=int(seq)<=b:
        if g:
            for m in re.finditer(g,c): print(f'#{seq} {role} {aid}: ...{c[max(0,m.start()-n):m.end()+n]}...')
        else: print(f'#{seq} {role} {aid}: {c[:n]}')
