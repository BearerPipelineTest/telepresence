package connector

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/datawire/dlib/dexec"
	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dlog"
	"github.com/datawire/telepresence2/rpc/v2/daemon"
)

// worker names
const (
	K8sBridgeWorker = "K8S"
	K8sSSHWorker    = "SSH"
)

// ProxyRedirPort is the port to which we redirect proxied IPs. It
// should probably eventually be configurable and/or dynamically
// chosen.
const ProxyRedirPort = "1234"

// teleproxy holds the configuration for a Teleproxy
type bridge struct {
	sshPort   int32
	namespace string
	daemon    daemon.DaemonClient
	cancel    context.CancelFunc
}

func newBridge(namespace string, daemon daemon.DaemonClient, sshPort int32) *bridge {
	return &bridge{
		namespace: namespace,
		daemon:    daemon,
		sshPort:   sshPort,
	}
}

func (br *bridge) start(c context.Context) error {
	if err := checkKubectl(c); err != nil {
		return err
	}
	c, br.cancel = context.WithCancel(c)

	g := dgroup.ParentGroup(c)
	g.Go(K8sSSHWorker, br.sshWorker)
	g.Go(K8sBridgeWorker, br.bridgeWorker)
	return nil
}

func (br *bridge) bridgeWorker(c context.Context) error {
	// setup kubernetes bridge
	dlog.Infof(c, "kubernetes namespace=%s", br.namespace)
	paths := []string{
		br.namespace + ".svc.cluster.local.",
		"svc.cluster.local.",
		"cluster.local.",
		"",
	}
	dlog.Infof(c, "Setting DNS search path: %s", paths[0])
	_, err := br.daemon.SetDnsSearchPath(c, &daemon.Paths{Paths: paths})
	if err != nil {
		if c.Err() != nil {
			err = nil
		} else {
			err = fmt.Errorf("error setting up search path: %v", err)
		}
	}
	return err
}

func (br *bridge) sshWorker(c context.Context) error {
	// XXX: probably need some kind of keepalive check for ssh, first
	// curl after wakeup seems to trigger detection of death
	ssh := dexec.CommandContext(c, "ssh",

		"-F", "none", // don't load the user's config file

		// connection settings
		"-C", // compression
		"-oConnectTimeout=5",
		"-oStrictHostKeyChecking=no",     // don't bother checking the host key...
		"-oUserKnownHostsFile=/dev/null", // and since we're not checking it, don't bother remembering it either

		// port-forward settings
		"-N", // no remote command; just connect and forward ports
		"-oExitOnForwardFailure=yes",
		"-D", "localhost:1080",

		// where to connect to
		"-p", strconv.Itoa(int(br.sshPort)),
		"telepresence@localhost",
	)
	err := ssh.Run()
	if err != nil && c.Err() != nil {
		err = nil
	}
	return err
}

const kubectlErr = "kubectl version 1.10 or greater is required"

func checkKubectl(c context.Context) error {
	output, err := dexec.CommandContext(c, "kubectl", "version", "--client", "-o", "json").Output()
	if err != nil {
		return errors.Wrap(err, kubectlErr)
	}

	var info struct {
		ClientVersion struct {
			Major string
			Minor string
		}
	}

	if err = json.Unmarshal(output, &info); err != nil {
		return errors.Wrap(err, kubectlErr)
	}

	major, err := strconv.Atoi(info.ClientVersion.Major)
	if err != nil {
		return errors.Wrap(err, kubectlErr)
	}
	minor, err := strconv.Atoi(info.ClientVersion.Minor)
	if err != nil {
		return errors.Wrap(err, kubectlErr)
	}

	if major != 1 || minor < 10 {
		return errors.Errorf("%s (found %d.%d)", kubectlErr, major, minor)
	}
	return nil
}

// check checks the status of teleproxy bridge by doing the equivalent of
//  curl http://traffic-manager.svc:8022.
// Note there is no namespace specified, as we are checking for bridge status in the
// current namespace.
func (br *bridge) check(c context.Context) bool {
	address := fmt.Sprintf("localhost:%d", br.sshPort)
	conn, err := net.DialTimeout("tcp", address, 15*time.Second)
	if err != nil {
		dlog.Errorf(c, "fail to establish tcp connection to %s: %v", address, err)
		return false
	}
	defer conn.Close()

	msg, _, err := bufio.NewReader(conn).ReadLine()
	if err != nil {
		dlog.Errorf(c, "tcp read: %v", err)
		return false
	}
	if !strings.Contains(string(msg), "SSH") {
		dlog.Errorf(c, "expected SSH prompt, got: %v", string(msg))
		return false
	}
	return true
}
