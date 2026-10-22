package lxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/secrets"
)

var (
	ErrInstanceNotFound = errors.New("VM instance not found")
	errExecTransport    = errors.New("vm exec transport failed")
)

const (
	rebootDownTimeout  = 2 * time.Minute
	rebootPollInterval = 250 * time.Millisecond
	cloudInitTimeout   = 5 * time.Minute
)

const guestUserSetup = `set -e
if ! id -u ubuntu >/dev/null 2>&1; then
	useradd --create-home --shell /bin/bash --groups sudo ubuntu
fi
usermod -aG sudo ubuntu
install -d -m 0755 /etc/sudoers.d
printf '%s\n' 'ubuntu ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-mph-ubuntu
chmod 0440 /etc/sudoers.d/90-mph-ubuntu
`

var (
	readyTimeout      = 5 * time.Minute
	readyPollInterval = 5 * time.Second
)

// vmIdentity is the guest user commands run as and the transfer owner.
type vmIdentity struct {
	uid  uint32
	gid  uint32
	home string
	user string
}

// rootIdentity is reserved for internal probes such as identity lookups and
// the readiness check. Agent commands always run as the image's ubuntu user.
var rootIdentity = vmIdentity{home: "/root", user: "root"}

// Client drives LXD over the local Unix socket.
type Client struct {
	log      zerolog.Logger
	server   lxd.InstanceServer
	redactor *secrets.Redactor
	images   map[string]lxd.ImageServer
	identity map[string]vmIdentity

	connectImage func(url string, args *lxd.ConnectionArgs) (lxd.ImageServer, error)
}

// New connects to the local LXD daemon and verifies the connection.
func New(log zerolog.Logger, resolver secrets.Resolver) (*Client, error) {
	server, err := lxd.ConnectLXDUnix("", nil)
	if err != nil {
		return nil, fmt.Errorf("connect to LXD: %w (is LXD installed (deb,snap, other package) and does the current user have access to its socket, typically via the lxd group?)", err)
	}
	if _, _, err := server.GetServer(); err != nil {
		return nil, fmt.Errorf("verify LXD server: %w (is LXD installed (deb,snap, other package) and does the current user have access to its socket, typically via the lxd group?)", err)
	}
	return NewWithServer(server, log, resolver), nil
}

// NewWithServer returns a Client backed by an existing LXD server.
func NewWithServer(server lxd.InstanceServer, log zerolog.Logger, resolver secrets.Resolver) *Client {
	return &Client{
		log:          log.With().Str("component", "lxd").Logger(),
		server:       server,
		redactor:     secrets.NewRedactor(resolver),
		images:       make(map[string]lxd.ImageServer),
		identity:     make(map[string]vmIdentity),
		connectImage: lxd.ConnectSimpleStreams,
	}
}

func (c *Client) imageServer(url string) (lxd.ImageServer, error) {
	if server, ok := c.images[url]; ok {
		return server, nil
	}
	server, err := c.connectImage(url, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to image server %s: %w", url, err)
	}
	c.images[url] = server
	return server, nil
}

func (c *Client) resolveImage(image string) (lxd.ImageServer, api.Image, error) {
	url, alias, err := config.ImageRemote(image)
	if err != nil {
		return nil, api.Image{}, err
	}
	server, err := c.imageServer(url)
	if err != nil {
		return nil, api.Image{}, err
	}
	entry, _, err := server.GetImageAliasType(string(api.InstanceTypeVM), alias)
	if err != nil {
		return nil, api.Image{}, fmt.Errorf("resolve image %q as a virtual machine: %w", image, err)
	}
	img, _, err := server.GetImage(entry.Target)
	if err != nil {
		return nil, api.Image{}, fmt.Errorf("resolve image %q: %w", image, err)
	}
	return server, *img, nil
}

// Info returns the LXD instance record as JSON.
func (c *Client) Info(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("instance name is required")
	}

	c.log.Info().Str("vm", name).Msg("fetching VM info")

	instance, _, err := c.server.GetInstanceFull(name)
	if err != nil {
		c.log.Debug().Err(err).Str("vm", name).Msg("LXD info failed")
		if api.StatusErrorCheck(err, http.StatusNotFound) {
			return "", fmt.Errorf("%w: %s", ErrInstanceNotFound, name)
		}
		return "", fmt.Errorf("vm info %q failed: %w", name, err)
	}

	data, err := json.Marshal(instance)
	if err != nil {
		return "", fmt.Errorf("vm info %q failed: %w", name, err)
	}
	return string(data), nil
}

// EnsureRunning starts an existing stopped instance and waits until it answers,
// and rejects instances that are not virtual machines.
func (c *Client) EnsureRunning(ctx context.Context, name string) error {
	instance, _, err := c.server.GetInstance(name)
	if err != nil {
		if api.StatusErrorCheck(err, http.StatusNotFound) {
			return fmt.Errorf("%w: %s", ErrInstanceNotFound, name)
		}
		return fmt.Errorf("vm check %q failed: %w", name, err)
	}
	if instance.Type != string(api.InstanceTypeVM) {
		return fmt.Errorf("instance %q is a %s, not a virtual-machine; mph requires a virtual-machine instance", name, instance.Type)
	}
	if instance.StatusCode == api.Running {
		return nil
	}

	c.log.Info().Str("vm", name).Msg("starting existing VM")
	op, err := c.server.UpdateInstanceState(name, api.InstanceStatePut{Action: "start"}, "")
	if err != nil {
		return fmt.Errorf("start VM %q: %w", name, err)
	}
	if err := waitOperationContext(ctx, op); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("start VM %q: %w", name, err)
	}
	if err := c.waitForVMReady(ctx, name); err != nil {
		return fmt.Errorf("VM %q did not become ready: %w", name, err)
	}
	return nil
}

func (c *Client) secureBootConfig() (string, string) {
	if c.server.HasExtension("instance_boot_mode") {
		return "boot.mode", "uefi-nosecureboot"
	}
	return "security.secureboot", "false"
}

func (c *Client) defaultRootDevice() (map[string]string, error) {
	profile, _, err := c.server.GetProfile("default")
	if err != nil {
		return nil, fmt.Errorf("read default LXD profile: %w", err)
	}
	root, ok := profile.Devices["root"]
	if !ok {
		return nil, fmt.Errorf("default LXD profile has no root device")
	}
	device := make(map[string]string, len(root))
	for key, value := range root {
		device[key] = value
	}
	device["type"] = "disk"
	device["path"] = "/"
	return device, nil
}

// Launch creates the VM from the configured image and waits for the guest to answer.
func (c *Client) Launch(ctx context.Context, vm config.VMConfig) error {
	ram, err := config.NormalizeSize(vm.RAM)
	if err != nil {
		return fmt.Errorf("vm.ram: %w", err)
	}
	disk, err := config.NormalizeSize(vm.Disk)
	if err != nil {
		return fmt.Errorf("vm.disk: %w", err)
	}
	rootDevice, err := c.defaultRootDevice()
	if err != nil {
		return err
	}
	rootDevice["size"] = disk
	imageServer, image, err := c.resolveImage(vm.Image)
	if err != nil {
		return err
	}

	c.log.Info().Str("vm", vm.Name).Str("image", vm.Image).Msgf("Creating VM %s", vm.Name)

	bootKey, bootValue := c.secureBootConfig()

	req := api.InstancesPost{
		Name:  vm.Name,
		Type:  api.InstanceTypeVM,
		Start: true,
		InstancePut: api.InstancePut{
			Config: map[string]string{
				"limits.cpu":    strconv.Itoa(vm.CPU),
				"limits.memory": ram,
				bootKey:         bootValue,
			},
			Devices: map[string]map[string]string{
				"root": rootDevice,
			},
		},
	}

	op, err := c.server.CreateInstanceFromImage(imageServer, image, req)
	if err != nil {
		return fmt.Errorf("launch VM %q: %w", vm.Name, err)
	}
	if err := waitRemote(ctx, op); err != nil {
		return fmt.Errorf("launch VM %q: %w", vm.Name, err)
	}

	if err := c.waitForVMReady(ctx, vm.Name); err != nil {
		return err
	}
	if _, err := c.identityFor(ctx, vm.Name); err != nil {
		return err
	}

	c.log.Info().Str("vm", vm.Name).Msg("VM launched")
	return nil
}

// Delete removes the VM; purge maps to the LXD delete force flag.
func (c *Client) Delete(ctx context.Context, name string, purge bool) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("instance name is required")
	}

	c.log.Info().Msgf("Deleting VM %s", name)

	op, err := c.server.DeleteInstance(name, purge)
	if err != nil {
		return fmt.Errorf("vm delete %q failed: %w", name, err)
	}
	if err := waitOperationContext(ctx, op); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("vm delete %q failed: %w", name, err)
	}

	delete(c.identity, name)
	c.log.Info().Str("vm", name).Bool("purge", purge).Msg("VM deleted")
	return nil
}

// Exec runs a command in the VM and returns the combined stdout and stderr.
func (c *Client) Exec(ctx context.Context, name string, command string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("instance name is required")
	}
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("command is required")
	}

	resolved, err := c.redactor.Redact(command)
	if err != nil {
		c.log.Debug().Err(err).Str("vm", name).Str("cmd", command).Msg("vm exec failed to resolve secrets")
		return "", fmt.Errorf("vm exec %q %q failed: %w", name, command, err)
	}

	identity, err := c.identityFor(ctx, name)
	if err != nil {
		return "", fmt.Errorf("vm exec %q %q failed: %w", name, command, err)
	}

	c.log.Debug().Str("vm", name).Str("cmd", command).Msgf("Running %s", command)

	out, exit, err := c.run(ctx, name, resolved, identity)
	if err != nil {
		c.log.Debug().Err(err).Str("vm", name).Str("cmd", command).Msg("vm exec failed")
		return "", fmt.Errorf("vm exec %q %q failed: %w", name, command, err)
	}
	if exit != 0 {
		msg := c.redactor.Scrub(strings.TrimSpace(out))
		err := fmt.Errorf("vm exec %q %q failed: exit status %d: %s", name, command, exit, msg)
		c.log.Debug().Err(err).Str("vm", name).Str("cmd", command).Msg("vm exec failed")
		return "", err
	}

	return c.redactor.Scrub(out), nil
}

// run executes one command over the websocket exec API, returning combined
// output and the exit status.
func (c *Client) run(ctx context.Context, name, command string, identity vmIdentity) (string, int, error) {
	var (
		mu  sync.Mutex
		out strings.Builder
	)
	writer := &lockedWriter{mu: &mu, w: &out}
	dataDone := make(chan bool)
	op, err := c.server.ExecInstance(name, api.InstanceExecPost{
		Command:   []string{"bash", "-c", command},
		WaitForWS: true,
		User:      identity.uid,
		Group:     identity.gid,
		Cwd:       identity.home,
		Environment: map[string]string{
			"HOME":    identity.home,
			"USER":    identity.user,
			"LOGNAME": identity.user,
			"PATH":    "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"LANG":    "C.UTF-8",
		},
	}, &lxd.InstanceExecArgs{Stdout: writer, Stderr: writer, DataDone: dataDone})
	if err != nil {
		return "", 0, fmt.Errorf("%w: %s", errExecTransport, c.redactor.Scrub(err.Error()))
	}
	if err := waitOperationContext(ctx, op); err != nil {
		if ctx.Err() != nil {
			return "", 0, err
		}
		return "", 0, fmt.Errorf("%w: %s", errExecTransport, c.redactor.Scrub(err.Error()))
	}
	select {
	case <-dataDone:
	case <-ctx.Done():
		_ = op.Cancel()
		return "", 0, ctx.Err()
	}

	exit := 0
	if ret, ok := op.Get().Metadata["return"].(float64); ok {
		exit = int(ret)
	}
	mu.Lock()
	defer mu.Unlock()
	return out.String(), exit, nil
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (c *Client) identityFor(ctx context.Context, name string) (vmIdentity, error) {
	if identity, ok := c.identity[name]; ok {
		return identity, nil
	}
	identity, err := c.resolveIdentity(ctx, name)
	if err != nil {
		return vmIdentity{}, err
	}
	c.identity[name] = identity
	return identity, nil
}

func (c *Client) ensureGuestUser(ctx context.Context, name string) (string, error) {
	c.log.Debug().Str("vm", name).Msg("waiting for cloud-init")
	initCtx, cancel := context.WithTimeout(ctx, cloudInitTimeout)
	_, exit, err := c.run(initCtx, name, "cloud-init status --wait", rootIdentity)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			c.log.Warn().Str("vm", name).Msg("cloud-init did not finish in time; continuing")
		} else {
			c.log.Debug().Err(err).Str("vm", name).Msg("cloud-init wait failed; continuing")
		}
	} else if exit != 0 {
		c.log.Debug().Str("vm", name).Int("exit", exit).Msg("cloud-init reported an error; continuing")
	}

	uidOut, exit, err := c.run(ctx, name, "id -u ubuntu", rootIdentity)
	if err != nil {
		return "", fmt.Errorf("check ubuntu user: %w", err)
	}
	if exit == 0 {
		_, exit, err = c.run(ctx, name, "sudo -u ubuntu sudo -n true", rootIdentity)
		if err != nil {
			return "", fmt.Errorf("check passwordless sudo: %w", err)
		}
		if exit == 0 {
			c.log.Debug().Str("vm", name).Msg("ubuntu user is ready")
			return uidOut, nil
		}
	} else {
		uidOut = ""
	}

	c.log.Info().Str("vm", name).Msg("setting up ubuntu user with passwordless sudo")
	out, exit, err := c.run(ctx, name, guestUserSetup, rootIdentity)
	if err != nil {
		return "", fmt.Errorf("set up ubuntu user: %w", err)
	}
	if exit != 0 {
		return "", fmt.Errorf("set up ubuntu user: guest command exited with status %d: %s", exit, strings.TrimSpace(out))
	}
	return uidOut, nil
}

func (c *Client) resolveIdentity(ctx context.Context, name string) (vmIdentity, error) {
	uidOut, err := c.ensureGuestUser(ctx, name)
	if err != nil {
		return vmIdentity{}, err
	}

	if strings.TrimSpace(uidOut) == "" {
		var exit int
		uidOut, exit, err = c.run(ctx, name, "id -u ubuntu", rootIdentity)
		if err != nil {
			return vmIdentity{}, err
		}
		if exit != 0 {
			return vmIdentity{}, fmt.Errorf("vm %q has no ubuntu user", name)
		}
	}
	uid, err := strconv.ParseUint(strings.TrimSpace(uidOut), 10, 32)
	if err != nil {
		return vmIdentity{}, fmt.Errorf("parse ubuntu uid %q: %w", strings.TrimSpace(uidOut), err)
	}

	gidOut, exit, err := c.run(ctx, name, "id -g ubuntu", rootIdentity)
	if err != nil {
		return vmIdentity{}, fmt.Errorf("look up ubuntu gid: %w", err)
	}
	if exit != 0 {
		return vmIdentity{}, fmt.Errorf("vm %q: lookup of ubuntu gid failed with exit status %d", name, exit)
	}
	gid, err := strconv.ParseUint(strings.TrimSpace(gidOut), 10, 32)
	if err != nil {
		return vmIdentity{}, fmt.Errorf("parse ubuntu gid %q: %w", strings.TrimSpace(gidOut), err)
	}

	homeOut, exit, err := c.run(ctx, name, "getent passwd ubuntu", rootIdentity)
	if err != nil {
		return vmIdentity{}, fmt.Errorf("look up ubuntu home: %w", err)
	}
	if exit != 0 {
		return vmIdentity{}, fmt.Errorf("vm %q: lookup of ubuntu home failed with exit status %d", name, exit)
	}
	fields := strings.Split(strings.TrimSpace(homeOut), ":")
	if len(fields) < 6 || fields[5] == "" {
		return vmIdentity{}, fmt.Errorf("vm %q: getent passwd ubuntu returned malformed entry %q", name, strings.TrimSpace(homeOut))
	}

	return vmIdentity{uid: uint32(uid), gid: uint32(gid), home: fields[5], user: "ubuntu"}, nil
}

// Transfer copies a host directory recursively into the VM under vmDest.
func (c *Client) Transfer(ctx context.Context, vmName, hostPath, vmDest string) error {
	if strings.TrimSpace(vmName) == "" {
		return fmt.Errorf("instance name is required")
	}
	if strings.TrimSpace(hostPath) == "" {
		return fmt.Errorf("host path is required")
	}
	if strings.TrimSpace(vmDest) == "" {
		return fmt.Errorf("destination is required")
	}

	c.log.Info().Str("vm", vmName).Str("src", hostPath).Str("dst", vmDest).Msg("transferring content to VM")

	identity, err := c.identityFor(ctx, vmName)
	if err != nil {
		return fmt.Errorf("transfer %q: %w", hostPath, err)
	}

	root, err := filepath.EvalSymlinks(hostPath)
	if err != nil {
		return fmt.Errorf("transfer %q: %w", hostPath, err)
	}

	destRoot := path.Join(vmDest, filepath.Base(filepath.Clean(hostPath)))
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		vmPath := path.Join(destRoot, filepath.ToSlash(rel))

		if info.Mode()&os.ModeSymlink != 0 {
			c.log.Warn().Str("path", p).Msg("skipping symlink in content transfer")
			return nil
		}
		if d.IsDir() {
			return c.server.CreateInstanceFile(vmName, vmPath, lxd.InstanceFileArgs{
				Type: "directory",
				Mode: 0o755,
				UID:  int64(identity.uid),
				GID:  int64(identity.gid),
			})
		}

		resolvedP, resolveErr := filepath.EvalSymlinks(p)
		if resolveErr != nil {
			c.log.Warn().Str("path", p).Msg("failed to resolve path symlinks")
			return resolveErr
		}
		if !strings.HasPrefix(resolvedP, root) {
			c.log.Warn().Str("path", p).Msg("path outside walk root after symlink resolution")
			return nil
		}

		file, err := os.Open(resolvedP) //#nosec G304
		if err != nil {
			return err
		}
		createErr := c.server.CreateInstanceFile(vmName, vmPath, lxd.InstanceFileArgs{
			Content: file,
			Type:    "file",
			Mode:    int(info.Mode().Perm()),
			UID:     int64(identity.uid),
			GID:     int64(identity.gid),
		})
		_ = file.Close()
		return createErr
	})
	if err != nil {
		return fmt.Errorf("transfer %q to %s:%s: %w", hostPath, vmName, vmDest, err)
	}

	c.log.Info().Str("vm", vmName).Str("src", hostPath).Str("dst", vmDest).Msg("transfer complete")
	return nil
}

// InstallKernel installs a kernel package in the VM, reboots, and verifies the
// new kernel is running.
func (c *Client) InstallKernel(ctx context.Context, name, kernelPkg string) error {
	c.log.Info().Str("vm", name).Str("kernel", kernelPkg).Msg("installing kernel")

	installCmd := fmt.Sprintf("sudo apt-get update && sudo apt-get install -y %s", kernelPkg)
	if _, err := c.Exec(ctx, name, installCmd); err != nil {
		return fmt.Errorf("failed to install kernel packages: %w", err)
	}

	if _, err := c.Exec(ctx, name, "sudo update-grub"); err != nil {
		return fmt.Errorf("failed to update grub: %w", err)
	}

	c.log.Info().Str("vm", name).Msg("rebooting VM for new kernel")
	if _, err := c.Exec(ctx, name, "sudo systemctl reboot --no-block"); err != nil {
		return fmt.Errorf("failed to reboot VM: %w", err)
	}

	if err := c.waitForVMDown(ctx, name); err != nil {
		return fmt.Errorf("VM did not shut down for reboot: %w", err)
	}

	if err := c.waitForVMReady(ctx, name); err != nil {
		return fmt.Errorf("VM did not come back after reboot: %w", err)
	}

	runningKernel, err := c.Exec(ctx, name, "uname -r")
	if err != nil {
		return fmt.Errorf("failed to get running kernel version: %w", err)
	}
	runningKernel = strings.TrimSpace(runningKernel)

	expectedVersion := strings.TrimPrefix(kernelPkg, "linux-image-unsigned-")
	expectedVersion = strings.TrimPrefix(expectedVersion, "linux-image-")

	if !strings.Contains(runningKernel, expectedVersion) {
		return fmt.Errorf("kernel version mismatch: expected %q to contain %q", runningKernel, expectedVersion)
	}

	c.log.Info().Str("vm", name).Str("kernel", runningKernel).Msg("kernel installed and verified")
	return nil
}

// waitForVMDown polls the instance state until the guest has left the running
// state after a reboot request.
func (c *Client) waitForVMDown(ctx context.Context, name string) error {
	deadline := time.Now().Add(rebootDownTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		state, _, err := c.server.GetInstanceState(name)
		if err != nil {
			return fmt.Errorf("get VM %q state: %w", name, err)
		}
		if state.StatusCode != api.Running {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timeout waiting for VM %s to leave the running state", name)
		}

		if err := sleepContext(ctx, rebootPollInterval); err != nil {
			return err
		}
	}
}

// waitForVMReady polls a root exec until the guest answers.
func (c *Client) waitForVMReady(ctx context.Context, name string) error {
	deadline := time.Now().Add(readyTimeout)
	var lastErr error

	for {
		if !time.Now().Before(deadline) {
			if lastErr != nil {
				return fmt.Errorf("timeout waiting for VM %s to become ready: %w", name, lastErr)
			}
			return fmt.Errorf("timeout waiting for VM %s to become ready", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, exit, err := c.run(ctx, name, "echo ready", rootIdentity)
		if err == nil && exit == 0 {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("guest command exited with status %d", exit)
		}
		c.log.Debug().Str("vm", name).Err(lastErr).Msg("waiting for VM to be ready")

		if err := sleepContext(ctx, readyPollInterval); err != nil {
			return err
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func waitOperationContext(ctx context.Context, op lxd.Operation) error {
	if err := op.WaitContext(ctx); err != nil {
		if ctx.Err() != nil {
			_ = op.Cancel()
			return ctx.Err()
		}
		return err
	}
	return nil
}

func waitRemote(ctx context.Context, op lxd.RemoteOperation) error {
	done := make(chan error, 1)
	go func() {
		done <- op.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = op.CancelTarget()
		return ctx.Err()
	}
}
