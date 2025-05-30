package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/abiosoft/colima/cli"
	"github.com/abiosoft/colima/config"
	"github.com/abiosoft/colima/environment"
	"github.com/abiosoft/colima/environment/container/containerd"
	"github.com/abiosoft/colima/environment/container/docker"
)

// Name is container runtime name

const (
	Name           = "kubernetes"
	DefaultVersion = "v1.23.6+k3s1"

	configKey = "kubernetes_config"
)

func newRuntime(host environment.HostActions, guest environment.GuestActions) environment.Container {
	return &kubernetesRuntime{
		host:         host,
		guest:        guest,
		CommandChain: cli.New(Name),
	}
}

func init() {
	environment.RegisterContainer(Name, newRuntime)
}

var _ environment.Container = (*kubernetesRuntime)(nil)

type kubernetesRuntime struct {
	host  environment.HostActions
	guest environment.GuestActions
	cli.CommandChain
}

func (c kubernetesRuntime) Name() string {
	return Name
}

func (c kubernetesRuntime) isInstalled() bool {
	// it is installed if uninstall script is present.
	return c.guest.RunQuiet("command", "-v", "k3s-uninstall.sh") == nil
}
func (c kubernetesRuntime) isVersionInstalled(version string) bool {
	// validate version change via cli flag/config.
	out, err := c.guest.RunOutput("k3s", "--version")
	if err != nil {
		return false
	}
	return strings.Contains(out, version)
}

func (c kubernetesRuntime) Running() bool {
	return c.guest.RunQuiet("sudo", "service", "k3s", "status") == nil
}

func (c kubernetesRuntime) runtime() string {
	return c.guest.Get(environment.ContainerRuntimeKey)
}

func (c kubernetesRuntime) config() config.Kubernetes {
	conf := config.Kubernetes{Version: DefaultVersion}
	if b := c.guest.Get(configKey); b != "" {
		_ = json.Unmarshal([]byte(b), &conf)
	}
	return conf
}

func (c kubernetesRuntime) setConfig(conf config.Kubernetes) error {
	b, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("error encoding kubernetes config to json: %w", err)
	}

	return c.guest.Set(configKey, string(b))
}

func (c *kubernetesRuntime) Provision(ctx context.Context) error {
	log := c.Logger()
	a := c.Init()
	if c.Running() {
		return nil
	}

	appConf, ok := ctx.Value(config.CtxKey()).(config.Config)
	runtime := appConf.Runtime
	conf := appConf.Kubernetes

	if !ok {
		// this should be a restart/start while vm is active
		// retrieve value in the vm
		runtime = c.runtime()
		conf = c.config()
	}

	if c.isVersionInstalled(conf.Version) {
		// runtime has changed, ensure the required images are in the registry
		if currentRuntime := c.runtime(); currentRuntime != "" && currentRuntime != runtime {
			a.Stagef("changing runtime to %s", runtime)
			installK3sCache(c.host, c.guest, a, log, runtime, conf.Version)
		}
		// other settings may have changed e.g. ingress
		installK3sCluster(c.host, c.guest, a, runtime, conf.Version, conf.Ingress)
	} else {
		if c.isInstalled() {
			a.Stagef("version changed to %s, downloading and installing", conf.Version)
		} else {
			if ok {
				a.Stage("downloading and installing")
			} else {
				a.Stage("installing")
			}
		}
		installK3s(c.host, c.guest, a, log, runtime, conf.Version, conf.Ingress)
	}

	// this needs to happen on each startup
	switch runtime {
	case containerd.Name:
		installContainerdDeps(c.guest, a)
	case docker.Name:
		a.Retry("waiting for docker cri", time.Second*2, 5, func(int) error {
			return c.guest.Run("sudo", "service", "cri-dockerd", "start")
		})
	}

	// provision successful, now we can persist the version
	a.Add(func() error { return c.setConfig(conf) })

	return a.Exec()
}

func (c kubernetesRuntime) Start(context.Context) error {
	log := c.Logger()
	a := c.Init()
	if c.Running() {
		log.Println("already running")
		return nil
	}

	a.Stage("starting")

	a.Add(func() error {
		return c.guest.Run("sudo", "service", "k3s", "start")
	})
	a.Retry("", time.Second*2, 10, func(int) error {
		return c.guest.RunQuiet("kubectl", "cluster-info")
	})

	if err := a.Exec(); err != nil {
		return err
	}

	return c.provisionKubeconfig()
}

func (c kubernetesRuntime) Stop(context.Context) error {
	a := c.Init()
	a.Stage("stopping")
	a.Add(func() error {
		return c.guest.Run("k3s-killall.sh")
	})

	// k3s is buggy with external containerd for now
	// cleanup is manual
	a.Add(c.stopAllContainers)

	return a.Exec()
}

func (c kubernetesRuntime) deleteAllContainers() error {
	ids := c.runningContainerIDs()
	if ids == "" {
		return nil
	}

	var args []string

	switch c.runtime() {
	case containerd.Name:
		args = []string{"nerdctl", "-n", "k8s.io", "rm", "-f"}
	case docker.Name:
		args = []string{"docker", "rm", "-f"}
	default:
		return nil
	}

	args = append(args, strings.Fields(ids)...)

	return c.guest.Run("sudo", "sh", "-c", strings.Join(args, " "))
}

func (c kubernetesRuntime) stopAllContainers() error {

	ids := c.runningContainerIDs()
	if ids == "" {
		return nil
	}

	var args []string

	switch c.runtime() {
	case containerd.Name:
		args = []string{"nerdctl", "-n", "k8s.io", "kill"}
	case docker.Name:
		args = []string{"docker", "kill"}
	default:
		return nil
	}

	args = append(args, strings.Fields(ids)...)

	return c.guest.Run("sudo", "sh", "-c", strings.Join(args, " "))
}

func (c kubernetesRuntime) runningContainerIDs() string {
	var args []string

	switch c.runtime() {
	case containerd.Name:
		args = []string{"sudo", "nerdctl", "-n", "k8s.io", "ps", "-q"}
	case docker.Name:
		args = []string{"sudo", "sh", "-c", `docker ps --format '{{.Names}}'| grep "k8s_"`}
	default:
		return ""
	}

	ids, _ := c.guest.RunOutput(args...)
	if ids == "" {
		return ""
	}
	return strings.ReplaceAll(ids, "\n", " ")
}

func (c kubernetesRuntime) Teardown(context.Context) error {
	a := c.Init()
	a.Stage("deleting")

	if c.isInstalled() {
		a.Add(func() error {
			return c.guest.Run("k3s-uninstall.sh")
		})
	}

	// k3s is buggy with external containerd for now
	// cleanup is manual
	a.Add(func() error {
		return c.deleteAllContainers()
	})

	c.teardownKubeconfig(a)

	return a.Exec()
}

func (c kubernetesRuntime) Dependencies() []string {
	return []string{"kubectl"}
}

func (c kubernetesRuntime) Version() string {
	version, _ := c.host.RunOutput("kubectl", "--context", config.Profile().ID, "version", "--short")
	return version
}
