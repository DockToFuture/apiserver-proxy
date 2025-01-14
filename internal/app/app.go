// SPDX-FileCopyrightText: 2020 SAP SE or an SAP affiliate company and Gardener contributors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/xerrors"
	"k8s.io/klog/v2"
	"k8s.io/utils/exec"

	utiliptables "github.com/gardener/apiserver-proxy/internal/iptables"
	"github.com/gardener/apiserver-proxy/internal/netif"
)

// NewSidecarApp returns a new instance of SidecarApp by applying the specified config params.
func NewSidecarApp(params *ConfigParams) (*SidecarApp, error) {
	c := &SidecarApp{params: params}

	ip, err := netip.ParseAddr(c.params.IPAddress)
	if err != nil {
		return nil, xerrors.Errorf("unable to parse IP address %q - %v", c.params.IPAddress, err)
	}

	addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", c.params.IPAddress, ip.BitLen()))
	if err != nil || addr == nil {
		return nil, xerrors.Errorf("unable to parse IP address %q - %v", c.params.IPAddress, err)
	}

	c.localIP = addr

	klog.Infof("Using IP address %q", params.IPAddress)

	return c, nil
}

// TeardownNetworking removes all custom iptables rules and network interface added by node-cache
func (c *SidecarApp) TeardownNetworking() error {
	klog.Infof("Cleaning up")

	err := c.netManager.RemoveIPAddress()

	if c.params.SetupIptables {
		for _, rule := range c.iptablesRules {
			exists := true
			for exists {
				err := c.iptables.DeleteRule(rule.table, rule.chain, rule.args...)
				if err != nil {
					klog.Errorf("Error deleting iptables rule %v - %s", rule, err)
				}
				exists, _ = c.iptables.EnsureRule(utiliptables.Prepend, rule.table, rule.chain, rule.args...)
			}
			// Delete the rule one last time since EnsureRule creates the rule if it doesn't exist
			err := c.iptables.DeleteRule(rule.table, rule.chain, rule.args...)
			if err != nil {
				klog.Errorf("Error deleting iptables rule %v - %s", rule, err)
			}
		}
	}

	return err
}

func (c *SidecarApp) getIPTables() utiliptables.Interface {
	// using the localIPStr param since we need ip strings here
	c.iptablesRules = append(c.iptablesRules, []iptablesRule{
		// Match traffic destined for localIp:localPort and set the flows to be NOTRACKED, this skips connection tracking
		{utiliptables.Table("raw"), utiliptables.ChainPrerouting, []string{"-p", "tcp", "-d", c.params.IPAddress,
			"--dport", c.params.LocalPort, "-j", "NOTRACK"}},
		// There are rules in filter table to allow tracked connections to be accepted. Since we skipped connection tracking,
		// need these additional filter table rules.
		{utiliptables.TableFilter, utiliptables.ChainInput, []string{"-p", "tcp", "-d", c.params.IPAddress,
			"--dport", c.params.LocalPort, "-j", "ACCEPT"}},
		// Match traffic from c.params.IPAddress:localPort and set the flows to be NOTRACKED, this skips connection tracking
		{utiliptables.Table("raw"), utiliptables.ChainOutput, []string{"-p", "tcp", "-s", c.params.IPAddress,
			"--sport", c.params.LocalPort, "-j", "NOTRACK"}},
		// Additional filter table rules for traffic frpm c.params.IPAddress:localPort
		{utiliptables.TableFilter, utiliptables.ChainOutput, []string{"-p", "tcp", "-s", c.params.IPAddress,
			"--sport", c.params.LocalPort, "-j", "ACCEPT"}},
		// Skip connection tracking for requests to apiserver-proxy that are locally generated, example - by hostNetwork pods
		{utiliptables.Table("raw"), utiliptables.ChainOutput, []string{"-p", "tcp", "-d", c.params.IPAddress,
			"--dport", c.params.LocalPort, "-j", "NOTRACK"}},
	}...)
	execer := exec.New()

	return utiliptables.New(execer, utiliptables.ProtocolIPv4)
}

func (c *SidecarApp) runPeriodic(ctx context.Context) {
	tick := time.NewTicker(c.params.Interval)

	for {
		select {
		case <-ctx.Done():
			klog.Warningf("Exiting iptables/interface check goroutine")

			return
		case <-tick.C:
			c.runChecks()
		}
	}
}

func (c *SidecarApp) runChecks() {
	if c.params.SetupIptables {
		for _, rule := range c.iptablesRules {
			exists, err := c.iptables.EnsureRule(utiliptables.Prepend, rule.table, rule.chain, rule.args...)

			switch {
			case exists:
				// debug messages can be printed by including "debug" plugin in coreFile.
				klog.V(2).Infof("iptables rule %v for apiserver-proxy-sidecar already exists", rule)

				continue
			case err == nil:
				klog.Infof("Added back apiserver-proxy-sidecar rule - %v", rule)

				continue
			case isLockedErr(err):
				// if we got here, either iptables check failed or adding rule back failed.
				klog.Infof("Error checking/adding iptables rule %v, due to xtables lock in use, retrying in %v",
					rule, c.params.Interval)
			default:
				klog.Errorf("Error adding iptables rule %v - %s", rule, err)
			}
		}
	}

	klog.V(2).Infoln("Ensuring ip address")

	if err := c.netManager.EnsureIPAddress(); err != nil {
		klog.Errorf("Error ensuring ip address: %v", err)
	}

	klog.V(2).Infoln("Ensured ip address")
}

// RunApp invokes the background checks and runs coreDNS as a cache
func (c *SidecarApp) RunApp(ctx context.Context) {
	c.netManager = netif.NewNetifManager(c.localIP, c.params.Interface)

	if c.params.SetupIptables {
		c.iptables = c.getIPTables()
	}

	if c.params.Cleanup {
		defer func() {
			if err := c.TeardownNetworking(); err != nil {
				klog.Fatalf("Failed to clean up - %v", err)
			}

			klog.Infoln("Successfully cleaned up everything. Bye!")
		}()
	}

	c.runChecks()

	if c.params.Daemon {
		klog.Infoln("Running as a daemon")
		// run periodic blocks
		c.runPeriodic(ctx)
	}

	klog.Infoln("Exiting... Bye!")
}

func isLockedErr(err error) bool {
	return strings.Contains(err.Error(), "holding the xtables lock")
}
