// +build linux

package libcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
	//"log"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fs2"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/intelrdt"
	"github.com/opencontainers/runc/libcontainer/logs"
	"github.com/opencontainers/runc/libcontainer/system"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// Synchronisation value for cgroup namespace setup.
// The same constant is defined in nsexec.c as "CREATECGROUPNS".
const createCgroupns = 0x80

type parentProcess interface {
	// pid returns the pid for the running process.
	pid() int

	// start starts the process execution.
	start() error

	// send a SIGKILL to the process and wait for the exit.
	terminate() error

	// wait waits on the process returning the process state.
	wait() (*os.ProcessState, error)

	// startTime returns the process start time.
	startTime() (uint64, error)

	signal(os.Signal) error

	externalDescriptors() []string

	setExternalDescriptors(fds []string)

	forwardChildLogs()
}

type filePair struct {
	parent *os.File
	child  *os.File
}

type setnsProcess struct {
	cmd             *exec.Cmd
	messageSockPair filePair
	logFilePair     filePair
	cgroupPaths     map[string]string
	rootlessCgroups bool
	intelRdtPath    string
	config          *initConfig
	fds             []string
	process         *Process
	bootstrapData   io.Reader
	initProcessPid  int
}

func (p *setnsProcess) startTime() (uint64, error) {
	stat, err := system.Stat(p.pid())
	return stat.StartTime, err
}

func (p *setnsProcess) signal(sig os.Signal) error {
	s, ok := sig.(unix.Signal)
	if !ok {
		return errors.New("os: unsupported signal type")
	}
	return unix.Kill(p.pid(), s)
}

func (p *setnsProcess) start() (retErr error) {
	
	
	defer p.messageSockPair.parent.Close()
	err := p.cmd.Start()
	// close the write-side of the pipes (controlled by child)
	p.messageSockPair.child.Close()
	p.logFilePair.child.Close()
	if err != nil {
		return newSystemErrorWithCause(err, "starting setns process")
	}
	defer func() {
		if retErr != nil {
			err := ignoreTerminateErrors(p.terminate())
			if err != nil {
				logrus.WithError(err).Warn("unable to terminate setnsProcess")
			}
		}
	}()
	if p.bootstrapData != nil {
		if _, err := io.Copy(p.messageSockPair.parent, p.bootstrapData); err != nil {
			return newSystemErrorWithCause(err, "copying bootstrap data to pipe")
		}
	}
	if err := p.execSetns(); err != nil {
		return newSystemErrorWithCause(err, "executing setns process")
	}
	if len(p.cgroupPaths) > 0 {
		if err := cgroups.EnterPid(p.cgroupPaths, p.pid()); err != nil && !p.rootlessCgroups {
			// On cgroup v2 + nesting + domain controllers, EnterPid may fail with EBUSY.
			// https://github.com/opencontainers/runc/issues/2356#issuecomment-621277643
			// Try to join the cgroup of InitProcessPid.
			if cgroups.IsCgroup2UnifiedMode() {
				initProcCgroupFile := fmt.Sprintf("/proc/%d/cgroup", p.initProcessPid)
				initCg, initCgErr := cgroups.ParseCgroupFile(initProcCgroupFile)
				if initCgErr == nil {
					if initCgPath, ok := initCg[""]; ok {
						initCgDirpath := filepath.Join(fs2.UnifiedMountpoint, initCgPath)
						logrus.Debugf("adding pid %d to cgroups %v failed (%v), attempting to join %q (obtained from %s)",
							p.pid(), p.cgroupPaths, err, initCg, initCgDirpath)
						// NOTE: initCgDirPath is not guaranteed to exist because we didn't pause the container.
						err = cgroups.WriteCgroupProc(initCgDirpath, p.pid())
					}
				}
			}
			if err != nil {
				return newSystemErrorWithCausef(err, "adding pid %d to cgroups", p.pid())
			}
		}
	}
	if p.intelRdtPath != "" {
		// if Intel RDT "resource control" filesystem path exists
		_, err := os.Stat(p.intelRdtPath)
		if err == nil {
			if err := intelrdt.WriteIntelRdtTasks(p.intelRdtPath, p.pid()); err != nil {
				return newSystemErrorWithCausef(err, "adding pid %d to Intel RDT resource control filesystem", p.pid())
			}
		}
	}
	// set rlimits, this has to be done here because we lose permissions
	// to raise the limits once we enter a user-namespace
	if err := setupRlimits(p.config.Rlimits, p.pid()); err != nil {
		return newSystemErrorWithCause(err, "setting rlimits for process")
	}
	if err := utils.WriteJSON(p.messageSockPair.parent, p.config); err != nil {
		return newSystemErrorWithCause(err, "writing config to pipe")
	}

	ierr := parseSync(p.messageSockPair.parent, func(sync *syncT) error {
		switch sync.Type {
		case procReady:
			// This shouldn't happen.
			panic("unexpected procReady in setns")
		case procHooks:
			// This shouldn't happen.
			panic("unexpected procHooks in setns")
		default:
			return newSystemError(errors.New("invalid JSON payload from child"))
		}
	})

	if err := unix.Shutdown(int(p.messageSockPair.parent.Fd()), unix.SHUT_WR); err != nil {
		return newSystemErrorWithCause(err, "calling shutdown on init pipe")
	}
	// Must be done after Shutdown so the child will exit and we can wait for it.
	if ierr != nil {
		p.wait()
		return ierr
	}
	return nil
}

// execSetns runs the process that executes C code to perform the setns calls
// because setns support requires the C process to fork off a child and perform the setns
// before the go runtime boots, we wait on the process to die and receive the child's pid
// over the provided pipe.
func (p *setnsProcess) execSetns() error {
	status, err := p.cmd.Process.Wait()
	if err != nil {
		p.cmd.Wait()
		return newSystemErrorWithCause(err, "waiting on setns process to finish")
	}
	if !status.Success() {
		p.cmd.Wait()
		return newSystemError(&exec.ExitError{ProcessState: status})
	}
	var pid *pid
	if err := json.NewDecoder(p.messageSockPair.parent).Decode(&pid); err != nil {
		p.cmd.Wait()
		return newSystemErrorWithCause(err, "reading pid from init pipe")
	}

	// Clean up the zombie parent process
	// On Unix systems FindProcess always succeeds.
	firstChildProcess, _ := os.FindProcess(pid.PidFirstChild)

	// Ignore the error in case the child has already been reaped for any reason
	_, _ = firstChildProcess.Wait()

	process, err := os.FindProcess(pid.Pid)
	if err != nil {
		return err
	}
	p.cmd.Process = process
	p.process.ops = p
	return nil
}

// terminate sends a SIGKILL to the forked process for the setns routine then waits to
// avoid the process becoming a zombie.
func (p *setnsProcess) terminate() error {
	if p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	if _, werr := p.wait(); err == nil {
		err = werr
	}
	return err
}

func (p *setnsProcess) wait() (*os.ProcessState, error) {
	err := p.cmd.Wait()

	// Return actual ProcessState even on Wait error
	return p.cmd.ProcessState, err
}

func (p *setnsProcess) pid() int {
	return p.cmd.Process.Pid
}

func (p *setnsProcess) externalDescriptors() []string {
	return p.fds
}

func (p *setnsProcess) setExternalDescriptors(newFds []string) {
	p.fds = newFds
}

func (p *setnsProcess) forwardChildLogs() {
	go logs.ForwardLogs(p.logFilePair.parent)
}

type initProcess struct {
	cmd             *exec.Cmd
	messageSockPair filePair
	logFilePair     filePair
	config          *initConfig
	manager         cgroups.Manager
	intelRdtManager intelrdt.Manager
	container       *linuxContainer
	fds             []string
	process         *Process
	bootstrapData   io.Reader
	sharePidns      bool
}

func (p *initProcess) pid() int {
	return p.cmd.Process.Pid
}

func (p *initProcess) externalDescriptors() []string {
	return p.fds
}

// getChildPid receives the final child's pid over the provided pipe.
func (p *initProcess) getChildPid() (int, error) {
	var pid pid
	if err := json.NewDecoder(p.messageSockPair.parent).Decode(&pid); err != nil {
		p.cmd.Wait()
		return -1, err
	}

	// Clean up the zombie parent process
	// On Unix systems FindProcess always succeeds.
	firstChildProcess, _ := os.FindProcess(pid.PidFirstChild)

	// Ignore the error in case the child has already been reaped for any reason
	_, _ = firstChildProcess.Wait()

	return pid.Pid, nil
}

func (p *initProcess) waitForChildExit(childPid int) error {
	status, err := p.cmd.Process.Wait()
	if err != nil {
		p.cmd.Wait()
		return err
	}
	if !status.Success() {
		p.cmd.Wait()
		return &exec.ExitError{ProcessState: status}
	}

	process, err := os.FindProcess(childPid)
	if err != nil {
		return err
	}
	p.cmd.Process = process
	p.process.ops = p
	return nil
}

func (p *initProcess) start() (retErr error) {
	logrus.Info(fmt.Sprintf("initProcess.start() starts from %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
	
	defer p.messageSockPair.parent.Close()
	logrus.Info(fmt.Sprintf("p.cmd.Start starts from %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
	err := p.cmd.Start()
	logrus.Info(fmt.Sprintf("p.cmd.Start ends at %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))

	p.process.ops = p
	// close the write-side of the pipes (controlled by child)
	p.messageSockPair.child.Close()
	p.logFilePair.child.Close()
	if err != nil {
		p.process.ops = nil
		return newSystemErrorWithCause(err, "starting init process command")
	}
	defer func() {
		if retErr != nil {
			// terminate the process to ensure we can remove cgroups
			if err := ignoreTerminateErrors(p.terminate()); err != nil {
				logrus.WithError(err).Warn("unable to terminate initProcess")
			}

			p.manager.Destroy()
			if p.intelRdtManager != nil {
				p.intelRdtManager.Destroy()
			}
		}
	}()

	// Do this before syncing with child so that no children can escape the
	// cgroup. We don't need to worry about not doing this and not being root
	// because we'd be using the rootless cgroup manager in that case.
	if err := p.manager.Apply(p.pid()); err != nil {
		return newSystemErrorWithCause(err, "applying cgroup configuration for process")
	}
	if p.intelRdtManager != nil {
		if err := p.intelRdtManager.Apply(p.pid()); err != nil {
			return newSystemErrorWithCause(err, "applying Intel RDT configuration for process")
		}
	}
	if _, err := io.Copy(p.messageSockPair.parent, p.bootstrapData); err != nil {
		return newSystemErrorWithCause(err, "copying bootstrap data to pipe")
	}
	childPid, err := p.getChildPid()
	if err != nil {
		return newSystemErrorWithCause(err, "getting the final child's pid from pipe")
	}

	// Save the standard descriptor names before the container process
	// can potentially move them (e.g., via dup2()).  If we don't do this now,
	// we won't know at checkpoint time which file descriptor to look up.
	fds, err := getPipeFds(childPid)
	if err != nil {
		return newSystemErrorWithCausef(err, "getting pipe fds for pid %d", childPid)
	}
	p.setExternalDescriptors(fds)

	// Now it's time to setup cgroup namesapce
	if p.config.Config.Namespaces.Contains(configs.NEWCGROUP) && p.config.Config.Namespaces.PathOf(configs.NEWCGROUP) == "" {
		if _, err := p.messageSockPair.parent.Write([]byte{createCgroupns}); err != nil {
			return newSystemErrorWithCause(err, "sending synchronization value to init process")
		}
	}

	// Wait for our first child to exit
	if err := p.waitForChildExit(childPid); err != nil {
		return newSystemErrorWithCause(err, "waiting for our first child to exit")
	}

	if err := p.createNetworkInterfaces(); err != nil {
		return newSystemErrorWithCause(err, "creating network interfaces")
	}
	if err := p.updateSpecState(); err != nil {
		return newSystemErrorWithCause(err, "updating the spec state")
	}
	if err := p.sendConfig(); err != nil {
		return newSystemErrorWithCause(err, "sending config to init process")
	}
	var (
		sentRun    bool
		sentResume bool
	)
	logrus.Info(fmt.Sprintf("parseSync starts from %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
	ierr := parseSync(p.messageSockPair.parent, func(sync *syncT) error {
		switch sync.Type {
		case procReady:	
			logrus.Info(fmt.Sprintf("procReady starts from %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
			// set rlimits, this has to be done here because we lose permissions
			// to raise the limits once we enter a user-namespace
			if err := setupRlimits(p.config.Rlimits, p.pid()); err != nil {
				return newSystemErrorWithCause(err, "setting rlimits for ready process")
			}
			// call prestart and CreateRuntime hooks
			if !p.config.Config.Namespaces.Contains(configs.NEWNS) {
				// Setup cgroup before the hook, so that the prestart and CreateRuntime hook could apply cgroup permissions.
				if err := p.manager.Set(p.config.Config); err != nil {
					return newSystemErrorWithCause(err, "setting cgroup config for ready process")
				}
				if p.intelRdtManager != nil {
					if err := p.intelRdtManager.Set(p.config.Config); err != nil {
						return newSystemErrorWithCause(err, "setting Intel RDT config for ready process")
					}
				}

				if p.config.Config.Hooks != nil {
					s, err := p.container.currentOCIState()
					if err != nil {
						return err
					}
					// initProcessStartTime hasn't been set yet.
					s.Pid = p.cmd.Process.Pid
					s.Status = specs.StateCreating
					hooks := p.config.Config.Hooks

					if err := hooks[configs.Prestart].RunHooks(s); err != nil {
						return err
					}
					if err := hooks[configs.CreateRuntime].RunHooks(s); err != nil {
						return err
					}
				}
			}

			// generate a timestamp indicating when the container was started
			p.container.created = time.Now().UTC()
			p.container.state = &createdState{
				c: p.container,
			}

			// NOTE: If the procRun state has been synced and the
			// runc-create process has been killed for some reason,
			// the runc-init[2:stage] process will be leaky. And
			// the runc command also fails to parse root directory
			// because the container doesn't have state.json.
			//
			// In order to cleanup the runc-init[2:stage] by
			// runc-delete/stop, we should store the status before
			// procRun sync.
			state, uerr := p.container.updateState(p)
			if uerr != nil {
				return newSystemErrorWithCause(err, "store init state")
			}
			p.container.initProcessStartTime = state.InitProcessStartTime

			// Sync with child.
			if err := writeSync(p.messageSockPair.parent, procRun); err != nil {
				return newSystemErrorWithCause(err, "writing syncT 'run'")
			}
			sentRun = true
			logrus.Info(fmt.Sprintf("procReady ends at %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
		case procHooks:
			logrus.Info(fmt.Sprintf("procHooks starts from %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
			// Setup cgroup before prestart hook, so that the prestart hook could apply cgroup permissions.
			if err := p.manager.Set(p.config.Config); err != nil {
				return newSystemErrorWithCause(err, "setting cgroup config for procHooks process")
			}
			if p.intelRdtManager != nil {
				if err := p.intelRdtManager.Set(p.config.Config); err != nil {
					return newSystemErrorWithCause(err, "setting Intel RDT config for procHooks process")
				}
			}
			if p.config.Config.Hooks != nil {
				s, err := p.container.currentOCIState()
				if err != nil {
					return err
				}
				// initProcessStartTime hasn't been set yet.
				s.Pid = p.cmd.Process.Pid
				s.Status = specs.StateCreating
				hooks := p.config.Config.Hooks

				if err := hooks[configs.Prestart].RunHooks(s); err != nil {
					return err
				}
				if err := hooks[configs.CreateRuntime].RunHooks(s); err != nil {
					return err
				}
			}
			// Sync with child.
			if err := writeSync(p.messageSockPair.parent, procResume); err != nil {
				return newSystemErrorWithCause(err, "writing syncT 'resume'")
			}
			sentResume = true
			logrus.Info(fmt.Sprintf("procHooks ends at %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
		default:
			return newSystemError(errors.New("invalid JSON payload from child"))
		}

		return nil
	})
	logrus.Info(fmt.Sprintf("parseSync ends at %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
	if !sentRun {
		return newSystemErrorWithCause(ierr, "container init")
	}
	if p.config.Config.Namespaces.Contains(configs.NEWNS) && !sentResume {
		return newSystemError(errors.New("could not synchronise after executing prestart and CreateRuntime hooks with container process"))
	}
	if err := unix.Shutdown(int(p.messageSockPair.parent.Fd()), unix.SHUT_WR); err != nil {
		return newSystemErrorWithCause(err, "shutting down init pipe")
	}

	// Must be done after Shutdown so the child will exit and we can wait for it.
	if ierr != nil {
		p.wait()
		return ierr
	}
	//log.Println("end timestamp ",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond))	
	logrus.Info(fmt.Sprintf("initProcess.start() ends at %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
	return nil
}

func (p *initProcess) wait() (*os.ProcessState, error) {
	err := p.cmd.Wait()
	// we should kill all processes in cgroup when init is died if we use host PID namespace
	if p.sharePidns {
		signalAllProcesses(p.manager, unix.SIGKILL)
	}
	return p.cmd.ProcessState, err
}

func (p *initProcess) terminate() error {
	if p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	if _, werr := p.wait(); err == nil {
		err = werr
	}
	return err
}

func (p *initProcess) startTime() (uint64, error) {
	stat, err := system.Stat(p.pid())
	return stat.StartTime, err
}

func (p *initProcess) updateSpecState() error {
	s, err := p.container.currentOCIState()
	if err != nil {
		return err
	}

	p.config.SpecState = s
	return nil
}

func (p *initProcess) sendConfig() error {
	// send the config to the container's init process, we don't use JSON Encode
	// here because there might be a problem in JSON decoder in some cases, see:
	// https://github.com/docker/docker/issues/14203#issuecomment-174177790
	return utils.WriteJSON(p.messageSockPair.parent, p.config)
}

func (p *initProcess) createNetworkInterfaces() error {
	for _, config := range p.config.Config.Networks {
		strategy, err := getStrategy(config.Type)
		if err != nil {
			return err
		}
		n := &network{
			Network: *config,
		}
		if err := strategy.create(n, p.pid()); err != nil {
			return err
		}
		p.config.Networks = append(p.config.Networks, n)
	}
	return nil
}

func (p *initProcess) signal(sig os.Signal) error {
	s, ok := sig.(unix.Signal)
	if !ok {
		return errors.New("os: unsupported signal type")
	}
	return unix.Kill(p.pid(), s)
}

func (p *initProcess) setExternalDescriptors(newFds []string) {
	p.fds = newFds
}

func (p *initProcess) forwardChildLogs() {
	go logs.ForwardLogs(p.logFilePair.parent)
}

func getPipeFds(pid int) ([]string, error) {
	fds := make([]string, 3)

	dirPath := filepath.Join("/proc", strconv.Itoa(pid), "/fd")
	for i := 0; i < 3; i++ {
		// XXX: This breaks if the path is not a valid symlink (which can
		//      happen in certain particularly unlucky mount namespace setups).
		f := filepath.Join(dirPath, strconv.Itoa(i))
		target, err := os.Readlink(f)
		if err != nil {
			// Ignore permission errors, for rootless containers and other
			// non-dumpable processes. if we can't get the fd for a particular
			// file, there's not much we can do.
			if os.IsPermission(err) {
				continue
			}
			return fds, err
		}
		fds[i] = target
	}
	return fds, nil
}

// InitializeIO creates pipes for use with the process's stdio and returns the
// opposite side for each. Do not use this if you want to have a pseudoterminal
// set up for you by libcontainer (TODO: fix that too).
// TODO: This is mostly unnecessary, and should be handled by clients.
func (p *Process) InitializeIO(rootuid, rootgid int) (i *IO, err error) {
	var fds []uintptr
	i = &IO{}
	// cleanup in case of an error
	defer func() {
		if err != nil {
			for _, fd := range fds {
				unix.Close(int(fd))
			}
		}
	}()
	// STDIN
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fds = append(fds, r.Fd(), w.Fd())
	p.Stdin, i.Stdin = r, w
	// STDOUT
	if r, w, err = os.Pipe(); err != nil {
		return nil, err
	}
	fds = append(fds, r.Fd(), w.Fd())
	p.Stdout, i.Stdout = w, r
	// STDERR
	if r, w, err = os.Pipe(); err != nil {
		return nil, err
	}
	fds = append(fds, r.Fd(), w.Fd())
	p.Stderr, i.Stderr = w, r
	// change ownership of the pipes in case we are in a user namespace
	for _, fd := range fds {
		if err := unix.Fchown(int(fd), rootuid, rootgid); err != nil {
			return nil, err
		}
	}
	return i, nil
}
