package kubernetes

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/abiosoft/colima/cli"
	"github.com/abiosoft/colima/config"
	"github.com/abiosoft/colima/environment/vm/lima"
)

const masterAddressKey = "master_address"

func (c kubernetesRuntime) provisionKubeconfig() error {
	ip := lima.IPAddress(config.Profile().ID)
	if ip == c.guest.Get(masterAddressKey) {
		return nil
	}

	log := c.Logger()
	a := c.Init()

	a.Stage("updating config")

	// remove existing configs (if any)
	// this is safe as the profile name is unique to colima
	c.unsetKubeconfig(a)

	// ensure host kube directory exists
	hostHome := c.host.Env("HOME")
	if hostHome == "" {
		return fmt.Errorf("error retrieving home directory on host")
	}

	profile := config.Profile().ID
	hostKubeDir := filepath.Join(hostHome, ".kube")
	a.Add(func() error {
		return c.host.Run("mkdir", "-p", filepath.Join(hostKubeDir, "."+profile))
	})

	kubeconfFile := filepath.Join(hostKubeDir, "config")
	tmpkubeconfFile := filepath.Join(hostKubeDir, "."+profile, "colima-temp")

	// manipulate in VM and save to host
	a.Add(func() error {
		kubeconfig, err := c.guest.Read("/etc/rancher/k3s/k3s.yaml")
		if err != nil {
			return fmt.Errorf("error fetching kubeconfig on guest: %w", err)
		}
		// replace name
		kubeconfig = strings.ReplaceAll(kubeconfig, ": default", ": "+profile)

		// save on the host
		return c.host.Write(tmpkubeconfFile, kubeconfig)
	})

	// merge on host
	a.Add(func() (err error) {
		// prepare new host with right env var.
		envVar := fmt.Sprintf("KUBECONFIG=%s:%s", kubeconfFile, tmpkubeconfFile)
		host := c.host.WithEnv(envVar)

		// get merged config
		kubeconfig, err := host.RunOutput("kubectl", "config", "view", "--raw")
		if err != nil {
			return err
		}

		// save
		return host.Write(tmpkubeconfFile, kubeconfig)
	})

	// backup current settings and save new config
	a.Add(func() error {
		// backup existing file if exists
		if stat, err := c.host.Stat(kubeconfFile); err == nil && !stat.IsDir() {
			backup := filepath.Join(filepath.Dir(tmpkubeconfFile), fmt.Sprintf("config-bak-%d", time.Now().Unix()))
			if err := c.host.Run("cp", kubeconfFile, backup); err != nil {
				return fmt.Errorf("error backing up kubeconfig: %w", err)
			}
		}
		// save new config
		if err := c.host.Run("cp", tmpkubeconfFile, kubeconfFile); err != nil {
			return fmt.Errorf("error updating kubeconfig: %w", err)
		}

		return nil
	})

	// set new context
	a.Add(func() error {
		out, err := c.host.RunOutput("kubectl", "config", "use-context", profile)
		if err != nil {
			return err
		}
		log.Println(out)
		return nil
	})

	// save settings
	a.Add(func() error {
		return c.guest.Set(masterAddressKey, ip)
	})

	return a.Exec()
}

func (c kubernetesRuntime) unsetKubeconfig(a *cli.ActiveCommandChain) {
	profile := config.Profile().ID
	a.Add(func() error {
		return c.host.Run("kubectl", "config", "unset", "users."+profile)
	})
	a.Add(func() error {
		return c.host.Run("kubectl", "config", "unset", "contexts."+profile)
	})
	a.Add(func() error {
		return c.host.Run("kubectl", "config", "unset", "clusters."+profile)
	})
}

func (c kubernetesRuntime) teardownKubeconfig(a *cli.ActiveCommandChain) {
	a.Stage("reverting config")
	c.unsetKubeconfig(a)
	a.Add(func() error {
		return c.guest.Set(masterAddressKey, "")
	})
}
