package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDaemonPollsOutsideFormerWorkingHours(t *testing.T) {
	r, d, _ := setup(t)
	var zone string
	for _, name := range []string{"UTC", "Etc/GMT+12", "Etc/GMT-12"} {
		loc, e := time.LoadLocation(name)
		if e != nil {
			t.Fatal(e)
		}
		hour := time.Now().In(loc).Hour()
		if hour < 7 || hour >= 19 {
			zone = name
			break
		}
	}
	if zone == "" {
		t.Fatal("no outside-hours timezone found")
	}
	t.Setenv("TZ", zone)
	config, e := configDir()
	if e != nil {
		t.Fatal(e)
	}
	db, e := dbOpen(config)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO prs VALUES(?,?)", r.URL, r.Thread); e != nil {
		t.Fatal(e)
	}
	db.Close()
	writeGraph(t, d, fmt.Sprintf(`{"data":{"p0":{"pullRequest":%s}}}`, openGraph))
	exe := filepath.Join(d, "monitor")
	build := exec.Command("go", "build", "-o", exe, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build: %s %v", out, e)
	}
	run := func(arg string) error {
		cmd := exec.Command(exe, arg)
		out, e := cmd.CombinedOutput()
		if e != nil {
			return fmt.Errorf("%s: %s: %w", arg, out, e)
		}
		return nil
	}
	if e = run("start"); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := run("stop"); e != nil {
			t.Log(e)
			return
		}
		pidPath := filepath.Join(config, "pid")
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(pidPath); os.IsNotExist(err) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("test daemon did not stop")
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, e = os.Stat(filepath.Join(d, "queued")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			logBytes, _ := os.ReadFile(filepath.Join(config, "monitor.log"))
			t.Fatalf("no poll outside working hours: %s", logBytes)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPollWithoutRegistrationsDoesNotCallGitHub(t *testing.T) {
	_, d, codex := setup(t)
	db, err := dbOpen(d)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	poll(db, codex)
	if _, err = os.Stat(filepath.Join(d, "requests")); !os.IsNotExist(err) {
		t.Fatalf("unexpected GitHub request: %v", err)
	}
	if _, err = os.Stat(filepath.Join(d, "queued")); !os.IsNotExist(err) {
		t.Fatalf("unexpected notification: %v", err)
	}
}
