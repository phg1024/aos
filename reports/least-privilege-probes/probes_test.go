//go:build darwin

package lpprobes

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aos/internal/capability"
)

var fileTools = []string{"read_file", "write_file", "list_dir"}

// P1: the denied-write arc. One process: write scope ws/out, read-only
// mount ref, the state directory inside ws; file tools only, then the same
// with run.
func TestP1DeniedWriteArc(t *testing.T) {
	w := newWorld(t, "", "")
	mounts := []capability.Mount{{Name: "ws", HostPath: w.ws}, {Name: "ref", HostPath: w.ref, ReadOnly: true}}
	set := capability.Set{ReadPaths: []string{w.ws, w.ref}, WritePaths: []string{filepath.Join(w.ws, "out")}, Tools: fileTools}
	grants := "mounts ws (rw), ref (ro); read ws, ref; write ws/out; state dir ws/.aos-state protected; tools " + strings.Join(set.Tools, ", ")
	refused := "refused: tool_error + capability.denied, disk unchanged"
	arc := []step{
		writeFile("1", "write_file outside the write scope (ws/probe-outside-scope.txt)", refused, filepath.Join(w.ws, "probe-outside-scope.txt")),
		writeFile("2", "write_file into the read-only mount (ref/probe-ref.txt)", refused, filepath.Join(w.ref, "probe-ref.txt")),
		writeFile("3", "write_file into the state dir (ws/.aos-state/kernel/probe-state.txt)", refused, filepath.Join(w.state, "kernel", "probe-state.txt")),
		runCmd("4", "run (not granted)", refused, "echo hi"),
		writeFile("5", "legal write_file (ws/out/probe-legal.txt)", "tool_result, file created", filepath.Join(w.ws, "out", "probe-legal.txt")),
	}
	runs := []Run{w.run("P1a without run", grants, set, mounts, arc, nil)}

	withRun := set
	withRun.Tools = append(append([]string{}, fileTools...), "run")
	viaRun := "sandbox refuses; expected by the code: tool_result with nonzero exit, no capability.denied"
	arcRun := []step{
		writeFile("1", "write_file outside the write scope", refused, filepath.Join(w.ws, "probe-outside-scope-b.txt")),
		writeFile("2", "write_file into the read-only mount", refused, filepath.Join(w.ref, "probe-ref-b.txt")),
		writeFile("3", "write_file into the state dir", refused, filepath.Join(w.state, "kernel", "probe-state-b.txt")),
		runCmd("4r", "run: write outside the write scope", viaRun, "echo x > "+filepath.Join(w.ws, "probe-outside-scope-run.txt")),
		runCmd("5r", "run: write into the read-only mount", viaRun, "echo x > "+filepath.Join(w.ref, "probe-ref-run.txt")),
		runCmd("6r", "run: write into the state dir", viaRun, "echo x > "+filepath.Join(w.state, "kernel", "probe-state-run.txt")),
		runCmd("7r", "run: legal write (ws/out/probe-legal-run.txt)", "tool_result, exit 0, file created", "echo x > "+filepath.Join(w.ws, "out", "probe-legal-run.txt")),
		writeFile("8", "legal write_file (ws/out/probe-legal-b.txt)", "tool_result, file created", filepath.Join(w.ws, "out", "probe-legal-b.txt")),
	}
	runs = append(runs, w.run("P1b with run", strings.Replace(grants, "list_dir", "list_dir, run", 1), withRun, mounts, arcRun, nil))

	// Which check refuses: with the write scope widened to ws and ref, only
	// the read-only mount and the protected state dir stand in the way.
	wide := set
	wide.WritePaths = []string{w.ws, w.ref}
	isolate := []step{
		writeFile("2w", "write_file into the read-only mount, write scope covering it", refused, filepath.Join(w.ref, "probe-ref-c.txt")),
		writeFile("3w", "write_file into the state dir, write scope covering it", refused, filepath.Join(w.state, "kernel", "probe-state-c.txt")),
	}
	runs = append(runs, w.run("P1c refusal by mount and protection alone (extra)", strings.Replace(grants, "write ws/out", "write ws, ref", 1), wide, mounts, isolate, nil))
	write(t, "P1", runs)
}

// P3: sandbox escapes, run granted, network off.
func TestP3SandboxEscapes(t *testing.T) {
	w := newWorld(t, "", "")
	tmpName := fmt.Sprintf("/tmp/aos-lp-probe-%d-x", os.Getpid())
	w.extraWatch = []string{tmpName, "/private" + tmpName}
	defer os.Remove(tmpName)

	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	must(t, err)
	defer udp.Close()
	dns, err := net.ListenPacket("udp4", "127.0.0.1:0")
	must(t, err)
	defer dns.Close()
	counts := map[string]int{}
	var mu sync.Mutex
	listen := func(name string, conn net.PacketConn) {
		buf := make([]byte, 2048)
		for {
			if _, _, err := conn.ReadFrom(buf); err != nil {
				return
			}
			mu.Lock()
			counts[name]++
			mu.Unlock()
		}
	}
	go listen("udp", udp)
	go listen("dns", dns)
	udpPort := udp.LocalAddr().(*net.UDPAddr).Port
	dnsPort := dns.LocalAddr().(*net.UDPAddr).Port

	mounts := []capability.Mount{{Name: "ws", HostPath: w.ws}}
	set := capability.Set{ReadPaths: []string{w.ws}, WritePaths: []string{filepath.Join(w.ws, "out")}, Tools: []string{"run"}}
	grants := "mount ws (rw); read ws; write ws/out; tools run; allow_network false"
	secret, movable := filepath.Join(w.outside, "secret.txt"), filepath.Join(w.outside, "movable.txt")
	link := filepath.Join(w.ws, "out", "probe-hardlink")
	refused := "refused by the sandbox, nothing changes"
	steps := []step{
		runCmd("h1", "hardlink ws/out/probe-hardlink -> outside/secret.txt", refused, "ln "+secret+" "+link+"; echo ln_exit=$?; ls -l "+filepath.Join(w.ws, "out")),
		runCmd("h2", "write through the hardlink path, if it exists", "outside/secret.txt unchanged", "if [ -e "+link+" ]; then cat "+link+"; echo pwned >> "+link+"; echo append_exit=$?; else echo no_link; fi"),
		runCmd("r1", "rename outside/movable.txt into ws/out", refused, "mv "+movable+" "+filepath.Join(w.ws, "out", "probe-moved.txt")+"; echo mv_exit=$?"),
		runCmd("t1", "write "+tmpName+" by name", refused, "echo x > "+tmpName+"; echo write_exit=$?"),
		runCmd("t2", "TMPDIR is the per-PID scratch dir, and writable", "TMPDIR = scratch/<pid>, write ok", `echo "TMPDIR=$TMPDIR"; echo x > "$TMPDIR/probe-tmp" && echo tmp_write_ok`),
		runCmd("p1", "relative ../ from inside the scope (cd out; write ../probe-rel.txt = ws/probe-rel.txt)", refused, "cd out && echo x > ../probe-rel.txt; echo write_exit=$?; pwd"),
		runCmd("p2", "relative ../ from the run directory (ws; ../probe-rel-up.txt = root/probe-rel-up.txt)", refused, "echo x > ../probe-rel-up.txt; echo write_exit=$?; pwd"),
		runCmd("u1", fmt.Sprintf("UDP datagram to a local listener on 127.0.0.1:%d", udpPort), "refused, listener receives nothing", fmt.Sprintf("printf probe | nc -u -w 1 127.0.0.1 %d; echo nc_exit=$?", udpPort)),
		runCmd("d1", fmt.Sprintf("DNS query over UDP to a local listener (nslookup -port=%d ... 127.0.0.1)", dnsPort), "refused, listener receives nothing", fmt.Sprintf("nslookup -port=%d -timeout=2 -retry=1 aos-probe.example 127.0.0.1; echo nslookup_exit=$?", dnsPort)),
		runCmd("d2", "DNS lookup through the system directory service (dscacheutil -q host -a name example.com)", "no address returned", "dscacheutil -q host -a name example.com; echo dscacheutil_exit=$?"),
		runCmd("d3", "DNS lookup through getaddrinfo (curl http://example.com/, connect refused either way)", "could not resolve", "curl -sS -m 5 -o /dev/null http://example.com/; echo curl_exit=$?"),
	}
	run := w.run("P3 escapes, network off", grants, set, mounts, steps, nil)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	extra := map[string]string{
		"udp_datagrams_received": fmt.Sprint(counts["udp"]),
		"dns_queries_received":   fmt.Sprint(counts["dns"]),
	}
	mu.Unlock()
	raw, _ := os.ReadFile(secret)
	extra["outside/secret.txt_after"] = string(raw)
	if info, err := os.Stat(secret); err == nil {
		extra["outside/secret.txt_links"] = fmt.Sprint(linkCount(info))
	}
	_, err = os.Stat(movable)
	extra["outside/movable.txt_still_there"] = fmt.Sprint(err == nil)
	_, err = os.Lstat(tmpName)
	extra[tmpName+"_exists"] = fmt.Sprint(err == nil)
	scratch, _ := filepath.EvalSymlinks(w.scratch)
	extra["scratch_root"] = scratch
	for i := range run.Records {
		switch run.Records[i].ID {
		case "u1":
			run.Records[i].Extra = map[string]string{"listener_received": extra["udp_datagrams_received"]}
		case "d1":
			run.Records[i].Extra = map[string]string{"listener_received": extra["dns_queries_received"]}
		case "t2":
			run.Records[i].Extra = map[string]string{"scratch_root": scratch}
		}
	}
	// Controls: the network rows again with unconfined_run (no sandbox),
	// to show the commands work here and the refusals are the sandbox's.
	control := set
	control.UnconfinedRun = true
	controls := []step{
		runCmd("u1c", "control, unconfined: UDP datagram to the loopback listener", "delivered", fmt.Sprintf("printf probe | nc -u -w 1 127.0.0.1 %d; echo nc_exit=$?", udpPort)),
		runCmd("d1c", "control, unconfined: DNS query to the loopback listener", "listener receives the query", fmt.Sprintf("nslookup -port=%d -timeout=1 -retry=0 aos-probe.example 127.0.0.1; echo nslookup_exit=$?", dnsPort)),
		runCmd("d2c", "control, unconfined: dscacheutil -q host -a name example.com", "an address", "dscacheutil -q host -a name example.com; echo dscacheutil_exit=$?"),
	}
	mu.Lock()
	before := map[string]int{"udp": counts["udp"], "dns": counts["dns"]}
	mu.Unlock()
	controlRun := w.run("P3 controls, unconfined_run", strings.Replace(grants, "tools run", "tools run; unconfined_run true", 1), control, mounts, controls, nil)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	for i := range controlRun.Records {
		switch controlRun.Records[i].ID {
		case "u1c":
			controlRun.Records[i].Extra = map[string]string{"listener_received": fmt.Sprint(counts["udp"] - before["udp"])}
		case "d1c":
			controlRun.Records[i].Extra = map[string]string{"listener_received": fmt.Sprint(counts["dns"] - before["dns"])}
		}
	}
	mu.Unlock()
	write(t, "P3", map[string]any{"runs": []Run{run, controlRun}, "checks": extra})
}

// P4: the two network paths, allow_network true and network_allow empty.
func TestP4NetworkPaths(t *testing.T) {
	w := newWorld(t, "", "")
	var mu sync.Mutex
	hits := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		rw.Write([]byte("reached " + r.URL.Path))
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	mounts := []capability.Mount{{Name: "ws", HostPath: w.ws}}
	set := capability.Set{ReadPaths: []string{w.ws}, WritePaths: []string{filepath.Join(w.ws, "out")}, Tools: []string{"web_fetch", "run"}, AllowNetwork: true}
	grants := "mount ws (rw); tools web_fetch, run; allow_network true; network_allow empty"
	steps := []step{
		tool("w1", "web_fetch http://127.0.0.1:<port>/fetch-ip", "refused: non-public address", "web_fetch", "url", fmt.Sprintf("http://127.0.0.1:%d/fetch-ip", port), "timeout", "5s"),
		tool("w2", "web_fetch http://localhost:<port>/fetch-name", "refused: non-public address", "web_fetch", "url", fmt.Sprintf("http://localhost:%d/fetch-name", port), "timeout", "5s"),
		runCmd("c1", "run curl http://127.0.0.1:<port>/curl", "allowed: exit 0, server reached", fmt.Sprintf("curl -sS -m 5 http://127.0.0.1:%d/curl; echo; echo curl_exit=$?", port)),
	}
	run := w.run("P4 network paths", grants, set, mounts, steps, nil)
	mu.Lock()
	paths := map[string]string{"w1": "/fetch-ip", "w2": "/fetch-name", "c1": "/curl"}
	for i := range run.Records {
		run.Records[i].Extra = map[string]string{"server_hits": fmt.Sprint(hits[paths[run.Records[i].ID]])}
	}
	mu.Unlock()
	write(t, "P4", map[string]any{"runs": []Run{run}, "port": port})
}

// P2: case-variant paths on the case-insensitive volume, each with a
// lowercase control. (A first run stopped at c1, as aos book asked; aos
// book then asked for every case.)
func TestP2CaseVariantPaths(t *testing.T) {
	w := newWorld(t, "", "")
	if _, err := os.Stat(filepath.Join(w.ws, ".AOS-STATE")); err != nil {
		t.Fatalf("volume is not case-insensitive here: %v", err)
	}
	tools := append(append([]string{}, fileTools...), "run")
	mounts := []capability.Mount{{Name: "ws", HostPath: w.ws}}
	set := capability.Set{ReadPaths: []string{w.ws}, WritePaths: []string{w.ws}, Tools: tools}
	grants := "mount ws (rw); read ws; write ws; state dir ws/.aos-state protected; tools read_file, write_file, list_dir, run"
	refused := "refused: tool_error + capability.denied, disk unchanged"
	viaRun := "refused by the sandbox: nonzero exit, disk unchanged"
	upper, mixed, lower := filepath.Join(w.ws, ".AOS-STATE"), filepath.Join(w.ws, ".Aos-State"), w.state
	readCmd := func(dir string) string {
		return "head -c 80 " + filepath.Join(dir, "kernel", "events.json") + "; echo; echo read_exit=$?"
	}
	writeCmd := func(path string) string { return "echo x > " + path + "; echo write_exit=$?" }
	steps := []step{
		writeFile("c1", "write_file ws/.AOS-STATE/probe-case1.txt", refused, filepath.Join(upper, "probe-case1.txt")),
		writeFile("c1L", "control: write_file ws/.aos-state/probe-case1-lower.txt", refused, filepath.Join(lower, "probe-case1-lower.txt")),
		writeFile("c2", "write_file ws/.Aos-State/kernel/probe-case2.txt", refused, filepath.Join(mixed, "kernel", "probe-case2.txt")),
		writeFile("c2L", "control: write_file ws/.aos-state/kernel/probe-case2-lower.txt", refused, filepath.Join(lower, "kernel", "probe-case2-lower.txt")),
		tool("c3", "read_file ws/.AOS-STATE/kernel/events.json", refused, "read_file", "path", filepath.Join(upper, "kernel", "events.json")),
		tool("c3L", "control: read_file ws/.aos-state/kernel/events.json", refused, "read_file", "path", filepath.Join(lower, "kernel", "events.json")),
		tool("c4", "read_file ws/.Aos-State/kernel/processes.json", refused, "read_file", "path", filepath.Join(mixed, "kernel", "processes.json")),
		tool("c4L", "control: read_file ws/.aos-state/kernel/processes.json", refused, "read_file", "path", filepath.Join(lower, "kernel", "processes.json")),
		runCmd("c5", "run: write ws/.AOS-STATE/probe-run1.txt", viaRun, writeCmd(filepath.Join(upper, "probe-run1.txt"))),
		runCmd("c5L", "control, run: write ws/.aos-state/probe-run1-lower.txt", viaRun, writeCmd(filepath.Join(lower, "probe-run1-lower.txt"))),
		runCmd("c6", "run: write ws/.Aos-State/kernel/probe-run2.txt", viaRun, writeCmd(filepath.Join(mixed, "kernel", "probe-run2.txt"))),
		runCmd("c6L", "control, run: write ws/.aos-state/kernel/probe-run2-lower.txt", viaRun, writeCmd(filepath.Join(lower, "kernel", "probe-run2-lower.txt"))),
		runCmd("c7", "run: read ws/.AOS-STATE/kernel/events.json", viaRun, readCmd(upper)),
		runCmd("c7L", "control, run: read ws/.aos-state/kernel/events.json", viaRun, readCmd(lower)),
	}
	runs := []Run{w.run("P2a state dir, case variants", grants, set, mounts, steps, nil)}

	// The kernel's event log and the artifact index: write_file with a copy
	// of the live file plus one marked forged entry, then ask the kernel and
	// the store what they hold; run only opens them for append (": >>").
	k := newWorld(t, "", "")
	kUpper, kMixed := filepath.Join(k.ws, ".AOS-STATE"), filepath.Join(k.ws, ".Aos-State")
	events := func(dir string) func() map[string]string {
		return func() map[string]string {
			var list []map[string]any
			raw, _ := os.ReadFile(filepath.Join(k.state, "kernel", "events.json"))
			json.Unmarshal(raw, &list)
			list = append(list, map[string]any{"id": "evt-999999", "process_id": "pid-forged", "type": "probe.forged",
				"timestamp": time.Now().UTC().Format(time.RFC3339), "data": map[string]string{"via": dir}})
			out, _ := json.Marshal(list)
			return map[string]string{"path": filepath.Join(dir, "kernel", "events.json"), "content": string(out)}
		}
	}
	index := func(dir string) func() map[string]string {
		return func() map[string]string {
			entries := map[string]any{}
			raw, _ := os.ReadFile(filepath.Join(k.state, "artifacts", "index.json"))
			json.Unmarshal(raw, &entries)
			entries["art-999999"] = map[string]any{"id": "art-999999", "type": "probe.forged", "producer_pid": "pid-000001",
				"path": "probe-forged.json", "created_at": time.Now().UTC().Format(time.RFC3339Nano), "metadata": map[string]string{"via": dir}}
			out, _ := json.Marshal(entries)
			return map[string]string{"path": filepath.Join(dir, "artifacts", "index.json"), "content": string(out)}
		}
	}
	appendCmd := func(path string) string { return ": >> " + path + "; echo open_append_exit=$?" }
	kSteps := []step{
		{ID: "k1L", Attempt: "control: write_file ws/.aos-state/kernel/events.json (copy + forged event evt-999999)", Expect: refused, Tool: "write_file", Dynamic: events(k.state)},
		{ID: "k1", Attempt: "write_file ws/.Aos-State/kernel/events.json (copy + forged event evt-999999)", Expect: refused, Tool: "write_file", Dynamic: events(kMixed)},
		{ID: "k2L", Attempt: "control: write_file ws/.aos-state/artifacts/index.json (copy + forged art-999999)", Expect: refused, Tool: "write_file", Dynamic: index(k.state)},
		{ID: "k2", Attempt: "write_file ws/.AOS-STATE/artifacts/index.json (copy + forged art-999999)", Expect: refused, Tool: "write_file", Dynamic: index(kUpper)},
		runCmd("k3L", "control, run: open ws/.aos-state/kernel/events.json for append", viaRun, appendCmd(filepath.Join(k.state, "kernel", "events.json"))),
		runCmd("k3", "run: open ws/.Aos-State/kernel/events.json for append", viaRun, appendCmd(filepath.Join(kMixed, "kernel", "events.json"))),
		runCmd("k4", "run: open ws/.AOS-STATE/artifacts/index.json for append", viaRun, appendCmd(filepath.Join(kUpper, "artifacts", "index.json"))),
	}
	kSet := capability.Set{ReadPaths: []string{k.ws}, WritePaths: []string{k.ws}, Tools: tools}
	kRun := k.run("P2b kernel files, case variants", grants, kSet, []capability.Mount{{Name: "ws", HostPath: k.ws}}, kSteps, nil)
	checks := map[string]string{}
	if list, err := k.k.Events(); err != nil {
		checks["kernel_events_error"] = err.Error()
	} else {
		forged := false
		for _, event := range list {
			forged = forged || event.ID == "evt-999999"
		}
		checks["kernel_reports_forged_event_evt-999999"] = fmt.Sprint(forged)
	}
	if items, err := k.store.List(); err != nil {
		checks["artifact_list_error"] = err.Error()
	} else {
		forged := false
		for _, item := range items {
			forged = forged || item.ID == "art-999999"
		}
		checks["store_lists_forged_artifact_art-999999"] = fmt.Sprint(forged)
	}
	// The snapshot leaves the runtime's own state files out, so the disk
	// column of k1 and k2 comes from the checks above.
	for i, record := range kRun.Records {
		switch {
		case record.ID == "k1" && record.ToolResult:
			kRun.Records[i].Disk = []string{"rewrote " + filepath.Join(k.state, "kernel", "events.json") + " (a state file, outside the snapshot); Kernel.Events() then reports evt-999999: " + checks["kernel_reports_forged_event_evt-999999"]}
		case record.ID == "k2" && record.ToolResult:
			kRun.Records[i].Disk = []string{"rewrote " + filepath.Join(k.state, "artifacts", "index.json") + " (a state file, outside the snapshot); Store.List() then lists art-999999: " + checks["store_lists_forged_artifact_art-999999"]}
		}
	}
	runs = append(runs, kRun)

	// A read-only mount nested in a writable one.
	n := newWorld(t, "ref", "")
	nMounts := []capability.Mount{{Name: "ws", HostPath: n.ws}, {Name: "ref", HostPath: n.ref, ReadOnly: true}}
	nSet := capability.Set{ReadPaths: []string{n.ws}, WritePaths: []string{n.ws}, Tools: tools}
	nGrants := "mounts ws (rw), ws/ref (ro, nested); read ws; write ws; tools read_file, write_file, list_dir, run"
	upperRef, mixedRef, lowerRef := filepath.Join(n.ws, "REF"), filepath.Join(n.ws, "Ref"), n.ref
	over := func(id, attempt, path, content string) step {
		return tool(id, attempt, refused, "write_file", "path", path, "content", content)
	}
	nSteps := []step{
		writeFile("c8", "write_file ws/REF/probe-ro.txt", refused, filepath.Join(upperRef, "probe-ro.txt")),
		writeFile("c8L", "control: write_file ws/ref/probe-ro-lower.txt", refused, filepath.Join(lowerRef, "probe-ro-lower.txt")),
		over("c9", "write_file over ws/Ref/reference.txt", filepath.Join(mixedRef, "reference.txt"), "tampered c9\n"),
		over("c9L", "control: write_file over ws/ref/reference.txt", filepath.Join(lowerRef, "reference.txt"), "tampered c9L\n"),
		runCmd("c10", "run: write ws/REF/probe-ro-run.txt", viaRun, writeCmd(filepath.Join(upperRef, "probe-ro-run.txt"))),
		runCmd("c10L", "control, run: write ws/ref/probe-ro-run-lower.txt", viaRun, writeCmd(filepath.Join(lowerRef, "probe-ro-run-lower.txt"))),
		runCmd("c11", "run: overwrite ws/Ref/reference.txt", viaRun, "echo 'tampered c11' > "+filepath.Join(mixedRef, "reference.txt")+"; echo write_exit=$?"),
		runCmd("c11L", "control, run: overwrite ws/ref/reference.txt", viaRun, "echo 'tampered c11L' > "+filepath.Join(lowerRef, "reference.txt")+"; echo write_exit=$?"),
	}
	runs = append(runs, n.run("P2c nested read-only mount, case variants", nGrants, nSet, nMounts, nSteps, nil))
	raw, _ := os.ReadFile(filepath.Join(n.ref, "reference.txt"))
	checks["nested_ref/reference.txt_after"] = string(raw)

	// Extra: the scripts/tui.sh layout, where earlier sessions' state
	// directories sit next to the current one inside the workdir.
	s := newWorld(t, "", filepath.Join(".aos-sessions", "current"))
	sibling := filepath.Join(s.ws, ".aos-sessions", "earlier")
	must(t, os.MkdirAll(filepath.Join(sibling, "kernel"), 0o755))
	must(t, os.WriteFile(filepath.Join(sibling, "kernel", "events.json"), []byte("[]\n"), 0o644))
	sGrants := "mount ws (rw); read ws; write ws; state dir ws/.aos-sessions/current protected; an earlier session in ws/.aos-sessions/earlier; tools read_file, write_file, list_dir, run"
	sSteps := []step{
		writeFile("x1", "write_file into the earlier session (ws/.aos-sessions/earlier/kernel/probe-sibling.txt), lowercase", "not covered by Protected", filepath.Join(sibling, "kernel", "probe-sibling.txt")),
		runCmd("x2", "run: write into the earlier session, lowercase", "not hidden from run", writeCmd(filepath.Join(sibling, "kernel", "probe-sibling-run.txt"))),
		writeFile("x3", "control: write_file into the current session (ws/.aos-sessions/current/probe-current.txt)", refused, filepath.Join(s.state, "probe-current.txt")),
	}
	sSet := capability.Set{ReadPaths: []string{s.ws}, WritePaths: []string{s.ws}, Tools: tools}
	runs = append(runs, s.run("P2d scripts/tui.sh layout: earlier sessions (extra)", sGrants, sSet, []capability.Mount{{Name: "ws", HostPath: s.ws}}, sSteps, nil))
	// Added for the b3c53b0 re-test. The same earlier-session writes with
	// the protection aos-tui sets (a copy of its protectedPaths), plus a
	// case variant into the earlier session.
	u := newWorld(t, "", filepath.Join(".aos-sessions", "current"))
	uSibling := filepath.Join(u.ws, ".aos-sessions", "earlier")
	must(t, os.MkdirAll(filepath.Join(uSibling, "kernel"), 0o755))
	must(t, os.WriteFile(filepath.Join(uSibling, "kernel", "events.json"), []byte("[]\n"), 0o644))
	protected, ok := tuiProtectedPaths(u.state, filepath.Join(u.ws, ".aos-sessions"), u.ws)
	if !ok {
		t.Fatal("this layout would take aos-tui's per-session branch, which is not copied")
	}
	u.tools.Protected = protected
	uSet := capability.Set{ReadPaths: []string{u.ws}, WritePaths: []string{u.ws}, Tools: tools}
	uGrants := "as P2d, with aos-tui's protectedPaths (b3c53b0): protected = current state dir, its lock, and ws/.aos-sessions"
	uSteps := []step{
		writeFile("x1", "write_file into the earlier session (ws/.aos-sessions/earlier/kernel/probe-sibling.txt), lowercase", refused, filepath.Join(uSibling, "kernel", "probe-sibling.txt")),
		runCmd("x2", "run: write into the earlier session, lowercase", viaRun, writeCmd(filepath.Join(uSibling, "kernel", "probe-sibling-run.txt"))),
		writeFile("x3", "control: write_file into the current session (ws/.aos-sessions/current/probe-current.txt)", refused, filepath.Join(u.state, "probe-current.txt")),
		writeFile("x4", "write_file into the earlier session through a case variant (ws/.AOS-SESSIONS/earlier/kernel/probe-sibling-case.txt)", refused, filepath.Join(u.ws, ".AOS-SESSIONS", "earlier", "kernel", "probe-sibling-case.txt")),
	}
	runs = append(runs, u.run("P2f scripts/tui.sh layout with aos-tui's protectedPaths (added for b3c53b0)", uGrants, uSet, []capability.Mount{{Name: "ws", HostPath: u.ws}}, uSteps, nil))

	// Added for the b3c53b0 re-test: a case variant that stays inside the
	// write scope must still be allowed.
	v := newWorld(t, "", "")
	vMounts := []capability.Mount{{Name: "ws", HostPath: v.ws}, {Name: "ref", HostPath: v.ref, ReadOnly: true}}
	vSet := capability.Set{ReadPaths: []string{v.ws, v.ref}, WritePaths: []string{filepath.Join(v.ws, "out")}, Tools: tools}
	vGrants := "as P1: mounts ws (rw), ref (ro); read ws, ref; write ws/out; tools read_file, write_file, list_dir, run"
	allowed := "allowed: tool_result, file created in ws/out"
	vSteps := []step{
		writeFile("i1", "write_file ws/OUT/probe-in-scope.txt (case variant inside the write scope)", allowed, filepath.Join(v.ws, "OUT", "probe-in-scope.txt")),
		tool("i2", "read_file ws/OUT/probe-in-scope.txt", "allowed: tool_result", "read_file", "path", filepath.Join(v.ws, "OUT", "probe-in-scope.txt")),
		runCmd("i3", "run: write ws/OUT/probe-in-scope-run.txt", "allowed: exit 0, file created in ws/out", writeCmd(filepath.Join(v.ws, "OUT", "probe-in-scope-run.txt"))),
		writeFile("i4", "control: write_file ws/OUTSIDE.txt (outside the write scope)", refused, filepath.Join(v.ws, "probe-OUTSIDE.txt")),
	}
	runs = append(runs, v.run("P2e case variant inside the write scope (added for b3c53b0)", vGrants, vSet, vMounts, vSteps, nil))
	write(t, "P2", map[string]any{"runs": runs, "checks": checks})
}
