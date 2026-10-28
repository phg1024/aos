//go:build unix

package ipc_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"aos/internal/ipc"
)

const busHelperEnv = "AOS_BUS_HELPER"

// TestBusHelper is a separate process with its own bus on the shared
// directory: it sends 100 messages to pid-sink, and receives and
// acknowledges whatever is there in between.
func TestBusHelper(t *testing.T) {
	spec := os.Getenv(busHelperEnv)
	if spec == "" {
		t.Skip("subprocess helper")
	}
	dir, name := filepath.Dir(spec), filepath.Base(spec)
	bus, err := ipc.New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := os.Create(filepath.Join(filepath.Dir(dir), name+".received"))
	defer out.Close()
	encoder := json.NewEncoder(out)
	for i := 0; i < 100; i++ {
		if _, err := bus.Send(name, "pid-sink", name+"-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
		if i%10 == 9 {
			messages, err := bus.Receive("pid-sink", false)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, message := range messages {
				encoder.Encode(message)
				ids = append(ids, message.ID)
			}
			if err := bus.Ack("pid-sink", ids); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Buses in separate processes on one directory: every message arrives
// exactly once, IDs are unique, and the files stay valid throughout.
func TestBusesShareADirectoryAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bus")
	os.MkdirAll(dir, 0o755)
	var helpers []*exec.Cmd
	for _, name := range []string{"pid-a", "pid-b", "pid-c"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBusHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), busHelperEnv+"="+filepath.Join(dir, name))
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		helpers = append(helpers, cmd)
	}
	for _, cmd := range helpers {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper: %v", err)
		}
	}
	bus, err := ipc.New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := bus.Receive("pid-sink", true)
	if err != nil {
		t.Fatalf("the mailbox must still be valid: %v", err)
	}
	bodies := map[string]int{}
	ids := map[string]bool{}
	collect := func(message ipc.Message) {
		bodies[message.Body]++
		if ids[message.ID] {
			t.Fatalf("message id %s handed out twice", message.ID)
		}
		ids[message.ID] = true
	}
	for _, name := range []string{"pid-a", "pid-b", "pid-c"} {
		file, _ := os.Open(filepath.Join(root, name+".received"))
		decoder := json.NewDecoder(file)
		for {
			var message ipc.Message
			if decoder.Decode(&message) != nil {
				break
			}
			collect(message)
		}
		file.Close()
	}
	for _, message := range rest {
		collect(message)
	}
	var missing []string
	for _, name := range []string{"pid-a", "pid-b", "pid-c"} {
		for i := 0; i < 100; i++ {
			body := name + "-" + strconv.Itoa(i)
			if bodies[body] != 1 {
				missing = append(missing, body+"x"+strconv.Itoa(bodies[body]))
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 || len(ids) != 300 {
		t.Fatalf("%d unique ids; messages not seen exactly once: %v", len(ids), missing)
	}
}
