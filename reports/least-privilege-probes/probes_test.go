//go:build darwin

package lpprobes

import (
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
	w := newWorld(t, "")
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
	w := newWorld(t, "")
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
	write(t, "P3", map[string]any{"runs": []Run{run}, "checks": extra})
}

// P4: the two network paths, allow_network true and network_allow empty.
func TestP4NetworkPaths(t *testing.T) {
	w := newWorld(t, "")
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

// P2: case-variant paths on the case-insensitive volume. Stops at the
// first write that lands, as aos book asked.
func TestP2CaseVariantPaths(t *testing.T) {
	landed := func(changes []string) bool { return len(changes) > 0 }
	w := newWorld(t, "")
	if _, err := os.Stat(filepath.Join(w.ws, ".AOS-STATE")); err != nil {
		t.Fatalf("volume is not case-insensitive here: %v", err)
	}
	mounts := []capability.Mount{{Name: "ws", HostPath: w.ws}}
	set := capability.Set{ReadPaths: []string{w.ws}, WritePaths: []string{w.ws}, Tools: append(append([]string{}, fileTools...), "run")}
	grants := "mount ws (rw); read ws; write ws; state dir ws/.aos-state protected (lexically); tools read_file, write_file, list_dir, run"
	refused := "refused: tool_error + capability.denied, disk unchanged"
	viaRun := "refused by the sandbox: nonzero exit, disk unchanged"
	upper, mixed := filepath.Join(w.ws, ".AOS-STATE"), filepath.Join(w.ws, ".Aos-State")
	steps := []step{
		writeFile("c1", "write_file ws/.AOS-STATE/probe-case1.txt", refused, filepath.Join(upper, "probe-case1.txt")),
		writeFile("c2", "write_file ws/.Aos-State/kernel/probe-case2.txt", refused, filepath.Join(mixed, "kernel", "probe-case2.txt")),
		tool("c3", "read_file ws/.AOS-STATE/kernel/events.json", refused, "read_file", "path", filepath.Join(upper, "kernel", "events.json")),
		tool("c4", "read_file ws/.Aos-State/kernel/processes.json", refused, "read_file", "path", filepath.Join(mixed, "kernel", "processes.json")),
		runCmd("c5", "run: write ws/.AOS-STATE/probe-run1.txt", viaRun, "echo x > "+filepath.Join(upper, "probe-run1.txt")+"; echo write_exit=$?"),
		runCmd("c6", "run: write ws/.Aos-State/kernel/probe-run2.txt", viaRun, "echo x > "+filepath.Join(mixed, "kernel", "probe-run2.txt")+"; echo write_exit=$?"),
		runCmd("c7", "run: read ws/.AOS-STATE/kernel/events.json", viaRun, "head -c 120 "+filepath.Join(upper, "kernel", "events.json")+"; echo; echo read_exit=$?"),
	}
	runs := []Run{w.run("P2a state dir, case variants", grants, set, mounts, steps, landed)}
	if runs[0].Stopped == "" {
		// A read-only mount nested in a writable one.
		n := newWorld(t, "ref")
		nMounts := []capability.Mount{{Name: "ws", HostPath: n.ws}, {Name: "ref", HostPath: n.ref, ReadOnly: true}}
		nSet := capability.Set{ReadPaths: []string{n.ws}, WritePaths: []string{n.ws}, Tools: set.Tools}
		nGrants := "mounts ws (rw), ws/ref (ro, nested); read ws; write ws; tools read_file, write_file, list_dir, run"
		upperRef, mixedRef := filepath.Join(n.ws, "REF"), filepath.Join(n.ws, "Ref")
		nSteps := []step{
			writeFile("c8", "write_file ws/REF/probe-ro.txt", refused, filepath.Join(upperRef, "probe-ro.txt")),
			tool("c9", "write_file over ws/Ref/reference.txt", refused, "write_file", "path", filepath.Join(mixedRef, "reference.txt"), "content", "tampered\n"),
			runCmd("c10", "run: write ws/REF/probe-ro-run.txt", viaRun, "echo x > "+filepath.Join(upperRef, "probe-ro-run.txt")+"; echo write_exit=$?"),
			runCmd("c11", "run: overwrite ws/Ref/reference.txt", viaRun, "echo tampered > "+filepath.Join(mixedRef, "reference.txt")+"; echo write_exit=$?"),
		}
		runs = append(runs, n.run("P2b nested read-only mount, case variants", nGrants, nSet, nMounts, nSteps, landed))
	}
	write(t, "P2", runs)
	for _, run := range runs {
		if run.Stopped != "" {
			t.Logf("STOPPED: a write landed at %s", run.Stopped)
		}
	}
}
