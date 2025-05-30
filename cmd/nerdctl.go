package cmd

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/abiosoft/colima/cli"
	"github.com/abiosoft/colima/cmd/root"
	"github.com/abiosoft/colima/config"
	"github.com/abiosoft/colima/environment/container/containerd"
	"github.com/spf13/cobra"
)

var nerdctlCmdArgs struct {
	force           bool
	path            string
	usrBinWriteable bool
	isColimaScript  bool
}

// nerdctlCmd represents the nerdctl command
var nerdctlCmd = &cobra.Command{
	Use:     "nerdctl",
	Aliases: []string{"nerd", "n"},
	Short:   "run nerdctl (requires containerd runtime)",
	Long: `Run nerdctl to interact with containerd.
This requires containerd runtime.

It is recommended to specify '--' to differentiate from Colima flags.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		app := newApp()
		r, err := app.Runtime()
		if err != nil {
			return err
		}
		if r != containerd.Name {
			return fmt.Errorf("nerdctl only supports %s runtime", containerd.Name)
		}

		nerdctlArgs := append([]string{"sudo", "nerdctl"}, args...)
		return app.SSH(nerdctlArgs...)
	},
}

// nerdctlLinkFunc represents the nerdctl command
var nerdctlLinkFunc = func() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "install nerdctl alias script on the host",
		Long:  `Install nerdctl alias script on the host. The script will be installed at ` + nerdctlDefaultInstallPath + `.`,
		Args:  cobra.NoArgs,
		PreRun: func(cmd *cobra.Command, args []string) {
			// check if /usr/local/bin is writeable and no need for sudo

			// if the path is user-specified, ignore.
			if nerdctlCmdArgs.path != nerdctlDefaultInstallPath {
				return
			}

			// attempt writing to the /usr/local/bin
			tmpFile := filepath.Join(filepath.Dir(nerdctlDefaultInstallPath), "colima.tmp")
			if err := os.WriteFile(tmpFile, []byte("tmp"), 0777); err == nil {
				nerdctlCmdArgs.usrBinWriteable = true
				_ = os.Remove(tmpFile)
			}

			// check if the current file (if exists) is generated by colima
			// in such case no need for confirmation before overwrite
			// TODO: this is too basic, should be better
			if b, err := os.ReadFile(nerdctlCmdArgs.path); err == nil {
				if strings.Contains(string(b), "colima nerdctl ") {
					nerdctlCmdArgs.isColimaScript = true
				}
			}
		},

		RunE: func(cmd *cobra.Command, args []string) error {
			exists := false
			if _, err := os.Stat(nerdctlCmdArgs.path); err == nil && !nerdctlCmdArgs.force && !nerdctlCmdArgs.isColimaScript {
				return fmt.Errorf("%s exists, use --force to replace", nerdctlCmdArgs.path)
			} else if err == nil {
				exists = true
			}

			t, err := template.New("").Parse(nerdctlScript)
			if err != nil {
				return fmt.Errorf("error parsing nerdctl script template: %w", err)
			}
			var values = struct {
				ColimaApp string
				Profile   string
			}{
				ColimaApp: os.Args[0],
				Profile:   strings.TrimPrefix(config.Profile().ID, "colima-"),
			}
			var buf bytes.Buffer
			if err := t.Execute(&buf, values); err != nil {
				return fmt.Errorf("error applying nerdctl script template: %w", err)
			}

			// /usr/local/bin writeable i.e. sudo not needed
			// or user-specified install path, we assume user specified path is writeable
			if nerdctlCmdArgs.usrBinWriteable || nerdctlCmdArgs.path != nerdctlDefaultInstallPath {
				if exists {
					if err := os.Rename(nerdctlCmdArgs.path, nerdctlCmdArgs.path+".moved"); err != nil {
						return fmt.Errorf("error backing up existing file: %w", err)
					}
				}
				return os.WriteFile(nerdctlCmdArgs.path, buf.Bytes(), 0755)
			}

			// sudo is needed for the default path
			log.Println("/usr/local/bin not writable, sudo password required to install nerdctl binary")
			if exists && !nerdctlCmdArgs.isColimaScript {
				c := cli.CommandInteractive("sudo", "mv", nerdctlCmdArgs.path, nerdctlCmdArgs.path+".moved")
				if err := c.Run(); err != nil {
					return fmt.Errorf("error backing up existing file: %w", err)
				}
			}
			{
				c := cli.CommandInteractive("sudo", "sh", "-c", "cat > "+nerdctlCmdArgs.path)
				c.Stdin = &buf
				if err := c.Run(); err != nil {
					return err
				}
			}
			// ensure it is executable
			if err := cli.Command("sudo", "chmod", "+x", nerdctlCmdArgs.path).Run(); err != nil {
				return err
			}

			return nil
		},
	}
}

const nerdctlDefaultInstallPath = "/usr/local/bin/nerdctl"

const nerdctlScript = `#!/usr/bin/env sh

{{.ColimaApp}} nerdctl --profile {{.Profile}} -- "$@"
`

func init() {
	root.Cmd().AddCommand(nerdctlCmd)

	nerdctlLink := nerdctlLinkFunc()
	nerdctlCmd.AddCommand(nerdctlLink)
	nerdctlLink.Flags().BoolVarP(&nerdctlCmdArgs.force, "force", "f", false, "replace "+nerdctlDefaultInstallPath+" (if exists)")
	nerdctlLink.Flags().StringVar(&nerdctlCmdArgs.path, "path", nerdctlDefaultInstallPath, "path to install nerdctl binary")
}
