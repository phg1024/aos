import json, random, re, hashlib, sys
root="/Users/peihongguo/Code/aos/"
def rd(p): return [dict(json.loads(l),_src=p) for l in open(root+p) if l.strip()]
P1=rd("reports/memory-probe-baseline/samples-rescored.jsonl")+rd("reports/memory-probe-phase2/archive-x6/samples-phase1.jsonl")
P2a=rd("reports/memory-probe-phase2/samples-rescored.jsonl")+rd("reports/memory-probe-phase2/archive-x6/samples-phase2.jsonl")
P2b=rd("reports/memory-probe-phase2/precedence/samples-rescored.jsonl")
P3=rd("reports/memory-probe-phase3/samples-memory.jsonl")
rng=random.Random(20261029)
def pick(pool,probe,correct=None):
    c=[s for s in pool if s["probe"]==probe and (correct is None or s["correct"]==correct) and s.get("answer")]
    return rng.choice(c)
chosen=[("phase 1","eceb917+phase-1 harness (aos-phase1)",pick(P1,"leaf2-speedup")),
("phase 2","ff46bbc order-only wording (aos-phase2)",pick(P2b,"leaf2-speedup",True)),
("phase 2","ff46bbc order-only wording (aos-phase2)",pick(P2b,"leaf2-speedup",False)),
("phase 3","74510b7, memory,check-rule, run by aos code",pick(P3,"leaf2-speedup")),
("phase 1","phase-1 binary",pick(P1,"tridata-order",True)),
("phase 2","ecb29d4 (aos-phase2 first build)",pick(P2a,"tridata-order",False)),
("phase 3","74510b7, memory,check-rule, run by aos code",pick(P3,"tridata-order")),
("phase 1","phase-1 binary",pick(P1,"tg-sweep")),
("phase 2","ecb29d4",pick(P2a,"tg-sweep")),
("phase 3","74510b7, memory,check-rule, run by aos code",pick(P3,"tg-sweep"))]
rng.shuffle(chosen)
def redact(a):
    a=re.sub(r"\b(search_memory|read_memory|update_memory)\b","[memory tool]",a)
    a=re.sub(r"(?i)\bworking[- ]memory\b","[my records]",a)
    a=re.sub(r"(?i)\b(compacted|archived) (memory|history)\b","[my records]",a)
    a=re.sub(r"(?i)\bmemory segments?\b|\bsegments?\b","[records]",a)
    a=re.sub(r"(?i)\barchive\b","[records]",a)
    a=re.sub(r"\[(no )?evidence[^\]]*\]","[ref]",a)
    a=re.sub(r"(?i)\b(with |from )?(history )?evidence( from)?( history)?[: ]*(\d+\s*[-–]\s*\d+|\[ref\])?","[ref]",a)
    a=re.sub(r"(?i)compacted (session )?(memory|notes)","[my records]",a)
    a=re.sub(r"\bart-\d{6}\b","[ref]",a)
    a=re.sub(r"#\d+(\s*[-–]\s*#?\d+)?","[ref]",a)
    a=re.sub(r"(?i)\b(messages?|msgs?)\s+\d+(\s*[-–]\s*\d+)?","[ref]",a)
    a=re.sub(r"(?i)\boffset[ =:]*\d+","[ref]",a)
    a=re.sub(r"(?i)\bpid-\d+","[process]",a)
    a=re.sub(r"(?i)\bmy \[my records\]","[my records]",a)
    a=re.sub(r"(?i)\(memory: ","(",a)
    a=re.sub(r"(?i) as the '[^']*' entry| under '[^']*'| entry \"[^\"]*\"","",a)
    a=re.sub(r"(?i)\bthat entry is recorded\b","That is recorded",a)
    a=re.sub(r"\[ref\]history \[ref\]","[ref]",a)
    a=re.sub(r"(\[ref\][ ,]*)+\[ref\]","[ref]",a)
    return a
questions={}
import os
probes={p["id"]:p for p in json.load(open(root+".claude/worktrees/competent-heyrovsky-566e83/bench/memory/pathtracer-bunny-memfix-probes.json"))}
blind=[];key=[]
for i,(ph,binary,s) in enumerate(chosen,1):
    blind.append({"item":i,"question":probes[s["probe"]]["question"],"answer":redact(s["answer"])})
    key.append({"item":i,"phase":ph,"binary":binary,"source":s["_src"],"probe":s["probe"],"sample":s["sample"],"pid":s.get("pid"),"frozen_pattern_score":"correct" if s["correct"] else ("superseded" if s.get("superseded") else ("off-topic" if s.get("off_topic") else "not correct")),"steps":s["steps"],"bytes_read":s.get("bytes_read")})
out=sys.argv[1]
json.dump(blind,open(out+"/blind.json","w"),indent=2,ensure_ascii=False)
json.dump(key,open(out+"/key.json","w"),indent=2,ensure_ascii=False)
for b,k in zip(blind,key): print(k["item"],k["phase"],k["probe"],k["frozen_pattern_score"],len(b["answer"]))
