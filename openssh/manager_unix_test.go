//go:build unix

package openssh

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
)

type fakeSSH struct {
	mu                 sync.Mutex
	calls              [][]string
	listeners          map[string]*net.UnixListener
	checkExitCode      int
	checkErr           error
	exitExitCode       int
	exitErr            error
	spawnExitCode      int
	spawnErr           error
	spawnStarted       chan<- struct{}
	spawnRelease       <-chan struct{}
	spawnSocketOnError bool
	checkStarted       chan<- struct{}
	checkRelease       <-chan struct{}
	exitStarted        chan<- struct{}
	exitRelease        <-chan struct{}
	spawnWithoutSocket bool
	onCheck            func(context.Context) (int, error)
	onExit             func(context.Context) (int, error)
}

func newFakeSSH() *fakeSSH {
	return &fakeSSH{listeners: make(map[string]*net.UnixListener)}
}

func newSocketDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "kit-ssh-")
	Require.NoError(t, err)
	t.Cleanup(func() { Require.NoError(t, os.RemoveAll(directory)) })
	return directory
}

func newTestManager(t *testing.T, directory string, fake *fakeSSH) *PersistentManager {
	t.Helper()
	manager, err := NewPersistentManager(directory, PersistentConfig{
		RunSSH:                  fake.run,
		MaximumControlPathBytes: 1_000,
		EstablishPollInterval:   time.Millisecond,
		EstablishTimeout:        time.Second,
	})
	Require.NoError(t, err)
	return manager
}

func newConnectedTestManager(
	t *testing.T,
	identity, destination string,
) (*fakeSSH, *PersistentManager, Generation) {
	t.Helper()
	fake := newFakeSSH()
	manager := newTestManager(t, newSocketDir(t), fake)
	generation, err := manager.Connect(context.Background(), identity, testTarget(destination))
	Require.NoError(t, err)
	t.Cleanup(func() { fake.closeAll() })
	return fake, manager, generation
}

func controlPath(arguments []string) string {
	for index, argument := range arguments {
		if argument == "-S" && index+1 < len(arguments) {
			return strings.ReplaceAll(arguments[index+1], "%%", "%")
		}
	}
	return ""
}

func operationKind(arguments []string) string {
	joined := strings.Join(arguments, " ")
	switch {
	case strings.Contains(joined, "-MNf"):
		return "spawn"
	case strings.Contains(joined, "-O check"):
		return "check"
	case strings.Contains(joined, "-O exit"):
		return "exit"
	default:
		return "other"
	}
}

func (f *fakeSSH) run(ctx context.Context, arguments []string) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), arguments...))
	kind := operationKind(arguments)
	path := controlPath(arguments)
	spawnStarted, spawnRelease := f.spawnStarted, f.spawnRelease
	checkStarted, checkRelease := f.checkStarted, f.checkRelease
	exitStarted, exitRelease := f.exitStarted, f.exitRelease
	checkExitCode, checkErr := f.checkExitCode, f.checkErr
	exitExitCode, exitErr := f.exitExitCode, f.exitErr
	spawnExitCode, spawnErr := f.spawnExitCode, f.spawnErr
	spawnSocketOnError := f.spawnSocketOnError
	spawnWithoutSocket := f.spawnWithoutSocket
	onCheck, onExit := f.onCheck, f.onExit
	_, alive := f.listeners[path]
	f.mu.Unlock()

	switch kind {
	case "spawn":
		if spawnStarted != nil {
			spawnStarted <- struct{}{}
		}
		if spawnRelease != nil {
			<-spawnRelease
		}
		if spawnSocketOnError {
			if err := f.openSocket(path); err != nil {
				return -1, err
			}
		}
		if spawnErr != nil || spawnExitCode != 0 {
			return spawnExitCode, spawnErr
		}
		if !spawnWithoutSocket {
			if err := f.openSocket(path); err != nil {
				return -1, err
			}
		}
		return 0, nil
	case "check":
		if checkStarted != nil {
			checkStarted <- struct{}{}
		}
		if checkRelease != nil {
			<-checkRelease
		}
		if onCheck != nil {
			return onCheck(ctx)
		}
		if checkErr != nil || checkExitCode != 0 {
			return checkExitCode, checkErr
		}
		if alive {
			return 0, nil
		}
		return 255, errors.New("no master running")
	case "exit":
		if exitStarted != nil {
			exitStarted <- struct{}{}
		}
		if exitRelease != nil {
			<-exitRelease
		}
		if onExit != nil {
			return onExit(ctx)
		}
		if exitErr != nil || exitExitCode != 0 {
			return exitExitCode, exitErr
		}
		if err := f.closeSocket(path, true); err != nil {
			return -1, err
		}
		return 0, nil
	default:
		return 0, nil
	}
}

func (f *fakeSSH) openSocket(path string) error {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.listeners[path] = listener
	f.mu.Unlock()
	go func() {
		for {
			conn, acceptErr := listener.AcceptUnix()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return nil
}

func (f *fakeSSH) closeSocket(path string, unlink bool) error {
	f.mu.Lock()
	listener := f.listeners[path]
	delete(f.listeners, path)
	f.mu.Unlock()
	if listener == nil {
		return nil
	}
	listener.SetUnlinkOnClose(unlink)
	return listener.Close()
}

func (f *fakeSSH) closeAll() {
	f.mu.Lock()
	paths := make([]string, 0, len(f.listeners))
	for path := range f.listeners {
		paths = append(paths, path)
	}
	f.mu.Unlock()
	for _, path := range paths {
		_ = f.closeSocket(path, true)
	}
}

func (f *fakeSSH) callsSnapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := make([][]string, len(f.calls))
	for index, call := range f.calls {
		calls[index] = append([]string(nil), call...)
	}
	return calls
}

func (f *fakeSSH) operationKinds() []string {
	calls := f.callsSnapshot()
	kinds := make([]string, len(calls))
	for index, call := range calls {
		kinds[index] = operationKind(call)
	}
	return kinds
}

func (f *fakeSSH) spawnCount() int {
	count := 0
	for _, kind := range f.operationKinds() {
		if kind == "spawn" {
			count++
		}
	}
	return count
}

func (f *fakeSSH) setCheckResult(exitCode int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkExitCode, f.checkErr = exitCode, err
}

func (f *fakeSSH) setExitResult(exitCode int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exitExitCode, f.exitErr = exitCode, err
}

func (f *fakeSSH) setSpawnGate(started chan<- struct{}, release <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawnStarted, f.spawnRelease = started, release
}

func (f *fakeSSH) setExitGate(started chan<- struct{}, release <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exitStarted, f.exitRelease = started, release
}

func (f *fakeSSH) setCheckGate(started chan<- struct{}, release <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkStarted, f.checkRelease = started, release
}

func TestPersistentManagerRejectsSymlinkedDirectoryBeforeSSH(t *testing.T) {
	target := t.TempDir()
	directory := filepath.Join(t.TempDir(), "control")
	Require.NoError(t, os.Symlink(target, directory))
	fake := newFakeSSH()
	manager := newTestManager(t, directory, fake)

	_, err := manager.Connect(context.Background(), "studio", testTarget("wes@studio"))

	Require.Error(t, err)
	Assert.Empty(t, fake.callsSnapshot())
}

func TestPersistentManagerAdoptsOwnedMuxSocket(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	directory := newSocketDir(t)
	fake := newFakeSSH()
	manager := newTestManager(t, directory, fake)
	require.NoError(fake.openSocket(manager.SocketPath("studio", testTarget("wes@studio"))))
	defer fake.closeAll()

	generation, err := manager.Connect(context.Background(), "studio", testTarget("wes@studio"))

	require.NoError(err)
	assert.Positive(generation)
	assert.Zero(fake.spawnCount())
}

func TestPersistentManagerRemovesOnlyPositivelyStaleSocket(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake, manager, _ := newConnectedTestManager(t, "studio", "wes@studio")
	path := manager.SocketPath("studio", testTarget("wes@studio"))
	require.NoError(fake.closeSocket(path, false))

	generation, err := manager.Connect(context.Background(), "studio", testTarget("wes@studio"))

	require.NoError(err)
	assert.Positive(generation)
	assert.Equal(2, fake.spawnCount())
}

func TestPersistentManagerPreservesOccupiedSocket(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	directory := newSocketDir(t)
	fake := newFakeSSH()
	manager := newTestManager(t, directory, fake)
	path := manager.SocketPath("studio", testTarget("wes@studio"))
	require.NoError(fake.openSocket(path))
	defer fake.closeAll()
	fake.setCheckResult(255, errors.New("invalid mux greeting"))

	_, err := manager.Connect(context.Background(), "studio", testTarget("wes@studio"))

	require.ErrorIs(err, ErrControlPathOccupied)
	assert.FileExists(path)
	assert.Zero(fake.spawnCount())
}

func TestPersistentManagerPreservesIndeterminateSocket(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	directory := newSocketDir(t)
	fake := newFakeSSH()
	manager := newTestManager(t, directory, fake)
	path := manager.SocketPath("studio", testTarget("wes@studio"))
	require.NoError(fake.openSocket(path))
	defer fake.closeAll()
	sentinel := errors.New("ssh failed to start")
	fake.setCheckResult(-1, sentinel)

	_, err := manager.Connect(context.Background(), "studio", testTarget("wes@studio"))

	require.ErrorIs(err, ErrProbeIndeterminate)
	require.ErrorIs(err, sentinel)
	assert.FileExists(path)
	assert.Zero(fake.spawnCount())
}

func TestPersistentManagerChangesDestinationByTeardownThenConnect(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake, manager, _ := newConnectedTestManager(t, "studio", "wes@old")
	oldCalls := len(fake.callsSnapshot())

	generation, err := manager.Connect(context.Background(), "studio", testTarget("wes@new"))

	require.NoError(err)
	assert.Positive(generation)
	assert.Equal([]string{"exit", "spawn", "check"}, fake.operationKinds()[oldCalls:])
	assert.Equal("wes@new", manager.Destination("studio"))
}

func TestPersistentManagerBindsRunnerToConnectionGeneration(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	defaultRunner := newFakeSSH()
	boundRunner := newFakeSSH()
	manager := newTestManager(t, newSocketDir(t), defaultRunner)
	t.Cleanup(defaultRunner.closeAll)
	t.Cleanup(boundRunner.closeAll)

	generation, err := manager.ConnectWithRunner(
		context.Background(), "studio", testTarget("wes@studio"), boundRunner.run,
	)
	require.NoError(err)
	alive, err := manager.IsAlive(context.Background(), "studio", generation)
	require.NoError(err)
	assert.True(alive)
	require.NoError(manager.Disconnect(context.Background(), "studio"))

	assert.Empty(defaultRunner.callsSnapshot())
	assert.Equal(
		[]string{"spawn", "check", "check", "exit"},
		boundRunner.operationKinds(),
	)
}

func TestPersistentManagerUsesOldRunnerForReplacementTeardown(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	defaultRunner := newFakeSSH()
	oldRunner := newFakeSSH()
	newRunner := newFakeSSH()
	manager := newTestManager(t, newSocketDir(t), defaultRunner)
	t.Cleanup(defaultRunner.closeAll)
	t.Cleanup(oldRunner.closeAll)
	t.Cleanup(newRunner.closeAll)

	_, err := manager.ConnectWithRunner(
		context.Background(), "studio", testTarget("wes@old"), oldRunner.run,
	)
	require.NoError(err)
	_, err = manager.ConnectWithRunner(
		context.Background(), "studio", testTarget("wes@new"), newRunner.run,
	)
	require.NoError(err)

	assert.Empty(defaultRunner.callsSnapshot())
	assert.Equal([]string{"spawn", "check", "exit"}, oldRunner.operationKinds())
	assert.Equal([]string{"spawn", "check"}, newRunner.operationKinds())
}

func TestPersistentManagerArgumentsRemainBoundToOriginalTarget(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	_, manager, oldGeneration := newConnectedTestManager(t, "studio", "wes@old")
	oldArguments, err := manager.ConnectionArguments("studio", oldGeneration)
	require.NoError(err)
	oldPath := controlPath(oldArguments)

	newGeneration, err := manager.Connect(
		context.Background(), "studio", testTarget("wes@new"),
	)
	require.NoError(err)
	_, err = manager.ConnectionArguments("studio", oldGeneration)
	require.ErrorIs(err, ErrConnectionChanged)
	newArguments, err := manager.ConnectionArguments("studio", newGeneration)
	require.NoError(err)
	newPath := controlPath(newArguments)

	assert.NotEqual(oldPath, newPath)
	assert.FileExists(newPath)
}

func TestPersistentManagerKeepsOldDestinationWhenReplacementTeardownFails(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake, manager, _ := newConnectedTestManager(t, "studio", "wes@old")
	sentinel := errors.New("exit failed")
	fake.setExitResult(255, sentinel)

	_, err := manager.Connect(context.Background(), "studio", testTarget("wes@new"))

	require.ErrorIs(err, sentinel)
	assert.Equal("wes@old", manager.Destination("studio"))
	assert.Equal(1, fake.spawnCount())
	assert.FileExists(manager.SocketPath("studio", testTarget("wes@old")))
}

func TestDisconnectWaitsForSocketDrainAfterExitReturns(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake, manager, _ := newConnectedTestManager(t, "studio", "wes@studio")
	path := manager.SocketPath("studio", testTarget("wes@studio"))
	exitReturned := make(chan struct{})
	releaseSocket := make(chan struct{})
	fake.onExit = func(context.Context) (int, error) {
		close(exitReturned)
		go func() {
			<-releaseSocket
			_ = fake.closeSocket(path, false)
		}()
		return 0, nil
	}
	disconnectResult := make(chan error, 1)
	go func() {
		disconnectResult <- manager.Disconnect(context.Background(), "studio")
	}()
	<-exitReturned

	returnedEarly := false
	select {
	case <-disconnectResult:
		returnedEarly = true
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseSocket)
	if !returnedEarly {
		require.NoError(<-disconnectResult)
	}

	assert.False(returnedEarly)
	assert.NoFileExists(path)
}

func TestDisconnectBoundsSocketDrainAndPreservesListener(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake := newFakeSSH()
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		CleanupTimeout:          5 * time.Millisecond,
		EstablishPollInterval:   time.Millisecond,
		MaximumControlPathBytes: 1_000,
	})
	require.NoError(err)
	_, err = manager.Connect(context.Background(), "studio", testTarget("wes@studio"))
	require.NoError(err)
	t.Cleanup(fake.closeAll)
	fake.onExit = func(context.Context) (int, error) { return 0, nil }

	err = manager.Disconnect(context.Background(), "studio")

	require.ErrorIs(err, context.DeadlineExceeded)
	path := manager.SocketPath("studio", testTarget("wes@studio"))
	assert.FileExists(path)
	assert.Equal(StateStopping, manager.State("studio"))
	entry := manager.host("studio", false)
	entry.mu.Lock()
	stoppingGeneration := entry.generation
	entry.mu.Unlock()
	arguments, argumentsErr := manager.ConnectionArguments("studio", stoppingGeneration)
	require.NoError(argumentsErr)
	masterlessArguments, argumentsErr := ClientArguments("")
	require.NoError(argumentsErr)
	assert.Equal(masterlessArguments, arguments)
	callsBeforeRecovery := len(fake.callsSnapshot())
	alive, aliveErr := manager.IsAlive(
		context.Background(), "studio", stoppingGeneration,
	)
	assert.False(alive)
	require.ErrorIs(aliveErr, ErrConnectionChanged)
	assert.Len(fake.callsSnapshot(), callsBeforeRecovery)
	err = manager.Disconnect(context.Background(), "studio")
	require.ErrorIs(err, context.DeadlineExceeded)
	assert.Len(fake.callsSnapshot(), callsBeforeRecovery)
	assert.Equal(StateStopping, manager.State("studio"))

	_, err = manager.Connect(context.Background(), "studio", testTarget("wes@studio"))
	require.ErrorIs(err, context.DeadlineExceeded)
	assert.Len(fake.callsSnapshot(), callsBeforeRecovery)
	assert.Equal(StateStopping, manager.State("studio"))
	require.NoError(fake.closeSocket(path, false))

	generation, err := manager.Connect(
		context.Background(), "studio", testTarget("wes@studio"),
	)
	require.NoError(err)
	assert.Positive(generation)
	assert.Equal(StateConnected, manager.State("studio"))
	assert.Equal(
		[]string{"spawn", "check"},
		fake.operationKinds()[callsBeforeRecovery:],
	)
}

func TestDisconnectBoundsExitCommand(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake := newFakeSSH()
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		CleanupTimeout:          10 * time.Millisecond,
		EstablishPollInterval:   time.Millisecond,
		MaximumControlPathBytes: 1_000,
	})
	require.NoError(err)
	_, err = manager.Connect(context.Background(), "studio", testTarget("wes@studio"))
	require.NoError(err)
	t.Cleanup(fake.closeAll)
	deadlineObserved := make(chan bool, 1)
	fake.onExit = func(ctx context.Context) (int, error) {
		_, bounded := ctx.Deadline()
		deadlineObserved <- bounded
		<-ctx.Done()
		return -1, ctx.Err()
	}

	err = manager.Disconnect(context.Background(), "studio")

	require.ErrorIs(err, context.DeadlineExceeded)
	assert.True(<-deadlineObserved)
}

func TestPersistentManagerReportsTypedSpawnFailure(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake := newFakeSSH()
	sentinel := errors.New("permission denied")
	fake.spawnExitCode, fake.spawnErr = 255, sentinel
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		CleanupTimeout:          5 * time.Millisecond,
		EstablishPollInterval:   time.Millisecond,
		MaximumControlPathBytes: 1_000,
	})
	require.NoError(err)

	_, err = manager.Connect(context.Background(), "studio", testTarget("wes@studio"))

	var commandErr *CommandError
	require.ErrorAs(err, &commandErr)
	require.ErrorIs(err, sentinel)
	assert.Equal(255, commandErr.ExitCode)
	assert.Equal(StateError, manager.State("studio"))
}

func TestPersistentManagerFailedStartCleansCreatedSocket(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake := newFakeSSH()
	fake.spawnSocketOnError = true
	fake.spawnExitCode = 255
	manager := newTestManager(t, newSocketDir(t), fake)
	t.Cleanup(fake.closeAll)

	_, err := manager.Connect(
		context.Background(), "studio", testTarget("wes@studio"),
	)

	require.Error(err)
	assert.NoFileExists(manager.SocketPath("studio", testTarget("wes@studio")))
	assert.Contains(fake.operationKinds(), "exit")
}

func TestPersistentManagerFailedStartCleansLateSocket(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake := newFakeSSH()
	fake.spawnWithoutSocket = true
	directory := newSocketDir(t)
	manager, err := NewPersistentManager(directory, PersistentConfig{
		RunSSH:                  fake.run,
		MaximumControlPathBytes: 1_000,
		EstablishPollInterval:   time.Second,
		EstablishTimeout:        5 * time.Millisecond,
		CleanupTimeout:          100 * time.Millisecond,
	})
	require.NoError(err)
	t.Cleanup(fake.closeAll)
	target := testTarget("wes@studio")
	path := manager.SocketPath("studio", target)
	lateSocketErr := make(chan error, 1)
	go func() {
		timer := time.NewTimer(20 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		lateSocketErr <- fake.openSocket(path)
	}()

	_, err = manager.Connect(context.Background(), "studio", target)

	require.NoError(<-lateSocketErr)
	require.ErrorIs(err, context.DeadlineExceeded)
	assert.NoFileExists(path)
	assert.Equal([]string{"spawn", "exit"}, fake.operationKinds())
	assert.Equal(StateError, manager.State("studio"))
}

func TestFailedStartDrainTimeoutQuarantinesSocket(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake := newFakeSSH()
	fake.spawnSocketOnError = true
	fake.spawnExitCode = 255
	fake.onExit = func(context.Context) (int, error) { return 0, nil }
	events := make(chan Event, 4)
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		CleanupTimeout:          5 * time.Millisecond,
		EstablishPollInterval:   time.Millisecond,
		MaximumControlPathBytes: 1_000,
		OnEvent: func(event Event) {
			events <- event
		},
	})
	require.NoError(err)
	t.Cleanup(fake.closeAll)
	target := testTarget("wes@studio")
	path := manager.SocketPath("studio", target)

	_, err = manager.Connect(context.Background(), "studio", target)

	require.ErrorIs(err, context.DeadlineExceeded)
	assert.Equal(StateStopping, manager.State("studio"))
	assert.FileExists(path)
	for {
		select {
		case event := <-events:
			if event.State == StateStopping {
				assert.Equal(err.Error(), event.Message)
				goto stoppingObserved
			}
		case <-time.After(time.Second):
			require.Fail("failed-start stopping event was not delivered")
		}
	}

stoppingObserved:
	callsBeforeRecovery := len(fake.callsSnapshot())
	_, err = manager.Connect(context.Background(), "studio", target)
	require.ErrorIs(err, context.DeadlineExceeded)
	assert.Len(fake.callsSnapshot(), callsBeforeRecovery)
	require.NoError(fake.closeSocket(path, false))
	fake.spawnSocketOnError = false
	fake.spawnExitCode = 0

	generation, err := manager.Connect(context.Background(), "studio", target)

	require.NoError(err)
	assert.Positive(generation)
	assert.Equal(StateConnected, manager.State("studio"))
	assert.Equal(
		[]string{"spawn", "check"},
		fake.operationKinds()[callsBeforeRecovery:],
	)
}

func TestPersistentManagerFailedCleanupBlocksTargetReplacement(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake := newFakeSSH()
	fake.spawnSocketOnError = true
	fake.spawnExitCode = 255
	firstCleanupErr := errors.New("first cleanup failed")
	fake.exitErr = firstCleanupErr
	manager := newTestManager(t, newSocketDir(t), fake)
	t.Cleanup(fake.closeAll)
	oldTarget := testTarget("wes@old")
	newTarget := testTarget("wes@new")
	oldPath := manager.SocketPath("studio", oldTarget)
	newPath := manager.SocketPath("studio", newTarget)

	_, err := manager.Connect(context.Background(), "studio", oldTarget)
	require.ErrorIs(err, firstCleanupErr)
	assert.FileExists(oldPath)

	fake.spawnSocketOnError = false
	fake.spawnExitCode = 0
	secondCleanupErr := errors.New("replacement cleanup failed")
	fake.exitErr = secondCleanupErr
	callCount := len(fake.callsSnapshot())
	_, err = manager.Connect(context.Background(), "studio", newTarget)

	require.ErrorIs(err, secondCleanupErr)
	assert.Equal("wes@old", manager.Destination("studio"))
	assert.Equal([]string{"exit"}, fake.operationKinds()[callCount:])
	assert.NoFileExists(newPath)

	fake.exitErr = nil
	_, err = manager.Connect(context.Background(), "studio", newTarget)
	require.NoError(err)
	assert.NoFileExists(oldPath)
	assert.FileExists(newPath)
	assert.Equal("wes@new", manager.Destination("studio"))
}

func TestStaleGenerationCannotTouchOrMarkReplacement(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	_, manager, oldGeneration := newConnectedTestManager(t, "studio", "wes@old")

	newGeneration, err := manager.Connect(context.Background(), "studio", testTarget("wes@new"))

	require.NoError(err)
	assert.NotEqual(oldGeneration, newGeneration)
	assert.False(manager.TouchActivity("studio", oldGeneration))
	assert.False(manager.SetProbeFailed("studio", oldGeneration, "stale probe"))
	assert.Equal(StateConnected, manager.State("studio"))
}

func TestDisconnectUnknownIdentityDoesNotTouchFilesystem(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "does-not-exist")
	manager, err := NewPersistentManager(directory, PersistentConfig{})
	Require.NoError(t, err)

	Require.NoError(t, manager.Disconnect(context.Background(), "unknown"))

	Assert.NoDirExists(t, directory)
}

func TestPersistentManagerRejectsEmptySocketDirectory(t *testing.T) {
	manager, err := NewPersistentManager("", PersistentConfig{})

	Assert.Nil(t, manager)
	var pathErr *PathError
	Require.ErrorAs(t, err, &pathErr)
	Assert.Equal(t, "empty control directory", pathErr.Reason)
}

func TestPersistentManagerSkipsIdleCandidateRefreshedAfterScan(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	_, manager, generation := newConnectedTestManager(t, "studio", "wes@studio")
	entry := manager.host("studio", false)
	entry.mu.Lock()
	entry.lastActive = time.Now().Add(-2 * time.Minute)
	entry.mu.Unlock()
	idleBefore := time.Now().Add(-time.Minute)

	require.True(manager.TouchActivity("studio", generation))
	require.NoError(manager.disconnectIdleCandidate(
		context.Background(), "studio", idleBefore,
	))

	assert.Equal(StateConnected, manager.State("studio"))
	assert.FileExists(manager.SocketPath("studio", testTarget("wes@studio")))
}

func TestPersistentManagerProbeFailureDoesNotRefreshActivity(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	_, manager, generation := newConnectedTestManager(
		t, "studio", "wes@studio",
	)
	entry := manager.host("studio", false)
	entry.mu.Lock()
	entry.lastActive = time.Now().Add(-2 * time.Minute)
	entry.mu.Unlock()
	idleBefore := time.Now().Add(-time.Minute)

	require.True(manager.SetProbeFailed("studio", generation, "probe one"))
	require.True(manager.SetProbeFailed("studio", generation, "probe two"))
	require.NoError(manager.disconnectIdleCandidate(
		context.Background(), "studio", idleBefore,
	))

	assert.Equal(StateDisconnected, manager.State("studio"))
	assert.NoFileExists(manager.SocketPath("studio", testTarget("wes@studio")))
}

func TestIdleScanStopsReservingConnectionsAfterCancellation(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake := newFakeSSH()
	manager := newTestManager(t, newSocketDir(t), fake)
	firstGeneration, err := manager.Connect(
		context.Background(), "first", testTarget("wes@first"),
	)
	require.NoError(err)
	secondGeneration, err := manager.Connect(
		context.Background(), "second", testTarget("wes@second"),
	)
	require.NoError(err)
	t.Cleanup(fake.closeAll)
	for _, identity := range []string{"first", "second"} {
		entry := manager.host(identity, false)
		entry.mu.Lock()
		entry.lastActive = time.Now().Add(-2 * time.Minute)
		entry.mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	fake.onExit = func(exitCtx context.Context) (int, error) {
		cancel()
		return -1, exitCtx.Err()
	}

	manager.disconnectIdle(ctx)

	firstEntry := manager.host("first", false)
	firstEntry.mu.Lock()
	firstAfter := firstEntry.generation
	firstEntry.mu.Unlock()
	secondEntry := manager.host("second", false)
	secondEntry.mu.Lock()
	secondAfter := secondEntry.generation
	secondEntry.mu.Unlock()
	assert.Equal(
		firstGeneration+secondGeneration+1,
		firstAfter+secondAfter,
	)
	assert.Equal(StateConnected, manager.State("first"))
	assert.Equal(StateConnected, manager.State("second"))
}

func TestIdleTeardownFailureEmitsErrorAndPreservesConnection(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	events := make(chan Event, 8)
	fake := newFakeSSH()
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		EstablishPollInterval:   time.Millisecond,
		MaximumControlPathBytes: 1_000,
		OnEvent: func(event Event) {
			events <- event
		},
	})
	require.NoError(err)
	generation, err := manager.Connect(
		context.Background(), "studio", testTarget("wes@studio"),
	)
	require.NoError(err)
	t.Cleanup(fake.closeAll)
	for {
		select {
		case event := <-events:
			if event.State == StateConnected {
				goto connected
			}
		case <-time.After(time.Second):
			require.Fail("connected event was not delivered")
		}
	}

connected:
	entry := manager.host("studio", false)
	entry.mu.Lock()
	entry.lastActive = time.Now().Add(-2 * time.Minute)
	entry.mu.Unlock()
	sentinel := errors.New("idle exit failed")
	fake.setExitResult(255, sentinel)

	err = manager.disconnectIdleCandidate(
		context.Background(), "studio", time.Now().Add(-time.Minute),
	)

	require.ErrorIs(err, sentinel)
	select {
	case event := <-events:
		assert.Equal(StateError, event.State)
		assert.Greater(event.Generation, generation)
		assert.Contains(event.Message, sentinel.Error())
	case <-time.After(time.Second):
		require.Fail("idle cleanup error event was not delivered")
	}
	assert.Equal(StateConnected, manager.State("studio"))
	assert.Equal("wes@studio", manager.Destination("studio"))
	assert.FileExists(manager.SocketPath("studio", testTarget("wes@studio")))
}

func TestEstablishTimeoutUsesBoundedCleanupAndKeepsOriginalError(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	cleanupDeadline := make(chan bool, 1)
	fake := newFakeSSH()
	fake.onCheck = func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	fake.onExit = func(ctx context.Context) (int, error) {
		_, bounded := ctx.Deadline()
		cleanupDeadline <- bounded
		<-ctx.Done()
		return -1, ctx.Err()
	}
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		EstablishTimeout:        5 * time.Millisecond,
		EstablishPollInterval:   time.Millisecond,
		CleanupTimeout:          10 * time.Millisecond,
		MaximumControlPathBytes: 1_000,
	})
	require.NoError(err)

	_, err = manager.Connect(context.Background(), "studio", testTarget("wes@studio"))

	require.Error(err)
	assert.Contains(err.Error(), "timeout waiting for control master")
	assert.True(<-cleanupDeadline)
	require.ErrorIs(err, context.DeadlineExceeded)
}

func TestCanceledEstablishKeepsCancellationWhenCleanupFails(t *testing.T) {
	cleanupSentinel := errors.New("cleanup failed")
	fake := newFakeSSH()
	fake.onCheck = func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	fake.onExit = func(context.Context) (int, error) {
		return -1, cleanupSentinel
	}
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		CleanupTimeout:          10 * time.Millisecond,
		MaximumControlPathBytes: 1_000,
	})
	Require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = manager.Connect(ctx, "studio", testTarget("wes@studio"))

	Require.ErrorIs(t, err, context.Canceled)
	Require.ErrorIs(t, err, cleanupSentinel)
}

func TestDisconnectRejectsSocketReplacedAfterExit(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake, manager, _ := newConnectedTestManager(t, "studio", "wes@studio")
	path := manager.SocketPath("studio", testTarget("wes@studio"))
	fake.onExit = func(context.Context) (int, error) {
		require.NoError(fake.closeSocket(path, true))
		require.NoError(os.WriteFile(path, nil, 0o600))
		return 0, nil
	}

	err := manager.Disconnect(context.Background(), "studio")

	var securityErr *ControlPathSecurityError
	require.ErrorAs(err, &securityErr)
	assert.FileExists(path)
	assert.Equal(StateStopping, manager.State("studio"))
}

func TestEventsCarryGenerationAndSuppressStaleState(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	events := make(chan Event, 8)
	fake := newFakeSSH()
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		MaximumControlPathBytes: 1_000,
		EstablishPollInterval:   time.Millisecond,
		OnEvent: func(event Event) {
			events <- event
		},
	})
	require.NoError(err)

	generation, err := manager.Connect(context.Background(), "studio", testTarget("wes@studio"))
	require.NoError(err)
	var received []Event
	for len(received) == 0 || received[len(received)-1].State != StateConnected {
		select {
		case event := <-events:
			received = append(received, event)
		case <-time.After(time.Second):
			require.Fail("connected event was not delivered")
		}
	}
	for _, event := range received {
		assert.Equal(generation, event.Generation)
	}

	manager.emit("studio", generation-1, StateDisconnected, "")

	select {
	case event := <-events:
		assert.Fail("stale event was delivered", "event: %+v", event)
	default:
	}
}

func TestEventQueueCoalescesWhileCallbackIsBlocked(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	t.Cleanup(func() { close(releaseCallback) })
	observed := make(chan Event, 8)
	var blockOnce sync.Once
	fake := newFakeSSH()
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		MaximumControlPathBytes: 1_000,
		EstablishPollInterval:   time.Millisecond,
		OnEvent: func(event Event) {
			blockOnce.Do(func() {
				close(callbackStarted)
				<-releaseCallback
			})
			observed <- event
		},
	})
	require.NoError(err)
	generation, err := manager.Connect(
		context.Background(), "studio", testTarget("wes@studio"),
	)
	require.NoError(err)
	t.Cleanup(fake.closeAll)
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		require.Fail("event callback did not start")
	}
	for range 100 {
		require.True(manager.SetProbeFailed("studio", generation, "probe failed"))
		recovered, connectErr := manager.Connect(
			context.Background(), "studio", testTarget("wes@studio"),
		)
		require.NoError(connectErr)
		assert.Equal(generation, recovered)
	}
	entry := manager.host("studio", false)
	entry.mu.Lock()
	pending := append([]Event(nil), entry.events...)
	entry.mu.Unlock()
	require.Len(pending, 2)
	assert.Equal(StateProbeFailed, pending[0].State)
	assert.Equal(StateConnected, pending[1].State)

	releaseCallback <- struct{}{}
	for _, expectedState := range []string{
		StateConnecting, StateProbeFailed, StateConnected,
	} {
		select {
		case event := <-observed:
			assert.Equal(expectedState, event.State)
		case <-time.After(time.Second):
			require.Fail("coalesced event was not delivered")
		}
	}
}

func TestConnectEmitsConnectedEventWhenProbeFailureRecovers(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	events := make(chan Event, 8)
	fake := newFakeSSH()
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		MaximumControlPathBytes: 1_000,
		EstablishPollInterval:   time.Millisecond,
		OnEvent: func(event Event) {
			events <- event
		},
	})
	require.NoError(err)
	generation, err := manager.Connect(context.Background(), "studio", testTarget("wes@studio"))
	require.NoError(err)
	for {
		select {
		case event := <-events:
			if event.State == StateConnected {
				goto initiallyConnected
			}
		case <-time.After(time.Second):
			require.Fail("initial connected event was not delivered")
		}
	}

initiallyConnected:
	require.True(manager.SetProbeFailed("studio", generation, "temporary failure"))
	select {
	case event := <-events:
		assert.Equal(StateProbeFailed, event.State)
	case <-time.After(time.Second):
		require.Fail("probe-failed event was not delivered")
	}

	recoveredGeneration, err := manager.Connect(
		context.Background(), "studio", testTarget("wes@studio"),
	)
	require.NoError(err)
	assert.Equal(generation, recoveredGeneration)
	select {
	case event := <-events:
		assert.Equal(StateConnected, event.State)
		assert.Equal(generation, event.Generation)
	case <-time.After(time.Second):
		require.Fail("recovered connected event was not delivered")
	}
}

func TestPersistentManagerEventCallbackCanDisconnectReentrantly(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake := newFakeSSH()
	callbackResult := make(chan error, 1)
	var manager *PersistentManager
	var err error
	manager, err = NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		MaximumControlPathBytes: 1_000,
		EstablishPollInterval:   time.Millisecond,
		OnEvent: func(event Event) {
			if event.State != StateConnecting {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			callbackResult <- manager.Disconnect(ctx, event.Identity)
		},
	})
	require.NoError(err)
	t.Cleanup(fake.closeAll)

	_, err = manager.Connect(context.Background(), "studio", testTarget("wes@studio"))

	require.NoError(err)
	select {
	case callbackErr := <-callbackResult:
		require.NoError(callbackErr)
	case <-time.After(time.Second):
		require.Fail("reentrant callback did not return")
	}
	assert.Equal(StateDisconnected, manager.State("studio"))
}

func TestPersistentManagerAcceptsExplicitConnectionOptions(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	fake := newFakeSSH()
	options := DefaultConnectionOptions()
	options.TCPKeepAlive = false
	manager, err := NewPersistentManager(newSocketDir(t), PersistentConfig{
		RunSSH:                  fake.run,
		ConnectionOptions:       &options,
		MaximumControlPathBytes: 1_000,
		EstablishPollInterval:   time.Millisecond,
	})
	require.NoError(err)

	_, err = manager.Connect(context.Background(), "studio", testTarget("wes@studio"))

	require.NoError(err)
	spawn := fake.callsSnapshot()[0]
	assert.Contains(spawn, "TCPKeepAlive=no")
}

func TestPersistentManagerResolvesSocketDirectoryAtConstruction(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	originalDirectory, err := os.Getwd()
	require.NoError(err)
	t.Cleanup(func() { require.NoError(os.Chdir(originalDirectory)) })
	temporaryDirectory, err := os.MkdirTemp("/tmp", "kit-cwd-")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(os.RemoveAll(temporaryDirectory)) })
	require.NoError(os.Chdir(temporaryDirectory))
	constructorDirectory, err := os.Getwd()
	require.NoError(err)
	fake := newFakeSSH()
	manager, err := NewPersistentManager("control", PersistentConfig{
		RunSSH:                  fake.run,
		MaximumControlPathBytes: 1_000,
		EstablishPollInterval:   time.Millisecond,
	})
	require.NoError(err)
	require.NoError(os.Chdir(t.TempDir()))

	_, err = manager.Connect(context.Background(), "studio", testTarget("wes@studio"))

	require.NoError(err)
	expectedPath := filepath.Join(
		constructorDirectory,
		"control",
		controlNameForTarget("studio", testTarget("wes@studio"))+".sock",
	)
	assert.Equal(expectedPath, manager.SocketPath("studio", testTarget("wes@studio")))
	assert.Equal(expectedPath, controlPath(fake.callsSnapshot()[0]))
}
