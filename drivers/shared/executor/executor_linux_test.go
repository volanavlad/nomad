package executor

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/client/testutil"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/plugins/drivers"
	tu "github.com/hashicorp/nomad/testutil"
	lconfigs "github.com/opencontainers/runc/libcontainer/configs"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func init() {
	executorFactories["LibcontainerExecutor"] = libcontainerFactory
}

var libcontainerFactory = executorFactory{
	new: NewExecutorWithIsolation,
	configureExecCmd: func(t *testing.T, cmd *ExecCommand) {
		cmd.ResourceLimits = true
		setupRootfs(t, cmd.TaskDir)
	},
}

// testExecutorContextWithChroot returns an ExecutorContext and AllocDir with
// chroot. Use testExecutorContext if you don't need a chroot.
//
// The caller is responsible for calling AllocDir.Destroy() to cleanup.
func testExecutorCommandWithChroot(t *testing.T) *testExecCmd {
	chrootEnv := map[string]string{
		"/etc/ld.so.cache":  "/etc/ld.so.cache",
		"/etc/ld.so.conf":   "/etc/ld.so.conf",
		"/etc/ld.so.conf.d": "/etc/ld.so.conf.d",
		"/lib":              "/lib",
		"/lib64":            "/lib64",
		"/usr/lib":          "/usr/lib",
		"/bin/ls":           "/bin/ls",
		"/bin/cat":          "/bin/cat",
		"/bin/echo":         "/bin/echo",
		"/bin/bash":         "/bin/bash",
		"/bin/sleep":        "/bin/sleep",
		"/foobar":           "/does/not/exist",
	}

	alloc := mock.Alloc()
	task := alloc.Job.TaskGroups[0].Tasks[0]
	taskEnv := taskenv.NewBuilder(mock.Node(), alloc, task, "global").Build()

	allocDir := allocdir.NewAllocDir(testlog.HCLogger(t), filepath.Join(os.TempDir(), alloc.ID))
	if err := allocDir.Build(); err != nil {
		t.Fatalf("AllocDir.Build() failed: %v", err)
	}
	if err := allocDir.NewTaskDir(task.Name).Build(true, chrootEnv); err != nil {
		allocDir.Destroy()
		t.Fatalf("allocDir.NewTaskDir(%q) failed: %v", task.Name, err)
	}
	td := allocDir.TaskDirs[task.Name]
	cmd := &ExecCommand{
		Env:     taskEnv.List(),
		TaskDir: td.Dir,
		Resources: &drivers.Resources{
			NomadResources: alloc.AllocatedResources.Tasks[task.Name],
		},
	}

	testCmd := &testExecCmd{
		command:  cmd,
		allocDir: allocDir,
	}
	configureTLogging(t, testCmd)
	return testCmd
}

func TestExecutor_IsolationAndConstraints(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	testutil.ExecCompatible(t)

	testExecCmd := testExecutorCommandWithChroot(t)
	execCmd, allocDir := testExecCmd.command, testExecCmd.allocDir
	execCmd.Cmd = "/bin/ls"
	execCmd.Args = []string{"-F", "/", "/etc/"}
	defer allocDir.Destroy()

	execCmd.ResourceLimits = true

	executor := NewExecutorWithIsolation(testlog.HCLogger(t))
	defer executor.Shutdown("SIGKILL", 0)

	ps, err := executor.Launch(execCmd)
	require.NoError(err)
	require.NotZero(ps.Pid)

	state, err := executor.Wait(context.Background())
	require.NoError(err)
	require.Zero(state.ExitCode)

	// Check if the resource constraints were applied
	if lexec, ok := executor.(*LibcontainerExecutor); ok {
		state, err := lexec.container.State()
		require.NoError(err)

		memLimits := filepath.Join(state.CgroupPaths["memory"], "memory.limit_in_bytes")
		data, err := ioutil.ReadFile(memLimits)
		require.NoError(err)

		expectedMemLim := strconv.Itoa(int(execCmd.Resources.NomadResources.Memory.MemoryMB * 1024 * 1024))
		actualMemLim := strings.TrimSpace(string(data))
		require.Equal(actualMemLim, expectedMemLim)
		require.NoError(executor.Shutdown("", 0))
		executor.Wait(context.Background())

		// Check if Nomad has actually removed the cgroups
		tu.WaitForResult(func() (bool, error) {
			_, err = os.Stat(memLimits)
			if err == nil {
				return false, fmt.Errorf("expected an error from os.Stat %s", memLimits)
			}
			return true, nil
		}, func(err error) { t.Error(err) })

	}
	expected := `/:
alloc/
bin/
dev/
etc/
lib/
lib64/
local/
proc/
secrets/
sys/
tmp/
usr/

/etc/:
ld.so.cache
ld.so.conf
ld.so.conf.d/`
	tu.WaitForResult(func() (bool, error) {
		output := testExecCmd.stdout.String()
		act := strings.TrimSpace(string(output))
		if act != expected {
			return false, fmt.Errorf("Command output incorrectly: want %v; got %v", expected, act)
		}
		return true, nil
	}, func(err error) { t.Error(err) })
}

// Exec Launch looks for the binary only inside the chroot
func TestExecutor_EscapeContainer(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	testutil.ExecCompatible(t)

	testExecCmd := testExecutorCommandWithChroot(t)
	execCmd, allocDir := testExecCmd.command, testExecCmd.allocDir
	execCmd.Cmd = "/bin/kill" // missing from the chroot container
	defer allocDir.Destroy()

	execCmd.ResourceLimits = true

	executor := NewExecutorWithIsolation(testlog.HCLogger(t))
	defer executor.Shutdown("SIGKILL", 0)

	_, err := executor.Launch(execCmd)
	require.Error(err)
	require.Regexp("^file /bin/kill not found under path", err)

	// Bare files are looked up using the system path, inside the container
	allocDir.Destroy()
	testExecCmd = testExecutorCommandWithChroot(t)
	execCmd, allocDir = testExecCmd.command, testExecCmd.allocDir
	execCmd.Cmd = "kill"
	_, err = executor.Launch(execCmd)
	require.Error(err)
	require.Regexp("^file kill not found under path", err)

	allocDir.Destroy()
	testExecCmd = testExecutorCommandWithChroot(t)
	execCmd, allocDir = testExecCmd.command, testExecCmd.allocDir
	execCmd.Cmd = "echo"
	_, err = executor.Launch(execCmd)
	require.NoError(err)
}

func TestExecutor_ClientCleanup(t *testing.T) {
	t.Parallel()
	testutil.ExecCompatible(t)
	require := require.New(t)

	testExecCmd := testExecutorCommandWithChroot(t)
	execCmd, allocDir := testExecCmd.command, testExecCmd.allocDir
	defer allocDir.Destroy()

	executor := NewExecutorWithIsolation(testlog.HCLogger(t))
	defer executor.Shutdown("", 0)

	// Need to run a command which will produce continuous output but not
	// too quickly to ensure executor.Exit() stops the process.
	execCmd.Cmd = "/bin/bash"
	execCmd.Args = []string{"-c", "while true; do /bin/echo X; /bin/sleep 1; done"}
	execCmd.ResourceLimits = true

	ps, err := executor.Launch(execCmd)

	require.NoError(err)
	require.NotZero(ps.Pid)
	time.Sleep(500 * time.Millisecond)
	require.NoError(executor.Shutdown("SIGINT", 100*time.Millisecond))

	ch := make(chan interface{})
	go func() {
		executor.Wait(context.Background())
		close(ch)
	}()

	select {
	case <-ch:
		// all good
	case <-time.After(5 * time.Second):
		require.Fail("timeout waiting for exec to shutdown")
	}

	output := testExecCmd.stdout.String()
	require.NotZero(len(output))
	time.Sleep(2 * time.Second)
	output1 := testExecCmd.stdout.String()
	require.Equal(len(output), len(output1))
}

func TestExecutor_cmdDevices(t *testing.T) {
	input := []*drivers.DeviceConfig{
		{
			HostPath:    "/dev/null",
			TaskPath:    "/task/dev/null",
			Permissions: "rwm",
		},
	}

	expected := &lconfigs.Device{
		Path:        "/task/dev/null",
		Type:        99,
		Major:       1,
		Minor:       3,
		Permissions: "rwm",
	}

	found, err := cmdDevices(input)
	require.NoError(t, err)
	require.Len(t, found, 1)

	// ignore file permission and ownership
	// as they are host specific potentially
	d := found[0]
	d.FileMode = 0
	d.Uid = 0
	d.Gid = 0

	require.EqualValues(t, expected, d)
}

func TestExecutor_cmdMounts(t *testing.T) {
	input := []*drivers.MountConfig{
		{
			HostPath: "/host/path-ro",
			TaskPath: "/task/path-ro",
			Readonly: true,
		},
		{
			HostPath: "/host/path-rw",
			TaskPath: "/task/path-rw",
			Readonly: false,
		},
	}

	expected := []*lconfigs.Mount{
		{
			Source:      "/host/path-ro",
			Destination: "/task/path-ro",
			Flags:       unix.MS_BIND | unix.MS_RDONLY,
			Device:      "bind",
		},
		{
			Source:      "/host/path-rw",
			Destination: "/task/path-rw",
			Flags:       unix.MS_BIND,
			Device:      "bind",
		},
	}

	require.EqualValues(t, expected, cmdMounts(input))
}
