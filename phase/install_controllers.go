package phase

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/k0sproject/k0sctl/pkg/apis/k0sctl.k0sproject.io/v1beta1"
	"github.com/k0sproject/k0sctl/pkg/apis/k0sctl.k0sproject.io/v1beta1/cluster"
	"github.com/k0sproject/k0sctl/pkg/node"
	"github.com/k0sproject/k0sctl/pkg/retry"
	"github.com/k0sproject/rig/exec"
	log "github.com/sirupsen/logrus"
)

// InstallControllers installs k0s controllers and joins them to the cluster
type InstallControllers struct {
	GenericPhase
	hosts  cluster.Hosts
	leader *cluster.Host
}

// Title for the phase
func (p *InstallControllers) Title() string {
	return "Install controllers"
}

// Prepare the phase
func (p *InstallControllers) Prepare(config *v1beta1.Cluster) error {
	p.Config = config
	p.leader = p.Config.Spec.K0sLeader()
	p.hosts = p.Config.Spec.Hosts.Controllers().Filter(func(h *cluster.Host) bool {
		return !h.Reset && !h.Metadata.NeedsUpgrade && (h != p.leader && h.Metadata.K0sRunningVersion == nil)
	})
	log.Debug("hosts selected for phase:")
	for _, h := range p.hosts {
		log.Debugf("  - %s", h)
	}
	log.Debug("leader:")
	log.Debugf(" - %s", p.leader)

	return nil
}

// ShouldRun is true when there are controllers
func (p *InstallControllers) ShouldRun() bool {
	return len(p.hosts) > 0
}

// CleanUp cleans up the environment override files on hosts
func (p *InstallControllers) CleanUp() {
	_ = p.After()
	_ = p.hosts.Filter(func(h *cluster.Host) bool {
		return !h.Metadata.Ready
	}).ParallelEach(func(h *cluster.Host) error {
		log.Infof("%s: cleaning up", h)
		if len(h.Environment) > 0 {
			if err := h.Configurer.CleanupServiceEnvironment(h, h.K0sServiceName()); err != nil {
				log.Warnf("%s: failed to clean up service environment: %v", h, err)
			}
		}
		if h.Metadata.K0sInstalled && p.IsWet() {
			if err := h.Exec(h.Configurer.K0sCmdf("reset --data-dir=%s", h.K0sDataDir()), exec.Sudo(h)); err != nil {
				log.Warnf("%s: k0s reset failed", h)
			}
		}
		return nil
	})
}

func (p *InstallControllers) After() error {
	for i, h := range p.hosts {
		h.Metadata.K0sTokenData.Token = ""
		if h.Metadata.K0sTokenData.Token == "" {
			continue
		}
		err := p.Wet(p.leader, fmt.Sprintf("invalidate k0s join token for controller %s", h), func() error {
			log.Debugf("%s: invalidating join token for controller %d", p.leader, i+1)
			return p.leader.Exec(p.leader.Configurer.K0sCmdf("token invalidate --data-dir=%s %s", p.leader.K0sDataDir(), h.Metadata.K0sTokenData.ID), exec.Sudo(p.leader))
		})
		if err != nil {
			log.Warnf("%s: failed to invalidate worker join token: %v", p.leader, err)
		}
		_ = p.Wet(h, "overwrite k0s join token file", func() error {
			if err := h.Configurer.WriteFile(h, h.K0sJoinTokenPath(), "# overwritten by k0sctl after join\n", "0600"); err != nil {
				log.Warnf("%s: failed to overwrite the join token file at %s", h, h.K0sJoinTokenPath())
			}
			return nil
		})
	}
	return nil
}

// Run the phase
func (p *InstallControllers) Run() error {
	for _, h := range p.hosts {
		if p.IsWet() {
			log.Infof("%s: generate join token for %s", p.leader, h)
			token, err := p.Config.Spec.K0s.GenerateToken(
				p.leader,
				"controller",
				time.Duration(10)*time.Minute,
			)
			if err != nil {
				return err
			}
			tokenData, err := cluster.ParseToken(token)
			if err != nil {
				return err
			}
			h.Metadata.K0sTokenData = tokenData
		} else {
			p.DryMsgf(p.leader, "generate a k0s join token for controller %s", h)
			h.Metadata.K0sTokenData.ID = "dry-run"
			h.Metadata.K0sTokenData.URL = p.Config.Spec.KubeAPIURL()
		}
	}
	err := p.parallelDo(p.hosts, func(h *cluster.Host) error {
		if p.IsWet() || !p.leader.Metadata.DryRunFakeLeader {
			log.Infof("%s: validating api connection to %s", h, h.Metadata.K0sTokenData.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := retry.Context(ctx, func(_ context.Context) error {
				out, err := h.ExecOutput(h.Configurer.KubectlCmdf(h, h.K0sDataDir(), "get --raw='/healthz'"), exec.Sudo(h), exec.StreamOutput(), exec.Stdin(string(h.Metadata.K0sTokenData.Kubeconfig)))
				if out != "ok" {
					return fmt.Errorf("kubernetes api /healthz responded with %q", out)
				}
				if err != nil {
					return fmt.Errorf("failed to connect from controller to kubernetes api - check networking: %w", err)
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("connectivity check failed: %w", err)
			}
		} else {
			log.Warnf("%s: dry-run: skipping api connection validation to because cluster is not actually running", h)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return p.parallelDo(p.hosts, func(h *cluster.Host) error {
		log.Infof("%s: writing join token", h)
		err = p.Wet(h, "write join token", func() error {
			return h.Configurer.WriteFile(h, h.K0sJoinTokenPath(), h.Metadata.K0sTokenData.Token, "0600")
		})
		if err != nil {
			return err
		}

		if p.Config.Spec.K0s.DynamicConfig {
			h.InstallFlags.AddOrReplace("--enable-dynamic-config")
		}

		if Force {
			log.Warnf("%s: --force given, using k0s install with --force", h)
			h.InstallFlags.AddOrReplace("--force=true")
		}

		cmd, err := h.K0sInstallCommand()
		if err != nil {
			return err
		}
		log.Infof("%s: installing k0s controller", h)
		err = p.Wet(h, fmt.Sprintf("install k0s controller using `%s", strings.ReplaceAll(cmd, h.Configurer.K0sBinaryPath(), "k0s")), func() error {
			return h.Exec(cmd, exec.Sudo(h))
		})
		if err != nil {
			return err
		}
		h.Metadata.K0sInstalled = true
		h.Metadata.K0sRunningVersion = p.Config.Spec.K0s.Version

		if p.IsWet() {
			if len(h.Environment) > 0 {
				log.Infof("%s: updating service environment", h)
				if err := h.Configurer.UpdateServiceEnvironment(h, h.K0sServiceName(), h.Environment); err != nil {
					return err
				}
			}

			log.Infof("%s: starting service", h)
			if err := h.Configurer.StartService(h, h.K0sServiceName()); err != nil {
				return err
			}

			log.Infof("%s: waiting for the k0s service to start", h)
			if err := retry.Timeout(context.TODO(), retry.DefaultTimeout, node.ServiceRunningFunc(h, h.K0sServiceName())); err != nil {
				return err
			}

			if err := p.waitJoined(h); err != nil {
				return err
			}

			log.Infof("%s: waiting for system pods to become ready", h)
			if err := retry.Timeout(context.TODO(), 10*time.Minute, node.SystemPodsRunningFunc(h)); err != nil {
				if !Force {
					return fmt.Errorf("all system pods not running after api start-up, you can ignore this check by using --force: %w", err)
				}
				log.Warnf("%s: failed to observe system pods running after api start-up: %s", h, err)
			}

			h.Metadata.Ready = true
		}

		return nil
	})
}

func (p *InstallControllers) waitJoined(h *cluster.Host) error {
	log.Infof("%s: waiting for kubernetes api to respond", h)
	return retry.Timeout(context.TODO(), retry.DefaultTimeout, node.KubeAPIReadyFunc(h, p.Config))
}
