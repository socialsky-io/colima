package gvproxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/abiosoft/colima/environment/vm/lima/network/daemon"
	"github.com/abiosoft/colima/util"
	"github.com/containers/gvisor-tap-vsock/pkg/transport"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"github.com/sirupsen/logrus"
)

// New creates a new Process for gvproxy.
func New() daemon.Process {
	return &gvproxyProcess{}
}

func Name() string { return "gvproxy" }

type Socket string

func (s Socket) Unix() string { return "unix://" + s.File() }
func (s Socket) File() string { return strings.TrimPrefix(string(s), "unix://") }

func Info() struct {
	Socket     Socket
	MacAddress string
} {
	return struct {
		Socket     Socket
		MacAddress string
	}{
		Socket:     Socket(filepath.Join(daemon.Dir(), socketFileName)),
		MacAddress: MacAddress(),
	}
}

var _ daemon.Process = (*gvproxyProcess)(nil)

type gvproxyProcess struct{}

func (*gvproxyProcess) Alive(context.Context) error {
	info := Info()
	if _, err := os.Stat(info.Socket.File()); err != nil {
		return fmt.Errorf("error checking gvproxy socket: %w", err)
	}
	return nil
}

// Name implements daemon.BgProcess
func (*gvproxyProcess) Name() string { return Name() }

// Start implements daemon.BgProcess
func (*gvproxyProcess) Start(ctx context.Context) error {
	info := Info()
	return run(ctx, info.Socket)
}

const (
	NetInterface     = "eth1"
	SubProcessEnvVar = "COLIMA_GVPROXY"

	socketFileName = "gvproxy.sock"

	gatewayMacAddress = "5a:94:ef:e4:0c:dd"

	deviceIP  = "192.168.107.2"
	GatewayIP = "192.168.107.1"
	natIP     = "192.168.107.254"
	subnet    = "192.168.107.0/24"

	mtu = 1500
)

var baseHWAddr = net.HardwareAddr{0x5a, 0x94, 0xef}
var macAddress net.HardwareAddr

func MacAddress() string {
	// there is not much concern about the precision of the uniqueness.
	// this can be revisited
	if macAddress == nil {
		sum := util.SHA256Hash(daemon.Dir())
		macAddress = append(macAddress, baseHWAddr...)
		macAddress = append(macAddress, sum[0:3]...)
	}
	return macAddress.String()
}

func configuration() types.Configuration {
	return types.Configuration{
		Debug:             true,
		CaptureFile:       "",
		MTU:               mtu,
		Subnet:            subnet,
		GatewayIP:         GatewayIP,
		GatewayMacAddress: gatewayMacAddress,
		DHCPStaticLeases: map[string]string{
			deviceIP: MacAddress(),
		},
		DNS: []types.Zone{
			{
				Name: "host.",
				Records: []types.Record{
					{
						Name: "docker.internal",
						IP:   net.ParseIP(GatewayIP),
					},
					{
						Name: "lima.internal",
						IP:   net.ParseIP(GatewayIP),
					},
				},
			},
		},
		DNSSearchDomains: searchDomains(),
		NAT: map[string]string{
			natIP: "127.0.0.1",
		},
		GatewayVirtualIPs: []string{natIP},
		Protocol:          types.QemuProtocol,
	}
}

func run(ctx context.Context, qemuSocket Socket) error {
	if _, err := os.Stat(qemuSocket.File()); err == nil {
		if err := os.Remove(qemuSocket.File()); err != nil {
			return fmt.Errorf("error removing existing qemu socket: %w", err)
		}
	}

	conf := configuration()
	vn, err := virtualnetwork.New(&conf)
	if err != nil {
		return err
	}

	logrus.Info("waiting for clients...")

	qemuListener, err := transport.Listen(qemuSocket.Unix())
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		conn, err := qemuListener.Accept()
		if err != nil {
			done <- fmt.Errorf("qemu accept error: %w", err)
			return

		}
		done <- vn.AcceptQemu(ctx, conn)
	}()

	select {
	case <-ctx.Done():
	case err := <-done:
		if err != nil {
			logrus.Errorf("virtual network err: %q", err)
		}
	}

	if err := qemuListener.Close(); err != nil {
		logrus.Errorf("error closing %s: %q", qemuSocket, err)
	}
	if _, err := os.Stat(qemuSocket.File()); err == nil {
		return os.Remove(qemuSocket.File())
	}
	return nil
}

func searchDomains() []string {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil
	}

	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		logrus.Errorf("open file error: %v", err)
		return nil
	}

	sc := bufio.NewScanner(bytes.NewReader(b))
	searchPrefix := "search "
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), searchPrefix) {
			continue
		}

		searchDomains := strings.Fields(strings.TrimPrefix(sc.Text(), searchPrefix))
		logrus.Infof("Using search domains: %v", searchDomains)
		return searchDomains
	}
	if err := sc.Err(); err != nil {
		logrus.Errorf("scan file error: %v", err)
		return nil
	}

	return nil
}

func (gvproxyProcess) Dependencies() (deps []daemon.Dependency, root bool) {
	return []daemon.Dependency{
		qemuBinsSymlinks{},
		qemuShareDirSymlink{},
	}, false
}
