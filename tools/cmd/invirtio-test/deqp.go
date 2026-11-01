package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"invirtio/internal/harness"
)

func init() {
	register("run-deqp", "run dEQP's Vulkan tests on the host's GPU with the Mesa prefix", cmdRunDeqp)
}

// deqpSyncGroups are the groups of dEQP's must-pass list that test
// synchronization and external memory.
var deqpSyncGroups = regexp.MustCompile(`^dEQP-VK\.(synchronization\.(basic|smoke|internally_synchronized_objects|timeline_semaphore|implicit)|synchronization2\.(basic|smoke)|api\.external)\.`)

// deqpMustpass are the files of the must-pass list that hold those groups.
var deqpMustpass = []string{"synchronization.txt", "synchronization2.txt", "api.txt"}

var (
	deqpCaseRE   = regexp.MustCompile(`^Test case '(.+)'\.\.`)
	deqpResultRE = regexp.MustCompile(`^  ([A-Za-z]+) \(`)
	deqpTotalRE  = regexp.MustCompile(`(?m)^  (Passed|Failed|Not supported|Warnings|Waived):\s+(\d+)/(\d+)`)
)

// deqpPassing are the results that count as passing, as in conformance runs,
// where warnings pass.
var deqpPassing = map[string]bool{
	"Pass": true, "NotSupported": true, "QualityWarning": true,
	"CompatibilityWarning": true, "CapabilityWarning": true, "Waiver": true,
}

// deqpPaths are the build's binary, the directory of its data, and the
// source's must-pass list.
type deqpPaths struct {
	bin, dir, mustpass string
}

func deqpDefaults(root string) deqpPaths {
	dir := filepath.Join(root, "build/deqp-build/external/vulkancts/modules/vulkan")
	return deqpPaths{
		bin:      filepath.Join(dir, "deqp-vk"),
		dir:      dir,
		mustpass: filepath.Join(root, "build/deqp-src/external/vulkancts/mustpass/main/vk-default"),
	}
}

// deqpResult is a run's totals and the cases that did not pass.
type deqpResult struct {
	executed, passed, failed, notSupported, warnings int
	complete                                         bool
	failures                                         []string
}

// deqpCaseList writes the cases of the must-pass list that match the groups
// to a file, and returns their number.
func deqpCaseList(mustpass, path string) (int, error) {
	var cases []string
	for _, name := range deqpMustpass {
		data, err := os.ReadFile(filepath.Join(mustpass, name))
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if deqpSyncGroups.MatchString(line) {
				cases = append(cases, line)
			}
		}
	}
	return len(cases), os.WriteFile(path, []byte(strings.Join(cases, "\n")+"\n"), 0644)
}

// runDeqp runs the cases of a case list, with its output and results in
// dir, and returns the results. A crash ends the run, which then has no
// totals and is not complete.
func runDeqp(ctx context.Context, p deqpPaths, env []string, caselist, dir string) (deqpResult, error) {
	var r deqpResult
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.bin,
		"--deqp-caselist-file="+caselist,
		"--deqp-archive-dir="+p.dir,
		"--deqp-log-filename="+filepath.Join(dir, "TestResults.qpa"),
		"--deqp-shadercache-filename="+filepath.Join(dir, "shadercache.bin"),
		"--deqp-log-images=disable",
		"--deqp-log-shader-sources=disable")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after 2 hours")
	}
	os.WriteFile(filepath.Join(dir, "deqp.log"), out, 0644)

	var current string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := deqpCaseRE.FindStringSubmatch(line); m != nil {
			current = m[1]
		} else if m := deqpResultRE.FindStringSubmatch(line); m != nil && current != "" {
			if !deqpPassing[m[1]] {
				r.failures = append(r.failures, m[1]+" "+current)
			}
			current = ""
		}
	}
	// A case that started and reported nothing crashed the run.
	if current != "" {
		r.failures = append(r.failures, "Crash "+current)
	}
	for _, m := range deqpTotalRE.FindAllStringSubmatch(string(out), -1) {
		n, _ := strconv.Atoi(m[2])
		r.executed, _ = strconv.Atoi(m[3])
		switch m[1] {
		case "Passed":
			r.passed = n
			r.complete = true
		case "Failed":
			r.failed = n
		case "Not supported":
			r.notSupported = n
		case "Warnings":
			r.warnings = n
		}
	}
	if bytes.Contains(out, []byte("Test run was ABORTED!")) {
		r.complete = false
	}
	return r, err
}

// passes reports whether a run of n cases ran them all, passed at least one,
// and failed none, by the totals and by the cases' own results.
func (r deqpResult) passes(n int) bool {
	return r.complete && r.executed == n && r.passed > 0 && r.failed == 0 &&
		len(r.failures) == 0
}

// summary prints a run's totals and its failures.
func (r deqpResult) summary() {
	fmt.Printf("dEQP: %d executed, %d passed, %d failed, %d not supported, %d warnings%s\n",
		r.executed, r.passed, r.failed, r.notSupported, r.warnings,
		map[bool]string{true: "", false: ", incomplete"}[r.complete])
	for _, f := range r.failures {
		fmt.Println("dEQP: " + f)
	}
}

func cmdRunDeqp(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("run-deqp", `
usage: invirtio-test run-deqp [options]

Run dEQP's Vulkan synchronization and external memory tests on the host's
GPU with the Mesa prefix, as the deqp test of test-gpu runs them through a
guest's GPU. With the host's own driver, this gives the results to compare
with. Build dEQP with scripts/deqp-build.sh. The output and results go to
the output directory, and the cases that did not pass are listed.`)
	mesa := fs.String("mesa", filepath.Join(root, "build/mesa"), "the prefix of the Mesa that scripts/mesa-build.sh built")
	out := fs.String("out", filepath.Join(root, "build/deqp-native"), "the output directory")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}

	p := deqpDefaults(root)
	if !harness.Exists(p.bin) {
		return fmt.Errorf("%s is not built, run scripts/deqp-build.sh", p.bin)
	}
	dir, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	caselist := filepath.Join(dir, "caselist.txt")
	n, err := deqpCaseList(p.mustpass, caselist)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("the case list is empty, check %s", p.mustpass)
	}
	fmt.Printf("running %d cases, see %s\n", n, filepath.Join(dir, "deqp.log"))
	r, err := runDeqp(context.Background(), p, mesaEnviron(*mesa), caselist, dir)
	r.summary()
	if !r.complete {
		return fmt.Errorf("the run did not finish%s", errSuffix(err))
	}
	if !r.passes(n) {
		return fmt.Errorf("%d of %d cases executed, %d passed, %d failed", r.executed, n,
			r.passed, max(r.failed, len(r.failures)))
	}
	return nil
}

// gpuTestDeqp runs the sync case list through the guest's iGPU.
func gpuTestDeqp(s *gpuSuite) {
	if s.device == "" {
		fmt.Println("the deqp test needs -device, so it is skipped")
		return
	}
	p := deqpDefaults(harness.RepoRoot())
	if !s.Check(harness.Exists(p.bin), "%s is built, with scripts/deqp-build.sh", p.bin) {
		return
	}
	if !s.Check(harness.Exists(filepath.Join(s.mesa, "lib/libvulkan_radeon.so")),
		"the Mesa prefix %s is built", s.mesa) {
		return
	}
	caselist := filepath.Join(s.dir, "caselist.txt")
	n, err := deqpCaseList(p.mustpass, caselist)
	if !s.Check(err == nil && n > 0, "the case list has %d cases%s", n, errSuffix(err)) {
		return
	}
	g := s.start()
	if g == nil {
		return
	}
	if s.attach() == "" {
		return
	}

	before := s.commands(g)
	r, err := runDeqp(s.ctx, p, s.mesaEnv(), caselist, s.dir)
	r.summary()
	s.Check(r.complete && r.executed == n, "dEQP runs its %d cases%s, %d ran, see deqp.log",
		n, errSuffix(err), r.executed)
	s.Check(r.passes(n), "every case passes or is not supported, and some pass: %d passed, %d failed",
		r.passed, max(r.failed, len(r.failures)))
	s.Check(before >= 0 && s.commands(g) > before, "the guest serves dEQP's commands")
	ok := harness.Wait(func() bool {
		v, ok := g.Counters()["resources"]
		return ok && v == 0
	}, 30*time.Second, 200*time.Millisecond)
	s.Check(ok, "the guest frees every resource once dEQP exits")
	s.counters(g)
}
