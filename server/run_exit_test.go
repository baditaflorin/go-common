package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/config"
)

// runHelperEnv names the mode the helper process below runs in. It is empty in
// a normal test run, which makes TestRunHelperProcess a no-op.
const runHelperEnv = "GO_COMMON_RUN_HELPER"

// TestRunHelperProcess is the child half of TestRunExitsNonZeroWhenTheListenFails:
// it is not a test on its own. The parent re-runs this test binary with the port
// already taken, so Start cannot listen, and looks at how the process ends.
func TestRunHelperProcess(t *testing.T) {
	mode := os.Getenv(runHelperEnv)
	if mode == "" {
		t.Skip("helper process for TestRunExitsNonZeroWhenTheListenFails")
	}
	h := func(http.ResponseWriter, *http.Request) {}
	switch mode {
	case "run":
		Run("go_run_exit_test", "0.0.0", h)
	case "run-drain":
		Run("go_run_exit_test", "0.0.0", h, WithGracefulDrain(time.Second, time.Second))
	case "runtier":
		RunTier("go_run_exit_test", "0.0.0", h, "vetted", false)
	case "start-drain":
		func() {
			defer fmt.Println("start caller cleanup completed")
			srv := New(config.Load("go_start_exit_test", "0.0.0"), WithGracefulDrain(time.Second, time.Second))
			if err := srv.Start(); err == nil || !strings.Contains(err.Error(), "listen") {
				t.Fatalf("Start error = %v; want a returned listen failure", err)
			}
			fmt.Println("start returned listen error")
		}()
		return
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	// Run returned, which means it swallowed the failure: report it as a success
	// so the parent sees what a caller's supervisor would see.
	os.Exit(0)
}

func TestStartGracefulListenFailureReturnsToCaller(t *testing.T) {
	// Use a subprocess so the old log.Fatalf behavior fails this assertion
	// without killing the parent test process.
	held, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Skipf("cannot reserve a port: %v", err)
	}
	defer held.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunHelperProcess$")
	cmd.Env = append(os.Environ(), runHelperEnv+"=start-drain", "PORT="+strconv.Itoa(held.Addr().(*net.TCPAddr).Port))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Start terminated its caller instead of returning: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "start returned listen error") || !strings.Contains(string(out), "start caller cleanup completed") {
		t.Fatalf("listen failure did not return and unwind caller cleanup:\n%s", out)
	}
}

// A service whose port is taken used to end with exit status 0 and no log line:
// Run and RunTier dropped the error Start returns, main returned, and the
// supervisor read that as a clean stop. They must log the error and exit 1.
func TestRunExitsNonZeroWhenTheListenFails(t *testing.T) {
	// Hold a port on all interfaces for the whole test, so the child's bind on
	// ":<port>" is refused.
	held, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Skipf("cannot reserve a port: %v", err)
	}
	defer held.Close()
	port := strconv.Itoa(held.Addr().(*net.TCPAddr).Port)

	for _, mode := range []string{"run", "runtier", "run-drain"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunHelperProcess$")
			cmd.Env = append(os.Environ(), runHelperEnv+"="+mode, "PORT="+port)
			out, err := cmd.CombinedOutput()

			var exitCode int
			switch e := err.(type) {
			case nil:
				exitCode = 0
			case *exec.ExitError:
				exitCode = e.ExitCode()
			default:
				t.Fatalf("running the helper: %v\n%s", err, out)
			}
			if ctx.Err() != nil {
				t.Fatalf("the helper did not exit within 30s (it should fail fast when it cannot listen)\n%s", out)
			}
			if exitCode != 1 {
				t.Errorf("exit status = %d, want 1 (a service that cannot listen must not report success)\n%s", exitCode, out)
			}
			if !strings.Contains(string(out), "listen") {
				t.Errorf("output does not say why the server stopped (want the listen error):\n%s", out)
			}
		})
	}
}
